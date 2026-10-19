package codearts

import (
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

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

func SupportsCheckin() bool {
	return true
}

// BenefitGatewayStatus 报告福利通道是否开放。
type BenefitGatewayStatus struct {
	Enabled                 bool `json:"enabled"`
	IsExaminationDomain     bool `json:"is_examination_domain"`
	ExaminationModelEnabled bool `json:"examination_model_enable"`
}

// FetchBenefitGatewayStatus 查询福利通道是否开放。
//
// 端点在 **snap 主机**上而不是 benefit 主机（放错主机只会拿到 404）。
func FetchBenefitGatewayStatus(ctx context.Context, cred *Credential) (*BenefitGatewayStatus, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpBenefitGateway, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit gateway status failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var status BenefitGatewayStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("decode benefit gateway status: %w", err)
	}
	return &status, nil
}

// Checkin 执行每日福利领取。
//
// 领取接口是**幂等 GET**（官方客户端开着 IDE 时已经替我们领过，这里是换账号/
// 跨天/没开 IDE 时的兜底），所以重复签到不会出事。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	status, errStatus := FetchBenefitGatewayStatus(ctx, cred)
	if errStatus != nil {
		return nil, errStatus
	}
	if !status.Enabled {
		return &CheckinResult{Already: true, Message: "当前账号未开放免费福利 Token 活动"}, nil
	}

	claim, errClaim := ClaimBenefit(ctx, cred)
	if errClaim != nil {
		return nil, errClaim
	}
	message := "已领取当日免费福利 Token（每日 1000 万，当日 24:00 清零）"
	if claim.UserName != "" {
		message = "已为 " + claim.UserName + " 领取当日免费福利 Token"
	}
	return &CheckinResult{Message: message}, nil
}

// BenefitClaim 是一次福利领取的结果。
type BenefitClaim struct {
	Channel  string `json:"channel"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// ClaimBenefit 调用福利领取入口（幂等 GET）。
func ClaimBenefit(ctx context.Context, cred *Credential) (*BenefitClaim, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitClaim, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts claim benefit failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim benefit response %d: %s", resp.StatusCode, DescribeClaimError(string(body)))
	}

	var envelope struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Channel  string `json:"channel"`
			UserID   string `json:"user_id"`
			UserName string `json:"user_name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode claim benefit response: %w", err)
	}
	// 0000 是成功码；当天已领过仍返回 0000（幂等）。
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("claim benefit %s: %s", envelope.ErrorCode, DescribeClaimError(envelope.ErrorMsg))
	}
	return &BenefitClaim{
		Channel:  core.FirstNonEmpty(envelope.Result.Channel, "codearts"),
		UserID:   envelope.Result.UserID,
		UserName: envelope.Result.UserName,
	}, nil
}

// DescribeClaimError 把福利接口的错误码翻成人话。
func DescribeClaimError(message string) string {
	switch {
	case strings.Contains(message, "APIG.0602"):
		return "凭据（STS security token）已过期，无法领取；请重新登录或等保活刷新"
	case strings.Contains(message, "APIG.0301"), strings.Contains(message, "APIG.0303"):
		return "签名校验失败，凭据可能已失效"
	case strings.Contains(message, "4006"):
		return "今日福利名额已用完"
	case strings.Contains(message, "4004"):
		return "福利权益不存在或已过期"
	case strings.Contains(message, "4003"):
		return "账号已被禁用"
	}
	return message
}

// FetchQuota 查询每日免费福利 Token 的额度与已用量。
//
// 只报福利通道的数字：订阅积分不在本插件支持范围内，也不该用一个写死的
// 「状态正常」冒充真实额度。
//
// 指标键必须用 remaining / total / used 这些**标准键**：控制台的「剩余 / 总额」
// 只认它们（internal/auth.Totals），自定义键会被忽略，列里就永远是 0/0。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	balance, errBalance := fetchBenefitBalance(ctx, cred)
	if errBalance != nil {
		return nil, errBalance
	}
	if balance == nil {
		// 上游对「没有福利权益」的账号回 4004 benefit not found。那是**账号状态**
		// 而不是插件故障（同一账号的福利模型清单能正常列出、签到也返回成功），
		// 如实回报 0/0 并写清原因，好过让整列显示成「-- / --」让人以为查询失败。
		return noBenefitQuota(), nil
	}

	dailyLeft := balance.DailyTokenLimit - balance.DailyTokensUsed
	if dailyLeft < 0 {
		dailyLeft = 0
	}
	monthlyLeft := balance.MonthlyTokenLimit - balance.MonthlyTokensUsed
	if monthlyLeft < 0 {
		monthlyLeft = 0
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts 每日免费福利 Token",
			TierName: core.FirstNonEmpty(balance.Channel, "benefit"),
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "每日免费额度", Value: float64(balance.DailyTokenLimit)},
			{Key: "remaining", Label: "今日剩余", Value: float64(dailyLeft)},
			{Key: "used", Label: "今日已用", Value: float64(balance.DailyTokensUsed)},
			{Key: "benefit_monthly_total", Label: "每月额度", Value: float64(balance.MonthlyTokenLimit)},
			{Key: "benefit_monthly_remaining", Label: "本月剩余", Value: float64(monthlyLeft)},
		},
	}, nil
}

// noBenefitQuota 是「账号没有福利权益」时的额度结果。
func noBenefitQuota() *pluginapi.QuotaFetchResponse {
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts 每日免费福利 Token",
			TierName: "未开通",
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "每日免费额度（上游未开通福利权益，无可查额度）", Value: 0},
		},
	}
}

// benefitBalance 是福利通道的额度数据。
type benefitBalance struct {
	Channel           string
	DailyTokenLimit   int64
	DailyTokensUsed   int64
	MonthlyTokenLimit int64
	MonthlyTokensUsed int64
}

// fetchBenefitBalance 查询福利额度。
//
// 返回 (nil, nil) 表示上游明确回「该账号没有福利记录」——不是错误，是答案。
func fetchBenefitBalance(ctx context.Context, cred *Credential) (*benefitBalance, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitBalance, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit balance failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Channel           string `json:"channel"`
			DailyTokenLimit   int64  `json:"daily_token_limit"`
			DailyTokensUsed   int64  `json:"daily_tokens_used"`
			MonthlyTokenLimit int64  `json:"monthly_token_limit"`
			MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit balance: %w", err)
	}
	// 0000 是成功码；4004 是「没有福利记录」，两者都不是错误退出。
	if envelope.ErrorCode == "4004" || strings.Contains(envelope.ErrorMsg, "benefit not found") {
		return nil, nil
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit balance %s: %s", envelope.ErrorCode, DescribeClaimError(envelope.ErrorMsg))
	}
	return &benefitBalance{
		Channel:           envelope.Result.Channel,
		DailyTokenLimit:   envelope.Result.DailyTokenLimit,
		DailyTokensUsed:   envelope.Result.DailyTokensUsed,
		MonthlyTokenLimit: envelope.Result.MonthlyTokenLimit,
		MonthlyTokensUsed: envelope.Result.MonthlyTokensUsed,
	}, nil
}
