package zcode

// 套餐探测与领取的纯函数测试（不触网）。
//
// 覆盖三类容易静默出错的地方：
//   - 归一化：plan_id 大小写/空白、status 非 active 的过期套餐不该算「已持有」；
//   - 集合运算：已投放 ∩ 已持有 = 按钮置灰的依据；
//   - 业务码翻译：页面上的文案直接来自这里。

import (
	"context"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

func TestNormalizePlanIDIsCaseAndSpaceInsensitive(t *testing.T) {
	// 上游的 plan_id 大小写不统一（「ZCode-V3-Start-Plan」与「zcode-v3-start-plan」
	// 指同一个套餐）。不归一化会让已领套餐被再次列成可领。
	if got := normalizePlanID("  ZCode-V3-Start-Plan \n"); got != "zcode-v3-start-plan" {
		t.Fatalf("normalizePlanID = %q, want zcode-v3-start-plan", got)
	}
	if got := normalizePlanID("   "); got != "" {
		t.Fatalf("normalizePlanID(空白) = %q, want 空串", got)
	}
}

func TestParsePlanRequiresPlanID(t *testing.T) {
	// 缺 plan_id 的条目无法用于领取，必须直接丢掉而不是生成空 ID 的套餐。
	if got := parsePlan(map[string]any{"name": "无 ID 套餐"}); got != nil {
		t.Fatalf("parsePlan(无 plan_id) = %+v, want nil", got)
	}
	// planId 是 camelCase 别名，上游两个字段名都在用。
	if got := parsePlan(map[string]any{"planId": "p1", "name": "套餐一"}); got == nil || got.PlanID != "p1" {
		t.Fatalf("parsePlan(planId) = %+v, want plan_id=p1", got)
	}
}

func TestParsePlanGrantsFromEntitlements(t *testing.T) {
	plan := parsePlan(map[string]any{
		"plan_id":   "p1",
		"name":      "GLM 活动",
		"priority":  float64(7),
		"starts_at": float64(1700000000),
		"ends_at":   float64(1800000000),
		"entitlements": []any{
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3", "grant_units": float64(250000), "period": "daily",
			},
			map[string]any{
				// 非 token 计量对页面无意义，应被过滤掉。
				"meter": "storage", "unit_type": "byte",
				"show_name": "网盘", "grant_units": float64(1),
			},
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3-Flash", "grant_units": float64(0),
			},
		},
	})
	if plan == nil {
		t.Fatal("parsePlan 返回 nil")
	}
	if plan.Priority != 7 || plan.StartsAt != 1700000000000 || plan.EndsAt != 1800000000000 {
		t.Fatalf("套餐元数据解析错误: %+v", plan)
	}
	if len(plan.Grants) != 2 {
		t.Fatalf("授予项数 = %d, want 2（storage 项被过滤，零额度项保留展示）: %+v", len(plan.Grants), plan.Grants)
	}
	if plan.Grants[0].Model != "GLM-5.3" || plan.Grants[0].Units != 250000 || plan.Grants[0].Period != "daily" {
		t.Fatalf("首个授予项 = %+v", plan.Grants[0])
	}
}

func TestParsePlanGrantsFallsBackToGrantsField(t *testing.T) {
	// 上游也可能直接给已解析好的 grants[]。
	plan := parsePlan(map[string]any{
		"plan_id": "p1",
		"grants":  []any{map[string]any{"name": "GLM-5.3", "units": float64(1000), "period": "one_time"}},
	})
	if plan == nil || len(plan.Grants) != 1 || plan.Grants[0].Model != "GLM-5.3" {
		t.Fatalf("grants 回退解析失败: %+v", plan)
	}
}

func TestHeldPlanIDsExcludesInactivePlans(t *testing.T) {
	body := map[string]any{"data": map[string]any{"plans": []any{
		map[string]any{"plan_id": "active-plan", "status": "active"},
		map[string]any{"plan_id": "expired-plan", "status": "expired"},
		map[string]any{"plan_id": "implicit-active"},
		map[string]any{"status": "active"}, // 无 ID：跳过
	}}}
	held := heldPlanIDs(body)
	if !held["active-plan"] || !held["implicit-active"] {
		t.Fatalf("生效套餐应计入持有: %+v", held)
	}
	// 过期套餐必须排除：否则活动下线后按钮会永远灰着。
	if held["expired-plan"] {
		t.Fatalf("过期套餐不应计入持有: %+v", held)
	}
}

