package main

// 本文件把根层的宿主能力接到 core.Host 端口。
//
// 这是「internal/vendors/* 不感知宿主」的唯一接缝：供应商包只认 core.Host，
// 根层在这里实现它并注入。依赖方向始终是 root → internal，绝不反向。

import (
	"context"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
)

// hostBridge 实现 core.Host。
type hostBridge struct{}

// ListAuths 复用根层的凭证枚举（含「被前置插件抢走但仍按文件名归属」的兜底）。
func (hostBridge) ListAuths(ctx context.Context) ([]core.HostAuth, error) {
	return hostabi.ListAuths(ctx, "")
}

// AuthJSON 复用根层的 host.auth.get 封装。
func (hostBridge) AuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	return hostabi.AuthJSON(ctx, "", authIndex)
}

// Settings 把根层配置映射成中立快照。
func (hostBridge) Settings() core.Settings {
	cfg := loadedConfig()
	return core.Settings{
		EnabledRealms:        append([]string(nil), cfg.EnabledRealms...),
		StateDir:             cfg.StateDir,
		PassthroughIP:        cfg.PassthroughIP,
		SanitizeFingerprints: cfg.SanitizeFingerprints,
		MachineSalt:          currentMachineSalt,
		AutoCheckin:          cfg.AutoCheckin,
		AutoCheckinAt:        cfg.AutoCheckinAt,
		AutoTasks:            cfg.AutoTasks,
		LogLevel:             cfg.LogLevel,
	}
}

// configureVendors 把全局设置与原始配置键推给每个供应商。
//
// 在 applyConfig（register / reconfigure 的共同入口）里调用一次；各供应商
// 自行解析自己的键并更新配置快照。
func configureVendors(cfg pluginConfig) {
	settings := hostBridge{}.Settings()
	raw := make(map[string]string, len(cfg.Raw)+1)
	for key, value := range cfg.Raw {
		raw[key] = value
	}
	// 页面设置覆盖 YAML 配置：用户在控制台页改过的提示词模式以页面为准。
	// 这是唯一一个「状态文件覆盖配置」的键，合并后再交给供应商解析。
	if state := snapshotState(); state.PromptMode != nil && strings.TrimSpace(*state.PromptMode) != "" {
		raw["prompt_mode"] = strings.TrimSpace(*state.PromptMode)
	}
	for _, vendor := range core.Vendors() {
		if configurable, okConfigurable := vendor.(core.Configurable); okConfigurable {
			configurable.Configure(settings, raw)
		}
	}
}

// init 注入宿主端口。
//
// 与 core.RegisterVendor 的时序无关：供应商只在使用时读取端口，
// 不会在注册期依赖配置。
func init() {
	core.ConfigureHost(hostBridge{})
}
