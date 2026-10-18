package minimaxcode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// Credential 是归一化后的 MiniMax Code 账号凭证。
type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	UUID         string `json:"uuid,omitempty"`
	ScreenWidth  int    `json:"screen_width,omitempty"`
	ScreenHeight int    `json:"screen_height,omitempty"`
	Email        string `json:"email,omitempty"`
	Label        string `json:"label,omitempty"`
	AuthMode     string `json:"auth_mode,omitempty"` // "oauth" 或 "token"
	BaseURL      string `json:"base_url,omitempty"`
}

// RandomUUID 生成 RFC 4122 v4 UUID。
func RandomUUID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40 // Version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// RandomDeviceID 生成 8 位十进制数字设备标识符（范围 10000000 ~ 99999999）。
func RandomDeviceID() string {
	n, err := rand.Int(rand.Reader, big.NewInt(90000000))
	if err != nil {
		return "12345678"
	}
	return fmt.Sprintf("%d", 10000000+n.Int64())
}

// EnsureIdentifiers 确保凭证包含必须的指纹标识符。
func (c *Credential) EnsureIdentifiers() {
	if c.UUID == "" {
		c.UUID = RandomUUID()
	}
	if c.DeviceID == "" {
		c.DeviceID = RandomDeviceID()
	}
	if c.ScreenWidth == 0 {
		c.ScreenWidth = 1920
	}
	if c.ScreenHeight == 0 {
		c.ScreenHeight = 1080
	}
	if c.AuthMode == "" {
		if c.RefreshToken != "" {
			c.AuthMode = "oauth"
		} else {
			c.AuthMode = "token"
		}
	}
}

// ParseCredential 解析 MiniMax Code 凭证 JSON。
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("minimax credential is empty")
	}

	var m map[string]any
	if errUnmarshal := json.Unmarshal(raw, &m); errUnmarshal != nil {
		return nil, fmt.Errorf("decode minimax credential JSON: %w", errUnmarshal)
	}

	authObj := getSubMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := getSubMap(m, "account")

	regionStr := strings.ToLower(core.FirstNonEmpty(
		core.GetString(m, "region"),
		core.GetString(m, "realm"),
		core.GetString(m, "_edition"),
		string(defaultRegion),
	))
	r := RegionCN
	if regionStr == "global" || regionStr == "io" || regionStr == "en" || regionStr == "minimaxcodeglobal" || regionStr == "minimaxglobal" {
		r = RegionGlobal
	}

	token := core.FirstNonEmpty(
		core.GetString(authObj, "access_token"),
		core.GetString(authObj, "accessToken"),
		core.GetString(authObj, "token"),
		core.GetString(authObj, "bearer"),
		core.GetString(m, "token"),
	)
	refreshToken := core.FirstNonEmpty(
		core.GetString(authObj, "refresh_token"),
		core.GetString(authObj, "refreshToken"),
		core.GetString(m, "refresh_token"),
	)

	if token == "" && refreshToken == "" {
		return nil, fmt.Errorf("minimax credential missing token or refresh_token")
	}

	authMode := strings.ToLower(core.FirstNonEmpty(
		core.GetString(m, "auth_mode"),
		core.GetString(authObj, "auth_mode"),
	))
	if authMode == "" {
		if refreshToken != "" {
			authMode = "oauth"
		} else {
			authMode = "token"
		}
	}

	screenWidth := getInt(m, "screen_width", "screenWidth")
	if screenWidth == 0 {
		screenWidth = 1920
	}
	screenHeight := getInt(m, "screen_height", "screenHeight")
	if screenHeight == 0 {
		screenHeight = 1080
	}

	cred := &Credential{
		Vendor:       core.FirstNonEmpty(core.GetString(m, "vendor"), VendorIDFor(r)),
		Region:       r,
		AccessToken:  token,
		RefreshToken: refreshToken,
		ExpiresAt:    core.GetInt64(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UserID: core.FirstNonEmpty(
			core.GetString(authObj, "user_id"),
			core.GetString(accountObj, "user_id"),
			core.GetString(m, "user_id"),
			core.GetString(m, "realUserID"),
			core.GetString(m, "real_user_id"),
			core.GetString(m, "uid"),
		),
		AgentID: core.FirstNonEmpty(
			core.GetString(authObj, "agent_id"),
			core.GetString(m, "agent_id"),
			core.GetString(m, "agentId"),
		),
		DeviceID: core.FirstNonEmpty(
			core.GetString(authObj, "device_id"),
			core.GetString(m, "device_id"),
			core.GetString(m, "deviceId"),
		),
		UUID: core.FirstNonEmpty(
			core.GetString(authObj, "uuid"),
			core.GetString(m, "uuid"),
		),
		ScreenWidth:  screenWidth,
		ScreenHeight: screenHeight,
		Email: core.FirstNonEmpty(
			core.GetString(accountObj, "email"),
			core.GetString(m, "email"),
			core.GetString(m, "identifier"),
		),
		Label:    core.FirstNonEmpty(core.GetString(m, "label"), core.GetString(m, "name")),
		AuthMode: authMode,
		BaseURL:  core.GetString(m, "base_url"),
	}

	cred.EnsureIdentifiers()
	return cred, nil
}

