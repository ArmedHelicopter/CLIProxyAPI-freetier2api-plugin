package admin

import (
	"context"
	"errors"
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
		{Name: "fake_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"a", "b"}},
		{Name: "fake_flag", Type: pluginapi.ConfigFieldTypeBoolean},
		{Name: "fake_text", Type: pluginapi.ConfigFieldTypeString},
	}
}

func TestNormalizeVendorSettings(t *testing.T) {
	registry := core.NewRegistry()
	if err := registry.Register(&fakeRuntimeVendor{Meta: core.Meta{VendorID: "fake"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	service := &Service{registry: registry}

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
