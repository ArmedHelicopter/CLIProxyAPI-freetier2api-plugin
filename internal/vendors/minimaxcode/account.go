package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"

	"freetier2api-plugin/cpasdk/pluginapi"
	"strconv"
	"strings"
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
	_ = prepare(ctx, cred, baseURLOverride)

	// 2. 查询签到看板状态
	statusTarget := buildSigninTarget(cred, base, EpCheckinStatus, http.MethodGet, "")
	statusResp, errStatus := doRequest(ctx, cred, statusTarget, base, 30*time.Second)
	if errStatus != nil {
		return nil, fmt.Errorf("check signin status: %w", errStatus)
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
		return nil, fmt.Errorf("claim signin: %w", errClaim)
	}
	defer func() { _ = claimResp.Body.Close() }()

	if claimResp.StatusCode == http.StatusUnauthorized || claimResp.StatusCode == http.StatusForbidden {
		return nil, core.NewPluginError("minimax_unauthorized", "MiniMax 凭证无效或已过期", http.StatusUnauthorized)
	}
	if claimResp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim signin HTTP %d", claimResp.StatusCode)
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
	target := buildSigninTarget(cred, base, EpMembership, http.MethodPost, "{}")

	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return nil, fmt.Errorf("fetch quota: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, core.NewPluginError("minimax_unauthorized", fmt.Sprintf("minimax quota HTTP %d", resp.StatusCode), http.StatusUnauthorized)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("minimax quota HTTP %d", resp.StatusCode)
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

	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     planName,
			TierName: planName,
			TierID:   "minimax",
		},
		Summary: summaryMetrics,
	}, nil
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
