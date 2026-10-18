// Package admin 实现插件的管理接口：路由分发、响应封装与资源页。
//
// 领域数据通过 Deps 注入：管理接口属于 ABI 层，但不应反向依赖 package main。
// 这样账号/模型/额度/设置的处理逻辑可以与插件入口分离，也便于单测。
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/console"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

const (
	// requestTimeout 是管理接口里上游调用的上限。
	requestTimeout = 3 * time.Minute
	// batchConcurrency 是批量操作的默认并发度上限。
	batchConcurrency = 4
)

// Identity 是插件的对外身份。
type Identity struct {
	ID          string
	DisplayName string
	ProviderKey string
	Version     string
}

// Deps 是管理接口依赖的根层能力。
type Deps interface {
	Identity() Identity
	Config() pluginconfig.Config
	State() state.State
	MutateState(func(*state.State))
	Model() *modelconfig.Manager
	PromptMode() string
	SchedulerStatus() map[string]any
	EffectiveSettings() map[string]any
	// ResolveCredential 取回凭证并解析出供应商。
	ResolveCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error)
	// SaveRefreshedCredential 把续期后的凭证写回宿主与磁盘。
	SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor, entry core.HostAuth, original []byte, updated *core.Credential, filePath string)
	RefreshModels(ctx context.Context) (int, error)
	RecordCheckin(*core.Credential, *core.CheckinResult)
	PokeScheduler()
	RunScheduled(kind string)
	// TaskRoutes 处理任务相关路由（实现留在根层，因为要共享队列与账号锁）。
	TaskRoutes(req pluginapi.ManagementRequest, route string) (pluginapi.ManagementResponse, bool)
}

var deps Deps

// Configure 注入依赖。由根层在 init 中调用一次。
func Configure(d Deps) { deps = d }

// Enabled 报告管理接口是否已注入依赖（测试未注入时返回 false）。
func Enabled() bool { return deps != nil }

// Registration 返回管理路由与资源页声明。
func Registration() pluginapi.ManagementRegistrationResponse {
	id := deps.Identity()
	prefix := "/plugins/" + id.ID
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: "GET", Path: prefix + "/status", Description: "账号、额度与任务状态概览。"},
			{Method: "GET", Path: prefix + "/vendors", Description: "列出已启用的供应商（供添加账号下拉与分组展示）。"},
			{Method: "POST", Path: prefix + "/checkin", Description: "对指定或全部账号执行签到。"},
			{Method: "POST", Path: prefix + "/quotas", Description: "批量查询账号额度。"},
			{Method: "POST", Path: prefix + "/accounts/delete", Description: "删除指定账号的凭证文件（宿主监听 auth 目录，会自动注销该账号）。"},
			{Method: "POST", Path: prefix + "/accounts/add", Description: "添加 API Key 类账号。"},
			{Method: "GET", Path: prefix + "/models", Description: "读取当前注册的模型清单。"},
			{Method: "POST", Path: prefix + "/models/toggle", Description: "批量禁用/启用模型。"},
			{Method: "POST", Path: prefix + "/models/alias", Description: "给模型设置/清除对外别名（跨供应商同名可合并路由）。"},
			{Method: "POST", Path: prefix + "/models/refresh", Description: "从上游拉取模型清单并缓存。"},
			{Method: "GET", Path: prefix + "/logs", Description: "读取插件日志（增量）。"},
			{Method: "POST", Path: prefix + "/settings", Description: "更新插件运行期设置（自动签到、任务排程、提示词）。"},
			{Method: "POST", Path: prefix + "/tasks/scan", Description: "扫描全部账号的待办任务。"},
			{Method: "POST", Path: prefix + "/tasks/run", Description: "执行任务队列（账号内串行、账号间并发）。"},
			{Method: "GET", Path: prefix + "/tasks/queue", Description: "查询任务队列进度。"},
			{Method: "POST", Path: prefix + "/tasks/auto", Description: "对单个账号执行单个任务或一键完成。"},
			{Method: "POST", Path: prefix + "/tasks/auto_all", Description: "对全部账号依次执行一键完成任务。"},
			{Method: "GET", Path: prefix + "/school/status", Description: "开学季活动状态。"},
			{Method: "POST", Path: prefix + "/school/run", Description: "执行开学季闭环。"},
			{Method: "POST", Path: prefix + "/travel", Description: "手动执行猫猫旅行。"},
			{Method: "POST", Path: prefix + "/activity", Description: "手动执行活跃上报。"},
			{Method: "POST", Path: prefix + "/keepalive", Description: "手动执行 token 保活。"},
			{Method: "POST", Path: prefix + "/blackcat", Description: "手动执行夜猫子补足。"},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/console",
			Menu:        id.DisplayName,
			Description: "FreeTier 账号、任务与额度管理页。",
		}},
	}
}

