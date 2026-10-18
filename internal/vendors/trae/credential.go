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
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

// 本文件实现 Trae 家族凭证的解析、特征识别与存储。

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证（保留给协议层测试使用）。
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	return parseCredential(raw, VendorIDFor(defaultRegion), defaultRegion)
}

// ParseCredentialFor 按实例 profile 解析凭证。
func ParseCredentialFor(raw []byte, cfg Config) (*Credential, error) {
	return parseCredential(raw, cfg.VendorID, cfg.Region)
}

func parseCredential(raw []byte, defaultVendor string, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("trae credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode trae credential json: %w", err)
	}

	authObj := core.GetMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := core.GetMap(m, "account")

	vendor := strings.ToLower(core.FirstNonEmpty(core.GetString(m, "vendor"), defaultVendor))
	r := defaultRegion
	regionStr := strings.ToLower(core.FirstNonEmpty(
		core.GetString(m, "region"),
		core.GetString(m, "realm"),
		core.GetString(m, "_edition"),
		string(defaultRegion),
	))
	switch {
	case vendor == VendorIDSolo:
		r = RegionCN
	case regionStr == "global" || regionStr == "sg" || regionStr == "traeglobal":
		r = RegionGlobal
	default:
		r = RegionCN
	}
	c := &Credential{
		Vendor:       core.FirstNonEmpty(vendor, defaultVendor),
		Region:       r,
		AccessToken:  core.FirstNonEmpty(core.GetString(authObj, "access_token"), core.GetString(authObj, "accessToken"), core.GetString(authObj, "token")),
		RefreshToken: core.FirstNonEmpty(core.GetString(authObj, "refresh_token"), core.GetString(authObj, "refreshToken")),
		ExpiresAt:    core.GetUnixSeconds(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UID:          core.FirstNonEmpty(core.GetString(authObj, "uid"), core.GetString(accountObj, "uid"), core.GetString(m, "uid")),
		Nickname:     core.FirstNonEmpty(core.GetString(authObj, "nickname"), core.GetString(accountObj, "nickname"), core.GetString(m, "nickname")),
		EnterpriseID: core.FirstNonEmpty(core.GetString(authObj, "enterprise_id"), core.GetString(accountObj, "enterprise_id"), core.GetString(m, "enterprise_id")),
		MachineID:    core.FirstNonEmpty(core.GetString(authObj, "machine_id"), core.GetString(authObj, "machineId"), core.GetString(m, "machine_id")),
		DeviceID:     core.FirstNonEmpty(core.GetString(authObj, "device_id"), core.GetString(authObj, "deviceId"), core.GetString(m, "device_id")),
		Label:        core.GetString(m, "label"),
	}

	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("trae credential missing access_token and refresh_token")
	}
	return c, nil
}

// VendorIDFor 返回某区域对应的 VendorID。
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// LooksLikeCredential 判断文件或数据是否属于本 Trae 区域实例。
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, ConfigFor(targetRegion))
}

// LooksLikeCredentialFor 判断文件或数据是否属于某个 Trae profile。
func LooksLikeCredentialFor(raw map[string]any, fileName, provider string, cfg Config) bool {
	if cfg.Solo {
		if decided, claimed := core.VendorFieldVerdict(raw, VendorIDSolo, "trae"); decided {
			return claimed
		}
		if core.FileNameMatchesConvention(fileName, VendorIDSolo) {
			return true
		}
		return core.GetString(raw, "function") == FunctionSolo || strings.Contains(core.GetString(raw, "api_host"), "mchost.guru")
	}

	expectedVendor := cfg.VendorID
	if decided, claimed := core.VendorFieldVerdict(raw, expectedVendor, "trae"); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, expectedVendor) {
		return true
	}
	// 旧的家族前缀 trae-*.json：按文件内 region 判定。
	if core.FileNameMatchesConvention(fileName, "trae") {
		rStr := strings.ToLower(core.FirstNonEmpty(core.GetString(raw, "region"), core.GetString(raw, "realm"), core.GetString(raw, "_edition")))
		if cfg.Region == RegionGlobal {
			return rStr == "global" || rStr == "sg"
		}
		return rStr == "cn" || rStr == ""
	}
	return false
}

func (c *Credential) StorageJSON() ([]byte, error) {
	vendor := c.Vendor
	if vendor == "" {
		vendor = VendorIDFor(c.Region)
	}
	m := map[string]any{
		"type":          "freetier",
		"vendor":        vendor,
		"region":        string(c.Region),
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"uid":           c.UID,
		"nickname":      c.Nickname,
		"enterprise_id": c.EnterpriseID,
		"machine_id":    c.MachineID,
		"device_id":     c.DeviceID,
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	return json.MarshalIndent(m, "", "  ")
}

// 签到相关实现：国内版与 TRAE SOLO 支持签到，国际版不支持。

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

	client := httpx.Client(ctx, 30*time.Second)
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

// 额度查询：获取 Trae / TRAE SOLO 账号积分与用量。

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

	client := httpx.Client(ctx, 30*time.Second)
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
