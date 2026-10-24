package raccoon

// 本文件实现小浣熊（Raccoon）凭证的解析与归属判定。
//
// 与 opencodezen / autoclaw 同型：小浣熊是 API key 型供应商，没有 OAuth、
// 没有token 交换、没有续期。凭证就是一把静态 key（用户到小浣熊控制台
// 自行申请），key 要么有效要么失效。
//
// 截至本文件编写时，小浣熊只有一个部署（无国内/国际之分）；若后续确认
// 有多区域部署，按 workbuddy / autoclaw 的双实例模式扩展即可。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/core"
)

// VendorID 是供应商实例标识。同时是凭证文件名前缀（raccoon-<uid>.json）。
const VendorID = "raccoon"

// VendorName 是管理端展示名。
const VendorName = "小浣熊"

// protocolName 是家族别名（文件名中缀匹配用，如 my-raccoon.json）。
const protocolName = "raccoon"

// ErrMissingAPIKey 表示凭证里没有可用的 API key。
var ErrMissingAPIKey = errors.New("credential has no api key")

// credentialKeyKeys 是凭证里可能承载 key 的字段名（按优先级）。
//
// raccoon_key 是专有字段名（归属判定用它）；其余是 OpenAI 风格控制台
// 发放的 key 常见写法。
var credentialKeyKeys = []string{
	"raccoon_key", "api_key", "apikey", "apiKey", "key", "token", "access_token",
}

// Credential 是归一化后的小浣熊凭证。
type Credential struct {
	// APIKey 是出站凭证（Bearer）。
	APIKey string
	// Label 是展示名。未提供时由调用方用脱敏后的 key 兜底。
	Label string
}

// ParseCredential 解析小浣熊凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{APIKey: firstKeyIn(payload)}
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

// LooksLikeCredential 判断一份 JSON 是否属于小浣熊。
//
// **归属判定必须偏严**（与各供应商同一纪律）：宿主会把所有非内建格式的
// 凭证依次喂给每个插件，误吞别家的会让宿主用本插件的结构覆盖对方账号
// （且是静默的）。漏认只是加载失败，用户立刻会发现，代价小得多。
//
// 只接受小浣熊特有的证据：
//  1. 文件名含 raccoon / 小浣熊拼音段（raccoon-*.json / xiaohuanxiong 等）；
//  2. 凭证里有小浣熊专有字段名（raccoon_key）；
//  3. 凭证显式声明 vendor: raccoon。
//
// 刻意不接受 api_key / token 这类通用字段名做判据：OpenCode ZEN、Cline
// 等供应商也用它们，拿通用名当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorID, protocolName, "xiaohuanxiong") {
		return true
	}
	if raw == nil {
		return false
	}
	if vendor, okVendor := raw[core.VendorKey].(string); okVendor {
		if strings.EqualFold(strings.TrimSpace(vendor), VendorID) {
			return true
		}
	}
	if value, okValue := raw["raccoon_key"].(string); okValue && strings.TrimSpace(value) != "" {
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

