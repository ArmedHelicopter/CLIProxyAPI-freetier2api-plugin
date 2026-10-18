package modelconfig

import (
	"context"
	"sync"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// fakeVendor 是最小可注册供应商：模型层只用到身份与区域。
type fakeVendor struct {
	core.Meta
}

func (v *fakeVendor) Match(string, string, map[string]any) bool { return false }
func (v *fakeVendor) Parse([]byte, string) (*core.Credential, error) {
	return &core.Credential{}, nil
}
func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) { return nil, nil }
func (v *fakeVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *fakeVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}

var registerFakes sync.Once

func registerTestVendors() {
	registerFakes.Do(func() {
		core.RegisterVendor(&fakeVendor{Meta: core.Meta{VendorID: "workbuddycn", VendorName: "WB CN", VendorRegion: "cn"}})
		core.RegisterVendor(&fakeVendor{Meta: core.Meta{VendorID: "workbuddyglobal", VendorName: "WB Global", VendorRegion: "global"}})
	})
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	return New(store)
}

func TestExpandKeysByVendorAndRegion(t *testing.T) {
	registerTestVendors()
	if got := ExpandKeys("glm-5.2"); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("bare key = %#v", got)
	}
	if got := ExpandKeys("workbuddycn:glm-5.2"); len(got) != 1 || got[0] != "workbuddycn:glm-5.2" {
		t.Fatalf("vendor key = %#v", got)
	}
	keys := ExpandKeys("cn:glm-5.2")
	if len(keys) == 0 {
		t.Fatal("region alias must expand to registered cn vendors")
	}
	for _, key := range keys {
		if key == "workbuddyglobal:glm-5.2" {
			t.Fatalf("cn alias leaked global key: %#v", keys)
		}
	}
}

func TestAliasUniquenessAndResolve(t *testing.T) {
	m := newTestManager(t)
	m.SetAlias("workbuddycn", "glm-5.2", "glm-max")
	if got := m.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("resolve = %q", got)
	}
	m.SetAlias("workbuddycn", "glm-5.3", "glm-max")
	if got := m.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("rebind resolve = %q", got)
	}
	if m.AliasFor("workbuddycn", "glm-5.2") != "" {
		t.Fatal("old alias should be replaced")
	}
}

func TestDisabledUnionConfigAndState(t *testing.T) {
	registerTestVendors()
	m := newTestManager(t)
	m.Configure(Settings{EnabledRealms: []string{"cn", "global"}, DisabledModels: []string{"cn:glm-5.2"}})
	if !m.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must apply")
	}
	if m.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("config disable must be region scoped")
	}
	m.SetDisabled([]string{"workbuddyglobal:glm-5.2"}, true)
	if !m.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("state disable must apply")
	}
	// 页面启用不能放开配置声明的禁用项。
	m.SetDisabled([]string{"cn:glm-5.2"}, false)
	if !m.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must survive page enable")
	}
}

func TestMergeSameNameModels(t *testing.T) {
	merged := MergeSameNameModels([]pluginapi.ModelInfo{
		{ID: "glm-5.2", ContextLength: 1000, SupportedInputModalities: []string{"text"}},
		{ID: "GLM-5.2", ContextLength: 2000, SupportedInputModalities: []string{"image"}},
	})
	if len(merged) != 1 {
		t.Fatalf("merged len = %d", len(merged))
	}
	if merged[0].ContextLength != 2000 {
		t.Fatalf("context = %d", merged[0].ContextLength)
	}
	if len(merged[0].SupportedInputModalities) != 2 {
		t.Fatalf("modalities = %#v", merged[0].SupportedInputModalities)
	}
}
