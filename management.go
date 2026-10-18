package main

// 本文件是 management 能力的根层薄层：把宿主请求交给 internal/admin，
// 并通过 Deps 提供插件进程内的领域能力（配置、状态、模型、凭证、任务队列）。
//
// 具体的管理接口实现（账号/模型/额度/设置/开学季）在 internal/admin。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

// adminDeps 实现 admin.Deps，把根层的进程级能力注入管理接口。
type adminDeps struct{}

func (adminDeps) Identity() admin.Identity {
	return admin.Identity{
		ID:          pluginID,
		DisplayName: pluginDisplayName,
		ProviderKey: providerKey,
		Version:     effectivePluginVersion(),
	}
}

func (adminDeps) Config() pluginconfig.Config { return loadedConfig() }

func (adminDeps) State() state.State { return snapshotState() }

func (adminDeps) MutateState(fn func(*state.State)) {
	mutateState(fn)
}

func (adminDeps) Model() *modelconfig.Manager { return modelCatalog }

func (adminDeps) PromptMode() string { return promptModeFor() }

func (adminDeps) SchedulerStatus() map[string]any { return schedulerStatus() }

func (adminDeps) EffectiveSettings() map[string]any { return effectiveSettings() }

func (adminDeps) ResolveCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error) {
	return resolveVendorCredential(ctx, callbackID, storageJSON, authID, attrs)
}

func (adminDeps) SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor, entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	saveRefreshedCredential(ctx, callbackID, vendor, entry, original, updated, filePath)
}

func (adminDeps) RefreshModels(ctx context.Context) (int, error) {
	return refreshModelsFromUpstream(ctx)
}

func (adminDeps) RecordCheckin(credential *core.Credential, result *core.CheckinResult) {
	recordCheckinResult(credential, result)
}

func (adminDeps) PokeScheduler() { pokeScheduler() }

func (adminDeps) RunScheduled(kind string) { runScheduledTask(kind) }

// TaskRoutes 把任务相关路由分发给根层实现（共享队列与账号锁）。
func (adminDeps) TaskRoutes(req pluginapi.ManagementRequest, route string) (pluginapi.ManagementResponse, bool) {
	switch route {
	case "/tasks/scan":
		return handleTasksScan(req), true
	case "/tasks/run":
		return handleTasksRun(req), true
	case "/tasks/queue":
		return jsonResponse(http.StatusOK, taskQueueSnapshot()), true
	case "/tasks/auto":
		return handleTasksAuto(req), true
	case "/tasks/auto_all":
		return handleTasksAutoAll(req), true
	case "/school/status":
		ctx, cancel := managementContext(req)
		defer cancel()
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "accounts": schoolStatusForAccounts(ctx, hostCallbackID(req)),
		}), true
	}
	return pluginapi.ManagementResponse{}, false
}

// managementRegistration 返回本插件声明的管理路由与资源页。
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return admin.Registration()
}

// handleManagement 是管理接口的总分发。
func handleManagement(request []byte) ([]byte, error) {
	return admin.Handle(request)
}

// 响应/上下文助手：任务中心仍在根层使用，保持原签名。

func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return admin.HTMLResponse(status, body)
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	return admin.JSONResponse(status, payload)
}

func errorResponse(err error) pluginapi.ManagementResponse {
	return admin.ErrorResponse(err)
}

// buildStatusPayload 保留给测试与兼容调用。
func buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return admin.StatusPayload(req)
}

func managementContext(req pluginapi.ManagementRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), managementRequestTimeout)
	return hostabi.WithCallbackID(ctx, hostCallbackID(req)), cancel
}

func hostCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// managementRequestTimeout 与管理接口的上游调用上限一致。
const managementRequestTimeout = 3 * time.Minute

func init() {
	admin.Configure(adminDeps{})
}
