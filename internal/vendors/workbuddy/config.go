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
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

const maxPromptFileSize = 1 << 20

// runtimeConfig 是单个适配器实例的配置快照。
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

// configure 解析 WorkBuddy 专属键并更新当前实例的运行时快照。
func (r *runtime) configure(settings core.Settings, raw map[string]string) {
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
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errRead)
		} else if text != "" {
			cfg.promptText = text
		}
	}
	r.cfg.Store(&cfg)
	r.prompt.Store(&promptState{Mode: cfg.promptMode, Text: cfg.promptText})
	salt := strings.TrimSpace(settings.MachineSalt)
	r.salt.Store(&salt)
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

// options 返回一次请求要用的 Options（配置快照 + 当前提示词降级态）。
func (r *runtime) options(ctx context.Context) Options {
	cfg := *r.cfg.Load()
	prompt := *r.prompt.Load()
	promptText := prompt.Text
	if prompt.Degraded {
		promptText = degradedPrompt
	}
	return Options{
		Context:              ctx,
		EnabledRealms:        append([]string(nil), cfg.enabledRealms...),
		PromptMode:           prompt.Mode,
		PromptText:           promptText,
		OnContentBlocked:     r.markPromptDegraded,
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

//go:embed prompt_default.md
var defaultPromptRaw []byte

const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

const (
	PromptModePassThrough = "passthrough"
	PromptModeCustom      = "custom"
	PromptModeAppend      = "append"
)

// BuiltinPrompt 返回内置的默认提示词文本。
func BuiltinPrompt() string {
	return strings.TrimSpace(string(defaultPromptRaw))
}

// 本文件是 WorkBuddy 每个适配器实例独占的运行时状态。
//
// 配置、提示词降级态、档位表、设备令牌缓存、机器盐与模型探测宿主面
// 都放在 runtime 里，不再使用包级变量；多个 App 实例互不影响。

type promptState struct {
	Mode     string
	Text     string
	Degraded bool
}

type effortState struct {
	Defaults  map[string]string
	Supported map[string][]string
}

type runtime struct {
	env core.Env

	cfg    atomic.Pointer[runtimeConfig]
	prompt atomic.Pointer[promptState]
	effort atomic.Pointer[effortState]
	salt   atomic.Pointer[string]

	deviceMu    sync.Mutex
	devicePath  string
	deviceToken string
	deviceAt    time.Time

	modelMu    sync.Mutex
	modelCache map[Region]modelCacheEntry
}

func newRuntime(env core.Env) *runtime {
	cfg := defaultRuntimeConfig()
	rt := &runtime{env: env, modelCache: map[Region]modelCacheEntry{}}
	rt.cfg.Store(&cfg)
	rt.prompt.Store(&promptState{Mode: PromptModePassThrough, Text: BuiltinPrompt()})
	rt.effort.Store(&effortState{Defaults: map[string]string{}, Supported: map[string][]string{}})
	salt := ""
	rt.salt.Store(&salt)
	return rt
}

func (r *runtime) markPromptDegraded() {
	current := *r.prompt.Load()
	current.Degraded = true
	r.prompt.Store(&current)
}

func (r *runtime) effortTables() (map[string]string, map[string][]string) {
	state := r.effort.Load()
	return state.Defaults, state.Supported
}

func (r *runtime) setEffortTables(defaults map[string]string, supported map[string][]string) {
	old := r.effort.Load()
	next := &effortState{
		Defaults:  make(map[string]string, len(old.Defaults)+len(defaults)),
		Supported: make(map[string][]string, len(old.Supported)+len(supported)),
	}
	for key, value := range old.Defaults {
		next.Defaults[key] = value
	}
	for key, value := range old.Supported {
		next.Supported[key] = value
	}
	for key, value := range defaults {
		next.Defaults[key] = value
	}
	for key, value := range supported {
		next.Supported[key] = value
	}
	r.effort.Store(next)
}

func (r *runtime) publishEffortTables(models []ModelInfo) {
	defaults := make(map[string]string, len(models))
	supported := make(map[string][]string, len(models))
	for _, model := range models {
		if model.DefaultEffort != "" {
			defaults[model.ID] = model.DefaultEffort
		}
		if len(model.Efforts) > 0 {
			supported[model.ID] = model.Efforts
		}
	}
	r.setEffortTables(defaults, supported)
}

func (r *runtime) deriveID(uid, purpose string) string {
	return deriveIDWithSalt(uid, purpose, *r.salt.Load())
}

func (r *runtime) fingerprint(credential *Credential, purpose string) string {
	return r.deriveID(credential.UIDValue(), purpose)
}

func (r *runtime) readDeviceTokenFile(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	r.deviceMu.Lock()
	defer r.deviceMu.Unlock()
	if r.devicePath == trimmed && time.Since(r.deviceAt) < deviceTokenFileTTL {
		return r.deviceToken
	}
	r.devicePath = trimmed
	r.deviceAt = time.Now()
	r.deviceToken = ""
	info, errStat := os.Stat(trimmed)
	if errStat != nil || info.Size() > deviceTokenFileMaxLen {
		return ""
	}
	raw, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		return ""
	}
	r.deviceToken = strings.TrimSpace(string(raw))
	return r.deviceToken
}

func (r *runtime) hostProbeCredential(region Region) *Credential {
	if r.env.Host == nil || r.env.Registry == nil {
		return nil
	}
	ctx := context.Background()
	entries, errList := r.env.Host.ListAuths(ctx)
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		stored, okStored := r.env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, okCredential := r.env.Registry.ParseCredential(stored, entry.Name, nil)
		if !okCredential || credential.RegionValue() != string(region) {
			continue
		}
		if native, okNative := credential.Native.(*Credential); okNative && native != nil {
			return native
		}
	}
	return nil
}
