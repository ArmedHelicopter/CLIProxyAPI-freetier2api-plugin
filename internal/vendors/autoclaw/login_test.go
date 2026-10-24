package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	cn := instances[0]
	_ = instances[1]
	// 登录能力随实例类型分派：cn 只有短信登录（SMSLoginProvider），
	// global 只有 OAuth（LoginProvider + CallbackSubmitter）。
	if _, okSMS := core.As[core.SMSLoginProvider](cn); !okSMS {
		t.Fatal("cn should implement SMSLoginProvider")
	}
	if _, okLogin := core.As[core.LoginProvider](cn); okLogin {
		t.Fatal("cn must NOT implement LoginProvider")
	}
	// 国际版 OAuth 第②步强制风控验证码（浏览器端 SDK），插件侧无法完成——
	// LoginStart 返回指引文案页，不声明 LoginProvider（控制台回退凭证填写）。
}

// testContext 返回测试用背景 context。
func testContext() context.Context { return context.Background() }

// TestOAuthDefaultCallbackPorts 锁定两个变体的回调端口：Zai 只能用官方客户端
// 登记的四个端口之一，Google 走 loopback 宽松匹配。
func TestOAuthDefaultCallbackPorts(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorZai, 0); got != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("zai navigate uri = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google navigate uri = %q", got)
	}
}

func TestParseOAuthCallbackAcceptsLoopbackAliases(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:46952/auth/callback-google?code=c1&state=s1",
		"http://[::1]:46952/auth/callback-google?code=c1&state=s1",
	} {
		if _, code, _, ok := ParseOAuthCallback(raw); !ok || code != "c1" {
			t.Fatalf("ParseOAuthCallback(%q) = %q, %v", raw, code, ok)
		}
	}
}

func TestOAuthCaptchaConfigParsesEnvelope(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != OAuthCaptchaConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, OAuthCaptchaConfigPath)
		}
		if r.Header.Get("X-Auth-Sign") == "" {
			t.Error("captcha-config 请求缺少签名头")
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"enabled":true,"region":"ga",
			"prefix":"sq51tr","scene_id":"18vhnjxl","captcha_supplier":"aliyun"}}`))
	}))
	defer server.Close()

	config, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig != nil {
		t.Fatalf("OAuthCaptchaConfig: %v", errConfig)
	}
	if !config.Enabled || config.SceneID != "18vhnjxl" || config.Region != "ga" || config.Prefix != "sq51tr" {
		t.Fatalf("config = %#v", config)
	}
}

func TestOAuthCaptchaConfigSurfacesUpstreamCode(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":630014,"msg":"Verification failed","data":null}`))
	}))
	defer server.Close()

	_, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig == nil || !strings.Contains(errConfig.Error(), "风控验证") {
		t.Fatalf("err = %v, want 风控验证提示", errConfig)
	}
}

func TestLoginStartRequiresCaptcha(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	// 请求地址不可达也没关系：缺验证码必须在发请求之前就被挡下。
	oauth.baseURL = "http://127.0.0.1:1"
	_, errStart := oauth.LoginStart(testContext(), map[string]any{"oauth_variant": "google"})
	if errStart == nil || !strings.Contains(errStart.Error(), "人机验证") {
		t.Fatalf("err = %v, want 人机验证提示", errStart)
	}
}

func TestLoginStartRequestsOAuthURLAndRegistersSession(t *testing.T) {
	useLoopbackHost(t)
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"oauth_url":"https://accounts.example/oauth?x=1"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	response, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant":            "zai",
		"ali_captcha_verify_param": "captcha-token-1",
	})
	if errStart != nil {
		t.Fatalf("LoginStart: %v", errStart)
	}
	if response.URL != "https://accounts.example/oauth?x=1" {
		t.Fatalf("url = %q", response.URL)
	}
	if !strings.HasPrefix(response.State, oauth.ID()+"_oauth_") {
		t.Fatalf("state = %q", response.State)
	}
	if response.Metadata["callback_hint"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if gotPath != OAuthURLPathZai {
		t.Fatalf("path = %q, want %q", gotPath, OAuthURLPathZai)
	}
	if gotBody["ali_captcha_verify_param"] != "captcha-token-1" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotBody["navigate_uri"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("navigate_uri = %#v", gotBody["navigate_uri"])
	}
	if gotBody["source_id"] != protocolName {
		t.Fatalf("source_id = %#v", gotBody["source_id"])
	}

	entry, pollState := oauth.Env.Logins.Poll(response.State)
	if pollState != core.PollReady || entry == nil {
		t.Fatalf("session not registered: %v", pollState)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		t.Fatal("session data type mismatch")
	}
	if pending.Vendor != oauthVendorZai || pending.NavigateURI != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("pending = %#v", pending)
	}
	if pending.DeviceID == "" || pending.DeviceID != gotBody["device_id"] {
		t.Fatalf("device id mismatch: pending=%q body=%#v", pending.DeviceID, gotBody["device_id"])
	}
}

func TestLoginStartRejectsUnknownVariant(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	_, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant": "github", "ali_captcha_verify_param": "x",
	})
	if errStart == nil || !strings.Contains(errStart.Error(), "未知的登录方式") {
		t.Fatalf("err = %v", errStart)
	}
}

func TestOAuthLoginCapabilitiesAndVariants(t *testing.T) {
	instances := Instances(envWithLogins())
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[1]); !okCaptcha {
		t.Fatal("global instance should implement LoginCaptchaProvider")
	}
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[0]); okCaptcha {
		t.Fatal("cn instance must NOT implement LoginCaptchaProvider")
	}
	variants, okVariants := core.As[core.LoginVariantProvider](instances[1])
	if !okVariants {
		t.Fatal("global instance should implement LoginVariantProvider")
	}
	list := variants.LoginVariants()
	if len(list) != 2 || list[0].ID != "zai" || list[1].ID != "google" {
		t.Fatalf("variants = %#v", list)
	}
	if list[1].CallbackHint != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google hint = %q", list[1].CallbackHint)
	}
}
