package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// postLoginCallback 调用管理路由并解出响应体。
func postLoginCallback(t *testing.T, service *Service, state, callbackURL string) map[string]any {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/freetier2api/login/callback",
		Body:   []byte(`{"state":"` + state + `","url":"` + callbackURL + `"}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	raw, errHandle := service.Handle(raw)
	if errHandle != nil {
		t.Fatalf("handle login callback: %v", errHandle)
	}
	var envelope struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v (body=%s)", errUnmarshal, envelope.Result.Body)
	}
	return payload
}

// TestRegistrationDeclaresLoginCallbackRoute 锁定粘贴回调入口必须出现在路由声明里：
// 宿主按这张表放行 /plugins/<id>/... 子路径，漏登记就等于 404。
func TestRegistrationDeclaresLoginCallbackRoute(t *testing.T) {
	for _, route := range newService(t).Registration().Routes {
		if route.Method == http.MethodPost && route.Path == "/plugins/freetier2api/login/callback" {
			return
		}
	}
	t.Fatalf("login/callback route missing: %#v", newService(t).Registration().Routes)
}

// TestHandleLoginCallbackReturnsVendorResult 验证成功路径：供应商给出的状态与提示
// 原样回给页面；CodeArts 的门户登录地址也必须带回去（页面据此渲染可点链接）。
func TestHandleLoginCallbackReturnsVendorResult(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status:      pluginapi.AuthLoginStatusPending,
		Message:     "等待用户完成华为云登录…",
		RedirectURL: "https://codearts.huaweicloud.com/portal/login?ticket_id=x",
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "codearts_x",
		"http://127.0.0.1/oauth/callback?secret=s")

	if payload["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("status = %v, want pending", payload["status"])
	}
	if payload["ok"] != false {
		t.Fatalf("ok = %v, want false", payload["ok"])
	}
	if payload["redirect_url"] == "" {
		t.Fatal("redirect_url 未回传：CodeArts 票据没换到凭证时用户没有下一步")
	}
}

// TestHandleLoginCallbackSurfacesVendorError 验证失败路径：供应商给出的原因要能直接
// 显示给用户，而不是变成一个技术性响应。
func TestHandleLoginCallbackSurfacesVendorError(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackErr = core.NewPluginError("callback_mismatch",
		"回调链接与当前登录会话不匹配，请重新发起登录后重新授权", http.StatusBadRequest)
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	message, _ := payload["message"].(string)
	if !strings.Contains(message, "回调链接与当前登录会话不匹配") {
		t.Fatalf("message = %q, want 供应商给出的可读原因", message)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("status = %v, want error", payload["status"])
	}
}

// TestHandleLoginCallbackRejectsMissingState 守住入参校验：没有 state 时不能把请求
// 透传给供应商（否则就是拿一条链接去撞所有会话）。
func TestHandleLoginCallbackRejectsMissingState(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{Status: pluginapi.AuthLoginStatusSuccess}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "", "http://127.0.0.1/oauth/callback?secret=s")
	if payload["ok"] != false {
		t.Fatalf("缺 state 时不应成功: %v", payload)
	}
}
