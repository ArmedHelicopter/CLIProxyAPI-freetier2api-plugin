package minimaxcode

import (
	"io"

	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// Checkin 执行账号每日签到（仅国际版支持）。
func Checkin(ctx context.Context, cred *Credential, baseURLOverride string) (*core.CheckinResult, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	if cred.Region == RegionCN {
		return &core.CheckinResult{
			Already: false,
			Message: "国内版暂无签到活动",
		}, nil
	}

	base := AgentHostFor(cred.Region, baseURLOverride)

	// 1. 签到前必须先调用 prepare 确保账号 record 在 agent 端建立
	_ = prepare(ctx, cred, baseURLOverride, nil)

	// 2. 查询签到看板状态
	statusTarget := buildSigninTarget(cred, base, EpCheckinStatus, http.MethodGet, "")
	statusResp, errStatus := doRequest(ctx, cred, statusTarget, base, 30*time.Second)
	if errStatus != nil {
		return nil, core.ToPluginError(errStatus)
	}
	defer func() { _ = statusResp.Body.Close() }()

	if statusResp.StatusCode == http.StatusOK {
		var statusRaw map[string]any
		if errDec := json.NewDecoder(statusResp.Body).Decode(&statusRaw); errDec == nil {
			coreMap := statusRaw
			if data, ok := statusRaw["data"].(map[string]any); ok {
				coreMap = data
			}
			if days, ok := coreMap["days"].([]any); ok {
				for _, d := range days {
					if dm, ok := d.(map[string]any); ok {
						isToday, _ := dm["is_today"].(bool)
						status := getInt(dm, "status")
						// status == 3 表示今日已领取
						if isToday && status == 3 {
							return &core.CheckinResult{
								Already: true,
								Message: "今日已完成签到",
							}, nil
						}
					}
				}
			}
		}
	}

	// 3. 领取签到积分
	claimTarget := buildSigninTarget(cred, base, EpCheckinClaim, http.MethodPost, "{}")
	claimResp, errClaim := doRequest(ctx, cred, claimTarget, base, 30*time.Second)
	if errClaim != nil {
		return nil, core.ToPluginError(errClaim)
	}
	defer func() { _ = claimResp.Body.Close() }()

	if claimResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(claimResp.Body, 4096))
		return nil, Classify(claimResp.StatusCode, string(body))
	}

	var claimRaw map[string]any
	if errDec := json.NewDecoder(claimResp.Body).Decode(&claimRaw); errDec != nil {
		return nil, fmt.Errorf("decode claim response: %w", errDec)
	}

	coreMap := claimRaw
	if data, ok := claimRaw["data"].(map[string]any); ok {
		coreMap = data
	}

	claimResult := getInt(coreMap, "claim_result", "result")
	points := core.GetInt64(coreMap, "points")

	// claim_result == 2 表示幂等命中（今日已领取过）
	if claimResult == 2 {
		return &core.CheckinResult{
			Already: true,
			Message: "今日已完成签到",
		}, nil
	}

	return &core.CheckinResult{
		Already: false,
		Credit:  points,
		Message: fmt.Sprintf("签到成功，获得 %d 积分", points),
	}, nil
}

