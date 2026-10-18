package core

// 本文件覆盖 core 的共享契约：归属判定、注册表、模型键协议、错误映射与
// 通用助手。core 是所有供应商共享的骨架，这些行为出错会影响全部供应商。

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// fakeVendor 是最小可用的 Vendor 实现（只关心归属与解析）。
//
// 内容嗅探按 marker 命中：注册表是全局的，测试之间会累积注册，
// 一个「恒返回 true」的假供应商会污染后续用例。
type fakeVendor struct {
	BaseVendor
	marker string
}

func (v *fakeVendor) Match(_ string, _ string, raw map[string]any) bool {
	return v.marker != "" && GetString(raw, "test_marker") == v.marker
}

func (v *fakeVendor) Parse(raw []byte, fileName string) (*Credential, error) {
	return &Credential{VendorID: v.ID(), FileID: fileName, Native: string(raw)}, nil
}

func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) { return nil, nil }
func (v *fakeVendor) ModelsForAuth(context.Context, *Credential) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *fakeVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *fakeVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}
func (v *fakeVendor) LoginStart(context.Context, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return nil, nil
}
func (v *fakeVendor) LoginPoll(context.Context, string) (*pluginapi.AuthLoginPollResponse, error) {
	return nil, nil
}
func (v *fakeVendor) Refresh(context.Context, *Credential) (*Credential, bool, error) {
	return nil, false, nil
}

func TestResolveVendorPrefersVendorField(t *testing.T) {
	fieldVendor := &fakeVendor{BaseVendor: BaseVendor{VendorID: "testfield"}}
	sniffVendor := &fakeVendor{
		BaseVendor: BaseVendor{VendorID: "testsniff"},
		marker:     "sniff",
	}
	RegisterVendor(fieldVendor)
	RegisterVendor(sniffVendor)

	// vendor 字段是插件自己写的，优先级最高：文件名与内容嗅探都不能翻盘。
	got, ok := ResolveVendor("whatever.json", "", map[string]any{VendorKey: "testfield"})
	if !ok || got.ID() != "testfield" {
		t.Fatalf("vendor field resolution = %v %v, want testfield", got, ok)
	}
}

func TestResolveVendorFallsBackToFileNameThenSniff(t *testing.T) {
	RegisterVendor(&fakeVendor{BaseVendor: BaseVendor{VendorID: "testfile"}})
	RegisterVendor(&fakeVendor{BaseVendor: BaseVendor{VendorID: "testsniffonly"}, marker: "sniffonly"})

	got, ok := ResolveVendor("testfile-abc.json", "", map[string]any{})
	if !ok || got.ID() != "testfile" {
		t.Fatalf("file name resolution = %v %v, want testfile", got, ok)
	}

	// 文件名没有任何线索时才走内容嗅探。
	got, ok = ResolveVendor("acc.json", "", map[string]any{"test_marker": "sniffonly"})
	if !ok || got.ID() != "testsniffonly" {
		t.Fatalf("sniff resolution = %v %v, want testsniffonly", got, ok)
	}
}

func TestResolveVendorRejectsUnknown(t *testing.T) {
	if _, ok := ResolveVendor("zzz.json", "", map[string]any{"foo": "bar"}); ok {
		t.Fatal("unknown credential must not resolve")
	}
}

func TestRegisterVendorRejectsDuplicate(t *testing.T) {
	RegisterVendor(&fakeVendor{BaseVendor: BaseVendor{VendorID: "testdup"}})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate vendor registration must panic")
		}
	}()
	RegisterVendor(&fakeVendor{BaseVendor: BaseVendor{VendorID: "testdup"}})
}

func TestFileNameForAndSanitize(t *testing.T) {
	if got := FileNameFor("Vendor", "a/b c"); got != "vendor-a-b-c.json" {
		t.Fatalf("FileNameFor = %q", got)
	}
	if got := SanitizeFileComponent("../etc/passwd"); got == "" || got == "../etc/passwd" {
		t.Fatalf("SanitizeFileComponent must strip unsafe characters, got %q", got)
	}
}

func TestSplitModelIDAndModelKey(t *testing.T) {
	RegisterVendor(&fakeVendor{BaseVendor: BaseVendor{VendorID: "testmodel"}})
	if prefix, bare := SplitModelID("testmodel:glm-5.2"); prefix != "testmodel" || bare != "glm-5.2" {
		t.Fatalf("SplitModelID = %q %q", prefix, bare)
	}
	// 未知前缀不得拆坏真实模型名。
	if prefix, bare := SplitModelID("gpt-5.5:turbo"); prefix != "" || bare != "gpt-5.5:turbo" {
		t.Fatalf("unknown prefix must not split: %q %q", prefix, bare)
	}
	if got := ModelKeyFor("testmodel", "testmodel:glm-5.2"); got != "testmodel:glm-5.2" {
		t.Fatalf("ModelKeyFor = %q", got)
	}
}

