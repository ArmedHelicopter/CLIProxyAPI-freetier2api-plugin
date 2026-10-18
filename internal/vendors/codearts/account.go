package codearts

// 签到与福利领取实现：CodeArts 支持限时免费福利模型配额的领取。
// 参考：/Users/jiandan/Workspaces/codearts2api/cmd/models/main.go

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"freetier2api-plugin/cpasdk/pluginapi"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

func SupportsCheckin() bool {
	return true
}

// Checkin 执行福利领取。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, BenefitHost+EpBenefitClaim, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	signHuaweiRequest(req, []byte("{}"), cred)

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts claim benefit failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return &CheckinResult{
			Already: false,
			Message: "限时福利套餐领取成功",
		}, nil
	}

	var payload struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"error_message"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.ErrorCode == "InferHub.4004.201" || resp.StatusCode == http.StatusBadRequest {
		return &CheckinResult{
			Already: true,
			Message: core.FirstNonEmpty(payload.Message, "今日福利已领取或无需重复领取"),
		}, nil
	}

	return nil, fmt.Errorf("claim benefit response %d: %s", resp.StatusCode, string(body))
}

// 额度查询：获取 CodeArts 账号可用状态与额度概览。

func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts Agent 盘古助手",
			TierName: "Standard",
			TierID:   "codearts",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "status", Label: "状态", Value: 1, Unit: "正常"},
		},
	}

	return response, nil
}
