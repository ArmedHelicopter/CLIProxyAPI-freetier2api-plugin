package workbuddy

// 本文件解析 WorkBuddy 的供应商专属配置，并保存出站请求需要的运行时快照。
//
// 归属边界：UA/设备令牌/提示词/机器盐都只影响 WorkBuddy 的上游协议，
// 由本包自己解释；根层只把全局设置与原始键交进来（core.Configurable）。

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

const maxPromptFileSize = 1 << 20

// runtimeConfig 是所有 Client 共享的配置快照。
type runtimeConfig struct {
	enabledRealms        []string
	sanitizeFingerprints bool
	passthroughIP        bool
	stateDir             string

	promptMode string
	promptText string

	userAgent     string
	clientVersion string
	cliVersion    string
	clientName    string

	deviceToken     string
	deviceTokenFile string
}

var (
	runtimeMu  sync.RWMutex
	runtimeCfg = defaultRuntimeConfig()
)

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		sanitizeFingerprints: true,
		clientVersion:        DefaultClientVersion,
		cliVersion:           DefaultCLIVersion,
		clientName:           DefaultClientName,
		promptMode:           PromptModePassThrough,
		promptText:           BuiltinPrompt(),
	}
}

// Configure 解析 WorkBuddy 专属键并更新运行时快照。
//
// 未知键一律忽略：配置里保留其它供应商的键不该让本包报错。
func Configure(settings core.Settings, raw map[string]string) {
	cfg := defaultRuntimeConfig()
	cfg.enabledRealms = append([]string(nil), settings.EnabledRealms...)
	cfg.sanitizeFingerprints = ParseBool(raw["workbuddy_sanitize_fingerprints"], true)
	cfg.passthroughIP = ParseBool(raw["workbuddy_passthrough_ip"], false)
	cfg.stateDir = settings.StateDir

	if value := strings.TrimSpace(raw["workbuddy_user_agent"]); value != "" {
		cfg.userAgent = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_version"]); value != "" {
		cfg.clientVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_cli_version"]); value != "" {
		cfg.cliVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_name"]); value != "" {
		cfg.clientName = value
	}
	cfg.deviceToken = strings.TrimSpace(raw["workbuddy_device_token"])
	cfg.deviceTokenFile = strings.TrimSpace(raw["workbuddy_device_token_file"])

	if value := strings.ToLower(strings.TrimSpace(raw["workbuddy_prompt_mode"])); value != "" {
		cfg.promptMode = value
	}
	if path := strings.TrimSpace(raw["workbuddy_prompt_file"]); path != "" {
		text, errRead := readPromptFile(path)
		if errRead != nil {
			// 不静默降级：运维会以为提示词生效了，实际还在用内置的。
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errRead)
		} else if text != "" {
			cfg.promptText = text
		}
	}
	SetPrompt(cfg.promptMode, cfg.promptText)
	SetInstallSalt(settings.MachineSalt)

	runtimeMu.Lock()
	runtimeCfg = cfg
	runtimeMu.Unlock()
}

func readPromptFile(path string) (string, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", errRead
	}
	if len(raw) > maxPromptFileSize {
		return "", fmt.Errorf("prompt file %s exceeds %d bytes", path, maxPromptFileSize)
	}
	return strings.TrimSpace(string(raw)), nil
}

// currentOptions 返回一次请求要用的 Options（快照 + 当前提示词降级态）。
func currentOptions(ctx context.Context) Options {
	runtimeMu.RLock()
	cfg := runtimeCfg
	runtimeMu.RUnlock()
	return Options{
		Context:              ctx,
		EnabledRealms:        append([]string(nil), cfg.enabledRealms...),
		PromptMode:           PromptMode(),
		PromptText:           PromptText(),
		OnContentBlocked:     MarkPromptDegraded,
		SanitizeFingerprints: cfg.sanitizeFingerprints,
		PassthroughIP:        cfg.passthroughIP,
		UserAgent:            cfg.userAgent,
		ClientVersion:        cfg.clientVersion,
		CLIVersion:           cfg.cliVersion,
		ClientName:           cfg.clientName,
		DeviceToken:          cfg.deviceToken,
		DeviceTokenFile:      cfg.deviceTokenFile,
		StateDir:             cfg.stateDir,
	}
}

