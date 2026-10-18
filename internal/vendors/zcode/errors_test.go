package zcode

import (
	"strings"
	"testing"
)

func TestClassifyOrderingCaptchaBeforeAuth(t *testing.T) {
	// 403 + captcha 文案是验证码挑战，不是凭证失效——先判 403 会把账号错杀。
	risk := Classify(403, `{"code":3007,"msg":"captcha verify failed"}`)
	if risk.HTTPStatus() != 403 {
		t.Fatalf("验证码挑战应映射为软冷却(403)，得到 %d", risk.HTTPStatus())
	}

	// 纯 403 才是凭证失效。
	dead := Classify(403, `{"msg":"forbidden"}`)
	if dead.HTTPStatus() != 403 {
		t.Fatalf("凭证失效应映射为 403，得到 %d", dead.HTTPStatus())
	}
	if !strings.Contains(strings.ToLower(dead.Msg), "forbidden") && !strings.Contains(dead.Msg, "凭证") {
		t.Fatalf("unexpected message: %q", dead.Msg)
	}

	// 401 → 鉴权失败。
	if got := Classify(401, `{}`); got.HTTPStatus() != 401 {
		t.Fatalf("401 应映射为 401，得到 %d", got.HTTPStatus())
	}
}

func TestClassifyBusinessCodeWithoutStatus(t *testing.T) {
	// 上游以 HTTP 200 包装业务错误时，status=0 必须由 Kind 推导，
	// 不能把 200 当成错误的 HTTP 码透传给宿主。
	errQuota := Classify(0, `{"code":1005,"msg":"daily quota exhausted"}`)
	if errQuota.HTTPStatus() != 402 {
		t.Fatalf("额度耗尽应映射为 402，得到 %d", errQuota.HTTPStatus())
	}
}
