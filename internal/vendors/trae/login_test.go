package trae

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSessionStoresAreIsolated 锁定「登录会话与回调监听都属于实例」。
//
// 回归的是重构前的缺陷：会话表与监听标记是包级变量，一个 App 发起的
// 登录会被另一个 App 认领，且两个 App 无法各自持有监听。
func TestSessionStoresAreIsolated(t *testing.T) {
	first := NewSessionStore()
	second := NewSessionStore()

	first.register(&PendingSession{
		SessionID: "traecn_first", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-a", TraceID: "trace-a", State: "pending", CreatedAt: time.Now(),
	})

	if _, ok := second.get("traecn_first"); ok {
		t.Fatal("第二个实例不应看到第一个实例的登录会话")
	}
	if matched := second.matchCallback(&CallbackInfo{TraceID: "trace-a"}); matched != nil {
		t.Fatalf("回调不应被另一个实例认领: %+v", matched)
	}
	if matched := first.matchCallback(&CallbackInfo{TraceID: "trace-a"}); matched == nil {
		t.Fatal("归属实例应能认领自己的回调")
	}
}

// TestCallbackURLFollowsActualListener 锁定“登录链接里的回调地址”
// 与真实监听端口一致（18080 被占用时必须退回随机端口）。
func TestCallbackURLFollowsActualListener(t *testing.T) {
	store := NewSessionStore()
	if err := store.EnsureCallbackServer(); err != nil {
		t.Fatalf("ensure callback server: %v", err)
	}
	addr := store.callbackURL()
	if !strings.HasSuffix(addr, "/authorize") {
		t.Fatalf("callback url = %q, want .../authorize", addr)
	}

	started, errStart := LoginStart(context.Background(), ConfigFor(RegionCN), store)
	if errStart != nil {
		t.Fatalf("login start: %v", errStart)
	}
	if !strings.Contains(started.URL, url.QueryEscape(addr)) {
		t.Fatalf("login url 未带本实例回调地址: %s (want encoded %s)", started.URL, url.QueryEscape(addr))
	}
}

// TestCallbackHandlerOnlyClaimsOwnSession 验证回调处理只认本实例会话。
func TestCallbackHandlerOnlyClaimsOwnSession(t *testing.T) {
	session := &PendingSession{
		SessionID: "traecn_owned", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-owned", TraceID: "trace-owned", State: "pending", CreatedAt: time.Now(),
	}
	owner := NewSessionStore()
	owner.register(session)
	other := NewSessionStore()

	record := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/authorize?loginTraceID=trace-owned&refreshToken=rt-test", nil)
	other.handleAuthorizeCallback(record, req)
	if record.Code != http.StatusNotFound {
		t.Fatalf("非归属实例的响应 = %d, want 404", record.Code)
	}
	if session.State != "pending" {
		t.Fatalf("非归属实例改动了会话状态: %s", session.State)
	}
}
