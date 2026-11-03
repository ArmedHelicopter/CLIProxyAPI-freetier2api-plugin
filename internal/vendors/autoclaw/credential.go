package autoclaw

// 本文件实现 AutoClaw 凭证的解析、归属判定、脱敏与写回合并。
//
// 凭证形态（与 Agent2API 的 auths/autoclaw-*.json 对齐）：
//
//	{
//	  "vendor": "autoclawcn",
//	  "accessToken": "eyJ…",
//	  "refreshToken": "eyJ…",
//	  "expiresAt": 1780000000,
//	  "userId": "123",
//	  "userName": "…",
//	  "region": "cn"
//	}
//
// 登录凭证由 login.go 的短信 / OAuth 链路生成；refresh_token 支持自动续期。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"freetier2api-plugin/internal/core"
)

// 区域实例标识。同时是凭证文件名前缀（autoclawcn-<uid>.json 等）。
const (
	VendorIDCN     = "autoclawcn"
	VendorIDGlobal = "autoclawglobal"
	// protocolName 是家族别名（文件名中缀匹配用，如 my-autoclaw.json）。
	protocolName = "autoclaw"
)

// 展示名。
const (
	VendorNameCN     = "AutoClaw 国内版"
	VendorNameGlobal = "AutoClaw 国际版"
)

// VendorIDFor 返回区域对应的实例标识。
func VendorIDFor(region Region) string {
	if region.IsGlobal() {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor 返回区域对应的展示名。
func VendorNameFor(region Region) string {
	if region.IsGlobal() {
		return VendorNameGlobal
	}
	return VendorNameCN
}

// Region 是 AutoClaw 的部署区域。
type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"
)

// IsGlobal 报告是否为国际版部署。
func (r Region) IsGlobal() bool { return r == RegionGlobal }

// NormalizeRegion 把任意区域字符串归一化（未知值回落到 cn）。
func NormalizeRegion(raw string) Region {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(RegionGlobal), "intl", "international":
		return RegionGlobal
	default:
		return RegionCN
	}
}

// RegionForCredential 从凭证 JSON 推断所属区域，推断不出返回空串。
func RegionForCredential(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	for _, key := range []string{"region", "realm"} {
		if value, okValue := raw[key].(string); okValue {
			trimmed := strings.ToLower(strings.TrimSpace(value))
			if trimmed == string(RegionCN) || trimmed == string(RegionGlobal) {
				return trimmed
			}
		}
	}
	return ""
}

// ErrMissingToken 表示凭证里没有可用的 access token。
var ErrMissingToken = errors.New("credential has no accessToken")

// Credential 是归一化后的 AutoClaw 凭证。
//
// 并发约定与 core.Credential 一致：出站取值经取值方法加锁快照，
// 刷新线程会并发改写 token 字段。
type Credential struct {
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	// ExpiresAt 是 access token 过期时刻（Unix 秒）；0 表示未知。
	// 上游登录响应无 expires_in，Agent2API 按 28 天保守缓存。
	ExpiresAt int64
	UID       string
	UserName  string
	Phone     string
	// DeviceID 是登录设备标识（刷新请求需要原样回传）。
	DeviceID string
	Region   Region
	// SandboxID / SandboxEndpoint 是云端沙箱标识与端点（可选，chat 走
	// region proxy 路径时不需要；保留以便后续接入沙箱 relay 通道）。
	SandboxID       string
	SandboxEndpoint string
}

