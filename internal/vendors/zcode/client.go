package zcode

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// Plan 通道的请求体变换（对齐官方客户端行为）。
//
// 三项变换的作用与必要性：
//  1. system 身份块：上游网关做内容审查，请求缺少官方身份块会被拒为
//     3012「unusual activity」；官客户端固定前置这三个块。
//  2. cache_control：最后一条非 system 消息的末块追加 ephemeral 标记。
//     低于缓存门槛时上游静默忽略，因此无条件追加是安全的。
//  3. metadata.user_id：官方客户端注入了登录用户标识。
//
// 所有变换对畸形输入保持 no-op：变换失败绝不能放大成请求失败。
// 参考 zcode2api/app/body_transform.py。

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeSystemOnce   sync.Once
	zcodeSystemBlocks []any
)

// systemBlocks 返回官方身份块（解析失败时为空，此时跳过注入）。
func systemBlocks() []any {
	zcodeSystemOnce.Do(func() {
		var blocks []any
		if errUnmarshal := json.Unmarshal(zcodeSystemJSON, &blocks); errUnmarshal != nil {
			blocks = nil
		}
		zcodeSystemBlocks = blocks
	})
	return zcodeSystemBlocks
}

// applyStartPlanSystem 前置官方身份块与当前模型块，保留客户端原有 system。
//
// 幂等：首块已是官方标识时直接返回，重复调用不会重复拼接。
func applyStartPlanSystem(body map[string]any, model string) {
	blocks := systemBlocks()
	if len(blocks) == 0 {
		return
	}

	existing := body["system"]
	if list, ok := existing.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]any); ok {
			if core.RawStringField(first, "text") == core.RawStringField(blocks[0].(map[string]any), "text") {
				return
			}
		}
	}

	official := make([]any, 0, len(blocks)+2)
	for _, block := range blocks {
		official = append(official, cloneBlock(block))
	}
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		official = append(official, map[string]any{
			"type":          "text",
			"text":          "- You are powered by the model named " + trimmed + ".",
			"cache_control": map[string]any{"type": "ephemeral"},
		})
	}
	body["system"] = append(official, normalizeUserSystem(existing)...)
}

// normalizeUserSystem 把客户端原有 system 归一成 text 块列表。
//
// Anthropic 要求 system 是 block 数组（或字符串），官方客户端注入后也是数组，
// 因此这里统一成数组形态，避免出现「数组 + 字符串」混合。
func normalizeUserSystem(system any) []any {
	switch v := system.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			switch block := item.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					out = append(out, map[string]any{"type": "text", "text": block})
				}
			case map[string]any:
				if core.RawStringField(block, "type") != "text" || strings.TrimSpace(core.RawStringField(block, "text")) == "" {
					continue
				}
				entry := map[string]any{"type": "text", "text": core.RawStringField(block, "text")}
				if cc, ok := block["cache_control"]; ok {
					entry["cache_control"] = cc
				}
				out = append(out, entry)
			}
		}
		return out
	}
	return nil
}

// applyCacheControl 给最后一条非 system 消息的末块加 ephemeral 缓存标记。
//
// 幂等：已有 cache_control 时不动。
func applyCacheControl(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || core.RawStringField(msg, "role") == "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = []any{map[string]any{
				"type":          "text",
				"text":          content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []any:
			if len(content) == 0 {
				return
			}
			last, ok := content[len(content)-1].(map[string]any)
			if !ok || last["cache_control"] != nil {
				return
			}
			last["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return
	}
}

// cloneBlock 深拷贝一个 system 块，避免调用方改动污染缓存的原型。
func cloneBlock(block any) any {
	raw, errMarshal := json.Marshal(block)
	if errMarshal != nil {
		return block
	}
	var cloned any
	if errUnmarshal := json.Unmarshal(raw, &cloned); errUnmarshal != nil {
		return block
	}
	return cloned
}

// 上游常量收口：URL、端点、客户端指纹与模型名。
//
// 约定与其它供应商一致：**禁止在别处硬编码上游常量**，一律引用本文件。
// 参考 zcode2api/app/constants.py（其自身又标注「值均来自源码实证」）。