// LooksLikeCredential 判断文件或数据是否属于本 MiniMax Code 区域实例。
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	expectedVendor := VendorIDFor(targetRegion)

	// 1/2. 显式 vendor 字段优先（家族别名交给区域判定）。
	if decided, claimed := core.VendorFieldVerdict(raw, expectedVendor, "minimax", "minimaxcode"); decided {
		return claimed
	}

	// 3. 文件名前缀匹配
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		if targetRegion == RegionGlobal {
			if strings.HasPrefix(base, "minimaxcodeglobal") || strings.HasPrefix(base, "minimaxglobal") {
				return true
			}
		} else {
			if strings.HasPrefix(base, "minimaxcodecn") || strings.HasPrefix(base, "minimaxcn") {
				return true
			}
		}
	}

	// 4. 特征字段识别
	hasAgentID := core.GetString(raw, "agent_id") != "" || core.GetString(raw, "agentId") != ""
	hasRealUserID := core.GetString(raw, "realUserID") != "" || core.GetString(raw, "real_user_id") != ""
	hasMinimaxSignature := strings.Contains(strings.ToLower(core.GetString(raw, "app_id")), "3001") ||
		strings.Contains(strings.ToLower(core.GetString(raw, "client_id")), "mcode-public")

	if hasAgentID || hasRealUserID || hasMinimaxSignature {
		r := strings.ToLower(core.FirstNonEmpty(core.GetString(raw, "region"), core.GetString(raw, "realm")))
		if targetRegion == RegionGlobal && (r == "global" || r == "en" || r == "io") {
			return true
		}
		if targetRegion == RegionCN && (r == "cn" || r == "") {
			return true
		}
	}

	return false
}

// StorageJSON 序列化为规范的凭证存储 JSON。
func (c *Credential) StorageJSON() ([]byte, error) {
	c.EnsureIdentifiers()
	m := map[string]any{
		"vendor":        c.Vendor,
		"region":        c.Region,
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"user_id":       c.UserID,
		"agent_id":      c.AgentID,
		"device_id":     c.DeviceID,
		"uuid":          c.UUID,
		"screen_width":  c.ScreenWidth,
		"screen_height": c.ScreenHeight,
		"auth_mode":     c.AuthMode,
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	if c.BaseURL != "" {
		m["base_url"] = c.BaseURL
	}
	return json.MarshalIndent(m, "", "  ")
}

// MergeStorageJSON 合并刷新后的令牌，保留用户自定义字段。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	if len(original) == 0 {
		return cred.StorageJSON()
	}
	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		return cred.StorageJSON()
	}

	m["access_token"] = cred.AccessToken
	m["refresh_token"] = cred.RefreshToken
	m["expires_at"] = cred.ExpiresAt
	if cred.UserID != "" {
		m["user_id"] = cred.UserID
	}
	if cred.AgentID != "" {
		m["agent_id"] = cred.AgentID
	}
	if cred.DeviceID != "" {
		m["device_id"] = cred.DeviceID
	}
	if cred.UUID != "" {
		m["uuid"] = cred.UUID
	}
	if cred.Email != "" && core.GetString(m, "email") == "" {
		m["email"] = cred.Email
	}
	if cred.Label != "" && core.GetString(m, "label") == "" {
		m["label"] = cred.Label
	}

	return json.MarshalIndent(m, "", "  ")
}
func getInt(m map[string]any, keys ...string) int {
	return int(core.GetInt64(m, keys...))
}
func getSubMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok && v != nil {
		if sub, ok := v.(map[string]any); ok {
			return sub
		}
	}
	return nil
}

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