// FetchQuota 查询 MiniMax 账号的额度余额。
func FetchQuota(ctx context.Context, cred *Credential, baseURLOverride string) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	requestMembership := func() (*http.Response, error) {
		target := buildSigninTarget(cred, base, EpMembership, http.MethodPost, "{}")
		return doRequest(ctx, cred, target, base, 30*time.Second)
	}

	resp, errDo := requestMembership()
	if errDo != nil {
		return nil, core.ToPluginError(errDo)
	}
	// 凭证刷新由 admin quota 编排统一负责：它会调用 Keepalive、持久化
	// access/refresh token、重新加载账号后再重试。这里不能自行刷新，因为
	// vendor 层拿不到原始 JSON/file path，刷新后只改内存会导致旧 token 留在磁盘，
	// 下一次请求再次使用旧凭证并消耗/吊销 refresh_token。
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, Classify(resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return nil, fmt.Errorf("decode quota response: %w", errDec)
	}

	coreMap := raw
	if data, ok := raw["data"].(map[string]any); ok {
		coreMap = data
	}

	planName := core.GetString(coreMap, "plan_name")
	if planName == "" {
		planName = "MiniMax Code"
	}

	var totalRemaining, freeRemaining, purchasedRemaining float64

	if summary, ok := coreMap["op_credit_summary"].(map[string]any); ok {
		totalRemaining = parseFloat(summary["total_remaining_amount"])
		freeRemaining = parseFloat(summary["free_remaining_amount"])
		purchasedRemaining = parseFloat(summary["purchased_remaining_amount"])
	} else {
		totalRemaining = parseFloat(coreMap["opcredit_balance"])
		if totalRemaining == 0 {
			totalRemaining = parseFloat(coreMap["total_remains_credit"])
		}
	}

	summaryMetrics := []pluginapi.QuotaMetric{
		{
			Key:   "remaining",
			Label: "总剩余额度",
			Value: totalRemaining,
			Unit:  "点",
		},
	}
	if freeRemaining > 0 {
		summaryMetrics = append(summaryMetrics, pluginapi.QuotaMetric{
			Key:   "free",
			Label: "免费额度",
			Value: freeRemaining,
			Unit:  "点",
		})
	}
	if purchasedRemaining > 0 {
		summaryMetrics = append(summaryMetrics, pluginapi.QuotaMetric{
			Key:   "purchased",
			Label: "付费额度",
			Value: purchasedRemaining,
			Unit:  "点",
		})
	}

	// Token Plan（订阅制）走独立的窗口体系：5 小时窗口 + 7 天窗口，
	// 由 coding_plan/remains 提供（纯 Bearer，不需要 account 签名）。
	// op 点数与 token 计划并存：点数为 0 时不占摘要，让位给 token 指标。
	if tokenPlan := extractTokenPlanHead(coreMap); tokenPlan != nil {
		planName = tokenPlan.planName
		// Token Plan 在场时，为 0 的 op 点数指标不进摘要（避免「总剩余额度 0」
		// 抢在 token 余量前面误导）。
		kept := summaryMetrics[:0]
		for _, metric := range summaryMetrics {
			if metric.Value > 0 {
				kept = append(kept, metric)
			}
		}
		summaryMetrics = kept
		summaryMetrics = append(summaryMetrics, tokenPlan.metrics...)
		if groups := tokenPlan.groups(ctx, cred); len(groups) > 0 {
			return &pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{
					Plan:     planName,
					TierName: planName,
					TierID:   "minimax-token-plan",
				},
				Summary: summaryMetrics,
				Groups:  groups,
			}, nil
		}
	}

	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     planName,
			TierName: planName,
			TierID:   "minimax",
		},
		Summary: summaryMetrics,
	}, nil
}

// tokenPlanHead 是 membership 响应里的 Token Plan 概要。
type tokenPlanHead struct {
	planName    string
	groupID     string
	expiresAtMS int64
	metrics     []pluginapi.QuotaMetric
}

// extractTokenPlanHead 从 membership 响应提取 Token Plan 概要。
//
// 未订阅（has_token_plan=false）或余量为 0 且无到期时间时返回 nil，
// 调用方回落到 op 点数展示。
func extractTokenPlanHead(coreMap map[string]any) *tokenPlanHead {
	if core.GetString(coreMap, "has_token_plan") != "true" && parseBool(coreMap["has_token_plan"]) != true {
		return nil
	}
	tier := strings.TrimSpace(core.GetString(coreMap, "token_plan_tier"))
	remains := parseFloat(coreMap["tp_remains"])
	expiresAtMS := int64(parseFloat(coreMap["token_plan_expires_at"]))
	if remains == 0 && expiresAtMS == 0 {
		return nil
	}
	info := &tokenPlanHead{
		planName:    "MiniMax Token Plan",
		groupID:     core.GetString(coreMap, "op_group_id"),
		expiresAtMS: expiresAtMS,
	}
	if tier != "" {
		info.planName += " " + strings.ToUpper(tier[:1]) + tier[1:]
	}
	if remains > 0 {
		// key 用独立的 token_plan 族（token_remaining）：与 op 点数的
		// default 族 remaining 并存时不会互相覆盖（Totals 按族取值）。
		info.metrics = append(info.metrics, pluginapi.QuotaMetric{
			Key:   "token_remaining",
			Label: "Token Plan 剩余",
			Value: remains,
			Unit:  "tokens",
		})
	}
	return info
}

// groups 拉取 M Plan 的 5 小时 / 7 天窗口，并附套餐有效期。
//
// coding_plan/remains 失败不拖垮整个额度响应：窗口缺失只是卡片少两条进度条，
// 余量指标仍然有效。
func (t *tokenPlanHead) groups(ctx context.Context, cred *Credential) []pluginapi.QuotaGroup {
	groups := fetchTokenPlanRemains(ctx, cred, t.groupID)
	if t.expiresAtMS > 0 {
		groups = append(groups, pluginapi.QuotaGroup{
			DisplayName: "套餐有效期",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "有效期",
				RemainingFraction: 1,
				ResetTime:         time.UnixMilli(t.expiresAtMS).Format(time.RFC3339),
			}},
		})
	}
	return groups
}