func TestParseCredentialFillsFileIDAndAttributes(t *testing.T) {
	// 用一个故意不设置 FileID 的供应商，验证 core 的兜底。
	v := &noFileIDVendor{BaseVendor: BaseVendor{VendorID: "testnofileid"}}
	RegisterVendor(v)

	credential, ok := ParseCredential([]byte(`{"vendor":"testnofileid"}`), "testnofileid-abc", map[string]string{"realm": "global"})
	if !ok {
		t.Fatal("credential should parse")
	}
	if got := credential.FileIDValue(); got != "testnofileid-abc" {
		t.Fatalf("FileID = %q", got)
	}
	if got := credential.RegionValue(); got != "global" {
		t.Fatalf("region override = %q", got)
	}
}

type noFileIDVendor struct{ BaseVendor }

func (v *noFileIDVendor) Match(_ string, _ string, raw map[string]any) bool {
	return GetString(raw, "vendor") == "testnofileid"
}
func (v *noFileIDVendor) Parse(_ []byte, _ string) (*Credential, error) {
	return &Credential{VendorID: "testnofileid"}, nil
}
func (v *noFileIDVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *noFileIDVendor) ModelsForAuth(context.Context, *Credential) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *noFileIDVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *noFileIDVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}
func (v *noFileIDVendor) LoginStart(context.Context, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return nil, nil
}
func (v *noFileIDVendor) LoginPoll(context.Context, string) (*pluginapi.AuthLoginPollResponse, error) {
	return nil, nil
}
func (v *noFileIDVendor) Refresh(context.Context, *Credential) (*Credential, bool, error) {
	return nil, false, nil
}

func TestJSONHelpers(t *testing.T) {
	payload := map[string]any{
		"a":          "  x  ",
		"b":          "",
		"c":          float64(1700000000000),
		"nested":     map[string]any{"k": "v"},
		"legacy_key": "y",
	}
	if got := GetString(payload, "missing", "b", "a"); got != "x" {
		t.Fatalf("GetString = %q", got)
	}
	if got := GetMap(payload, "nested"); got["k"] != "v" {
		t.Fatalf("GetMap = %v", got)
	}
	if got := GetInt64(payload, "c"); got != 1700000000000 {
		t.Fatalf("GetInt64 = %d", got)
	}
	if got := GetUnixSeconds(payload, "c"); got != 1700000000 {
		t.Fatalf("GetUnixSeconds = %d", got)
	}
	if got := FirstNonEmpty("", "  ", "v"); got != "v" {
		t.Fatalf("FirstNonEmpty = %q", got)
	}
}

func TestVendorFieldVerdict(t *testing.T) {
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "other"}, "mine"); !decided || claimed {
		t.Fatal("foreign vendor must be rejected")
	}
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "mine"}, "mine"); !decided || !claimed {
		t.Fatal("own vendor must be claimed")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{"vendor": "family"}, "mine", "family"); decided {
		t.Fatal("alias must defer to other evidence")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{}, "mine"); decided {
		t.Fatal("missing vendor must defer")
	}
}

// testUpstreamError 是用于错误映射测试的分类错误。
type testUpstreamError struct {
	status    int
	retryable bool
	kind      string
	message   string
}

func (e *testUpstreamError) Error() string        { return e.message }
func (e *testUpstreamError) HTTPStatus() int      { return e.status }
func (e *testUpstreamError) Retryable() bool      { return e.retryable }
func (e *testUpstreamError) UpstreamKind() string { return e.kind }

func TestToPluginError(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantRetry  bool
	}{
		{&testUpstreamError{status: 401, kind: "credential", message: "bad"}, 401, false},
		{&testUpstreamError{status: 429, retryable: true, kind: "rate_limit", message: "slow"}, 429, true},
		{&testUpstreamError{status: 503, retryable: true, kind: "transient", message: "down"}, 503, true},
		{errors.New("plain"), http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		err := ToPluginError(tc.err)
		pluginErr, okPlugin := err.(*PluginError)
		if !okPlugin {
			t.Fatalf("%v -> %T, want *PluginError", tc.err, err)
		}
		if pluginErr.HTTPStatus != tc.wantStatus || pluginErr.Retryable != tc.wantRetry {
			t.Fatalf("%v -> status=%d retry=%v", tc.err, pluginErr.HTTPStatus, pluginErr.Retryable)
		}
	}
	if ToPluginError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}

func TestBaseVendorDefaults(t *testing.T) {
	v := &BaseVendor{VendorID: "x", VendorName: "X", VendorRegion: "cn"}
	if v.ID() != "x" || v.Name() != "X" || v.Region() != "cn" || v.ModelPrefix() != "" {
		t.Fatal("identity defaults wrong")
	}
	tokens, err := v.CountTokens(context.Background(), nil, &ExecuteRequest{Payload: []byte("123456")})
	if err != nil || tokens != 2 {
		t.Fatalf("CountTokens = %d %v", tokens, err)
	}
	if _, errQuota := v.Quota(context.Background(), nil); errQuota == nil {
		t.Fatal("default quota must report unsupported")
	}
	if result, errCheckin := v.Checkin(context.Background(), nil); result != nil || errCheckin != nil {
		t.Fatal("default checkin must be a no-op")
	}
	if v.SupportsCheckin() || len(v.Tasks()) != 0 {
		t.Fatal("default checkin/tasks must be empty")
	}
}
