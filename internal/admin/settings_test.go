package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// fakeRuntimeVendor 只实现运行期字段声明，用来验证 /settings 的通用入口
// 不依赖任何具体供应商。
type fakeRuntimeVendor struct{ core.Meta }

func (*fakeRuntimeVendor) Match(string, string, map[string]any) bool { return false }
func (*fakeRuntimeVendor) Parse([]byte, string) (*core.Credential, error) {
	return nil, errors.New("fake vendor 不支持凭证解析")
}
func (*fakeRuntimeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (*fakeRuntimeVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, errors.New("fake vendor 不支持对话")
}
func (*fakeRuntimeVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return errors.New("fake vendor 不支持对话")
}
func (*fakeRuntimeVendor) RuntimeFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "fake_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"a", "b"}, Description: "假模式"},
		{Name: "fake_flag", Type: pluginapi.ConfigFieldTypeBoolean, Description: "假开关"},
		{Name: "fake_text", Type: pluginapi.ConfigFieldTypeString, Description: "假文本"},
	}
}

// newFakeService 注册两个实例（模拟 workbuddycn / workbuddyglobal 这类多实例供应商）。
func newFakeService(t *testing.T) *Service {
	t.Helper()
	deps := newStubDeps(t)
	for _, id := range []string{"fakecn", "fakeglobal"} {
		meta := core.Meta{VendorID: id, VendorName: "Fake " + id}
		if err := deps.registry.Register(&fakeRuntimeVendor{Meta: meta}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return newServiceWithDeps(t, deps)
}

func TestSettingsViewExposesVendorFields(t *testing.T) {
	raw, err := json.Marshal(newFakeService(t).settingsView()["fields"])
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	// 页面契约是 snake_case：大写 ABI 字段直接输出会让脚本读到 undefined。
	for _, key := range []string{`"name":"fake_mode"`, `"type":"enum"`, `"enum_values":["a","b"]`, `"vendor":"Fake fakecn"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("fields 载荷缺少 %s: %s", key, raw)
		}
	}
	if strings.Contains(string(raw), `"EnumValues"`) {
		t.Errorf("fields 载荷泄漏了 ABI 字段名: %s", raw)
	}

	var views []vendorFieldView
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	// 两个实例声明同一批键，按键名去重后应只剩 3 个。
	if len(views) != 3 || views[0].Name != "fake_mode" || views[0].Vendor != "Fake fakecn" {
		t.Fatalf("fields 未按供应商去重: %#v", views)
	}
}

func TestNormalizeVendorSettings(t *testing.T) {
	service := newFakeService(t)

	got, err := service.normalizeVendorSettings(map[string]string{
		"fake_mode": " a ", "fake_flag": " YES ", "fake_text": " x ",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for key, want := range map[string]string{"fake_mode": "a", "fake_flag": "true", "fake_text": "x"} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}

	for _, raw := range []map[string]string{
		{"fake_mode": "z"},     // 枚举越界
		{"fake_flag": "maybe"}, // 非布尔值
		{"unknown_key": "1"},   // 未经声明的键
	} {
		if _, err := service.normalizeVendorSettings(raw); err == nil {
			t.Errorf("expected error for %#v", raw)
		}
	}

	// 空串表示撤销运行期覆盖，交给供应商端点与 YAML 决定。
	got, err = service.normalizeVendorSettings(map[string]string{"fake_mode": ""})
	if err != nil {
		t.Fatalf("normalize empty: %v", err)
	}
	if value, ok := got["fake_mode"]; !ok || value != "" {
		t.Fatalf("empty value should clear the override: %#v", got)
	}

	if got, err := service.normalizeVendorSettings(nil); err != nil || got != nil {
		t.Fatalf("nil input = %#v, %v", got, err)
	}
}
