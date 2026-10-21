package admin

import (
	"context"
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

func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	for _, vendor := range []core.Vendor{
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddycn", VendorName: "WB CN", VendorRegion: "cn"}},
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddyglobal", VendorName: "WB Global", VendorRegion: "global"}},
	} {
		if err := registry.Register(vendor); err != nil {
			t.Fatal(err)
		}
	}
	return NewCatalog(store, registry)
}

func TestExpandKeysByVendorAndRegion(t *testing.T) {
	manager := newTestCatalog(t)
	if got := manager.ExpandKeys("glm-5.2"); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("bare key = %#v", got)
	}
	if got := manager.ExpandKeys("workbuddycn:glm-5.2"); len(got) != 1 || got[0] != "workbuddycn:glm-5.2" {
		t.Fatalf("vendor key = %#v", got)
	}
	keys := manager.ExpandKeys("cn:glm-5.2")
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
	c := newTestCatalog(t)
	c.SetAlias("workbuddycn", "glm-5.2", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("resolve = %q", got)
	}
	c.SetAlias("workbuddycn", "glm-5.3", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("rebind resolve = %q", got)
	}
	if c.AliasFor("workbuddycn", "glm-5.2") != "" {
		t.Fatal("old alias should be replaced")
	}
}

// TestResolveOutboundAliasRoundTrip 验证别名正向设置与反向还原是对称的。
func TestResolveOutboundAliasRoundTrip(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("alias resolve = %q, want glm-5.3", got)
	}
	// 未设别名的官方 ID 原样返回。
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("passthrough = %q, want deepseek-flash", got)
	}
}

// TestStaleModelStateIsIgnoredNotDeleted 覆盖升级后的陈旧键语义：
//
// 供应商换了对外模型 ID（Qoder 从上游 key 改为规范化 slug），用户状态里仍留着
// 旧键别名与旧键禁用项。它们必须对当前模型失效（否则请求被翻回旧 ID 再撞上
// 禁用），但**不能被删除**——清单可能因为探测失败而暂时拿不到，删除会让
// 用户配置永久丢失。
func TestStaleModelStateIsIgnoredNotDeleted(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "dfmodel", "deepseek-flash")
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	c.SetDisabled([]string{"qodercn:dfmodel", "qodercn:glm-5.3"}, true)
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}, {ID: "glm-5.3"}})

	// 旧 ID 不再注册：别名与禁用都不作用于请求。
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("stale alias must be ignored, resolve = %q", got)
	}
	if c.IsDisabled("qodercn", "deepseek-flash") {
		t.Fatal("stale disabled key must not disable the current model id")
	}

	// 仍然注册着的官方 ID 不受影响。
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("live alias resolve = %q, want glm-5.3", got)
	}
	if !c.IsDisabled("qodercn", "glm-5.3") {
		t.Fatal("live disabled key must still apply")
	}

	// 状态原样保留：清单恢复后旧配置能自动重新生效。
	if c.AliasFor("qodercn", "dfmodel") != "deepseek-flash" {
		t.Fatal("stale alias must be kept in state, only ignored at resolution")
	}
	for _, key := range c.store.Snapshot().DisabledModels {
		if key == "qodercn:dfmodel" {
			return
		}
	}
	t.Fatal("stale disabled key must be kept in state, only ignored at lookup")
}

// TestFallbackModelListDoesNotUnlockDisabledModels 覆盖「清单暂时拿不全」的风险：
//
// 某次上游清单拉取可能只拿到部分模型（探测失败、降级或上游抖动）。此时绝不能
// 放开用户禁用的模型，也不能丢掉禁用配置——一次抖动不能改变鉴权语义。
func TestFallbackModelListDoesNotUnlockDisabledModels(t *testing.T) {
	c := newTestCatalog(t)
	c.SetDisabled([]string{"qodercn:kimi-k3"}, true)

	// 兜底/不完整清单里没有 kimi-k3（不能据此判定它已消失）。
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}})
	if c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("a model missing from a partial list must not be treated as registered")
	}

	// 清单为空（全部探测失败）：再保守一层，禁用照旧生效，不放行。
	c.CacheStatic("qodercn", nil)
	if !c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("with no model list at all, disabled models must stay disabled")
	}
}

func TestDisabledUnionConfigAndState(t *testing.T) {
	c := newTestCatalog(t)
	c.Configure(ModelSettings{EnabledRealms: []string{"cn", "global"}, DisabledModels: []string{"cn:glm-5.2"}})
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must apply")
	}
	if c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("config disable must be region scoped")
	}
	c.SetDisabled([]string{"workbuddyglobal:glm-5.2"}, true)
	if !c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("state disable must apply")
	}
	// 页面启用不能放开配置声明的禁用项。
	c.SetDisabled([]string{"cn:glm-5.2"}, false)
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
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
