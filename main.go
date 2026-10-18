package main

// freetier2api —— 把多个免费额度供应商接入 CLIProxyAPI 的原生动态库插件。
//
// 本文件包含插件身份、RPC 方法分发、注册响应与供应商注册表（composition root）。
// C ABI 在 cgo.go；宿主面在 host.go；五类能力分别在 auth/models/executor/tasks。
//
// 供应商插桩：宿主 ABI 只允许一个 provider key，因此统一 providerKey="freetier"，
// 供应商靠凭证文件名前缀与文件内 vendor 字段区分，各自实现 core.Vendor。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/cline"
	"freetier2api-plugin/internal/vendors/codearts"
	"freetier2api-plugin/internal/vendors/minimaxcode"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/internal/vendors/qoder"
	"freetier2api-plugin/internal/vendors/trae"
	"freetier2api-plugin/internal/vendors/traesolo"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/vendors/zcode"
)

// pluginVersion 是插件版本，会在注册/状态接口里回给宿主。
//
// 故意写成 var：CI 用 `-ldflags -X main.pluginVersion=<tag>` 把发行版号注入产物，
// 本地直接 ./build.sh 则保留 defaultPluginVersion。
var pluginVersion = defaultPluginVersion

// defaultPluginVersion 是未注入时的版本号。
const defaultPluginVersion = "0.0.10"

// effectivePluginVersion 返回对外上报的版本号。
//
// 防御 -X 传成空值（或构建脚本变量为空）的情况：宁可显示默认版本，
// 也不要让管理端与注册信息里出现空版本号。
func effectivePluginVersion() string {
	if version := strings.TrimSpace(pluginVersion); version != "" {
		return version
	}
	return defaultPluginVersion
}

const (
	// pluginID 必须与动态库文件名一致（freetier2api.so → plugins.configs.freetier2api）。
	pluginID = "freetier2api"
	// pluginDisplayName 是管理端展示名。
	pluginDisplayName = "FreeTier 2API"
	pluginAuthor      = "jiandanc"
	pluginRepository  = "https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin"

	// providerKey 是本插件在 CPA 里占用的 provider 键。
	//
	// 它同时是四处的取值：模型注册 provider、执行器 identifier、
	// auth provider identifier、quota provider identifier。
	//
	// **所有供应商共用这一个键**：宿主的 auth.identifier 只返回单个字符串、
	// 注册期调一次永久缓存，且与 auth 文件的 type 字段严格相等才认领该文件
	// （internal/pluginhost/auth_provider.go:130-147）。因此供应商的区分落在
	// 凭证文件名前缀与文件内的 vendor 字段上，而不是 provider key。
	providerKey = "freetier"

	managementRoutePrefix = "/plugins/" + pluginID
	jsonContentType       = "application/json; charset=utf-8"
	htmlContentType       = "text/html; charset=utf-8"

	// formatChatCompletions 是本插件唯一声明支持的协议格式。
	//
	// 所有供应商统一只处理 chat-completions：Claude / Codex / Gemini 客户端
	// 由宿主翻译成 chat-completions 再交给插件。插件不重复做协议转换，
	// 避免与宿主的能力重复且在两侧产生不一致的分帧行为。
	formatChatCompletions = "chat-completions"
)

// lifecycleRequest 是 plugin.register / plugin.reconfigure 的请求体。
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// registration 是 plugin.register / plugin.reconfigure 的响应体，
// 结构与宿主 internal/pluginhost/rpcRegistration 对齐。
type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

// registrationCapabilities 与宿主 rpcCapabilities 对齐（只声明本插件实现的能力）。
type registrationCapabilities struct {
	ModelProvider bool `json:"model_provider"`
	AuthProvider  bool `json:"auth_provider"`
	Executor      bool `json:"executor"`
	QuotaProvider bool `json:"quota_provider"`
	ManagementAPI bool `json:"management_api"`
	// ExecutorModelScope=both：静态模型与按账号模型都由本执行器承担。
	ExecutorModelScope string `json:"executor_model_scope"`
	// 输入/输出协议：只声明 chat-completions。多协议客户端由宿主翻译。
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

// handleMethod 是插件 ABI 的总入口。
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return handlePluginRegister(request)
	case pluginabi.MethodPluginReconfigure:
		return handlePluginReconfigure(request)
	case pluginabi.MethodPluginQuiesce:
		// 宿主要求停止接收新工作：停掉后台循环即可，在途请求由宿主等待。
		stopBackgroundWork()
		return core.OKEnvelope(struct{}{})

	case pluginabi.MethodModelRegister:
		return handleModelRegister(request)
	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

	case pluginabi.MethodExecutorIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return handleExecutorHTTPRequest(request)

	case pluginabi.MethodQuotaIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodQuotaDescribe:
		return handleQuotaDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return handleQuotaReset(request)

	case pluginabi.MethodManagementRegister:
		return core.OKEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		// 未知方法以信封形式报错，而不是让上层 panic。
		return core.ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// handlePluginRegister 首次注册：读取配置、初始化状态、启动后台循环。
func handlePluginRegister(request []byte) ([]byte, error) {
	pluginLifecycleMu.Lock()
	defer pluginLifecycleMu.Unlock()

	cfg, errConfig := decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.register: %w", errConfig)
	}
	if errApply := applyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.register: %w", errApply)
	}
	if _, errState := loadState(cfg); errState != nil {
		logger.Error("load state failed: %v", errState)
	}
	applyModelCatalog(cfg)
	if !pluginRegistered {
		pluginRegistered = true
		startBackgroundWork()
	}
	logger.Info("registered: realms=%v state_dir=%s prompt=%s",
		cfg.EnabledRealms, cfg.StateDir, cfg.Raw["prompt_mode"])
	return core.OKEnvelope(pluginRegistration())
}

