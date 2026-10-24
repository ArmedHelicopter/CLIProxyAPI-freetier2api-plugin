package raccoon

import (
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestInstancesSingle(t *testing.T) {
	instances := Instances(core.Env{})
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	if instances[0].ID() != VendorID || instances[0].Name() != VendorName {
		t.Fatalf("instance = %q / %q", instances[0].ID(), instances[0].Name())
	}
	if instances[0].Region() != "" {
		t.Fatalf("raccoon has no region concept, got %q", instances[0].Region())
	}
}

func TestParseCredential(t *testing.T) {
	cred, errParse := ParseCredential([]byte(`{"access_token":"eyJ.a.b","refresh_token":"r1","uid":"7591340","nickname":"小浣熊-1","expires_at":1791529021000}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cred.AccessTokenValue() != "eyJ.a.b" || cred.RefreshTokenValue() != "r1" {
		t.Fatalf("tokens = %+v", cred)
	}
	if cred.UIDValue() != "7591340" || cred.NicknameValue() != "小浣熊-1" {
		t.Fatalf("identity = %+v", cred)
	}
	if cred.ExpiresAtValue() != 1791529021 {
		t.Fatalf("expires_at should normalize ms→s, got %d", cred.ExpiresAtValue())
	}
	if _, errParse := ParseCredential([]byte(`{"nickname":"no token"}`)); errParse == nil {
		t.Fatal("expected error for credential without any token")
	}
}

func TestJWTExpiry(t *testing.T) {
	// eyJ… payload = {"exp":1791529021,"user_id":"7591340"}
	token := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTE1MjkwMjEsInVzZXJfaWQiOiI3NTkxMzQwIn0.sig"
	if got := jwtExpiry(token); got != 1791529021 {
		t.Fatalf("jwtExpiry = %d", got)
	}
	if got := jwtUserID(token); got != "7591340" {
		t.Fatalf("jwtUserID = %q", got)
	}
	if got := jwtExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("jwtExpiry(garbage) = %d", got)
	}
}

func TestLooksLikeCredentialStrict(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      string
		want     bool
	}{
		{"filename", "raccoon-u1.json", `{}`, true},
		{"filename pinyin", "xiaohuanxiong-u1.json", `{}`, true},
		{"filename family", "my-raccoon.json", `{}`, true},
		{"vendor field", "acc.json", `{"vendor":"raccoon"}`, true},
		{"snake token pair + uid", "acc.json", `{"access_token":"a","refresh_token":"r","uid":"1"}`, true},
		{"pair without uid is foreign", "acc.json", `{"access_token":"a","refresh_token":"r"}`, false},
		{"camel pair is foreign", "acc.json", `{"accessToken":"a","refreshToken":"r"}`, false},
		{"generic token is foreign", "acc.json", `{"token":"t"}`, false},
		{"foreign vendor is foreign", "acc.json", `{"vendor":"opencodezen"}`, false},
		{"empty is nothing", "acc.json", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			_ = jsonUnmarshalHelper(tc.raw, &payload)
			if got := LooksLikeCredential(payload, tc.fileName, ""); got != tc.want {
				t.Fatalf("LooksLikeCredential(%q, %q) = %v, want %v", tc.fileName, tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseCatalogShapes(t *testing.T) {
	// 数字与字符串两种 context_window 形态，params 嵌套优先级低于顶层。
	body := `{"code":0,"data":{"categories":[
		{"type":"a","models":[{"name":"m1","description":"d1","context_window":100000,"max_tokens":"50000"}]},
		{"type":"b","models":[{"name":"m2","description":"d2","params":{"context_window":20,"max_tokens":10}}]},
		{"name":"m1","description":"dup"},
		{"name":"","description":"drop"},
		{"name":"m3","visible":false,"description":"invisible"}
	]}}`
	models, errParse := parseCatalog([]byte(body))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 (m1,m2)", models)
	}
	if models[0].ID != "m1" || models[0].ContextWindow != 100000 || models[0].MaxOutputTokens != 50000 {
		t.Fatalf("m1 = %+v", models[0])
	}
	if models[1].ID != "m2" || models[1].ContextWindow != 20 || models[1].MaxOutputTokens != 10 {
		t.Fatalf("m2 = %+v", models[1])
	}
}

func TestParseCatalogBusinessError(t *testing.T) {
	if _, err := parseCatalog([]byte(`{"code":14018,"message":"积分不足"}`)); err == nil {
		t.Fatal("expected business error")
	}
}

func TestConfigureBaseURLs(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{
		"raccoon_llm_base_url":    "http://llm.test",
		"raccoon_auth_base_url":   "http://auth.test",
		"raccoon_points_base_url": "http://points.test",
		"raccoon_origin":          "http://origin.test",
	})
	if v.resolveLLMBase() != "http://llm.test" || v.resolveAuthBase() != "http://auth.test" ||
		v.resolvePointsBase() != "http://points.test" || v.resolveOrigin() != "http://origin.test" {
		t.Fatalf("base URLs = %q/%q/%q/%q", v.resolveLLMBase(), v.resolveAuthBase(), v.resolvePointsBase(), v.resolveOrigin())
	}
	v.Configure(core.Settings{}, map[string]string{})
	if v.resolveLLMBase() != DefaultLLMBase || v.resolveAuthBase() != DefaultAuthBase {
		t.Fatalf("fallback base URLs = %q/%q", v.resolveLLMBase(), v.resolveAuthBase())
	}
}

func TestParseCallbackCode(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
		want bool
	}{
		{"official scheme", "office-raccoon://auth/callback?code=ac_1&state=s1", "ac_1", true},
		{"loopback callback rejected (authorize-page flow only)", "http://127.0.0.1:17867/auth/callback?code=ac_2&state=s2", "", false},
		{"evil host", "office-raccoon://evil.com/callback?code=x", "", false},
		{"wrong path", "http://127.0.0.1:1/other?code=x", "", false},
		{"garbage", "not a url ::", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, ok := ParseCallbackCode(tc.raw)
			if ok != tc.want || code != tc.code {
				t.Fatalf("ParseCallbackCode(%q) = %q/%v, want %q/%v", tc.raw, code, ok, tc.code, tc.want)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	got := AuthorizeURL("https://x.test", "办公小浣熊客户端", "st")
	if !strings.HasPrefix(got, "https://x.test/code/authorize?") || !strings.Contains(got, "state=st") {
		t.Fatalf("AuthorizeURL = %q", got)
	}
}

func TestClassifyByStatusAndText(t *testing.T) {
	if f := Classify(401, "denied"); f.Kind != core.FailureKindSessionDead {
		t.Fatalf("401 kind = %q", f.Kind)
	}
	if f := Classify(429, "slow"); f.Kind != core.FailureKindSoftRate {
		t.Fatalf("429 kind = %q", f.Kind)
	}
	if f := Classify(200, "积分不足"); f.Kind != core.FailureKindHardCredit {
		t.Fatalf("quota text kind = %q", f.Kind)
	}
	// "insufficient" 单独出现不判额度（避免 context window 误伤）。
	if f := Classify(400, "context window is insufficient for this model"); f.Kind != core.FailureKindClient {
		t.Fatalf("insufficient-without-quota-word kind = %q", f.Kind)
	}
	if f := ClassifyBusiness(200, 14018, "额度不足"); f.Kind != core.FailureKindHardCredit {
		t.Fatalf("14018 kind = %q", f.Kind)
	}
}

func TestMaskedKey(t *testing.T) {
	got := MaskedKey("eyJ-abcdefghijk")
	if !strings.HasPrefix(got, "eyJ-") || !strings.HasSuffix(got, "hijk") {
		t.Fatalf("MaskedKey = %q", got)
	}
}
