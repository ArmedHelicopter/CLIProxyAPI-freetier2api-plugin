package workbuddy

// 本文件解析 WorkBuddy 的供应商专属配置，并保存出站请求需要的运行时快照。
//
// 归属边界：UA/设备令牌/提示词/机器盐都只影响 WorkBuddy 的上游协议，
// 由本包自己解释；根层只把全局设置与原始键交进来（core.Configurable）。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

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
	cfg.sanitizeFingerprints = settings.SanitizeFingerprints
	cfg.passthroughIP = settings.PassthroughIP
	cfg.stateDir = settings.StateDir

	if value := strings.TrimSpace(raw["user_agent"]); value != "" {
		cfg.userAgent = value
	}
	if value := strings.TrimSpace(raw["client_version"]); value != "" {
		cfg.clientVersion = value
	}
	if value := strings.TrimSpace(raw["cli_version"]); value != "" {
		cfg.cliVersion = value
	}
	if value := strings.TrimSpace(raw["client_name"]); value != "" {
		cfg.clientName = value
	}
	cfg.deviceToken = strings.TrimSpace(raw["device_token"])
	cfg.deviceTokenFile = strings.TrimSpace(raw["device_token_file"])

	if value := strings.ToLower(strings.TrimSpace(raw["prompt_mode"])); value != "" {
		cfg.promptMode = value
	}
	if path := strings.TrimSpace(raw["prompt_file"]); path != "" {
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