const (
	// VendorID 是本供应商在 freetier2api 插件内的唯一标识。
	VendorID = "zcode"
	// VendorName 是管理端与列表的展示名。
	VendorName = "ZCode"

	// ── 上游 origin ─────────────────────────────────────────────────────────

	// ZCodeOrigin 承载 Plan 通道、计费族与 OAuth CLI。
	ZCodeOrigin = "https://zcode.z.ai"
	// ZAIOrigin 承载 API Key 通道与 API Key 兑换链。
	ZAIOrigin = "https://api.z.ai"

	// ── 对话端点 ────────────────────────────────────────────────────────────

	// PathPlanMessages 是 Plan 通道（Bearer JWT）。上游**强制**校验
	// X-Aliyun-Captcha-Verify-Param：没有真实浏览器求解器时该通道不可用，
	// 因此本插件默认走 PathAPIMessages。
	PathPlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"
	// PathAPIMessages 是 API Key 回退通道，免验证码。
	PathAPIMessages = "/api/anthropic/v1/messages"

	// ── OAuth CLI（headless，服务端中转）─────────────────────────────────────
	//
	// 从 origin 起算的完整路径（与 ZCodeOrigin 拼接后使用）。

	PathOAuthInit = "/api/v1/oauth/cli/init"
	PathOAuthPoll = "/api/v1/oauth/cli/poll" // + /{flow_id}

	// ── API Key 兑换链（api.z.ai）───────────────────────────────────────────

	PathZLogin       = "/api/auth/z/login"
	PathCustomerInfo = "/api/biz/customer/getCustomerInfo"

	// ── 计费 / 额度（zcode.z.ai）────────────────────────────────────────────

	// BillingBase 是计费族基准（current/balance/preview/claim）。
	BillingBase = ZCodeOrigin + "/api/v1/zcode-plan"

	PathBillingCurrent = "/billing/current"
	PathBillingBalance = "/billing/balance"
	// PathBillingPreview 列出上游当前投放的套餐（未过滤已持有）。
	PathBillingPreview = "/billing/preview"
	// PathBillingClaim 领取套餐，需要人机验证令牌。
	PathBillingClaim  = "/billing/claim"
	PathUsage         = "/usage"
	PathClientConfigs = "/api/v1/client/configs"

	// ── 客户端指纹 ──────────────────────────────────────────────────────────

	// ClientAppVersion 是官方桌面端现行版本。
	//
	// 上游按客户端版本判定兼容性（版本过旧会被拒），因此跟进官方升版时要
	// 同步改这里。
	ClientAppVersion = "3.14.3"

	// UserAgent 与 ClientAppVersion 同源，避免两者漂移。
	UserAgent = "ZCode/" + ClientAppVersion

	AnthropicVersion = "2023-06-01"
	ClientAgent      = "glm"
	HTTPReferer      = ZCodeOrigin + "/"
	ClientTitle      = "Z Code@electron"
	ReleaseChannel   = "stable"

	// DefaultPlatform/DefaultOSVersion/DefaultLanguage/DefaultTimezone 是
	// **无账号上下文**时的伪装值（模型探测、测试）。有账号时一律取该账号的
	// 设备档案——每账号一台独立设备，避免多账号聚成「同一台机器」。
	DefaultPlatform  = "darwin-arm64"
	DefaultOSVersion = "25.5.0"
	DefaultLanguage  = "zh-CN"
	DefaultTimezone  = "Asia/Shanghai"

	// ── 限制 ────────────────────────────────────────────────────────────────

	// MaxTokensLimit 是上游 max_tokens 的合法上限（超限报 400 code 1210）。
	MaxTokensLimit = 131072

	// DefaultMaxTokens 是请求未指定 max_tokens 时的取值。
	DefaultMaxTokens = 8192

	// ── 验证码头 ────────────────────────────────────────────────────────────

	// CaptchaHeader 是 Plan 通道的验证令牌；REGION 与它成对下发，缺失易 3007。
	CaptchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	CaptchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

// client/configs 拉不到验证码配置时的兜底值（prefix / region / scene_id）。
//
// 与参考实现 zcode2api/app/constants.py 的 CAPTCHA_DEFAULTS 同源：这三个值是
// 官方桌面端现行配置，configs 端点不可用时用它们仍能渲染滑块。
const (
	captchaFallbackPrefix = "no8xfe"
	captchaFallbackRegion = "cn"
	captchaFallbackScene  = "11xygtvd"
)

// 本文件解析 ZCode 的供应商专属配置。

// ConfigFields 声明 zcode 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "zcode_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "上游基地址覆盖。"},
		{Name: "zcode_claim_min_interval", Type: pluginapi.ConfigFieldTypeString, Description: "两次套餐领取之间的最小间隔（Go 时长，如 15s；0 表示不节流）。"},
	}
}
