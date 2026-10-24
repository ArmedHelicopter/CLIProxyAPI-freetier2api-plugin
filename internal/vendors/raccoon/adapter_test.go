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
	cred, errParse := ParseCredential([]byte(`{"raccoon_key":"rk-abc12345","label":"测试"}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cred.APIKey != "rk-abc12345" || cred.Label != "测试" {
		t.Fatalf("credential = %+v", cred)
	}
	if _, errParse := ParseCredential([]byte(`{"label":"no key"}`)); errParse == nil {
		t.Fatal("expected error for credential without key")
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
		{"dedicated key", "acc.json", `{"raccoon_key":"rk-abc"}`, true},
		// 通用字段名不得作为判据：api_key 单独出现是 ZEN 的形态。
		{"generic api_key is foreign", "acc.json", `{"api_key":"sk-abc"}`, false},
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

func TestConfigureBaseURL(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"raccoon_base_url": "http://raccoon.test"})
	if v.resolveBaseURL() != "http://raccoon.test" {
		t.Fatalf("resolveBaseURL = %q", v.resolveBaseURL())
	}
	v.Configure(core.Settings{}, map[string]string{})
	if v.resolveBaseURL() != DefaultBaseURL {
		t.Fatalf("resolveBaseURL fallback = %q", v.resolveBaseURL())
	}
}

func TestBaseURLNormalizesTrailingSlash(t *testing.T) {
	if got := BaseURL("http://x.test/"); got != "http://x.test" {
		t.Fatalf("BaseURL trailing slash = %q", got)
	}
}

func TestMaskedKey(t *testing.T) {
	got := MaskedKey("rk-abcdefghijk")
	if !strings.HasPrefix(got, "rk-a") || !strings.HasSuffix(got, "hijk") {
		t.Fatalf("MaskedKey = %q", got)
	}
}

func TestClassifyStatuses(t *testing.T) {
	if f := Classify(401, "denied"); f.Kind != core.FailureKindCredential {
		t.Fatalf("401 kind = %q", f.Kind)
	}
	if f := Classify(429, "slow down"); f.Kind != core.FailureKindRateLimit {
		t.Fatalf("429 kind = %q", f.Kind)
	}
}