// ParseCredential 解析 AutoClaw 凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{}
	getStr := func(keys ...string) string {
		for _, key := range keys {
			if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	cred.AccessToken = getStr("accessToken", "access_token", "token")
	cred.RefreshToken = getStr("refreshToken", "refresh_token")
	cred.UserName = getStr("userName", "user_name", "name", "label")
	cred.Phone = getStr("phone")
	cred.DeviceID = getStr("deviceId", "device_id")
	cred.SandboxID = getStr("sandboxId", "sandbox_id")
	cred.SandboxEndpoint = getStr("sandboxEndpoint", "sandbox_endpoint")
	if uid, okUID := payload["userId"]; okUID {
		switch value := uid.(type) {
		case string:
			cred.UID = strings.TrimSpace(value)
		case float64:
			cred.UID = strings.TrimSuffix(fmt.Sprintf("%.0f", value), ".0")
		}
	}
	if cred.UID == "" {
		cred.UID = getStr("uid", "user_id")
	}
	if cred.DeviceID == "" {
		// 与 Agent2API 一致：deviceId 允许留空，留空时用 uid 兜底
		//（刷新请求按账号原样回传，上游只要求稳定性）。
		cred.DeviceID = strings.TrimSpace(cred.UID)
	}
	if region := RegionForCredential(payload); region != "" {
		cred.Region = NormalizeRegion(region)
	} else if vendor, okVendor := payload[core.VendorKey].(string); okVendor {
		// vendor 字段里带区域（autoclawcn / autoclawglobal）时顺带推断。
		if strings.Contains(strings.ToLower(vendor), "global") {
			cred.Region = RegionGlobal
		} else if strings.Contains(strings.ToLower(vendor), protocolName) {
			cred.Region = RegionCN
		}
	}
	if seconds := core.GetUnixSeconds(payload, "expiresAt", "expires_at"); seconds > 0 {
		cred.ExpiresAt = seconds
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return nil, ErrMissingToken
	}
	return cred, nil
}

// LooksLikeCredential 判断一份 JSON 是否属于 AutoClaw（任意区域）。
//
// **归属判定必须偏严**（与各供应商同一纪律）：宿主会把所有非内建格式的
// 凭证依次喂给每个插件，误吞别家的会让宿主用本插件的结构覆盖对方账号
// （且是静默的）。漏认只是加载失败，用户立刻会发现，代价小得多。
//
// 只接受 AutoClaw 特有的证据：
//  1. 文件名含 autoclaw 段（autoclawcn / autoclawglobal / my-autoclaw 等）；
//  2. 凭证显式声明 vendor: autoclaw*；
//  3. 凭证具备 autoclaw 特征组合（sandboxEndpoint 或 X 侧 device 字段名）。
//
// 刻意不接受 accessToken / refreshToken 这类通用字段名做判据：workbuddy /
// trae 等也用它们，拿通用名当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorIDCN, VendorIDGlobal, protocolName) {
		return true
	}
	if raw == nil {
		return false
	}
	if vendor, okVendor := raw[core.VendorKey].(string); okVendor {
		trimmed := strings.ToLower(strings.TrimSpace(vendor))
		if strings.HasPrefix(trimmed, protocolName) {
			return true
		}
	}
	// 特征组合：Agent2API 凭证专属的沙箱端点字段。
	// deviceId / device_id 单独出现太宽（qoder 等也用），不作判据。
	for _, key := range []string{"sandboxEndpoint", "sandbox_endpoint"} {
		if _, okKey := raw[key]; okKey {
			return true
		}
	}
	_ = provider
	return false
}

// ---- 加锁取值（供并发出站使用） ----

// AccessTokenValue 返回加锁快照的 access token。
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue 返回加锁快照的 refresh token。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// ExpiresAtValue 返回加锁快照的过期时刻（Unix 秒）。
func (c *Credential) ExpiresAtValue() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExpiresAt
}

// UIDValue 返回加锁快照的用户 ID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// DeviceIDValue 返回加锁快照的设备 ID。
func (c *Credential) DeviceIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceID
}

// UserNameValue 返回加锁快照的展示名。
func (c *Credential) UserNameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UserName
}

// UnchangedSince 报告刷新前后 token 是否都没变。
func (c *Credential) UnchangedSince(accessBefore, refreshBefore string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessBefore && c.RefreshToken == refreshBefore
}

// Snapshot 返回加锁快照的 token 对。
func (c *Credential) Snapshot() (access, refresh string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// ApplyTokens 写入刷新结果（refresh token 为空表示上游未轮换，沿用旧值）。
func (c *Credential) ApplyTokens(access, refresh string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(access) != "" {
		c.AccessToken = strings.TrimSpace(access)
	}
	if strings.TrimSpace(refresh) != "" {
		c.RefreshToken = strings.TrimSpace(refresh)
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// Realm 返回凭证所属区域。
func (c *Credential) Realm() Region {
	if c == nil {
		return RegionCN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Region
}

// StorageJSON 把凭证序列化成落盘 JSON（vendor 字段冗余声明归属）。
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := map[string]any{
		core.VendorKey: VendorIDFor(c.Region),
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
		"region":       string(c.Region),
	}
	if c.UID != "" {
		payload["userId"] = c.UID
	}
	if c.UserName != "" {
		payload["userName"] = c.UserName
	}
	if c.Phone != "" {
		payload["phone"] = c.Phone
	}
	if c.DeviceID != "" {
		payload["deviceId"] = c.DeviceID
	}
	if c.SandboxID != "" {
		payload["sandboxId"] = c.SandboxID
	}
	if c.SandboxEndpoint != "" {
		payload["sandboxEndpoint"] = c.SandboxEndpoint
	}
	return json.MarshalIndent(payload, "", "  ")
}

// ---------------------------------------------------------------------------
// 凭证写回合并
// ---------------------------------------------------------------------------

// 复用 core.MergeCredentialJSON 骨架：只覆盖 token 相关字段，
// 用户手工维护的 note / disabled 等字段原样保留。

// MergeCredentialJSON 把刷新后的令牌合并回原凭证 JSON。
func MergeCredentialJSON(original []byte, cred *Credential) ([]byte, error) {
	return core.MergeCredentialJSON(original, cred.StorageJSON, func(target map[string]any) {
		target["accessToken"] = cred.AccessTokenValue()
		target["refreshToken"] = cred.RefreshTokenValue()
		target["expiresAt"] = cred.ExpiresAtValue()
	})
}
