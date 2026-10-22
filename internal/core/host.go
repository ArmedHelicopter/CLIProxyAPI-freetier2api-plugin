package core

// 本文件定义「宿主端口」：供应商层需要、但只能由根层（ABI 适配层）提供的
// 宿主能力。
//
// 为什么需要它：internal/vendors/* 必须对宿存在无感知（不能 import package main），
// 但它们确实需要三样东西——列宿主凭证、按 auth_index 取凭证 JSON、读全局配置。
// 依赖方向由根层在 init 里注入实现来保证（与 internal/hostbridge.Configure 同一风格）。
//
// 注入前端口为 nil，所有包装函数都返回零值而不是 panic：单元测试与
// 「插件还没注册」这两个场景都会走这条路。

import (
	"context"
	"errors"
	"strings"
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

// Usable 报告该凭证是否可用于插件自身的操作（模型探测、额度查询、签到、保活、
// 控制台展示）。
//
// 只看 Disabled：它是用户显式关闭，必须尊重。Unavailable 是宿主**路由层**的
// 临时状态（上游限流、超时后打上的冷却），含义是「暂时别把用户对话流量打到
// 这条凭证上」，并不代表凭证不存在或不可读。把两者混为一谈，一次限流就会让
// 该供应商整家从模型列表、额度查询里消失——页面表现为「模型没了」「剩余/总额
// 永远查询中」，而凭证文件本身完好无损。这是实际发生过的事故。
func (a HostAuth) Usable() bool { return !a.Disabled }

// Settings 是供应商无关的全局配置快照（只读）。
//
// 只放「多个供应商都要看」的字段：供应商专属配置由各包自己从原始键里解析
// （见 Configurable），不进这里。
type Settings struct {
	// EnabledRealms 是启用的域（cn / global），空表示全启用。
	EnabledRealms []string
	// StateDir 是插件状态目录（落盘模型的根）。
	StateDir string
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
	return RealmEnabled(s.EnabledRealms, region)
}

// RealmEnabled 判断区域是否在启用列表中；空区域或空列表视为启用。
func RealmEnabled(enabled []string, region string) bool {
	if strings.TrimSpace(region) == "" || len(enabled) == 0 {
		return true
	}
	for _, realm := range enabled {
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