// handlePluginReconfigure 配置热更新。
//
// 不重新加载状态文件（除非 state_dir 变了），避免覆盖内存中的设置；
// 也不重启后台循环——调度循环每轮自取最新配置，因此热改排程无需重启。
func handlePluginReconfigure(request []byte) ([]byte, error) {
	pluginLifecycleMu.Lock()
	defer pluginLifecycleMu.Unlock()

	cfg, errConfig := decodeConfig(configYAMLFrom(request))
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errConfig)
	}
	previous := loadedConfig()
	if errApply := applyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errApply)
	}
	if previous.StateDir != cfg.StateDir {
		if _, errState := loadState(cfg); errState != nil {
			logger.Error("reload state failed: %v", errState)
		}
	}
	applyModelCatalog(cfg)
	logger.Info("reconfigured: realms=%v state_dir=%s", cfg.EnabledRealms, cfg.StateDir)
	return core.OKEnvelope(pluginRegistration())
}

// configYAMLFrom 从生命周期请求里取出配置 YAML。
//
// 解码很宽容：宿主传的是 {"config_yaml": ..., "schema_version": ...}，
// 但不同版本可能带额外字段，解析失败时退回「无配置」而不是拒绝启动。
func configYAMLFrom(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil
	}
	return req.ConfigYAML
}

// pluginRegistration 构造注册响应。
func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          effectivePluginVersion(),
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled_realms", Type: pluginapi.ConfigFieldTypeString,
					Description: "启用的域（逗号分隔）：cn 国内 / global 国际。默认 cn,global。只留 cn 时 global 账号不会被路由到。"},
				{Name: "extra_models", Type: pluginapi.ConfigFieldTypeString,
					Description: "额外注册的模型名（不含 cn: / global: 前缀），逗号分隔。"},
				{Name: "disabled_models", Type: pluginapi.ConfigFieldTypeString,
					Description: "禁用的模型列表（支持厂商限定如 workbuddycn:deepseek-v4-pro、区域别名如 cn:deepseek-v4-pro 或裸模型名），逗号分隔或 YAML 列表。与控制台页面的启用/禁用按钮取并集：配置声明适合批量管理，页面适合临时调整。"},
				{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString,
					Description: "插件状态目录（机器盐、模型缓存、任务记录、日志），默认 ~/.freetier2api-plugin。"},
				{Name: "log_level", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"debug", "info", "error"},
					Description: "插件日志级别，默认 info。"},
				{Name: "log_to_file", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否把日志写入 <state_dir>/logs，默认关闭。"},
				{Name: "prompt_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"passthrough", "custom", "append"},
					Description: "系统提示词处理：passthrough 透传（默认）/ custom 替换为网关提示词 / append 在开头追加。"},
				{Name: "prompt_file", Type: pluginapi.ConfigFieldTypeString,
					Description: "自定义提示词文件路径；留空使用内置默认提示词。"},
				{Name: "sanitize_fingerprints", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "出站请求体黑名单指纹脱敏，默认开启。"},
				{Name: "passthrough_ip", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否把客户端 IP 透传给上游，默认关闭。"},
				{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString,
					Description: "出站 User-Agent 显式覆盖；留空使用内置 WorkBuddy 标识。"},
				{Name: "client_version", Type: pluginapi.ConfigFieldTypeString,
					Description: "UA 中 WorkBuddy 版本段；留空使用内置值。"},
				{Name: "cli_version", Type: pluginapi.ConfigFieldTypeString,
					Description: "UA 中 CLI 版本段；留空使用内置值。"},
				{Name: "device_token", Type: pluginapi.ConfigFieldTypeString,
					Description: "X-Device-Token 全局兜底；账号凭证里的 device_token 优先。"},
				{Name: "machine_salt", Type: pluginapi.ConfigFieldTypeString,
					Description: "设备指纹盐覆盖项；从 workbuddy2api 迁移且希望指纹不变时填原值。"},
				{Name: "auto_checkin", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否每日自动签到，默认关闭。"},
				{Name: "auto_checkin_at", Type: pluginapi.ConfigFieldTypeString,
					Description: "自动签到时间（本地时区 HH:MM），默认 10:00。"},
				{Name: "auto_tasks", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否每日自动跑成长任务闭环（连登兑换、抽奖、旅行、夜猫子），默认关闭。"},
			},
		},
		Capabilities: registrationCapabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			QuotaProvider:         true,
			ManagementAPI:         true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{formatChatCompletions},
			ExecutorOutputFormats: []string{formatChatCompletions},
		},
	}
}

// decodeRequest 解码只带字符串/字节字段的 RPC 请求。
func decodeRequest(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		return fmt.Errorf("decode request: %w", errUnmarshal)
	}
	return nil
}

// registerVendors 注册全部供应商实例。
//
// 顺序与重构前各 vendor_*.go 的 init 顺序一致：core.Vendors() 的返回顺序
// 会进入控制台列表与内容嗅探兜底，顺序变化会让 UI 抖动。
func registerVendors() {
	for _, vendor := range cline.Instances(workbuddy.DefaultContextWindowFor) {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range codearts.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range minimaxcode.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range opencodezen.Instances(workbuddy.DefaultContextWindowFor) {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range qoder.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range trae.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range traesolo.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range workbuddy.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range zcode.Instances() {
		core.RegisterVendor(vendor)
	}
}

// init 在包初始化期完成注册：宿主的 plugin.register 之前就绪，
// 因此任何时刻调用 core.ResolveVendor 都能拿到完整清单。
func init() {
	registerVendors()
}
