package autoclaw

import (
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// stubLoginSessions 是 core.LoginSessions 的内存实现（单测用）。
type stubLoginSessions struct {
	sessions map[string]*core.LoginSession
}

func newStubLoginSessions() *stubLoginSessions {
	return &stubLoginSessions{sessions: map[string]*core.LoginSession{}}
}

func (s *stubLoginSessions) Register(vendorID, sessionID string, ttl, interval time.Duration, data any) {
	s.sessions[sessionID] = &core.LoginSession{VendorID: vendorID, Data: data}
}

func (s *stubLoginSessions) Poll(sessionID string) (*core.LoginSession, core.PollState) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, core.PollMissing
	}
	return session, core.PollReady
}

func (s *stubLoginSessions) Owns(sessionID string) (string, bool) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return "", false
	}
	return session.VendorID, true
}

func (s *stubLoginSessions) Delete(sessionID string) { delete(s.sessions, sessionID) }

// envWithLogins 返回带会话表的 Env（登录链路单测用）。
func envWithLogins() core.Env { return core.Env{Logins: newStubLoginSessions()} }

var _ = testing.Short
