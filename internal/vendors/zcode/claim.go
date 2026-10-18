package zcode

// Coding Plan 套餐的探测与人工领取。
//
// 链路只有两步，与参考实现 zcode2api/app/claim.py 同形：
//
//  1. GETbilling/preview?app_version=&platform= → data.plans[]（上游投放）
//  2. POST billing/claim  body {"plan_id":...}   （必须带人机验证令牌）
//
// **领取必须由人完成**：上游对 billing/claim 强制校验阿里云无痕验证令牌，
// 没有令牌就是 3007。参考项目为此写的「无痕验证（免浏览器）」求解器有 4500 行
// Node，本插件不重复造；改为把 SDK 初始化参数交给控制台页，在用户浏览器里
// 渲染滑块，用户通过后插件只做转发（见 core.PlanClaimer 契约）。
//
// 上游业务码：
//
//	0 成功 / 1001 套餐不存在 / 1002 活动结束 / 1003 已领取过
//	1004 不符合条件 / 1005 今日名额用完 / 3007 验证码失败 / 3012 风控拦截

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 领取相关的业务码（errors.go 已有的通用码不在此重复）。
const (
	codePlanNotFound    = 1001 // 套餐不存在（活动已下线）
	codePlanClosed      = 1002 // 活动已结束或暂不可领取
	codePlanClaimed     = 1003 // 已经领取过
	codePlanNotEligible = 1004 // 不符合领取条件
	codeDailyQuotaUsed  = 1005 // 今日领取名额已用完
)

// 领取节流：同一出口（= 同一台 CPA 主机）连打会触发 WAF 1005/3012，
// 因此两次领取之间留出最小间隔。参考实现按代理端口做同样的事，这里没有
// 代理概念，按进程内全局节流等价。
const defaultClaimMinInterval = 15 * time.Second

var (
	claimMu       sync.Mutex
	lastClaimTime time.Time
)

// claimThrottle 阻塞到允许发起下一次领取为止。
//
// 返回 false 表示 ctx 已取消：调用方应放弃本次领取而不是强发。
func claimThrottle(ctx context.Context, minInterval time.Duration) bool {
	if minInterval <= 0 {
		return true
	}
	claimMu.Lock()
	wait := minInterval - time.Since(lastClaimTime)
	claimMu.Unlock()
	if wait <= 0 {
		return true
	}
	if !core.SleepContext(ctx, wait) {
		return false
	}
	return true
}

// markClaimIssued 记录一次领取已发出（节流计时起点）。
func markClaimIssued() {
	claimMu.Lock()
	lastClaimTime = time.Now()
	claimMu.Unlock()
}

// ── 验证码配置 ──────────────────────────────────────────────────────────────

// FetchCaptchaConfig 取阿里云无痕验证的 SDK 初始化参数。
//
// 来源是**免鉴权公开端点** client/configs 的 data.configs.captcha——插件读模型
// 清单时已经在打这个端点，这里只是多解析一层字段，不额外增加请求。
func FetchCaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	fallback := core.CaptchaConfig{
		Enabled: true,
		SceneID: captchaFallbackScene,
		Region:  captchaFallbackRegion,
		Prefix:  captchaFallbackPrefix,
	}

	endpoint := ZCodeOrigin + PathClientConfigs + "?app_version=" + ClientAppVersion
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errNew != nil {
		return fallback, nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return fallback, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return fallback, nil
	}

	config := parseCaptchaConfig(raw)
	if config.SceneID == "" {
		// 上游没给 sceneId 时保留兜底的prefix/region，只补齐能拿到的字段。
		config.Prefix = core.FirstNonEmpty(config.Prefix, fallback.Prefix)
		config.Region = core.FirstNonEmpty(config.Region, fallback.Region)
		return fallback, nil
	}
	config.Enabled = true
	return config, nil
}

