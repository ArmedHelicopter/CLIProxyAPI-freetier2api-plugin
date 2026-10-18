package minimaxcode

import (
	"net/http"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestClassifyCredentialBeatsBalanceWording(t *testing.T) {
	// 上游会把登录过期写成余额话术的变体；凭证判定必须排在余额之前。
	got := Classify(http.StatusBadRequest, "token invalid, balance unchanged")
	if got.Kind != "credential" {
		t.Fatalf("kind = %s, want credential", got.Kind)
	}
	if got.HTTPStatus() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got.HTTPStatus())
	}
	if got.Retryable() {
		t.Fatal("credential errors must not rotate accounts")
	}
}

func TestClassifyStatusCodesWinWhenNoMarker(t *testing.T) {
	cases := []struct {
		status int
		kind   string
	}{
		{http.StatusUnauthorized, "credential"},
		{http.StatusForbidden, "credential"},
		{http.StatusPaymentRequired, "hard_credit"},
		{http.StatusTooManyRequests, "rate_limit"},
		{http.StatusInternalServerError, "transient"},
		{http.StatusBadRequest, "client"},
	}
	for _, tc := range cases {
		got := Classify(tc.status, "plain upstream body")
		if string(got.Kind) != tc.kind {
			t.Fatalf("Classify(%d).Kind = %s, want %s", tc.status, got.Kind, tc.kind)
		}
		if got.HTTPStatus() != tc.status {
			t.Fatalf("Classify(%d) status = %d, want passthrough", tc.status, got.HTTPStatus())
		}
	}
}

func TestClassifyStreamBodyWithoutStatus(t *testing.T) {
	// 流内业务错误没有 HTTP 状态：必须按正文分类，且不能把 200 透传给宿主。
	cases := []struct {
		body string
		kind string
		want int
	}{
		{"50110:insufficient balance (1008)", "hard_credit", http.StatusPaymentRequired},
		{"1008: too many requests", "rate_limit", http.StatusTooManyRequests},
		{"1001: token expired", "credential", http.StatusUnauthorized},
		// 无状态、无关键词：按可重试的瞬时故障处理（换号重试是安全的）。
		{"provider_error: connection reset", "transient", http.StatusBadGateway},
	}
	for _, tc := range cases {
		got := Classify(0, tc.body)
		if string(got.Kind) != tc.kind {
			t.Fatalf("Classify(0, %q).Kind = %s, want %s", tc.body, got.Kind, tc.kind)
		}
		if got.HTTPStatus() != tc.want {
			t.Fatalf("Classify(0, %q) status = %d, want %d", tc.body, got.HTTPStatus(), tc.want)
		}
	}
}

func TestClassifyKeepsUpstreamCode(t *testing.T) {
	if got := Classify(429, `{"code":50110,"message":"insufficient balance"}`); got.Code != "50110" {
		t.Fatalf("json code = %q, want 50110", got.Code)
	}
	if got := Classify(0, "50110:insufficient balance (1008)"); got.Code != "50110" {
		t.Fatalf("text code = %q, want 50110", got.Code)
	}
	if got := Classify(http.StatusBadGateway, "gateway down"); got.Code != "" {
		t.Fatalf("code = %q, want empty (must not invent one)", got.Code)
	}
}

func TestClassifyBalanceIsRetryableThroughCooldown(t *testing.T) {
	got := Classify(0, "50110:insufficient balance (1008)")
	if !got.Retryable() {
		t.Fatal("credit exhaustion should rotate to another account")
	}
}

// TestClassifySurvivesToPluginError 锁定「分类结果能走到宿主」。
//
// 分类再正确，若 adapter 把它包进 fmt.Errorf，宿主只会看到一个
// 502 可重试错误，坏凭证永远不会被停用——这是重构前的真实缺陷。
func TestClassifySurvivesToPluginError(t *testing.T) {
	failure := Classify(401, `{"code":1001,"message":"token expired"}`)
	pluginErr, ok := core.ToPluginError(failure).(*core.PluginError)
	if !ok {
		t.Fatalf("ToPluginError returned %T, want *core.PluginError", core.ToPluginError(failure))
	}
	if pluginErr.HTTPStatus != 401 {
		t.Fatalf("host status = %d, want 401 (bad credential must be disabled)", pluginErr.HTTPStatus)
	}
	if pluginErr.Retryable {
		t.Fatal("credential failure must not rotate accounts")
	}
}