// fetchTokenPlanRemains 查询 coding_plan/remains 并折成窗口组。
//
// 端点为纯 Bearer（平台域，非签名的 account 域），按模型行返回 5 小时与
// 7 天两个窗口；status=2 表示「已耗尽且不再发布百分比」，按 0% 展示；
// status=3 的窗口不展示——与官方客户端的呈现一致。
func fetchTokenPlanRemains(ctx context.Context, cred *Credential, groupID string) []pluginapi.QuotaGroup {
	platform := PlatformHostFor(cred.Region)
	// 超时收紧到 10s：该探测挂在额度查询路径上，账号列表逐账号拉额度时
	// 30s 超时会被放大成整轮刷新的长尾。
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, platform+"/v1/api/openplatform/coding_plan/remains", nil)
	if errNew != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, errDo := hostbridge.Client(ctx, 10*time.Second).Do(req)
	if errDo != nil {
		logger.Debug("minimax token plan windows: request failed: %v", errDo)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Debug("minimax token plan windows: endpoint returned %s", resp.Status)
		return nil
	}
	var body struct {
		ModelRemains []map[string]any `json:"model_remains"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); errDecode != nil {
		logger.Debug("minimax token plan windows: decode failed: %v", errDecode)
		return nil
	}

	interval := pluginapi.QuotaGroup{DisplayName: "5 小时窗口"}
	weekly := pluginapi.QuotaGroup{DisplayName: "7 天窗口"}
	for _, row := range body.ModelRemains {
		name := strings.TrimSpace(core.GetString(row, "model_name"))
		if name == "" {
			continue
		}
		display := name
		if strings.EqualFold(name, "general") {
			display = "通用额度"
		}
		if bucket, ok := planWindowBucket(display, "5 小时",
			row["current_interval_remaining_percent"], row["current_interval_status"],
			row["start_time"], row["end_time"],
			row["current_interval_usage_count"], row["current_interval_total_count"]); ok {
			interval.Buckets = append(interval.Buckets, bucket)
		}
		if bucket, ok := planWindowBucket(display, "7 天",
			row["current_weekly_remaining_percent"], row["current_weekly_status"],
			row["weekly_start_time"], row["weekly_end_time"],
			row["current_weekly_usage_count"], row["current_weekly_total_count"]); ok {
			weekly.Buckets = append(weekly.Buckets, bucket)
		}
	}
	groups := make([]pluginapi.QuotaGroup, 0, 2)
	if len(interval.Buckets) > 0 {
		groups = append(groups, interval)
	}
	if len(weekly.Buckets) > 0 {
		groups = append(groups, weekly)
	}
	return groups
}

// planWindowBucket 把一行窗口折成展示桶。
//
// 返回 false 表示该窗口不值得展示：status=3（上游不展示），或字段缺失/
// 改名导致没有百分比且未显式标记耗尽——后者渲染成 0% 会制造假的
// 「已用尽」进度条，比不显示更误导。
func planWindowBucket(display, window string, remaining, status, start, end, usage, total any) (pluginapi.QuotaBucket, bool) {
	statusCode := parseFloat(status)
	if statusCode == 3 {
		return pluginapi.QuotaBucket{}, false
	}
	// status=2 是「耗尽且不再发布百分比」：按 0% 展示（账号真的没额度）。
	// 其余情况缺百分比（nil/空串）一律不展示。
	var percent float64
	hasPercent := false
	switch typed := remaining.(type) {
	case float64:
		percent, hasPercent = typed, true
	case string:
		if strings.TrimSpace(typed) != "" {
			percent, hasPercent = parseFloat(typed), true
		}
	}
	if !hasPercent {
		if statusCode == 2 {
			percent, hasPercent = 0, true
		} else {
			return pluginapi.QuotaBucket{}, false
		}
	}
	fraction := 0.0
	if statusCode != 2 {
		if percent <= 1 && percent > 0 {
			// 上游以 0..1 小数表达时按小数处理，其余按 0..100 百分比。
			fraction = percent
		} else {
			fraction = percent / 100
		}
		if fraction < 0 {
			fraction = 0
		} else if fraction > 1 {
			fraction = 1
		}
	}
	bucket := pluginapi.QuotaBucket{
		Window:            display + " · " + window,
		RemainingFraction: fraction,
	}
	if endMS := int64(parseFloat(end)); endMS > 0 {
		bucket.ResetTime = time.UnixMilli(endMS).Format(time.RFC3339)
	}
	if used, total := parseFloat(usage), parseFloat(total); used > 0 || total > 0 {
		bucket.Description = fmt.Sprintf("窗口已用次数 %s / %s", strconv.FormatFloat(used, 'f', 0, 64), strconv.FormatFloat(total, 'f', 0, 64))
	}
	_ = start
	return bucket, true
}

// parseBool 宽松解析布尔字段。
func parseBool(val any) bool {
	switch v := val.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

func parseFloat(val any) float64 {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}
