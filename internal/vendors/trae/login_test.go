package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
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

// pastedCallback 是一份真实浏览器回调的结构样本（值已脱敏）：Trae 授权成功后
// 浏览器停在 127.0.0.1:18080/authorize 上，整条地址里带着 userJwt 与 userInfo。
//
// 刻意**不含** refreshToken / userJwt.RefreshToken：带的话 completeLogin 会去
// 上游换 token，单测就得联网；这里要验的是「粘贴能被解析并落出凭证」。
const pastedCallback = "http://127.0.0.1:18080/authorize?isRedirect=true&scope=solo" +
	"&loginTraceID=trace-paste-001&machine_id=machine-paste-001&device_id=device-paste-001" +
	"&userJwt=%7B%22ClientID%22%3A%22en1oxy7wnw8j9n%22%2C%22Token%22%3A%22jwt-access-token%22%2C%22TokenExpireAt%22%3A1793607527194%7D" +
	"&userInfo=%7B%22Region%22%3A%22CN%22%2C%22ScreenName%22%3A%22tester%22%2C%22TenantID%22%3A%22tenant-1%22%2C%22UserID%22%3A%22user-1%22%7D"

// TestSubmitCallbackCompletesPendingSession 锁定远程部署下的兜底路径：浏览器到不了
// 插件的 127.0.0.1 监听时，用户粘回回调链接应当完成登录并产出凭证。
func TestSubmitCallbackCompletesPendingSession(t *testing.T) {
	store := NewSessionStore()
	store.register(&PendingSession{
		SessionID: "traecn_paste", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-paste-001", TraceID: "trace-paste-001", State: "pending", CreatedAt: time.Now(),
	})

	result, errSubmit := store.SubmitCallback(context.Background(), "traecn_paste", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (message=%q)", result.Status, result.Message)
	}
	if result.Auth.Label != "Trae-tester" {
		t.Fatalf("label = %q, want Trae-tester", result.Auth.Label)
	}

	var storage map[string]any
	if err := json.Unmarshal(result.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("decode storage json: %v", err)
	}
	if storage["access_token"] != "jwt-access-token" {
		t.Fatalf("access_token = %v, want jwt-access-token", storage["access_token"])
	}
	if storage["uid"] != "user-1" || storage["enterprise_id"] != "tenant-1" {
		t.Fatalf("uid/enterprise_id = %v/%v, want user-1/tenant-1", storage["uid"], storage["enterprise_id"])
	}
	if result.Auth.FileName != "traecn-user-1.json" {
		t.Fatalf("file name = %q, want traecn-user-1.json", result.Auth.FileName)
	}

	// 轮询应当立刻拿到同一个会话的成功结果。
	polled, errPoll := LoginPoll(context.Background(), "traecn_paste", store)
	if errPoll != nil {
		t.Fatalf("poll: %v", errPoll)
	}
	if polled.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("polled status = %q, want success", polled.Status)
	}
}

// TestSubmitCallbackRejectsForeignSession 锁定粘贴的严格认领：trace 对不上的链接
// 必须报错，且**不能**像 HTTP 回调那样退回归「最近一个待处理会话」——否则一次
// 粘贴就能把凭证写进别的登录会话里。
func TestSubmitCallbackRejectsForeignSession(t *testing.T) {
	store := NewSessionStore()
	session := &PendingSession{
		SessionID: "traecn_owner", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-owner", TraceID: "trace-owner", State: "pending", CreatedAt: time.Now(),
	}
	store.register(session)

	_, errSubmit := store.SubmitCallback(context.Background(), "traecn_owner", pastedCallback)
	if errSubmit == nil {
		t.Fatal("trace 不匹配的回调必须报错")
	}
	if session.State != "pending" || session.AuthData != nil {
		t.Fatalf("会话被不该认领的回调改写了: state=%s", session.State)
	}
}

// TestSubmitCallbackRejectsExpiredSession 锁定过期会话的提示：用户看到的是
// 「重新发起后立即粘贴」，而不是一句无操作建议的「已过期」。
func TestSubmitCallbackRejectsExpiredSession(t *testing.T) {
	store := NewSessionStore()
	store.register(&PendingSession{
		SessionID: "traecn_old", VendorID: "traecn", Region: RegionCN,
		MachineID: "machine-paste-001", TraceID: "trace-paste-001", State: "pending",
		CreatedAt: time.Now().Add(-20 * time.Minute),
	})

	_, errSubmit := store.SubmitCallback(context.Background(), "traecn_old", pastedCallback)
	if errSubmit == nil {
		t.Fatal("过期会话必须报错")
	}
	if !strings.Contains(errSubmit.Error(), "粘贴") {
		t.Fatalf("过期提示应指引用户粘贴，实际 = %q", errSubmit.Error())
	}
}

// TestParseCallbackAcceptsHostPrefixedURL 锁定粘贴串的宽容度：浏览器复制出来的
// 可能是不带 scheme 的路径形式，ParseCallback 要能自己补上 host。
func TestParseCallbackAcceptsHostPrefixedURL(t *testing.T) {
	info, errParse := ParseCallback("/authorize?loginTraceID=trace-paste-001&refreshToken=rt-only")
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if info.TraceID != "trace-paste-001" || info.RefreshToken != "rt-only" {
		t.Fatalf("parsed = %+v, want trace-paste-001 / rt-only", info)
	}
}
