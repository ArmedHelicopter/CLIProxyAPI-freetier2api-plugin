package core

// 本文件定义供应商运行时环境与组合根工厂。
//
// Env 是供应商唯一允许依赖的宿主面：host 端口、注册表、登录会话表。
// 供应商不再读取包级全局注册表或全局 host port。

import (
	"context"
	"time"
)

// VendorFactory 负责构造一个供应商家族的实例（区域变体在 family 内展开）。
type VendorFactory struct {
	Instances func(Env) []Vendor
}

// Env 是供应商实例的运行时依赖。
type Env struct {
	Host     Host
	Registry *Registry
	Logins   LoginSessions
}

// Settings 返回当前全局设置快照。
func (e Env) Settings() Settings {
	if e.Host == nil {
		return Settings{}
	}
	return e.Host.Settings()
}

// PollState 是登录轮询的准入结果。
type PollState int

const (
	PollMissing PollState = iota
	PollThrottled
	PollReady
)

// LoginSessions 是登录会话登记表接口；实现由 internal/loginsession 提供。
type LoginSession struct {
	VendorID string
	Data     any
}

type LoginSessions interface {
	Register(vendorID, sessionID string, ttl, interval time.Duration, data any)
	Poll(sessionID string) (*LoginSession, PollState)
	Owns(sessionID string) (vendorID string, ok bool)
	Delete(sessionID string)
}

// ProbeNativeCredential 从宿主凭证中挑第一个属于 vendorID 的可用原生凭证。
func ProbeNativeCredential[T any](env Env, vendorID string, extract NativeExtractor[T]) (*T, bool) {
	if env.Host == nil || env.Registry == nil {
		return nil, false
	}
	ctx := context.Background()
	entries, errList := env.Host.ListAuths(ctx)
	if errList != nil {
		return nil, false
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, okRaw := env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, okParse := env.Registry.ParseCredential(raw, entry.Name, nil)
		if !okParse || credential.VendorIDValue() != vendorID {
			continue
		}
		if native, okNative := extract(credential); okNative && native != nil {
			return native, true
		}
	}
	return nil, false
}
