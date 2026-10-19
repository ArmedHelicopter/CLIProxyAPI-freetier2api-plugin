package codearts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/hostbridge"
)

// pastedCallback 是浏览器授权后停在 127.0.0.1 上的真实回调链接样本。
//
// 它是整个 bug 的关键证据：外层 secret 与 redirect 里的 ticket_id / secret 都是
// **华为云签发**的，不是 LoginStart 送进 /authorize 的那一对。插件此前一直拿自造
// 值轮询，所以控制台永远停在「等待授权中…」。
const pastedCallback = "http://127.0.0.1/oauth/callback?secret=d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280" +
	"&redirect=https%3A%2F%2Fcodearts.huaweicloud.com%2Fportal%2Flogin%3Ftheme%3Dlight%26locale%3Dzh-cn" +
	"%26ticket_id%3D657abbb247eab363d1a461285fc50145%26authorizationAvailable%3Dfalse" +
	"%26secret%3Dd55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"

const (
	upstreamTicketID = "657abbb247eab363d1a461285fc50145"
	upstreamSecret   = "d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"
)

// useLoopbackHost 把宿主的 host.http.do 落到真实 http.Client 上。
//
// 出站请求一律走宿主桥（代理、审计、统计都挂在那条链路上），所以单测想验证
// 「打出去的查询参数对不对」，就得先把这条桥搭起来——否则请求连发都发不出去。
func useLoopbackHost(t *testing.T) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, nil
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		var bodyReader io.Reader
		if len(req.Body) > 0 {
			bodyReader = strings.NewReader(string(req.Body))
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(key, value)
			}
		}
		resp, errDo := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{"StatusCode": resp.StatusCode, "Headers": resp.Header, "Body": respBody})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// TestParseCallbackTakesUpstreamTicket 锁定解析口径：ticket_id / secret 取自回调
// （含 redirect 里的门户登录地址），而不是插件自造的那一对。
func TestParseCallbackTakesUpstreamTicket(t *testing.T) {
	info, errParse := ParseCallback(pastedCallback)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	if info.TicketID != upstreamTicketID {
		t.Fatalf("ticket id = %q, want %q", info.TicketID, upstreamTicketID)
	}
	if info.Secret != upstreamSecret {
		t.Fatalf("secret = %q, want %q", info.Secret, upstreamSecret)
	}
	if info.Redirect == "" {
		t.Fatal("redirect 未保留：票据没换到凭证时用户还要靠它继续授权")
	}
}

// TestParseCallbackRejectsLinkWithoutTicket 守住解析下限：没有票据的链接不能被
// 当成有效回调（否则用户只会看到一句莫名其妙的上游报错）。
func TestParseCallbackRejectsLinkWithoutTicket(t *testing.T) {
	if _, errParse := ParseCallback("http://127.0.0.1/oauth/callback?secret=only-secret"); errParse == nil {
		t.Fatal("缺 ticket_id 的链接必须解析失败")
	}
}

// TestSubmitCallbackSwapsInUpstreamTicket 是本文件的核心守卫：提交粘贴链接后，
// 票据查询必须用**上游签发**的 ticket/secret，且凭证能落到返回值里。
func TestSubmitCallbackSwapsInUpstreamTicket(t *testing.T) {
	useLoopbackHost(t)
	var gotTicket, gotSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTicket = r.URL.Query().Get("ticket_id")
		gotSecret = r.URL.Query().Get("secret")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_id":       "user-1",
			"user_name":     "tester",
			"refresh_token": "refresh-1",
			"credentials": map[string]string{
				"access_key_id":     "AK-test",
				"secret_access_key": "SK-test",
				"security_token":    "ST-test",
				"expiration":        "2026-10-19T18:00:00Z",
			},
		})
	}))
	defer server.Close()

	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_paste", 15*time.Minute, 0, &pendingLogin{
		TicketID: "self-made-ticket", Secret: "self-made-secret",
		TicketURL: server.URL,
	})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_paste", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (message=%q)", result.Status, result.Message)
	}
	if gotTicket != upstreamTicketID || gotSecret != upstreamSecret {
		t.Fatalf("票据查询用了 %q/%q，want 上游签发的 %q/%q", gotTicket, gotSecret, upstreamTicketID, upstreamSecret)
	}
	if result.Auth.Label != "CodeArts-user-1" {
		t.Fatalf("label = %q, want CodeArts-user-1", result.Auth.Label)
	}

	var storage map[string]any
	if err := json.Unmarshal(result.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("decode storage json: %v", err)
	}
	if storage["access_key_id"] != "AK-test" || storage["secret_access_key"] != "SK-test" {
		t.Fatalf("凭证未落盘: %v / %v", storage["access_key_id"], storage["secret_access_key"])
	}
	if storage["refresh_token"] != "refresh-1" {
		t.Fatalf("refresh_token = %v, want refresh-1（续期要用它）", storage["refresh_token"])
	}
}

// TestSubmitCallbackKeepsPortalURLWhilePending 锁定「还没换到凭证」这一步：把回调里
// 的门户登录地址带回去，让用户能在能联网的浏览器里继续完成授权，插件则继续轮询。
func TestSubmitCallbackKeepsPortalURLWhilePending(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_pending", 15*time.Minute, 0, &pendingLogin{TicketURL: server.URL})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_pending", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", result.Status)
	}
	if result.RedirectURL == "" {
		t.Fatal("pending 时必须带回门户登录地址，否则用户没有下一步")
	}
}

// TestSubmitCallbackRejectsUnknownSession 守住会话边界：没有登录会话时不能凭一条
// 链接就换出凭证。
func TestSubmitCallbackRejectsUnknownSession(t *testing.T) {
	if _, errSubmit := SubmitCallback(context.Background(), auth.NewSessionRegistry(), "codearts_missing", pastedCallback); errSubmit == nil {
		t.Fatal("未知会话必须报错")
	}
}
