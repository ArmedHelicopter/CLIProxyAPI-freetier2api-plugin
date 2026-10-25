package minimaxcode

import (
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
)

// membershipFixture 是 2026-10-25 实测 get_membership_info 的裁剪响应。
var membershipFixture = map[string]any{
	"has_token_plan":        true,
	"token_plan_tier":       "Go",
	"token_plan_expires_at": float64(1795219200000),
	"tp_remains":            14291681135.49,
	"op_group_id":           "2104384490896888672",
	"opcredit_balance":      float64(0),
	"is_migrated_to_op":     true,
}

// TestExtractTokenPlanHead 验证 membership 的 Token Plan 概要提取。
func TestExtractTokenPlanHead(t *testing.T) {
	head := extractTokenPlanHead(membershipFixture)
	if head == nil {
		t.Fatal("head = nil, want token plan")
	}
	if head.planName != "MiniMax Token Plan Go" {
		t.Fatalf("planName = %q", head.planName)
	}
	if head.groupID != "2104384490896888672" {
		t.Fatalf("groupID = %q", head.groupID)
	}
	if head.expiresAtMS != 1795219200000 {
		t.Fatalf("expiresAtMS = %d", head.expiresAtMS)
	}
	if len(head.metrics) != 1 || head.metrics[0].Key != "token_remaining" {
		t.Fatalf("metrics = %+v", head.metrics)
	}
}

// TestExtractTokenPlanHeadUnsubscribed 验证未订阅（或全零）时返回 nil，
// 调用方回落 op 点数展示。
func TestExtractTokenPlanHeadUnsubscribed(t *testing.T) {
	if head := extractTokenPlanHead(map[string]any{
		"has_token_plan": false,
		"tp_remains":     0.0,
	}); head != nil {
		t.Fatalf("unsubscribed head = %+v", head)
	}
}

// TestTokenPlanRemainFeedsTotals 验证 Token Plan 余量进独立 token_plan 族：
// 与 op 点数的 default 族并存时互不覆盖（同 key 会撞族导致点数被 token
// 数静默吞掉，见 PR 审查 P1-1）。
func TestTokenPlanRemainFeedsTotals(t *testing.T) {
	response := &pluginapi.QuotaFetchResponse{
		Summary: []pluginapi.QuotaMetric{
			{Key: "remaining", Label: "总剩余额度", Value: 500, Unit: "点"},
			{Key: "token_remaining", Label: "Token Plan 剩余", Value: 14291681135, Unit: "tokens"},
		},
	}
	totals := accountpkg.Totals(response)
	// default 族（点数）有容量，优先于 token_plan 族：点数不再被覆盖。
	if totals.Remaining != 500 {
		t.Fatalf("totals = %+v, want default-family remaining 500", totals)
	}
	// 只剩 token_plan 族时也能驱动「剩余」列。
	tokenOnly := &pluginapi.QuotaFetchResponse{
		Summary: []pluginapi.QuotaMetric{
			{Key: "token_remaining", Label: "Token Plan 剩余", Value: 14291681135, Unit: "tokens"},
		},
	}
	if got := accountpkg.Totals(tokenOnly).Remaining; got != 14291681135 {
		t.Fatalf("token-only remaining = %d", got)
	}
}

// TestPlanWindowBucket 验证窗口行的折算：百分比、重置时间、已耗尽与跳过语义。
func TestPlanWindowBucket(t *testing.T) {
	// status=1：正常，99% 剩余。
	bucket, ok := planWindowBucket("通用额度", "5 小时", float64(99), float64(1),
		float64(1792897791846), float64(1792915791846), float64(0), float64(0))
	if !ok {
		t.Fatal("active window must render")
	}
	if bucket.RemainingFraction < 0.98 || bucket.RemainingFraction > 1.0 {
		t.Fatalf("fraction = %v", bucket.RemainingFraction)
	}
	if bucket.ResetTime == "" {
		t.Fatal("ResetTime missing")
	}

	// status=2：耗尽且无百分比，按 0% 展示。
	bucket, ok = planWindowBucket("通用额度", "5 小时", nil, float64(2), nil, nil, nil, nil)
	if !ok || bucket.RemainingFraction != 0 {
		t.Fatalf("exhausted window = ok:%v fraction:%v", ok, bucket.RemainingFraction)
	}

	// status=3：上游不展示，跳过。
	if _, ok = planWindowBucket("通用额度", "5 小时", float64(50), float64(3), nil, nil, nil, nil); ok {
		t.Fatal("dropped window must not render")
	}

	// 非 status=2 且缺百分比（字段缺失/改名）：不渲染——0% 假进度条比不显示更误导。
	if _, ok = planWindowBucket("通用额度", "5 小时", nil, float64(1), nil, nil, nil, nil); ok {
		t.Fatal("missing percent without status=2 must not render")
	}
	if _, ok = planWindowBucket("通用额度", "7 天", "", float64(0), nil, nil, nil, nil); ok {
		t.Fatal("empty-string percent must not render")
	}
}
