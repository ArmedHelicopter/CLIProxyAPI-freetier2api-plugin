package autoclaw

import (
	"context"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"13812345678":    "13812345678",
		" 138-1234-5678": "13812345678",
		"+8613812345678": "13812345678",
		"8613812345678":  "13812345678",
	}
	for raw, want := range cases {
		got, err := normalizePhone(raw)
		if err != nil || got != want {
			t.Fatalf("normalizePhone(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"12345", "23812345678", "1381234567a", ""} {
		if _, err := normalizePhone(bad); err == nil {
			t.Fatalf("normalizePhone(%q) should fail", bad)
		}
	}
}

func TestNormalizeSMSCode(t *testing.T) {
	if got, err := normalizeSMSCode(" 123456 "); err != nil || got != 123456 {
		t.Fatalf("normalizeSMSCode = %d, %v", got, err)
	}
	// 前导零无所谓：服务端只看数值。
	if got, err := normalizeSMSCode("000000"); err != nil || got != 0 {
		t.Fatalf("normalizeSMSCode(000000) = %d, %v", got, err)
	}
	if _, err := normalizeSMSCode("12345"); err == nil {
		t.Fatal("5-digit code should fail")
	}
	if _, err := normalizeSMSCode("12345a"); err == nil {
		t.Fatal("non-digit code should fail")
	}
}

func TestDescribeSMSLoginError(t *testing.T) {
	// 400001 是参数错误而不是「验证码不正确」——混为一谈会把排查方向带偏。
	if got := describeSMSLoginError(400001, "请求数据有问题"); !strings.Contains(got, "参数") {
		t.Fatalf("400001 message = %q", got)
	}
	if got := describeSMSLoginError(630202, ""); !strings.Contains(got, "验证码不正确") {
		t.Fatalf("630202 message = %q", got)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("13812345678"); got != "138****5678" {
		t.Fatalf("MaskPhone = %q", got)
	}
	if got := MaskPhone("123"); got != "123" {
		t.Fatalf("MaskPhone(short) = %q", got)
	}
}

func TestSMSLoginProviderCapability(t *testing.T) {
	instances := Instances(core.Env{})
	// 登录能力随实例类型分派：cn → smsLoginAdapter，global → oauthLoginAdapter。
	if _, cnOK := core.As[core.SMSLoginProvider](instances[0]); !cnOK {
		t.Fatal("cn instance should implement SMSLoginProvider")
	}
	if _, glSMS := core.As[core.SMSLoginProvider](instances[1]); glSMS {
		t.Fatal("global instance must NOT implement SMSLoginProvider (it has OAuth instead)")
	}
	if _, glLogin := core.As[core.LoginProvider](instances[1]); !glLogin {
		t.Fatal("global instance should implement LoginProvider")
	}
	if _, cnLogin := core.As[core.LoginProvider](instances[0]); cnLogin {
		t.Fatal("cn instance must NOT implement LoginProvider (it has SMS login instead)")
	}
}

func TestParseOAuthCallback(t *testing.T) {
	vendor, code, state, ok := ParseOAuthCallback("http://localhost:46952/auth/callback-google?code=ac1&state=up1")
	if !ok || vendor != oauthVendorGoogle || code != "ac1" || state != "up1" {
		t.Fatalf("google callback = %v/%v/%v/%v", vendor, code, state, ok)
	}
	vendor, code, _, ok = ParseOAuthCallback("http://localhost:18432/auth/callback-zai?code=ac2")
	if !ok || vendor != oauthVendorZai || code != "ac2" {
		t.Fatalf("zai callback = %v/%v/%v", vendor, code, ok)
	}
	if _, _, _, ok := ParseOAuthCallback("http://evil.com/auth/callback-google?code=x"); ok {
		t.Fatal("foreign host should be rejected")
	}
	if _, _, _, ok := ParseOAuthCallback("http://localhost:46952/other?code=x"); ok {
		t.Fatal("wrong path should be rejected")
	}
}

func TestOAuthNavigateURI(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("OAuthNavigateURI = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorZai, 18432); got != "http://localhost:18432/auth/callback-zai" {
		t.Fatalf("OAuthNavigateURI zai = %q", got)
	}
}

func TestLoginStartRegionGuards(t *testing.T) {
	instances := Instances(core.Env{})
	cn, gl := instances[0], instances[1]
	// 登录能力随实例类型分派：cn 只有短信登录（SMSLoginProvider），
	// global 只有 OAuth（LoginProvider + CallbackSubmitter）。
	if _, okSMS := core.As[core.SMSLoginProvider](cn); !okSMS {
		t.Fatal("cn should implement SMSLoginProvider")
	}
	if _, okLogin := core.As[core.LoginProvider](cn); okLogin {
		t.Fatal("cn must NOT implement LoginProvider")
	}
	if _, okLogin := core.As[core.LoginProvider](gl); !okLogin {
		t.Fatal("global should implement LoginProvider")
	}
}

// testContext 返回测试用背景 context。
func testContext() context.Context { return context.Background() }