// parseCaptchaConfig 从 client/configs 响应里提取 data.configs.captcha。
func parseCaptchaConfig(body []byte) core.CaptchaConfig {
	var payload struct {
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					SceneID string `json:"sceneId"`
					Region  string `json:"region"`
					Prefix  string `json:"prefix"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return core.CaptchaConfig{}
	}
	captcha := payload.Data.Configs.Captcha
	return core.CaptchaConfig{
		Enabled: captcha.Enabled,
		SceneID: strings.TrimSpace(captcha.SceneID),
		Region:  strings.TrimSpace(captcha.Region),
		Prefix:  strings.TrimSpace(captcha.Prefix),
	}
}

// ── 套餐探测 ────────────────────────────────────────────────────────────────

// planSnapshot 是一次「账号套餐视图」：上游投放了什么、账号已经持有什么。
//
// 两者都取自 billing 族：持有情况在 billing/current，投放情况在
// billing/preview。一次快照就是两个请求，没有多余开销——它同时供额度查询、
// 按钮状态与领取前的校验复用。
type planSnapshot struct {
	// offered 是上游当前投放的套餐（按优先级降序）。
	offered []core.PlanInfo
	// held 是账号已持有且仍生效的套餐 ID 集合。
	held map[string]bool
	// errPreview 是 preview 请求的失败原因（current 失败时 held 为空）。
	errPreview error
}

// claimable 返回尚未领取的投放套餐。
func (s planSnapshot) claimable() []core.PlanInfo {
	out := make([]core.PlanInfo, 0, len(s.offered))
	for _, plan := range s.offered {
		if !s.held[plan.PlanID] {
			out = append(out, plan)
		}
	}
	return out
}

// fetchPlanSnapshot 拉取套餐视图。
//
// current 失败不算整体失败：此时把持有集合当作空（结果偏保守——已领的套餐
// 会被再列成可领，用户点下去拿到上游 1003，语义依然正确）。preview 失败会
// 记进 errPreview，由调用方决定是降级还是报错。
func fetchPlanSnapshot(ctx context.Context, cred *Credential) planSnapshot {
	snapshot := planSnapshot{held: map[string]bool{}}
	if code, body, err := billingRequest(ctx, cred, http.MethodGet, PathBillingCurrent, nil); err == nil && code == codeSuccess {
		snapshot.held = heldPlanIDs(body)
	}
	offered, code, errOffered := fetchOfferedPlans(ctx, cred)
	if errOffered != nil {
		snapshot.errPreview = errOffered
		return snapshot
	}
	if code != codeSuccess {
		snapshot.errPreview = claimFailure(code, nil)
		return snapshot
	}
	snapshot.offered = offered
	return snapshot
}

// heldPlanIDs 从 billing/current 响应里取账号已持有且生效的套餐 ID。
//
// status 非 active（已过期 / 已停用）的不算持有，否则活动下线后按钮会永远灰着。
func heldPlanIDs(body map[string]any) map[string]bool {
	held := map[string]bool{}
	raw, _ := dataMap(body)["plans"].([]any)
	for _, item := range raw {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(core.RawStringField(entry, "status")))
		if status != "" && status != "active" {
			continue
		}
		planID := normalizePlanID(core.FirstNonEmpty(
			core.RawStringField(entry, "plan_id"),
			core.RawStringField(entry, "planId"),
		))
		if planID != "" {
			held[planID] = true
		}
	}
	return held
}

// fetchOfferedPlans 拉取上游投放的套餐并按优先级排序。
func fetchOfferedPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, int, error) {
	path := PathBillingPreview +
		"?app_version=" + url.QueryEscape(ClientAppVersion) +
		"&platform=" + url.QueryEscape(cred.Profile.PlatformFull())
	code, body, errRequest := billingRequest(ctx, cred, http.MethodGet, path, nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	if code != codeSuccess {
		return nil, code, nil
	}

	raw, _ := dataMap(body)["plans"].([]any)
	plans := make([]core.PlanInfo, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			continue
		}
		plan := parsePlan(entry)
		if plan == nil || seen[plan.PlanID] {
			continue
		}
		seen[plan.PlanID] = true
		plans = append(plans, *plan)
	}
	// 优先级降序；同优先级按 plan_id 排序保证顺序稳定（否则同批活动的展示
	// 顺序会随上游返回顺序抖动）。
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Priority != plans[j].Priority {
			return plans[i].Priority > plans[j].Priority
		}
		return plans[i].PlanID < plans[j].PlanID
	})
	return plans, codeSuccess, nil
}

// PreviewPlans 返回账号当前可领的套餐（已剔除已持有的）。
//
// 空切片表示没有可领的，页面据此把按钮置灰成「已领取」。
func PreviewPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return nil, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	snapshot := fetchPlanSnapshot(ctx, cred)
	if snapshot.errPreview != nil {
		return nil, snapshot.errPreview
	}
	claimable := snapshot.claimable()
	if len(claimable) == 0 {
		return nil, nil
	}
	return claimable, nil
}

// claimStatusOf 把套餐视图折成额度响应里的领取状态。
func claimStatusOf(snapshot planSnapshot) *pluginapi.ClaimStatus {
	status := &pluginapi.ClaimStatus{
		Available: len(snapshot.offered),
		Claimable: len(snapshot.claimable()),
	}
	if snapshot.errPreview != nil {
		status.Error = snapshot.errPreview.Error()
	}
	return status
}

// parsePlan 归一化一个套餐，缺失 plan_id 时返回 nil。
func parsePlan(raw map[string]any) *core.PlanInfo {
	planID := normalizePlanID(core.FirstNonEmpty(
		core.RawStringField(raw, "plan_id"),
		core.RawStringField(raw, "planId"),
	))
	if planID == "" {
		return nil
	}
	return &core.PlanInfo{
		PlanID:      planID,
		Name:        core.FirstNonEmpty(core.RawStringField(raw, "name"), planID),
		Description: core.RawStringField(raw, "description"),
		Priority:    int(floatField(raw, "priority")),
		Grants:      parsePlanGrants(raw),
		StartsAt:    secondsToMillis(floatField(raw, "starts_at")),
		EndsAt:      secondsToMillis(floatField(raw, "ends_at")),
	}
}

// parsePlanGrants 提取套餐授予的 token 额度明细。
//
// 上游可能直接给 grants[]（已解析形态），也可能给 entitlements[]（原始态）。
// 两种都收，且都为空时返回 nil 而不是空切片——nil 在 JSON 里是"没有"，便于
// 前端区分"没有额度信息"与"额度为 0"。
func parsePlanGrants(raw map[string]any) []core.PlanGrant {
	var grants []core.PlanGrant
	appendGrant := func(name string, units float64, period string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		grants = append(grants, core.PlanGrant{Model: name, Units: units, Period: period})
	}

	if entitlements, okList := raw["entitlements"].([]any); okList {
		for _, item := range entitlements {
			entry, okEntry := item.(map[string]any)
			if !okEntry {
				continue
			}
			// 只认模型用量类的 token 授予：其它计量单位对页面无意义。
			if meter := core.RawStringField(entry, "meter"); meter != "" && meter != "model_usage" {
				continue
			}
			if unit := core.RawStringField(entry, "unit_type"); unit != "" && unit != "token" {
				continue
			}
			appendGrant(
				core.FirstNonEmpty(core.RawStringField(entry, "show_name"), core.RawStringField(entry, "showName")),
				firstPositiveFloat(entry, "grant_units", "grantUnits"),
				core.RawStringField(entry, "period"),
			)
		}
	}
	if len(grants) == 0 {
		if list, okList := raw["grants"].([]any); okList {
			for _, item := range list {
				entry, okEntry := item.(map[string]any)
				if !okEntry {
					continue
				}
				appendGrant(
					core.RawStringField(entry, "name"),
					firstPositiveFloat(entry, "units"),
					core.RawStringField(entry, "period"),
				)
			}
		}
	}
	return grants
}

// firstPositiveFloat 取第一个存在且为正的浮点字段。
func firstPositiveFloat(raw map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value := floatField(raw, key); value > 0 {
			return value
		}
	}
	return 0
}

// normalizePlanID 归一化套餐 ID（上游大小写不统一，去空白后比较）。
func normalizePlanID(planID string) string {
	return strings.ToLower(strings.TrimSpace(planID))
}

// secondsToMillis 把上游秒级时间戳转毫秒；缺失/非正值返回 0。
func secondsToMillis(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}

// ── 领取 ────────────────────────────────────────────────────────────────────

// ClaimPlan 领取指定套餐（PlanID 为空时自动挑优先级最高的可领套餐）。
//
// 每次调用都会先做一次实时 preview：领取是用户手动触发的动作，拿到的是
// 点击那一刻的真实可领集合，不复用任何缓存。
func ClaimPlan(ctx context.Context, cred *Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return core.ClaimPlanResponse{}, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	if strings.TrimSpace(req.VerifyParam) == "" {
		// 没有令牌就不打上游：必然 3007，白白消耗一次风控计数。
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_captcha_required", "请先在页面上完成人机验证", http.StatusBadRequest)
	}

	plan, errPick := pickClaimPlan(ctx, cred, req.PlanID)
	if errPick != nil {
		return core.ClaimPlanResponse{}, errPick
	}

	if !claimThrottle(ctx, claimMinInterval) {
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_claim_canceled", "领取已取消（等待下一次可提交时间时中断）", http.StatusRequestTimeout)
	}

	headers := map[string]string{CaptchaHeader: strings.TrimSpace(req.VerifyParam)}
	if region := strings.TrimSpace(req.VerifyRegion); region != "" {
		headers[CaptchaRegionHeader] = region
	}
	markClaimIssued()

	code, body, errRequest := billingRequestWithHeaders(
		ctx, cred, http.MethodPost, PathBillingClaim,
		map[string]string{"plan_id": plan.PlanID}, headers,
	)
	if errRequest != nil {
		return core.ClaimPlanResponse{}, errRequest
	}
	if code != codeSuccess {
		return core.ClaimPlanResponse{}, claimFailure(code, body)
	}

	// 成功响应带服务端时钟与套餐起止，原样透传给页面展示。
	result := core.ClaimPlanResponse{PlanID: plan.PlanID, PlanName: plan.Name}
	if claimed, okClaimed := dataMap(body)["plan"].(map[string]any); okClaimed {
		result.PlanName = core.FirstNonEmpty(core.RawStringField(claimed, "name"), plan.Name)
		result.StartsAt = secondsToMillis(floatField(claimed, "starts_at"))
		result.EndsAt = secondsToMillis(floatField(claimed, "ends_at"))
	}
	return result, nil
}

// pickClaimPlan 决定本次领取的目标套餐。
//
// 指定 plan_id 时校验它当前确实可领（预览里存在且未持有）；没指定就取
// 优先级最高的可领项。两者都拿不到候选时报错，避免把无效plan_id 打给上游。
func pickClaimPlan(ctx context.Context, cred *Credential, planID string) (*core.PlanInfo, error) {
	available, errPreview := PreviewPlans(ctx, cred)
	if errPreview != nil {
		return nil, errPreview
	}
	if len(available) == 0 {
		return nil, core.NewPluginError(
			"zcode_nothing_to_claim", "该账号已领取当前全部可用套餐", http.StatusBadRequest)
	}

	wanted := normalizePlanID(planID)
	if wanted == "" {
		best := available[0]
		return &best, nil
	}
	for index := range available {
		if available[index].PlanID == wanted {
			return &available[index], nil
		}
	}
	return nil, core.NewPluginError(
		"zcode_plan_not_claimable", "该套餐已领取或当前不可领取，请刷新后重试", http.StatusBadRequest)
}

// claimFailure 把领取的业务码翻成对用户可读的错误。
//
// 与额度路径的 classifyBillingOutcome 同源，但领取多了 1003（已领取，按成功
// 语义回报页面好让按钮置灰）与 1005（附名额恢复时间）。
func claimFailure(code int, body map[string]any) error {
	serverMsg := core.FirstNonEmpty(core.RawStringField(body, "msg"), core.RawStringField(body, "message"))
	base := classifyBillingOutcome(code, body)

	// 1005 带名额恢复时间：上游在 data.plan.ends_at 下发，页面据此提示何时再试。
	if code == codeDailyQuotaUsed && serverMsg == "" {
		if nextAt := claimQuotaRecovery(body); nextAt > 0 {
			return core.NewPluginError(
				"zcode_claim_quota_used",
				base+"（名额恢复时间："+formatClaimRecovery(nextAt)+"）",
				http.StatusTooManyRequests,
			)
		}
	}
	return core.NewPluginError("zcode_claim_failed", base, http.StatusBadRequest)
}

// claimQuotaRecovery 从 1005 响应里取名额恢复时间（毫秒）。
func claimQuotaRecovery(body map[string]any) int64 {
	plan, okPlan := dataMap(body)["plan"].(map[string]any)
	if !okPlan {
		return 0
	}
	return secondsToMillis(floatField(plan, "ends_at"))
}

// formatClaimRecovery 把恢复时间格式化成「今日/次日 HH:MM:SS」。
func formatClaimRecovery(at int64) string {
	if at <= 0 {
		return "-"
	}
	return time.UnixMilli(at).In(claimTimezone).Format("01-02 15:04:05")
}

// claimTimezone 是名额恢复时间的展示时区（北京时间），
// 与上游「每日 0 点重置」的口径一致。
var claimTimezone = time.FixedZone("Asia/Shanghai", 8*3600)

// claimMinInterval 可被 Configure 覆盖（0 表示不节流）。
var claimMinInterval = defaultClaimMinInterval

// CaptchaConfig 实现 core.CaptchaProvider。
func (v *adapter) CaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	return FetchCaptchaConfig(ctx)
}

// PreviewPlans 实现 core.PlanPreviewProvider。
func (v *adapter) PreviewPlans(ctx context.Context, cred *core.Credential) ([]core.PlanInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return PreviewPlans(ctx, native)
}

// ClaimPlan 实现 core.PlanClaimProvider。
func (v *adapter) ClaimPlan(ctx context.Context, cred *core.Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.ClaimPlanResponse{}, errNative
	}
	return ClaimPlan(ctx, native, req)
}

// compile-time断言：adapter 具备领取套餐的三项能力。
var _ core.PlanClaimer = (*adapter)(nil)
