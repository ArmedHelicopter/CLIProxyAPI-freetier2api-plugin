package core

// 本文件定义「宿主端口」：供应商层需要、但只能由根层（ABI 适配层）提供的
// 宿主能力。
//
// 为什么需要它：internal/vendors/* 必须对宿存在无感知（不能 import package main），
// 但它们确实需要三样东西——列宿主凭证、按 auth_index 取凭证 JSON、读全局配置。
// 依赖方向由根层在 init 里注入实现来保证（与 internal/httpx.Configure 同一风格）。
//
// 注入前端口为 nil，所有包装函数都返回零值而不是 panic：单元测试与
// 「插件还没注册」这两个场景都会走这条路。

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrHostNotConfigured 表示宿主端口尚未注入。
var ErrHostNotConfigured = errors.New("core: host port is not configured")

// HostAuth 是宿主凭证列表里的一条（中立形态，字段与 host.auth.list 对齐）。
type HostAuth struct {
	ID          string    `json:"id"`
	AuthIndex   string    `json:"auth_index"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Label       string    `json:"label"`
	Status      string    `json:"status"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	RuntimeOnly bool      `json:"runtime_only"`
	Path        string    `json:"path,omitempty"`
	ModTime     time.Time `json:"modtime,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// Settings 是供应商无关的全局配置快照（只读）。
//
// 只放「多个供应商都要看」的字段：供应商专属配置由各包自己从原始键里解析
// （见 Configurable），不进这里。
type Settings struct {
	// EnabledRealms 是启用的域（cn / global），空表示全启用。
	EnabledRealms []string
	// StateDir 是插件状态目录（落盘模型的根）。
	StateDir string
	// PassthroughIP 报告是否把客户端 IP 透传给上游。
	PassthroughIP bool
	// SanitizeFingerprints 报告是否开启出站请求体指纹脱敏。
	SanitizeFingerprints bool
	// MachineSalt 是本部署的设备指纹盐（为空表示供应商未配置）。
	MachineSalt string
	// AutoCheckin / AutoCheckinAt / AutoTasks 是后台自动化开关。
	AutoCheckin   bool
	AutoCheckinAt string
	AutoTasks     bool
	// LogLevel 是日志级别。
	LogLevel string
}

// RealmEnabled 报告某个域是否被配置启用。
func (s Settings) RealmEnabled(region string) bool {
	if len(s.EnabledRealms) == 0 {
		return true
	}
	for _, realm := range s.EnabledRealms {
		if realm == region {
			return true
		}
	}
	return false
}

// Host 是根层必须提供的宿主能力集合。
type Host interface {
	// ListAuths 列出本插件名下的全部凭证。
	ListAuths(ctx context.Context) ([]HostAuth, error)
	// AuthJSON 按 auth_index 取回凭证 JSON。
	AuthJSON(ctx context.Context, authIndex string) ([]byte, bool)
	// Settings 返回当前全局配置快照。
	Settings() Settings
}

var (
	hostPortMu sync.RWMutex
	hostPort   Host
)

// ConfigureHost 注入宿主端口实现。由根层在 init 里调用一次。
func ConfigureHost(host Host) {
	hostPortMu.Lock()
	hostPort = host
	hostPortMu.Unlock()
}

// HostPort 返回当前注入的宿主端口；未注入时为 nil。
func HostPort() Host {
	hostPortMu.RLock()
	host := hostPort
	hostPortMu.RUnlock()
	return host
}

// HostConfigured 报告宿主端口是否已注入。
func HostConfigured() bool { return HostPort() != nil }

// HostSettings 返回全局配置快照；端口缺失时返回零值。
func HostSettings() Settings {
	host := HostPort()
	if host == nil {
		return Settings{}
	}
	return host.Settings()
}

// ListHostAuths 是端口的安全包装：端口缺失或调用失败都返回错误。
func ListHostAuths(ctx context.Context) ([]HostAuth, error) {
	host := HostPort()
	if host == nil {
		return nil, ErrHostNotConfigured
	}
	return host.ListAuths(ctx)
}

// HostAuthJSON 按 auth_index 取凭证 JSON；端口缺失时返回 false。
func HostAuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	host := HostPort()
	if host == nil {
		return nil, false
	}
	return host.AuthJSON(ctx, authIndex)
}
