package autoclaw

// 本文件实现 AutoClaw 凭证的解析与归属判定。
//
// 与 opencodezen 同型：AutoClaw 是 API key 型供应商，没有 OAuth、没有
// token 交换、没有续期。凭证就是一把静态 key（用户到 AutoClaw 控制台
// 自行申请），key 要么有效要么失效。
//
// 国内版与国际版是两套独立部署（域名独立），key 不通用；但凭证文件本身
// 结构一致，区域靠文件名前缀与 vendor 字段区分（与 WorkBuddy / Qoder
// 的双实例约定一致）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
//
// 只认显式的 region / realm 字段，不猜：AutoClaw 的两个部署域名在拿到
// 上游资料前无法作为判据（见 docs/vendor-autoclaw-raccoon.md）。
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

// ErrMissingAPIKey 表示凭证里没有可用的 API key。
var ErrMissingAPIKey = errors.New("credential has no api key")

// credentialKeyKeys 是凭证里可能承载 key 的字段名（按优先级）。
var credentialKeyKeys = []string{
	"api_key", "apikey", "apiKey", "autoclaw_key", "key", "token", "access_token",
}

// Credential 是归一化后的 AutoClaw 凭证。
type Credential struct {
	// APIKey 是出站凭证（Bearer）。
	APIKey string
	// Region 是凭证声明的区域（可能为空，归属以实例为准）。
	Region Region
	// Label 是展示名。未提供时由调用方用脱敏后的 key 兜底。
	Label string
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
	cred := &Credential{APIKey: firstKeyIn(payload), Region: NormalizeRegion(RegionForCredential(payload))}
	if cred.APIKey == "" {
		return nil, fmt.Errorf("credential has no api key field (tried: %s)", strings.Join(credentialKeyKeys, ", "))
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if cred.Label == "" {
		if email, okEmail := payload["email"].(string); okEmail {
			cred.Label = strings.TrimSpace(email)
		}
	}
	return cred, nil
}

// firstKeyIn 返回一层 JSON 里第一个非空的 key 字段。
func firstKeyIn(payload map[string]any) string {
	for _, key := range credentialKeyKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential 判断一份 JSON 是否属于 AutoClaw（任意区域）。
//
// **归属判定必须偏严**（与各供应商同一纪律）：宿主会把所有非内建格式的
// 凭证依次喂给每个插件，误吞别家的会让宿主用本插件的结构覆盖对方账号
// （且是静默的）。漏认只是加载失败，用户立刻会发现，代价小得多。
//
// 只接受 AutoClaw 特有的证据：
//  1. 文件名含 autoclaw 段（autoclawcn / autoclawglobal / my-autoclaw 等）；
//  2. 凭证里有 autoclaw 专有字段名（autoclaw_key）；
//  3. 凭证显式声明 vendor: autoclaw*。
//
// 刻意不接受 api_key / token 这类通用字段名做判据：OpenCode ZEN、Cline
// 等供应商也用它们，拿通用名当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorIDCN, VendorIDGlobal, protocolName) {
		return true
	}
	if raw == nil {
		return false
	}
	// 显式 vendor 声明：autoclaw 家族都认（区域再由实例 Match 区分）。
	if vendor, okVendor := raw[core.VendorKey].(string); okVendor {
		trimmed := strings.ToLower(strings.TrimSpace(vendor))
		if strings.HasPrefix(trimmed, protocolName) {
			return true
		}
	}
	// 专有字段名：名字里带 autoclaw，通用名不算。
	if value, okValue := raw["autoclaw_key"].(string); okValue && strings.TrimSpace(value) != "" {
		return true
	}
	_ = provider
	return false
}

// MaskedKey 返回脱敏后的 key（展示用）：只保留前 4 后 4 位。
func MaskedKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", len(trimmed)-8) + trimmed[len(trimmed)-4:]
}
