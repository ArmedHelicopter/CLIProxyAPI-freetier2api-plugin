package loginsession

// Package loginsession 提供供应商无关的登录会话登记表。
//
// 为什么需要它：多步登录（设备码 / OAuth 回调）都有一张「进行中会话」表，
// 每张表都自带 TTL、轮询节流与归属查询。此前每个供应商各写一份，根层为了
// 把 poll 路由回正确的供应商，还长出了一段靠 session id 前缀猜归属的补丁。
//
// 这里的职责只有三件事：
//   - 登记会话（会话 id → 供应商 + 供应商自定义数据 + TTL）；
//   - 轮询节流（控制台页面轮询远高于上游建议频率）；
//   - 归属查询（根层按 session id 找供应商，不再猜前缀）。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Session 是一次进行中的登录会话。
type Session struct {
	// VendorID 是创建会话的供应商实例。
	VendorID string
	// ExpiresAt 是会话失效时刻。
	ExpiresAt time.Time
	// PollMinGap 是同一会话两次访问上游 poll 端点之间的最小间隔。
	PollMinGap time.Duration
	// Data 是供应商自定义的会话数据（设备码、PKCE verifier 等）。
	Data any

	lastPoll time.Time
}

// PollState 描述一次轮询的准入结果。
type PollState int

const (
	// PollMissing 表示会话不存在或已过期。
	PollMissing PollState = iota
	// PollThrottled 表示还没到下一次允许打上游的时刻。
	PollThrottled
	// PollReady 表示可以向上游发起一次 poll。
	PollReady
)

var (
	storeMu sync.Mutex
	store   = map[string]*Session{}
	// fallbackCounter 给随机源不可用时的确定性 id 用。
	fallbackCounter int64
)

// Register 登记一个登录会话。
//
// ttl <= 0 时按 15 分钟处理；pollMinGap <= 0 表示不节流。
func Register(vendorID, sessionID string, ttl, pollMinGap time.Duration, data any) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	storeMu.Lock()
	store[id] = &Session{
		VendorID:   vendorID,
		ExpiresAt:  time.Now().Add(ttl),
		PollMinGap: pollMinGap,
		Data:       data,
	}
	storeMu.Unlock()
}

// Poll 查询一次会话，并按需做过期清理与轮询节流。
//
// 返回的 *Session 可直接读取 Data；供应商不应长期持有该指针（会话可能被删除）。
func Poll(sessionID string) (*Session, PollState) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return nil, PollMissing
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	session, okSession := store[id]
	if !okSession {
		return nil, PollMissing
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		delete(store, id)
		return nil, PollMissing
	}
	if session.PollMinGap > 0 && time.Since(session.lastPoll) < session.PollMinGap {
		return session, PollThrottled
	}
	session.lastPoll = time.Now()
	return session, PollReady
}

// Delete 删除一个会话（登录完成或失败后调用）。
func Delete(sessionID string) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	storeMu.Lock()
	delete(store, id)
	storeMu.Unlock()
}

// Owns 报告会话是否由某个供应商创建：返回 (vendorID, true)。
func Owns(sessionID string) (string, bool) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return "", false
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	session, okSession := store[id]
	if !okSession {
		return "", false
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		delete(store, id)
		return "", false
	}
	return session.VendorID, true
}

// NewID 生成 n 字节随机十六进制串（2n 个字符）。
//
// 随机源不可用时退回基于时间与计数的确定性串：登录会话 id 只影响会话查找的
// 唯一性，退化的熵在这种场景下可接受——比返回空串让登录链路崩掉好。
func NewID(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&fallbackCounter, 1))
	}
	return hex.EncodeToString(buf)
}

// ResetForTest 清空全部会话（仅测试使用）。
//
// 会话表是包级的：测试之间不清空会让上一个用例的会话泄漏到下一个，
// 表现为「随便一个 state 都能命中旧会话」。
func ResetForTest() {
	storeMu.Lock()
	store = map[string]*Session{}
	storeMu.Unlock()
}
