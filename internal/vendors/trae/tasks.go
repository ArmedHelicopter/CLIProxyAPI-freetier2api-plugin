package trae

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

func SupportsCheckin(cfg Config) bool { return cfg.supportsCheckin() }

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

func Checkin(ctx context.Context, cred *Credential, cfg Config) (*CheckinResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if !cfg.supportsCheckin() {
		return nil, fmt.Errorf("trae 国际版无签到活动")
	}

	identity := cred.UID
	if identity == "" {
		identity = cred.DeviceID
	}
	deviceID := deriveCheckinDeviceID(identity)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CheckinHost+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-Device-Brand", cfg.DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae checkin request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit int64 `json:"credit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode checkin response: %w", err)
	}

	if result.Code == 0 {
		return &CheckinResult{
			Already: false,
			Credit:  result.Data.Credit,
			Message: "签到成功",
		}, nil
	}

	if result.Code == 9074 || result.Code == 1001 {
		return &CheckinResult{
			Already: true,
			Message: core.FirstNonEmpty(result.Msg, "今日已完成签到"),
		}, nil
	}

	return nil, fmt.Errorf("checkin failed: code=%d msg=%s", result.Code, result.Msg)
}

func deriveCheckinDeviceID(identity string) string {
	if identity == "" {
		identity = "default_trae_user"
	}
	sum := sha256.Sum256([]byte(identity))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(1e16))
	return fmt.Sprintf("%016d", n)
}

func FetchQuota(ctx context.Context, cred *Credential, cfg Config) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if !cfg.supportsCheckin() && cfg.Region == RegionGlobal {
		return &pluginapi.QuotaFetchResponse{
			Subscription: &pluginapi.QuotaSubscription{
				Plan:     "Trae 国际版",
				TierName: "Standard",
				TierID:   cfg.VendorID,
			},
			Summary: []pluginapi.QuotaMetric{
				{Key: "status", Label: "状态", Value: 1, Unit: "有效"},
			},
		}, nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, cfg.QuotaHost+EpEntUsage, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cfg.clientUA())

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae quota request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		Code int `json:"code"`
		Data struct {
			TotalCredit     float64 `json:"total_credit"`
			RemainingCredit float64 `json:"remaining_credit"`
			UsedCredit      float64 `json:"used_credit"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &result)

	plan, tier := "Trae 国内版权益", "Standard"
	if cfg.Solo {
		plan, tier = "TRAE SOLO 权益包", "SOLO"
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     plan,
			TierName: tier,
			TierID:   cfg.VendorID,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "remaining_credit", Label: "剩余积分", Value: result.Data.RemainingCredit, Unit: "credits"},
			{Key: "total_credit", Label: "总额度", Value: result.Data.TotalCredit, Unit: "credits"},
			{Key: "used_credit", Label: "已用积分", Value: result.Data.UsedCredit, Unit: "credits"},
		},
	}, nil
}
