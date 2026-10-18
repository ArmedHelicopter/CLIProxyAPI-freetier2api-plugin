package main

// 本文件锁死对外契约：vendor 实例清单（ID/名称/区域）与供应商专属配置键。
//
// 这些是用户可见的红线（凭证文件名前缀、控制台下拉项、配置键语义），
// 任何重构都不应改变它们；一旦变了，这里会先失败。

import (
	"testing"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/cline"
	"freetier2api-plugin/internal/vendors/minimaxcode"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/internal/vendors/zcode"
)

// TestVendorRegistryCompatibility 锁定 13 个供应商实例的对外身份与顺序。
func TestVendorRegistryCompatibility(t *testing.T) {
	want := []struct {
		id     string
		name   string
		region string
	}{
		{"cline", "Cline", ""},
		{"codearts", "CodeArts", "cn"},
		{"minimaxcodecn", "MiniMax Code 国内版", "cn"},
		{"minimaxcodeglobal", "MiniMax Code 国际版", "global"},
		{"opencodezen", "OpenCode ZEN", ""},
		{"qodercn", "Qoder 国内版", "cn"},
		{"qoderglobal", "Qoder 国际版", "global"},
		{"traecn", "Trae 国内版", "cn"},
		{"traeglobal", "Trae 国际版", "global"},
		{"traesolo", "TRAE SOLO", "cn"},
		{"workbuddycn", "WorkBuddy 国内版", "cn"},
		{"workbuddyglobal", "WorkBuddy 国际版", "global"},
		{"zcode", "ZCode", "global"},
	}
	vendors := core.Vendors()
	if len(vendors) != len(want) {
		t.Fatalf("vendor count = %d, want %d", len(vendors), len(want))
	}
	for index, expected := range want {
		vendor := vendors[index]
		if vendor.ID() != expected.id || vendor.Name() != expected.name || vendor.Region() != expected.region {
			t.Fatalf("vendor[%d] = %q/%q/%q, want %q/%q/%q", index,
				vendor.ID(), vendor.Name(), vendor.Region(), expected.id, expected.name, expected.region)
		}
		if vendor.ModelPrefix() != "" {
			t.Fatalf("vendor %s model prefix = %q, want empty", vendor.ID(), vendor.ModelPrefix())
		}
	}
}

// TestVendorConfigKeysStillWork 锁定各家的 base_url 覆盖键（含历史别名）。
func TestVendorConfigKeysStillWork(t *testing.T) {
	raw := map[string]string{
		"cline_base_url":           "http://cline.test",
		"zen_base_url":             "http://zen.test",
		"minimax_code_cn_base_url": "http://minimax-cn.test",
		"minimax_global_base_url":  "http://minimax-global.test", // 历史别名
		"zcode_base_url":           "http://zcode.test",
	}
	configureVendors(pluginConfig{Raw: raw})
	t.Cleanup(func() {
		configureVendors(pluginConfig{Raw: map[string]string{
			"cline_base_url":           "",
			"zen_base_url":             "",
			"minimax_code_cn_base_url": "",
			"minimax_global_base_url":  "",
			"zcode_base_url":           "",
		}})
	})

	if got := cline.BaseURLOverride(); got != "http://cline.test" {
		t.Fatalf("cline base url = %q", got)
	}
	if got := opencodezen.BaseURLOverride(); got != "http://zen.test" {
		t.Fatalf("zen base url = %q", got)
	}
	if got := minimaxcode.BaseURLFor(minimaxcode.RegionCN); got != "http://minimax-cn.test" {
		t.Fatalf("minimax cn base url = %q", got)
	}
	if got := minimaxcode.BaseURLFor(minimaxcode.RegionGlobal); got != "http://minimax-global.test" {
		t.Fatalf("minimax global base url = %q", got)
	}
	if got := zcode.BaseURLOverride(); got != "http://zcode.test" {
		t.Fatalf("zcode base url = %q", got)
	}
}