// Handle 是管理接口的总分发。
func Handle(request []byte) ([]byte, error) {
	if deps == nil {
		return nil, core.NewPluginError("admin_not_configured", "management deps are not configured", http.StatusInternalServerError)
	}
	var rpc pluginapi.ManagementRequest
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &rpc); errDecode != nil {
			return nil, core.NewPluginError("invalid_request", "decode request: "+errDecode.Error(), http.StatusBadRequest)
		}
	}
	method := strings.ToUpper(strings.TrimSpace(rpc.Method))
	if method == "" {
		method = http.MethodGet
	}
	id := deps.Identity()

	switch {
	case method == http.MethodGet && matchesResourcePath(rpc.Path, id.ID, "/console"):
		return core.OKEnvelope(HTMLResponse(http.StatusOK, []byte(console.Page("/plugins/"+id.ID))))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/status"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, buildStatusPayload(rpc)))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/vendors"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, buildVendorsPayload()))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/logs"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, buildLogsPayload(rpc)))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/checkin"):
		return core.OKEnvelope(handleCheckinRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/quotas"):
		return core.OKEnvelope(handleQuotasRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/delete"):
		return core.OKEnvelope(handleAccountDeleteRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/add"):
		return core.OKEnvelope(handleAccountAddRequest(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/models"):
		return core.OKEnvelope(handleModelsList(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/toggle"):
		return core.OKEnvelope(modelToggleHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/alias"):
		return core.OKEnvelope(modelAliasHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/refresh"):
		ctx, cancel := managementContext(rpc)
		defer cancel()
		count, errRefresh := deps.RefreshModels(ctx)
		if errRefresh != nil {
			return core.OKEnvelope(ErrorResponse(errRefresh))
		}
		return core.OKEnvelope(JSONResponse(http.StatusOK, map[string]any{"ok": true, "models": count}))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/settings"):
		return core.OKEnvelope(handleSettingsRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/school/run"):
		return core.OKEnvelope(handleScheduledRun(rpc, core.TaskKindSchool))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/travel"):
		return core.OKEnvelope(handleScheduledRun(rpc, core.TaskKindTravel))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/activity"):
		return core.OKEnvelope(handleScheduledRun(rpc, core.TaskKindActivity))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/blackcat"):
		return core.OKEnvelope(handleScheduledRun(rpc, core.TaskKindBlackcat))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/keepalive"):
		return core.OKEnvelope(handleKeepaliveRequest(rpc))
	}

	// 任务路由：扫描/队列/一键完成等需要与根层共享队列状态。
	if suffix, okSuffix := managementSuffix(rpc.Path, id.ID); okSuffix && strings.HasPrefix(suffix, "/tasks/") {
		if response, okResponse := deps.TaskRoutes(rpc, suffix); okResponse {
			return core.OKEnvelope(response)
		}
	}
	if method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/school/status") {
		if response, okResponse := deps.TaskRoutes(rpc, "/school/status"); okResponse {
			return core.OKEnvelope(response)
		}
	}

	return core.OKEnvelope(JSONResponse(http.StatusNotFound, map[string]any{
		"error": "not found", "path": rpc.Path, "method": method,
	}))
}

// matchesManagementPath 判断路径是否命中某个管理子路径。
func matchesManagementPath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	prefix := "/plugins/" + pluginID
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == prefix+suffix || normalized == "/v0/management"+prefix+suffix
}

func matchesResourcePath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == "/v0/resource/plugins/"+pluginID+suffix
}

// managementSuffix 返回去掉前缀后的子路径。
func managementSuffix(path, pluginID string) (string, bool) {
	normalized := normalizeRequestPath(path)
	for _, prefix := range []string{"/plugins/" + pluginID, "/v0/management/plugins/" + pluginID} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimPrefix(normalized, prefix), true
		}
	}
	return "", false
}

func normalizeRequestPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// managementContext 给管理接口里的上游调用一个可取消的上下文。
func managementContext(req pluginapi.ManagementRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	return hostabi.WithCallbackID(ctx, hostCallbackID(req)), cancel
}

func hostCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// HTMLResponse 构造 HTML 管理响应。
func HTMLResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       body,
	}
}

// JSONResponse 构造 JSON 管理响应。
func JSONResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

// ErrorResponse 把内部错误转成带状态码的 JSON 响应。
func ErrorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	if pluginErr, okPlugin := err.(*core.PluginError); okPlugin {
		message = pluginErr.Message
		if pluginErr.HTTPStatus > 0 {
			status = pluginErr.HTTPStatus
		}
	}
	return JSONResponse(status, map[string]any{"error": message, "ok": false})
}

func queryInt64(query map[string][]string, key string, fallback int64) int64 {
	if query == nil {
		return fallback
	}
	values, okValues := query[key]
	if !okValues || len(values) == 0 {
		return fallback
	}
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if errParse != nil {
		return fallback
	}
	return parsed
}

// 供子文件使用的共享助手。

func realmEnabled(cfg pluginconfig.Config, realm string) bool {
	if strings.TrimSpace(realm) == "" {
		return true
	}
	for _, item := range cfg.EnabledRealms {
		if item == realm {
			return true
		}
	}
	return false
}

// hostActiveVendors 返回有凭证的供应商集合。
func hostActiveVendors(ctx context.Context, callbackID string) map[string]bool {
	return hostabi.ActiveVendorSet(ctx, callbackID)
}