// ConfigFields 声明 WorkBuddy 专属 YAML 配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "workbuddy_prompt_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"passthrough", "custom", "append"}, Description: "系统提示词处理模式。"},
		{Name: "workbuddy_prompt_file", Type: pluginapi.ConfigFieldTypeString, Description: "自定义提示词文件路径。"},
		{Name: "workbuddy_sanitize_fingerprints", Type: pluginapi.ConfigFieldTypeBoolean, Description: "出站请求体指纹脱敏，默认开启。"},
		{Name: "workbuddy_passthrough_ip", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否透传客户端 IP，默认关闭。"},
		{Name: "workbuddy_user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "出站 User-Agent 覆盖。"},
		{Name: "workbuddy_client_version", Type: pluginapi.ConfigFieldTypeString, Description: "UA 中 WorkBuddy 版本段。"},
		{Name: "workbuddy_cli_version", Type: pluginapi.ConfigFieldTypeString, Description: "UA 中 CLI 版本段。"},
		{Name: "workbuddy_client_name", Type: pluginapi.ConfigFieldTypeString, Description: "归属头客户端名。"},
		{Name: "workbuddy_device_token", Type: pluginapi.ConfigFieldTypeString, Description: "X-Device-Token 全局兜底。"},
		{Name: "workbuddy_device_token_file", Type: pluginapi.ConfigFieldTypeString, Description: "设备令牌文件路径。"},
	}
}

// RuntimeFields 声明管理页可改字段。
func (v *adapter) RuntimeFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "workbuddy_prompt_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"passthrough", "custom", "append"}, Description: "系统提示词处理模式（页面覆盖 YAML）。"},
	}
}

func ParseBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

// 本文件实现系统提示词体系。
//
// 为什么需要它：客户端的 system prompt 里带着大量固定模板句（工具说明、
// 身份声明、环境描述），上游的内容审核按逐字匹配拦截。把 system 换成
// 网关自有提示词是从源头解决，比逐句改写（sanitize）更彻底。
//
// 三种模式：
//   - passthrough：完全透传客户端 system（默认，最不干扰）；
//   - custom：删除全部 system/developer 消息，换成网关自有提示词；
//   - append：在开头连续的 system/developer 块之后追加一条网关提示词，
//     既有消息逐字保留。
//
// 这是**本供应商特有**的机制：CodeBuddy 的上游按逐字匹配做内容审核，
// 其它供应商的上游未必如此，因此提示词体系属于协议层而非通用核心。

//go:embed prompt_default.md
var defaultPromptRaw []byte

// degradedPrompt 是内容被拦截后降级重试时使用的极简提示词。
//
// 长提示词更容易命中逐字审核，降级到一句中性指令能显著提高通过率。
const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

// 提示词模式取值（与根层 config 的解析结果对齐）。
const (
	// PromptModePassThrough 完全透传客户端 system。
	PromptModePassThrough = "passthrough"
	// PromptModeCustom 替换为网关自有提示词。
	PromptModeCustom = "custom"
	// PromptModeAppend 在既有 system 之后追加网关提示词。
	PromptModeAppend = "append"
)

var (
	promptMu       sync.RWMutex
	promptMode     = PromptModePassThrough
	promptText     = ""
	promptDegraded bool
)

// SetPrompt 设置当前生效的提示词模式与文本。
//
// 由根层在配置热更新时调用：配置结构属于根层，供应商包只接收结果，
// 避免反向依赖 package main。
func SetPrompt(mode, text string) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = PromptModePassThrough
	}
	promptMu.Lock()
	promptMode = mode
	promptText = strings.TrimSpace(text)
	promptMu.Unlock()
}

// BuiltinPrompt 返回内置的默认提示词文本。
func BuiltinPrompt() string {
	return strings.TrimSpace(string(defaultPromptRaw))
}

// PromptMode 返回当前生效的提示词模式。
func PromptMode() string {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptMode
}

// PromptText 返回当前生效的提示词文本（降级态返回极简提示词）。
func PromptText() string {
	if PromptDegraded() {
		return degradedPrompt
	}
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptText
}

// MarkPromptDegraded 标记进入降级态。
//
// 降级态一旦开启就持续到进程重启：上游的内容审核策略变化通常是长期性的，
// 反复在「完整提示词 ↔ 降级提示词」之间来回试探只会让一半请求失败。
func MarkPromptDegraded() {
	promptMu.Lock()
	promptDegraded = true
	promptMu.Unlock()
}

// PromptDegraded 报告是否处于降级态。
func PromptDegraded() bool {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptDegraded
}
