package autoclaw

import (
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestRegionVendorIDs(t *testing.T) {
	if got := VendorIDFor(RegionCN); got != VendorIDCN {
		t.Fatalf("VendorIDFor(cn) = %q", got)
	}
	if got := VendorIDFor(RegionGlobal); got != VendorIDGlobal {
		t.Fatalf("VendorIDFor(global) = %q", got)
	}
	if VendorNameFor(RegionCN) != "AutoClaw 国内版" || VendorNameFor(RegionGlobal) != "AutoClaw 国际版" {
		t.Fatalf("unexpected vendor names")
	}
}

func TestNormalizeRegion(t *testing.T) {
	cases := map[string]Region{
		"cn":     RegionCN,
		"":       RegionCN,
		"GLOBAL": RegionGlobal,
		"intl":   RegionGlobal,
		"junk":   RegionCN,
	}
	for raw, want := range cases {
		if got := NormalizeRegion(raw); got != want {
			t.Fatalf("NormalizeRegion(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestInstancesAreTwoRegions(t *testing.T) {
	instances := Instances(core.Env{})
	if len(instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(instances))
	}
	if instances[0].ID() != VendorIDCN || instances[1].ID() != VendorIDGlobal {
		t.Fatalf("instance IDs = %q, %q", instances[0].ID(), instances[1].ID())
	}
	if instances[0].Region() != string(RegionCN) || instances[1].Region() != string(RegionGlobal) {
		t.Fatalf("instance regions = %q, %q", instances[0].Region(), instances[1].Region())
	}
}

func TestParseCredential(t *testing.T) {
	cred, errParse := ParseCredential([]byte(`{"api_key":"sk-abc12345","region":"global","label":"测试"}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cred.APIKey != "sk-abc12345" || cred.Region != RegionGlobal || cred.Label != "测试" {
		t.Fatalf("credential = %+v", cred)
	}
	if _, errParse := ParseCredential([]byte(`{"label":"no key"}`)); errParse == nil {
		t.Fatal("expected error for credential without key")
	}
}

func TestParseCredentialAcceptsKeyAliases(t *testing.T) {
	for _, field := range []string{"apikey", "autoclaw_key", "token"} {
		raw := `{"` + field + `":"sk-abc"}`
		if _, errParse := ParseCredential([]byte(raw)); errParse != nil {
			t.Fatalf("parse %s: %v", field, errParse)
		}
	}
}

func TestLooksLikeCredentialStrict(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      string
		want     bool
	}{
		{"filename cn", "autoclawcn-u1.json", `{}`, true},
		{"filename global", "autoclawglobal-u1.json", `{}`, true},
		{"filename family", "my-autoclaw.json", `{}`, true},
		{"vendor field", "acc.json", `{"vendor":"autoclawglobal"}`, true},
		{"dedicated key", "acc.json", `{"autoclaw_key":"sk-abc"}`, true},
		// 通用字段名不得作为判据：api_key 单独出现是 ZEN 的形态。
		{"generic api_key is foreign", "acc.json", `{"api_key":"sk-abc"}`, false},
		{"generic token is foreign", "acc.json", `{"token":"t"}`, false},
		{"foreign vendor is foreign", "acc.json", `{"vendor":"qodercn"}`, false},
		{"empty is nothing", "acc.json", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			// 允许空对象：手工构造的测试输入。
			_ = jsonUnmarshalForTest(tc.raw, &payload)
			if got := LooksLikeCredential(payload, tc.fileName, ""); got != tc.want {
				t.Fatalf("LooksLikeCredential(%q, %q) = %v, want %v", tc.fileName, tc.raw, got, tc.want)
			}
		})
	}
}

func TestMatchRoutesRegionsByVendorField(t *testing.T) {
	env := core.Env{}
	instances := Instances(env)
	cn := instances[0].(*adapter)
	gl := instances[1].(*adapter)

	cnPayload := map[string]any{core.VendorKey: "autoclawcn"}
	glPayload := map[string]any{core.VendorKey: "autoclawglobal"}
	if !cn.Match("acc.json", "", cnPayload) || cn.Match("acc.json", "", glPayload) {
		t.Fatal("cn instance misroutes by vendor field")
	}
	if !gl.Match("acc.json", "", glPayload) || gl.Match("acc.json", "", cnPayload) {
		t.Fatal("global instance misroutes by vendor field")
	}
	// 文件名前缀兜底：无 vendor 字段时按前缀归属。
	if !cn.Match("autoclawcn-u1.json", "", map[string]any{}) {
		t.Fatal("cn instance should claim autoclawcn-*.json")
	}
	if cn.Match("autoclawglobal-u1.json", "", map[string]any{}) {
		t.Fatal("cn instance must not claim autoclawglobal-*.json")
	}
}

func TestConfigureBaseURLPerInstance(t *testing.T) {
	instances := Instances(core.Env{})
	cn := instances[0].(*adapter)
	gl := instances[1].(*adapter)
	cn.Configure(core.Settings{}, map[string]string{"autoclawcn_base_url": "http://cn.test"})
	if cn.baseURL != "http://cn.test" {
		t.Fatalf("cn baseURL = %q", cn.baseURL)
	}
	if cn.resolveBaseURL() != "http://cn.test" {
		t.Fatalf("resolveBaseURL = %q", cn.resolveBaseURL())
	}
	// 区域实例只读自己的键：autoclawglobal_* 不影响 cn 实例。
	gl.Configure(core.Settings{}, map[string]string{"autoclawglobal_base_url": "http://gl.test"})
	if gl.resolveBaseURL() != "http://gl.test" {
		t.Fatalf("gl resolveBaseURL = %q", gl.resolveBaseURL())
	}
	// 未配置时骨架返回空基地址（默认域名待回填），调用方据此报错而不是打猜的域名。
	gl.Configure(core.Settings{}, map[string]string{})
	if gl.resolveBaseURL() != DefaultBaseURLGlobal {
		t.Fatalf("resolveBaseURL fallback = %q", gl.resolveBaseURL())
	}
}

func TestBaseURLNormalizesTrailingSlash(t *testing.T) {
	if got := BaseURL("http://x.test/", RegionCN); got != "http://x.test" {
		t.Fatalf("BaseURL trailing slash = %q", got)
	}
	if got := BaseURL("  ", RegionGlobal); got != DefaultBaseURLGlobal {
		t.Fatalf("BaseURL empty override = %q", got)
	}
}

func TestMaskedKey(t *testing.T) {
	if got := MaskedKey("sk-abcdefghijk"); !strings.HasPrefix(got, "sk-a") || !strings.HasSuffix(got, "hijk") || strings.Contains(got, "bcdefg") {
		t.Fatalf("MaskedKey = %q", got)
	}
	if got := MaskedKey("short"); got != "*****" {
		t.Fatalf("MaskedKey short = %q", got)
	}
}

func TestClassifyStatuses(t *testing.T) {
	f := Classify(401, "denied")
	if f.Kind != core.FailureKindCredential {
		t.Fatalf("401 kind = %q", f.Kind)
	}
	f = Classify(429, "slow down")
	if f.Kind != core.FailureKindRateLimit {
		t.Fatalf("429 kind = %q", f.Kind)
	}
	f = Classify(402, "insufficient quota")
	if f.Kind != core.FailureKindHardCredit {
		t.Fatalf("402 kind = %q", f.Kind)
	}
}

func jsonUnmarshalForTest(raw string, target *map[string]any) error {
	return jsonUnmarshalHelper(raw, target)
}
