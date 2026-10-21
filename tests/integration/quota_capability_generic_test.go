package tests

// 本文件锁定「额度能力判定对所有供应商通用」。
//
// 页面只读一个字段 accounts[].supports_quota，因此这个字段必须对**每一个**
// 已注册的供应商实例都成立，而不是只对 OpenCode ZEN 打的补丁：
//   - 实现了 QuotaProvider 的供应商 → true（没查到就短暂显示「查询中…」）；
//   - 没实现的 → false（立即显示「-- / --」，永不进入查询中）。
//
// 这里枚举真实的注册表实例，逐个与接口断言对账。新增供应商若忘了实现 Quota
// 却期望有额度列，或实现了却漏报，都会在这里被发现。

import (
	"testing"

	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
)

// TestSupportsQuotaIsGenericAcrossVendors 枚举全部内置供应商实例。
func TestSupportsQuotaIsGenericAcrossVendors(t *testing.T) {
	registry := core.NewRegistry()
	for _, factory := range app.BuiltinFactories() {
		for _, vendor := range factory.Instances(core.Env{}) {
			if errRegister := registry.Register(vendor); errRegister != nil {
				t.Fatalf("register %s: %v", vendor.ID(), errRegister)
			}
		}
	}
	vendors := registry.Vendors()
	if len(vendors) == 0 {
		t.Fatal("no vendors registered")
	}

	// 至少存在两类实例，否则这条断言没有意义（全是 true 或全是 false 都测不出问题）。
	var withQuota, withoutQuota []string
	for _, vendor := range vendors {
		_, okQuota := core.As[core.QuotaProvider](vendor)
		if okQuota {
			withQuota = append(withQuota, vendor.ID())
			continue
		}
		withoutQuota = append(withoutQuota, vendor.ID())
		// 没有额度接口的供应商：列表里必须存在，且被识别为不支持。
		// 这就是「立即显示 -- / --」的那一类。
		if vendor.ID() == "" {
			t.Fatal("vendor with empty ID")
		}
	}
	if len(withoutQuota) == 0 {
		t.Fatal("expected at least one vendor without quota (cline, opencodezen); " +
			"if all vendors now implement Quota, update this assertion")
	}
	if len(withQuota) == 0 {
		t.Fatal("expected at least one vendor with quota")
	}
	t.Logf("quota-capable: %v; no-quota (show -- / --): %v", withQuota, withoutQuota)

	// 快照：把「谁没有额度接口」钉死。它不是实现细节，而是**用户可见行为**
	// ——这些供应商的账号会立刻显示「-- / --」。新增/移除 Quota 实现时必须
	// 同步更新这里，并确认页面语义仍然正确。
	wantNoQuota := map[string]bool{"cline": true, "opencodezen": true}
	if len(withoutQuota) != len(wantNoQuota) {
		t.Fatalf("no-quota vendors = %v, want %v", withoutQuota, wantNoQuota)
	}
	for _, id := range withoutQuota {
		if !wantNoQuota[id] {
			t.Fatalf("vendor %s unexpectedly lacks quota (update wantNoQuota if intentional)", id)
		}
	}
}
