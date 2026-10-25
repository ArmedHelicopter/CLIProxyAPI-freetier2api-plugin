package zcode

import (
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
)

// balanceFixture 返回 2026-10-25 实测 billing/balance 的裁剪响应（字段结构一致）。
// 每次返回新副本，避免用例间共享可变包级状态。
func balanceFixture() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"server_time": float64(1792900000),
			"plans": []any{
				map[string]any{
					"plan_id": "zcode-v3-start-plan-1024",
					"name":    "ZCode 1024 Dev Day",
					"status":  "active",
					"ends_at": float64(1792976400),
				},
				map[string]any{
					"plan_id": "zcode-basic",
					"name":    "Basic",
					"status":  "",
					"ends_at": float64(1795219200),
				},
				map[string]any{
					"plan_id": "zcode-expired",
					"name":    "Expired",
					"status":  "active",
					"ends_at": float64(1792800000),
				},
			},
			"balances": []any{
				map[string]any{
					"show_name":       "GLM-5.3-Flash",
					"total_units":     float64(300000000),
					"used_units":      float64(169395514),
					"remaining_units": float64(130604486),
					"expires_at":      float64(1792976400),
				},
				map[string]any{
					"show_name":       "GLM-5.3",
					"total_units":     float64(200000000),
					"used_units":      float64(15638817),
					"remaining_units": float64(184361183),
					"expires_at":      float64(1792976400),
				},
			},
		},
	}
}

// TestBalanceQuotaExposeTotalsFamily 验证 default 族汇总：账号表的
// 「剩余 / 总额」列靠它们算比例，逐模型指标保持独立键。
func TestBalanceQuotaExposeTotalsFamily(t *testing.T) {
	metrics, groups := balanceQuota(balanceFixture())
	response := &pluginapi.QuotaFetchResponse{Summary: metrics, Groups: groups}
	totals := accountpkg.Totals(response)
	if totals.Remaining != 130604486+184361183 {
		t.Fatalf("remaining = %d", totals.Remaining)
	}
	if totals.Total != 500000000 {
		t.Fatalf("total = %d", totals.Total)
	}
	// 模型额度组：每桶一个，分数与到期时间齐备。
	if len(groups) != 1 || len(groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	first := groups[0].Buckets[0]
	want := 130604486.0 / 300000000.0
	if first.RemainingFraction < want-0.0001 || first.RemainingFraction > want+0.0001 {
		t.Fatalf("fraction = %v, want ~%v", first.RemainingFraction, want)
	}
	if first.ResetTime != "2026-10-26T01:00:00Z" {
		t.Fatalf("ResetTime = %q, want 2026-10-26T01:00:00Z (UTC)", first.ResetTime)
	}
}

// TestBalanceQuotaEdgeCases 覆盖边界：空 balances、total=0、越界分数 clamp、
// 全部条目解析失败不给假汇总。
func TestBalanceQuotaEdgeCases(t *testing.T) {
	if m, g := balanceQuota(map[string]any{}); m != nil || g != nil {
		t.Fatalf("empty balances must yield nil/nil, got %+v %+v", m, g)
	}

	// total=0：分数 0，但 remaining 仍计入汇总。
	metrics, _ := balanceQuota(map[string]any{"data": map[string]any{"balances": []any{
		map[string]any{"show_name": "X", "total_units": float64(0), "remaining_units": float64(5)},
	}}})
	if len(metrics) == 0 || metrics[0].Value != 5 {
		t.Fatalf("total=0 metrics = %+v", metrics)
	}

	// remaining 超过 total：clamp 到 1。
	_, groups := balanceQuota(map[string]any{"data": map[string]any{"balances": []any{
		map[string]any{"show_name": "X", "total_units": float64(10), "remaining_units": float64(50)},
	}}})
	if groups[0].Buckets[0].RemainingFraction != 1 {
		t.Fatalf("clamp fraction = %v", groups[0].Buckets[0].RemainingFraction)
	}

	// 条目全部缺 show_name：不给「剩余 0」的假汇总。
	metrics, _ = balanceQuota(map[string]any{"data": map[string]any{"balances": []any{
		map[string]any{"total_units": float64(10)},
	}}})
	if metrics != nil {
		t.Fatalf("unparsable entries must yield nil metrics, got %+v", metrics)
	}
}

// TestPlanExpiryGroupNearestActive 验证有效期口径：空 status 视作 active、
// active 但已过 ends_at 排除、多套餐取最近未过期；到期走 Description。
func TestPlanExpiryGroupNearestActive(t *testing.T) {
	body := balanceFixture()
	plans := planRows(body)
	serverTime := serverSeconds(body)
	group := planExpiryGroup(plans, serverTime)
	if group == nil {
		t.Fatal("expiry group missing")
	}
	bucket := group.Buckets[0]
	// 最近未过期的是 1792976400（1024 礼包），不是更晚的 basic（空 status 也算 active）。
	if bucket.Description == "" {
		t.Fatal("Description must carry the expiry")
	}
	want := "套餐到期 2026-10-26T01:00:00Z"
	if bucket.Description != want {
		t.Fatalf("Description = %q, want %q", bucket.Description, want)
	}
	if bucket.ResetTime != "" {
		t.Fatalf("ResetTime must stay empty for one-time plans, got %q", bucket.ResetTime)
	}
}

// TestActivePlansSharedSemantics 验证 activePlans 的共用口径被
// heldPlanIDs 与 planExpiryGroup 一致消费。
func TestActivePlansSharedSemantics(t *testing.T) {
	plans := planRows(balanceFixture())
	active := activePlans(plans, serverSeconds(balanceFixture()))
	if len(active) != 2 {
		t.Fatalf("active plans = %d, want 2 (1024 礼包 + 空 status basic；过期项排除)", len(active))
	}
	held := heldPlanIDs(plans, serverSeconds(balanceFixture()))
	if len(held) != 2 || held["zcode-expired"] {
		t.Fatalf("held = %+v", held)
	}
}
