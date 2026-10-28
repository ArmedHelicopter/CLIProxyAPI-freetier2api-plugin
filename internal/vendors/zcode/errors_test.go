package zcode

import (
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestClassifyOrderingCaptchaBeforeAuth(t *testing.T) {
	// 403 + captcha 文案是验证码挑战，不是凭证失效——先判 403 会把账号错杀。
	risk := Classify(403, `{"code":3007,"msg":"captcha verify failed"}`)
	if risk.HTTPStatus() != 403 {
		t.Fatalf("验证码挑战应映射为软冷却(403)，得到 %d", risk.HTTPStatus())
	}
	dead := Classify(403, `{"msg":"forbidden"}`)
	if dead.HTTPStatus() != 403 {
		t.Fatalf("凭证失效应映射为 403，得到 %d", dead.HTTPStatus())
	}
	if !strings.Contains(strings.ToLower(dead.Msg), "forbidden") && !strings.Contains(dead.Msg, "凭证") {
		t.Fatalf("unexpected message: %q", dead.Msg)
	}
	if got := Classify(401, `{}`).HTTPStatus(); got != 401 {
		t.Fatalf("401 应映射为 401，得到 %d", got)
	}
}

func TestClassifyBusinessCodeWithoutStatus(t *testing.T) {
	errQuota := Classify(0, `{"code":1005,"msg":"daily quota exhausted"}`)
	if errQuota.HTTPStatus() != 402 {
		t.Fatalf("额度耗尽应映射为 402，得到 %d", errQuota.HTTPStatus())
	}
}

func TestClassify500WrappedPlanConcurrencyLimit(t *testing.T) {
	body := `{"message":"model concurrency limit exceeded","type":"server_error","code":"internal_server_error"}`
	if failure := Classify(http.StatusInternalServerError, body); failure.Kind != core.FailureKindSoftRate {
		t.Fatalf("kind = %v, want SoftRate", failure.Kind)
	}
}

func TestClassifyModelBucketExhausted(t *testing.T) {
	body := `{"code":1005,"msg":"exceed quota limit","logid":"x"}`
	failure := Classify(200, body)
	if failure.Kind != core.FailureKindModelBlocked || failure.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("failure = %+v, want model blocked / 400", failure)
	}
}

func TestClassifyAccountLevelQuotaStillHardCredit(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{"payment_required", http.StatusPaymentRequired, `{"msg":"no credit"}`},
		{"code1005_other_wording", http.StatusOK, `{"code":1005,"msg":"daily claim quota used"}`},
		{"insufficient_marker", http.StatusOK, `{"code":7,"msg":"insufficient balance"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if failure := Classify(testCase.status, testCase.body); failure.Kind != core.FailureKindHardCredit {
				t.Fatalf("kind = %v, want HardCredit", failure.Kind)
			}
		})
	}
}

func TestZCodeFailoverEligibleErrorsAreRetryable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   core.FailureKind
	}{
		{"quota exhausted", http.StatusPaymentRequired, `{"msg":"no credit"}`, core.FailureKindHardCredit},
		{"rate limited", http.StatusTooManyRequests, `{"msg":"rate limit"}`, core.FailureKindSoftRate},
		{"upstream unavailable", http.StatusBadGateway, `{"msg":"bad gateway"}`, core.FailureKindServer},
		{"model bucket exhausted", http.StatusOK, `{"code":1005,"msg":"exceed quota limit"}`, core.FailureKindModelBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := Classify(tc.status, tc.body)
			if failure.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", failure.Kind, tc.kind)
			}
			if !failure.Retryable() {
				t.Fatalf("%s must be retryable so host can select another account", tc.name)
			}
		})
	}
}

func TestZCodeCredentialAndBadParamsAreNotFailoverEligible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"credential", http.StatusUnauthorized, `{"msg":"token expired"}`},
		{"bad params", http.StatusBadRequest, `{"code":1210,"msg":"max_tokens too large"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if failure := Classify(tc.status, tc.body); failure.Retryable() {
				t.Fatalf("%s must not be retried with another account", tc.name)
			}
		})
	}
}
