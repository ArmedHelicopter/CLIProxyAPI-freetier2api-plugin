package main

// 本文件把根层的宿主能力接到 core.Host 端口。
//
// 这是「internal/vendors/* 不感知宿主」的唯一接缝：供应商包只认 core.Host，
// 根层在这里实现它并注入。依赖方向始终是 root → internal，绝不反向。

import (
	"context"

	"freetier2api-plugin/internal/core"
)

// hostBridge 实现 core.Host。
type hostBridge struct{}

// ListAuths 复用根层的凭证枚举（含「被前置插件抢走但仍按文件名归属」的兜底）。
func (hostBridge) ListAuths(ctx context.Context) ([]core.HostAuth, error) {
	entries, errList := listHostAuths(ctx, "")
	if errList != nil {
		return nil, errList
	}
	out := make([]core.HostAuth, 0, len(entries))
	for _, entry := range entries {
		out = append(out, core.HostAuth{
			ID:          entry.ID,
			AuthIndex:   entry.AuthIndex,
			Name:        entry.Name,
			Provider:    entry.Provider,
			Label:       entry.Label,
			Status:      entry.Status,
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			RuntimeOnly: entry.RuntimeOnly,
			Path:        entry.Path,
		})
	}
	return out, nil
}

// AuthJSON 复用根层的 host.auth.get 封装。
func (hostBridge) AuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	return getAuthJSONByIndex(ctx, "", authIndex)
}

// Settings 把根层配置映射成中立快照。
func (hostBridge) Settings() core.Settings {
	cfg := loadedConfig()
	return core.Settings{
		EnabledRealms: append([]string(nil), cfg.EnabledRealms...),
		StateDir:      cfg.StateDir,
		PassthroughIP: cfg.PassthroughIP,
		AutoCheckin:   cfg.AutoCheckin,
		AutoCheckinAt: cfg.AutoCheckinAt,
		AutoTasks:     cfg.AutoTasks,
		LogLevel:      cfg.LogLevel,
	}
}

// configureVendors 把全局设置与原始配置键推给每个供应商。
//
// 在 applyConfig（register / reconfigure 的共同入口）里调用一次；各供应商
// 自行解析自己的键并更新配置快照。
func configureVendors(cfg pluginConfig) {
	settings := hostBridge{}.Settings()
	for _, vendor := range core.Vendors() {
		if configurable, okConfigurable := vendor.(core.Configurable); okConfigurable {
			configurable.Configure(settings, cfg.Raw)
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
