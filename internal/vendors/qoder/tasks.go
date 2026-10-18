package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件承载 Qoder 的账号运营类可选能力：额度查询与签到。

// 本文件实现 Qoder 的额度查询。
//
// 与 WorkBuddy 的积分余额不同，Qoder 的额度是「套餐用量桶」：上游返回
// userQuota / addOnQuota 两个对象，各有 used / total / remaining。
// 归一成宿主的 QuotaFetchResponse（Summary 放关键数字，Groups 放明细）。

// quotaHTTPTimeout 是额度相关请求的上限。
const quotaHTTPTimeout = 30 * time.Second

// QuotaBucket 是一个额度桶。
type QuotaBucket struct {
	Used      float64
	Total     float64
	Remaining float64
	ResetTime string
}

// Quota 是一次额度查询结果。
type Quota struct {
	Plan            string
	UserQuota       *QuotaBucket
	AddonQuota      *QuotaBucket
	IsQuotaExceeded bool
	ExpiresAt       int64
}

// FetchQuota 查询账号额度。
func FetchQuota(ctx context.Context, cred *Credential) (*Quota, error) {
	bearer, errToken := quotaBearerToken(ctx, cred)
	if errToken != nil {
		return nil, errToken
	}
	endpoints := GetEndpoints(cred.Region)

	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return nil, errQuota
	}
	return &Quota{
		Plan:            fetchPlanTierName(ctx, endpoints, bearer),
		IsQuotaExceeded: raw["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(raw, "expiresAt")),
		UserQuota:       extractQuotaBucket(raw, "userQuota"),
		AddonQuota:      extractQuotaBucket(raw, "addOnQuota"),
	}, nil
}

// quotaBearerToken 把凭证换成额度接口认识的 bearer token。
//
// 设备令牌（dt-…）可直接用；PAT（pt-…）必须先经 jobToken 交换拿到
// securityOauthToken——上游额度接口不认 PAT 本身。
func quotaBearerToken(ctx context.Context, cred *Credential) (string, error) {
	deviceToken, _ := ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		return deviceToken, nil
	}
	seed := FingerprintSeed("", cred.Token)
	endpoints := GetEndpoints(cred.Region)
	jobToken, errExchange := ExchangeJobToken(ctx, cred.Token,
		DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return "", classifyCredentialError(fmt.Errorf("exchange token: %w", errExchange))
	}
	oauthToken := StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return "", NewError("qoder_credential_invalid", "jobToken 响应里没有 securityOauthToken", http.StatusUnauthorized)
	}
	return oauthToken, nil
}

// httpGetBearerJSON 发一个带 Bearer 的 GET 并解析成 JSON 对象。
func httpGetBearerJSON(ctx context.Context, endpoint, token string) (map[string]any, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, NewError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errDo := hostbridge.Client(ctx, quotaHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, NewError("qoder_upstream_transient", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, NewError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, NewError("qoder_upstream_error",
			fmt.Sprintf("额度接口返回 HTTP %d: %s", resp.StatusCode, core.Truncate(string(body), 300)), status)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return nil, NewError("qoder_upstream_error", "额度接口返回的不是 JSON: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// fetchPlanTierName 查询套餐名；失败不影响额度展示，只留空。
func fetchPlanTierName(ctx context.Context, endpoints Endpoints, token string) string {
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	if name, okName := result["plan_tier_name"].(string); okName {
		return name
	}
	return ""
}

// extractQuotaBucket 提取额度桶。
//
// 「桶不存在」的判据是**空对象**（上游对没有的桶返回 `{}`），不是「值全零」：
// Free 计划的 userQuota 就是 `{used:0, total:0, remaining:0}`，按全零吞掉会让
// 页面显示「上游未返回额度」，而真实语义是「有额度信息，只是余额为 0」。
func extractQuotaBucket(data map[string]any, key string) *QuotaBucket {
	obj, okObj := data[key].(map[string]any)
	if !okObj || len(obj) == 0 {
		return nil
	}
	bucket := &QuotaBucket{
		Used:      toFloat(obj, "used"),
		Total:     toFloat(obj, "total"),
		Remaining: toFloat(obj, "remaining"),
	}
	if reset, okReset := obj["resetTime"].(string); okReset {
		bucket.ResetTime = reset
	} else if reset, okReset := obj["reset_time"].(string); okReset {
		bucket.ResetTime = reset
	}
	return bucket
}

// BuildQuotaResponse 把上游额度翻译成宿主的归一化结构。
func BuildQuotaResponse(quota *Quota) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	if quota.Plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{
			Plan: quota.Plan, TierName: quota.Plan, TierID: quota.Plan,
		}
	}
	if group := quotaGroup("套餐额度", quota.UserQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if group := quotaGroup("个人拓展包", quota.AddonQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if quota.UserQuota != nil {
		// total 必须一并输出：页面的「剩余/总额 + 使用率」要它才能算比例。
		// 上游对 Free 计划可能给 total=0（额度按剩余量计），此时仍输出 0——
		// 页面会显示 "—"，比编一个数诚实。
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "user_quota_remaining", Label: "套餐剩余额度", Value: quota.UserQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_total", Label: "套餐总额度", Value: quota.UserQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_used", Label: "套餐已用额度", Value: quota.UserQuota.Used, Unit: "credits"})
	}
	if quota.AddonQuota != nil {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "addon_quota_remaining", Label: "拓展包剩余额度", Value: quota.AddonQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_total", Label: "拓展包总额度", Value: quota.AddonQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_used", Label: "拓展包已用额度", Value: quota.AddonQuota.Used, Unit: "credits"})
	}
	if quota.IsQuotaExceeded {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "quota_exceeded", Label: "额度状态", Value: 1, Unit: "已用尽"})
	}
	if quota.ExpiresAt > 0 {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "expires_at", Label: "额度到期时间", Value: float64(quota.ExpiresAt), Format: "number", Unit: "unix"})
	}
	return response
}