func TestPlanSnapshotClaimableSubtractsHeld(t *testing.T) {
	snapshot := planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}, {PlanID: "c"}},
		held:    map[string]bool{"b": true},
	}
	claimable := snapshot.claimable()
	if len(claimable) != 2 {
		t.Fatalf("可领数 = %d, want 2: %+v", len(claimable), claimable)
	}
	for _, plan := range claimable {
		if plan.PlanID == "b" {
			t.Fatal("已持有的套餐不应出现在可领列表里")
		}
	}
}

func TestClaimStatusCountsAndErrors(t *testing.T) {
	clean := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}},
		held:    map[string]bool{"b": true},
	})
	if clean.Available != 2 || clean.Claimable != 1 || clean.Error != "" {
		t.Fatalf("领取状态错误: %+v", clean)
	}
	// 全领完 → Claimable=0，页面据此把按钮置灰成「已领取」。
	all := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}},
		held:    map[string]bool{"a": true},
	})
	if all.Claimable != 0 {
		t.Fatalf("全部领取后 claimable = %d, want 0", all.Claimable)
	}
	// preview 失败时必须带 error：页面据此保持按钮可点，而不是误显示「已领取」。
	failed := claimStatusOf(planSnapshot{
		held:       map[string]bool{},
		errPreview: core.NewPluginError("x", "preview 失败", 502),
	})
	if failed.Error == "" || failed.Claimable != 0 {
		t.Fatalf("preview 失败应带 error: %+v", failed)
	}
}

func TestParseCaptchaConfig(t *testing.T) {
	body := "{\"data\":{\"configs\":{\"captcha\":{\"enabled\":true,\"sceneId\":\"sid\",\"region\":\"cn\",\"prefix\":\"pfx\"}}}}"
	config := parseCaptchaConfig([]byte(body))
	if config.SceneID != "sid" || config.Region != "cn" || config.Prefix != "pfx" || !config.Enabled {
		t.Fatalf("验证码配置解析错误: %+v", config)
	}
	if got := parseCaptchaConfig([]byte("{\"data\":{}}")); got != (core.CaptchaConfig{}) {
		t.Fatalf("缺失字段应返回零值, got %+v", got)
	}
	// 兜底常量必须齐备（configs 不可用时靠它渲染滑块）。
	if captchaFallbackScene == "" || captchaFallbackRegion == "" || captchaFallbackPrefix == "" {
		t.Fatal("验证码兜底常量不应为空")
	}
}

func TestClaimFailureMessages(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{codePlanClaimed, "已经领取过"},
		{codePlanNotFound, "套餐不存在"},
		{codeDailyQuotaUsed, "名额已用完"},
		{codeCaptchaFailed, "验证码校验失败"},
	}
	for _, item := range cases {
		message := claimFailure(item.code, map[string]any{}).Error()
		if !strings.Contains(message, item.want) {
			t.Fatalf("code=%d 文案 = %q, want 包含 %q", item.code, message, item.want)
		}
	}
}

func TestClaimFailureQuotaRecoveryCarriesTime(t *testing.T) {
	// 1005 会带上名额恢复时间，页面要靠它提示何时再试。
	err := claimFailure(codeDailyQuotaUsed, map[string]any{
		"data": map[string]any{"plan": map[string]any{"ends_at": float64(1800000000)}},
	})
	if !strings.Contains(err.Error(), "名额恢复时间") {
		t.Fatalf("1005 应带名额恢复时间, got %q", err.Error())
	}
}

func TestClaimEndpointsUseBillingBase(t *testing.T) {
	// 领取与预览都挂在 billing 族下：路径写错会 404，而 404 在页面上只表现为
	// 「查询失败」，很难定位。
	if got := BillingBase + PathBillingPreview; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/preview" {
		t.Fatalf("preview 端点 = %q", got)
	}
	if got := BillingBase + PathBillingClaim; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/claim" {
		t.Fatalf("claim 端点 = %q", got)
	}
}

func TestClaimThrottleBlocksAndHonorsCancellation(t *testing.T) {
	// 节流的语义是「阻塞到允许为止」，不是「拒绝」：调用方在同一次领取里等
	// 一下即可，不该把等待当成失败。
	markClaimIssued()
	if !claimThrottle(context.Background(), 0) {
		t.Fatal("间隔为 0 时不应节流")
	}

	// 间隔远大于 context 剩余时间时应放弃，而不是把请求拖死。
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if claimThrottle(ctx, time.Hour) {
		t.Fatal("context 取消后应放弃本次领取")
	}

	// 短间隔应等到点后放行。
	markClaimIssued()
	start := time.Now()
	if !claimThrottle(context.Background(), 60*time.Millisecond) {
		t.Fatal("短间隔应等待后放行")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("实际等待 %s，短于节流间隔", waited)
	}
}
