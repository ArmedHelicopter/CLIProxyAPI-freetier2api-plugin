package loginsession

// Package loginsession 提供供应商无关的登录会话登记表。
//
// 会话表是 Registry 实例：每个 App 一份，测试与多实例互不共享。

import (
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
)

// Session 是一次进行中的登录会话。
type Session struct {
	VendorID   string
	ExpiresAt  time.Time
	PollMinGap time.Duration
	Data       any
	LastPoll   time.Time
}

// Registry 是登录会话登记表。
type Registry struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*Session
}

// NewRegistry 构造一个会话表。
func NewRegistry() *Registry { return NewRegistryWithClock(time.Now) }

// NewRegistryWithClock 构造一个使用指定时钟的会话表（测试用）。
func NewRegistryWithClock(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, m: map[string]*Session{}}
}

// Register 登记一个登录会话。ttl <= 0 按 15 分钟处理。
func (r *Registry) Register(vendorID, sessionID string, ttl, pollMinGap time.Duration, data any) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	r.mu.Lock()
	r.m[id] = &Session{VendorID: vendorID, ExpiresAt: r.now().Add(ttl), PollMinGap: pollMinGap, Data: data}
	r.mu.Unlock()
}

// Poll 查询会话并按需做过期清理与轮询节流。
func (r *Registry) Poll(sessionID string) (*core.LoginSession, core.PollState) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return nil, core.PollMissing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.m[id]
	if !ok {
		return nil, core.PollMissing
	}
	if !session.ExpiresAt.IsZero() && r.now().After(session.ExpiresAt) {
		delete(r.m, id)
		return nil, core.PollMissing
	}
	if session.PollMinGap > 0 && r.now().Sub(session.LastPoll) < session.PollMinGap {
		return &core.LoginSession{VendorID: session.VendorID, Data: session.Data}, core.PollThrottled
	}
	session.LastPoll = r.now()
	return &core.LoginSession{VendorID: session.VendorID, Data: session.Data}, core.PollReady
}

// Delete 删除一个会话。
func (r *Registry) Delete(sessionID string) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

// Owns 报告会话归属。
func (r *Registry) Owns(sessionID string) (string, bool) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.m[id]
	if !ok {
		return "", false
	}
	if !session.ExpiresAt.IsZero() && r.now().After(session.ExpiresAt) {
		delete(r.m, id)
		return "", false
	}
	return session.VendorID, true
}

// NewID 生成会话 ID。
func NewID(n int) string { return core.RandomHex(n) }

// Default 返回进程级默认会话表，仅用于逐步迁移中的旧调用点。
var defaultRegistry = NewRegistry()

func Register(vendorID, sessionID string, ttl, interval time.Duration, data any) {
	defaultRegistry.Register(vendorID, sessionID, ttl, interval, data)
}
func Poll(sessionID string) (*core.LoginSession, core.PollState) {
	return defaultRegistry.Poll(sessionID)
}
func Delete(sessionID string)              { defaultRegistry.Delete(sessionID) }
func Owns(sessionID string) (string, bool) { return defaultRegistry.Owns(sessionID) }

const (
	PollMissing   = core.PollMissing
	PollThrottled = core.PollThrottled
	PollReady     = core.PollReady
)

type PollState = core.PollState