// quotaGroup 把额度桶转成「剩余比例」分组。
func quotaGroup(displayName string, bucket *QuotaBucket) *pluginapi.QuotaGroup {
	if bucket == nil {
		return nil
	}
	fraction := 0.0
	switch {
	case bucket.Total > 0:
		fraction = clampFraction(bucket.Remaining / bucket.Total)
	case bucket.Remaining > 0:
		fraction = 1
	}
	description := fmt.Sprintf("已用 %.0f / 总计 %.0f，剩余 %.0f", bucket.Used, bucket.Total, bucket.Remaining)
	return &pluginapi.QuotaGroup{
		DisplayName: displayName,
		Buckets: []pluginapi.QuotaBucket{{
			Window:            "billing-cycle",
			RemainingFraction: fraction,
			ResetTime:         bucket.ResetTime,
			Description:       description,
		}},
	}
}

// clampFraction 把比例夹到 [0, 1]。
func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// toFloat 把 JSON 里的数值字段读成 float64（兼容多种数字形态）。
func toFloat(m map[string]any, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	}
	return 0
}

// 签到状态取值。
const (
	// CheckinClaimed 表示本次成功领取。
	CheckinClaimed = "claimed"
	// CheckinAlreadyClaimed 表示今日已领取（幂等命中）。
	CheckinAlreadyClaimed = "already_claimed"
	// CheckinNoCampaign 表示没有可领取的活动。
	CheckinNoCampaign = "no_campaign"
	// CheckinNoToken 表示凭证里没有可用 token。
	CheckinNoToken = "no_token"
	// CheckinError 表示执行失败。
	CheckinError = "error"
)

// checkinHTTPTimeout 是签到相关请求的上限。
const checkinHTTPTimeout = 60 * time.Second

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Status 是结果状态（见上面的常量）。
	Status string
	// Amount 是本次领取的积分数。
	Amount int
	// Message 是可读说明。
	Message string
	// WindowDate 是签到窗口日期（YYYY-MM-DD），用于按窗口幂等。
	WindowDate string
	// StreakDays 是连续签到天数（来自只读统计）。
	StreakDays int
	// TotalClaimDays 是累计领取天数。
	TotalClaimDays int
	// TotalRewardCredits 是累计获得积分。
	TotalRewardCredits int
}

// campaignInfo 是活动列表条目。
type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// claimResponse 是领取响应。
type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// dailyCheckinStatus 是 legacy 接口的只读统计。
type dailyCheckinStatus struct {
	Status             string `json:"status"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// Checkin 执行一次签到。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	result := &CheckinResult{Status: CheckinError}
	bearer := checkinBearer(cred.Token)
	if bearer == "" {
		result.Status = CheckinNoToken
		result.Message = "凭证里没有可用 token"
		return result, nil
	}
	result = campaignsCheckin(ctx, cred.Region, bearer, result)
	readDailyCheckinStats(ctx, cred.Region, bearer, result)
	return result, nil
}

// checkinBearer 取签到用的 bearer。
//
// 只需 device token：上游对签到接口不校验 cosy 签名，用 PAT 反而会失败。
func checkinBearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	deviceToken, _ := ParseOAuthSecret(trimmed)
	if deviceToken != "" {
		return deviceToken
	}
	return trimmed
}

// campaignsCheckin 是真实发放积分的领取流程。
func campaignsCheckin(ctx context.Context, region Region, bearer string, result *CheckinResult) *CheckinResult {
	status, body, raw := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/campaigns", bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, core.Truncate(raw, 300))
		return result
	}
	list, okList := body.(map[string]any)
	if !okList {
		result.Message = fmt.Sprintf("活动列表格式异常: %s", core.Truncate(raw, 300))
		return result
	}
	campaigns := parseCampaigns(list["campaigns"])

	var target *campaignInfo
	var benefitCampaign *campaignInfo
	alreadyClaimed := false
	for index := range campaigns {
		campaign := &campaigns[index]
		if campaign.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		benefitCampaign = campaign
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			target = campaign
		case "CLAIMED":
			alreadyClaimed = true
		}
	}

	// 窗口日期取活动 startAt：每日窗口 10:00 → 次日 10:00，
	// 用窗口日期可避免 10:00 前把昨日窗口误记到今天。
	windowSource := target
	if windowSource == nil {
		windowSource = benefitCampaign
	}
	if windowSource != nil && windowSource.StartAt > 0 {
		result.WindowDate = time.Unix(windowSource.StartAt, 0).Format("2006-01-02")
	}

	if target == nil {
		if alreadyClaimed {
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		}
		result.Status = CheckinNoCampaign
		// 实测：国际版根本没有每日签到计划（/sash/api/v1/me/daily-check-in*
		// 返回 404），只有 VIEW_DETAILS 类促销活动。明确说明比笼统的
		// 「无可用活动」好——否则用户会反复排查自己哪里配错了。
		if region == RegionGlobal {
			result.Message = "该区域无每日签到活动（国际版没有签到计划）"
		} else {
			result.Message = "无可用签到活动"
		}
		return result
	}

	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest(ctx, region, http.MethodPost, claimPath, bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, core.Truncate(raw, 300))
		return result
	}
	var claim claimResponse
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &claim) != nil {
		result.Message = fmt.Sprintf("领取响应格式异常: %s", core.Truncate(raw, 300))
		return result
	}
	switch claim.Status {
	case "CLAIMED":
		if claim.Replayed {
			// 上游对重复领取返回 CLAIMED + replayed:true，语义是幂等命中。
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		}
		result.Status = CheckinClaimed
		if claim.Benefit != nil {
			result.Amount = claim.Benefit.Amount
		}
		if result.Amount == 0 {
			result.Amount = 100
		}
		result.Message = fmt.Sprintf("领取成功 +%d (%s)", result.Amount, target.CampaignKey)
		return result
	default:
		result.Message = fmt.Sprintf("未知状态: %s", claim.Status)
		return result
	}
}

// readDailyCheckinStats 只读 legacy 统计（绝不通过它领取）。
//
// 该接口对未领取日也返回 409，因此它的 status 不能用来判断「是否已领取」；
// 但它的连续天数与累计数据是准的，用来补充展示。
func readDailyCheckinStats(ctx context.Context, region Region, bearer string, result *CheckinResult) {
	status, body, _ := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/daily-check-in/status", bearer, nil)
	if status != http.StatusOK {
		return
	}
	var parsed dailyCheckinStatus
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &parsed) != nil || parsed.Status == "" {
		return
	}
	if parsed.CurrentStreakDays > result.StreakDays {
		result.StreakDays = parsed.CurrentStreakDays
	}
	if parsed.TotalClaimDays > result.TotalClaimDays {
		result.TotalClaimDays = parsed.TotalClaimDays
	}
	if parsed.TotalRewardCredits > result.TotalRewardCredits {
		result.TotalRewardCredits = parsed.TotalRewardCredits
	}
}

// parseCampaigns 把活动列表条目解成结构体。
func parseCampaigns(raw any) []campaignInfo {
	items, okItems := raw.([]any)
	if !okItems {
		return nil
	}
	out := make([]campaignInfo, 0, len(items))
	for _, item := range items {
		encoded, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			continue
		}
		var parsed campaignInfo
		if json.Unmarshal(encoded, &parsed) == nil {
			out = append(out, parsed)
		}
	}
	return out
}

// checkinHeaders 是桌面端签到所需的请求头（抓包确认）。
//
// cosy-clienttype: 10 是桌面端标识；上游按它放行签到接口，
// 不需要 cosy 签名（这是签到与对话接口最大的差异）。
func checkinHeaders(bearer string) map[string]string {
	return map[string]string{
		"authorization":   "Bearer " + bearer,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
}

// checkinBase 返回该区域签到接口的域名。
//
// 必须按账号区域选：国际版 device token 打到国内域名会得到 401 TOKEN_EXPIRE
// （上游 qoder2api 写死了国内域名，因此国际版账号签到必然失败）。
func checkinBase(region Region) string {
	return GetEndpoints(region).SashBase
}

// doCheckinRequest 发送签到相关请求，返回 (状态码, 解析后的 JSON, 原始正文)。
func doCheckinRequest(ctx context.Context, region Region, method, path, bearer string, reqBody any) (int, any, string) {
	base := checkinBase(region)
	url := base + path
	var body io.Reader
	if reqBody != nil {
		encoded, errMarshal := json.Marshal(reqBody)
		if errMarshal != nil {
			return 0, nil, "encode request: " + errMarshal.Error()
		}
		body = bytes.NewReader(encoded)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, body)
	if errRequest != nil {
		return 0, nil, fmt.Sprintf("build request: %v", errRequest)
	}
	for key, value := range checkinHeaders(bearer) {
		req.Header.Set(key, value)
	}
	if method == http.MethodPost {
		req.Header.Set("origin", base)
		if reqBody == nil {
			// 抓包确认 claim 请求无 body；显式置 0 避免上游把空 body 当异常。
			req.ContentLength = 0
		}
	}
	resp, errDo := hostbridge.Client(ctx, checkinHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Sprintf("do request: %v", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, string(data)
}
