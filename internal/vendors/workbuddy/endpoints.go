// Package cb 实现腾讯 CodeBuddy（WorkBuddy）的上游协议。
//
// 本包是纯逻辑层：不知道 CPA 宿存在，出站 HTTP 一律经 internal/httpx
// 走宿主的 HTTP 桥（这样宿主的代理、请求日志、账号级代理都生效）。
//
// 层次：
//
//	endpoints.go  域与端点表（cn / global 两套部署）
//	credential.go 凭证解析与写回（双形态兼容）
//	client.go     客户端与统一信封
//	errors.go     错误分类（决定凭证处置语义）
//	headers.go    四类出站请求头 + 会话头族
//	payload.go    请求体改写管线（OpenAI → CodeBuddy）
//	sanitize.go   出站请求体指纹脱敏
//	thinking.go   思维链注入与回填
//	toolpair.go   tool_call 配对重排与孤儿裁剪
//	sse.go        流式透传与聚合
//	catalog.go    模型目录探测
//	efforts.go    reasoning effort 档位表
package workbuddy

import (
	"encoding/json"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/upstream"
	"net/http"
	"regexp"
	"strings"
	"time"

	"fmt"
	"strconv"
)

// Region 是 CodeBuddy 的部署域。
//
// cn 与 global 是**两套独立部署**：域名、模型清单、reasoning 档位表都不同，
// 且**凭证不通用**——用 cn 的 token 打 global 域名会得到 401 TOKEN_EXPIRE。
type Region string

const (
	// RegionCN 是国内版（copilot.tencent.com / www.codebuddy.cn）。
	RegionCN Region = "cn"
	// RegionGlobal 是国际版（www.workbuddy.ai）。
	RegionGlobal Region = "global"
)

// NormalizeRegion 把字符串归一化为合法 Region，未知值回落 cn。
func NormalizeRegion(raw string) Region {
	if strings.EqualFold(strings.TrimSpace(raw), string(RegionGlobal)) {
		return RegionGlobal
	}
	return RegionCN
}

// IsGlobal 报告是否为国际版。
func (r Region) IsGlobal() bool { return r == RegionGlobal }

// String 实现 fmt.Stringer。
func (r Region) String() string { return string(r) }

// 供应商实例标识。同时是凭证文件名前缀（workbuddycn-<uid>.json）。
//
// CN 与 GLOBAL 在本插件里是两个独立供应商：域名、模型清单、reasoning 档位表、
// 签到活动都不同，且凭证不通用。因此各自有独立的 ID 与文件名前缀，
// 让账号、模型、认证文件都能按供应商区分开。
const (
	// VendorIDCN 是国内版供应商标识。
	VendorIDCN = "workbuddycn"
	// VendorIDGlobal 是国际版供应商标识。
	VendorIDGlobal = "workbuddyglobal"
)

// VendorIDFor 返回该区域对应的供应商标识。
func VendorIDFor(region Region) string {
	if region.IsGlobal() {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor 返回该区域的展示名（供管理端与页面使用）。
func VendorNameFor(region Region) string {
	if region.IsGlobal() {
		return "WorkBuddy 国际版"
	}
	return "WorkBuddy 国内版"
}

// Endpoints 是某个域的全部上游端点。
//
// 所有 URL 集中成一张表、按 domain 取：散落在各处拼 URL 是维护灾难，
// 而且域切换（cn/global）必须成组发生，不能只换一半。
type Endpoints struct {
	// ChatBase 是对话域基址（/v2/chat/completions 在此之下）。
	ChatBase string
	// BillingBase 是计费与埋点域基址（/v2/report、/billing/meter/*）。
	BillingBase string
	// WebBase 是 Web 成长中心域基址（任务领奖 /activity/growth/tasks/<code>/claim）。
	WebBase string
	// Origin 是出站请求的 Origin / Referer 基址。
	Origin string
	// AcceptLanguage 是该域的默认语言偏好。
	AcceptLanguage string
	// PlatformLabel 是 UA 中的平台段（global 必须是 "WorkBuddy AI"，否则触发风控）。
	PlatformLabel string
}

var (
	endpointsCN = Endpoints{
		ChatBase:       "https://copilot.tencent.com",
		BillingBase:    "https://www.codebuddy.cn",
		WebBase:        "https://www.workbuddy.cn",
		Origin:         "https://www.codebuddy.cn",
		AcceptLanguage: "zh-CN",
		PlatformLabel:  "WorkBuddy",
	}
	endpointsGlobal = Endpoints{
		ChatBase:       "https://www.workbuddy.ai",
		BillingBase:    "https://www.workbuddy.ai",
		WebBase:        "https://www.workbuddy.ai",
		Origin:         "https://www.workbuddy.ai",
		AcceptLanguage: "en-US",
		PlatformLabel:  "WorkBuddy AI",
	}
)

// GetEndpoints 返回该域对应的端点表。
func GetEndpoints(region Region) Endpoints {
	if region.IsGlobal() {
		return endpointsGlobal
	}
	return endpointsCN
}

// 对话域路径。
const (
	// chatCompletionsPath 是唯一的对话出口。
	//
	// 历史：global 曾先打 /console/chat/completions，因 /console 挂腾讯云 WAF
	// 的 body 内容规则（反引号 + printf/whoami 等命令执行特征确定性 403）而改为固定 /v2。
	chatCompletionsPath = "/v2/chat/completions"
	// tokenRefreshPath 是凭证刷新端点（X-Refresh-Token 只允许出现在这里）。
	tokenRefreshPath = "/v2/plugin/auth/token/refresh"
	// v3ConfigPath 是官方 IDE 配置目录（模型能力的权威来源，UA 敏感）。
	v3ConfigPath = "/v3/config"
	// enterpriseModelsPathCN 是企业模型目录（CN 补缺路）。
	enterpriseModelsPathCN = "/console/enterprises/personal/models"
	// enterpriseModelsPathGlobal 是国际版企业模型目录首选路径。
	enterpriseModelsPathGlobal = "/v2/enterprises/personal/models"
)

// 计费域路径。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath   = "/billing/meter/daily-checkin"
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
	claimGiftPath      = "/billing/meter/claim-gift"
	claimCompPath      = "/billing/meter/claim-compensation"
	reportPath         = "/v2/report"
	trialPath          = "/billing/ide/trial"
)

// 成长 / 任务域路径（挂在 ChatBase 之下）。
const (
	growthTasksPath        = "/v2/activity/growth/tasks"
	growthTasksAcceptPath  = "/v2/activity/growth/tasks/accept"
	growthTasksClaimBase   = "/activity/growth/tasks"
	growthStreakPath       = "/activity/growth/streak"
	growthRedeemPath       = "/activity/growth/redeem"
	growthLotterySummary   = "/activity/growth/lottery/summary"
	growthLotteryDraw      = "/activity/growth/lottery/draw"
	growthHeatmapPath      = "/activity/growth/heatmap"
	growthMakeupCardsPath  = "/activity/growth/makeup-cards/use"
	buddyInfoPath          = "/activity/growth/buddy/info"
	buddyFirstPath         = "/activity/growth/buddy/first"
	buddyAgreementPath     = "/activity/growth/buddy/agreement"
	buddyTravelStatusPath  = "/activity/growth/buddy/travel/status"
	buddyTravelDepartPath  = "/activity/growth/buddy/travel/depart"
	buddyTravelClaimPath   = "/activity/growth/buddy/travel/claim"
	appearanceSetPath      = "/v2/user-asset/appearance/set"
	expertMarketListPath   = "/portal/operation-platform/market/expert/list"
	schoolBasePath         = "/portal/activity/school"
	globalCountryCodePath  = "/billing/area/get-country-code"
	globalSubmitRegionPath = "/console/login/account"
	globalRegisterPath     = "/auth/realms/copilot/overseas/user/register"
)

// 出站标识内置值。都允许被配置覆盖。
const (
	defaultClientVersion = "5.5.4"
	defaultCLIVersion    = "2.137.1"
	defaultClientName    = "WorkBuddy"
	// DesktopClientVersion 是桌面指纹族使用的版本（与 CLI 族刻意不同）。
	// 导出供 tasks 子包构造桌面指纹事件使用。
	DesktopClientVersion = "5.5.6"
	// codeBuddyIDEUA / codeBuddyCLIUA 是模型目录探测专用 UA。
	//
	// /v3/config 对 UA 敏感：过旧会拿到精简目录或 400 code=12403。
	// 实测两路各有独有模型（IDE 含 o4-mini/enhance-1.0，CLI 含 deepseek 系），缺一不可。
	codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	// chromeUA 是 web 指纹族使用的浏览器 UA。
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// 以下常量对外导出，供 package main 的配置默认值使用。
const (
	// DefaultClientVersion 是 UA 中 WorkBuddy 段的默认版本。
	DefaultClientVersion = defaultClientVersion
	// DefaultCLIVersion 是 UA 中 CLI 段的默认版本。
	DefaultCLIVersion = defaultCLIVersion
	// DefaultClientName 是归属头的默认取值。
	DefaultClientName = defaultClientName
)

// 本文件构造四类出站请求头。
//
// 为什么值得单独成文件：上游风控对请求头极其敏感——UA 的平台段写错、
// 少了 X-CodeBuddy-Request、会话头族不全会得到 403 code=11140 "request illegal"，
// 且报错信息完全不指向真正的原因。集中管理是唯一可靠的做法。

// ChatMeta 是与单次对话关联的会话头族。
//
// 由调用方在**轮转循环之外**生成一次，循环内复用：上游后台按
// X-Conversation-Request-ID 聚合同一轮请求，每次重试都换 ID 会被拆成多次会话。
type ChatMeta struct {
	// ConversationID 是多轮会话标识（可为空，为空时不发该头）。
	ConversationID string
	// ConversationRequestID 是轮级聚合主键（必发）。
	ConversationRequestID string
	// MessageID 是单条消息标识（每条独立）。
	MessageID string
	// TraceID 是入站透传的链路 ID（为空时回落 ConversationRequestID）。
	TraceID string
}

// applyCommonHeaders 写入所有 API 共享的头。
func applyCommonHeaders(req *http.Request, c *Client, cred *Credential) {
	endpoints := GetEndpoints(cred.Realm())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", c.userAgent(cred.Realm()))
	req.Header.Set("Accept-Language", endpoints.AcceptLanguage)
	// 这个头是上游识别"来自官方客户端"的基础判据之一，缺失会被风控拦。
	req.Header.Set("X-CodeBuddy-Request", "1")

	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-Machine-ID", deriveID(uid, "machine"))
		req.Header.Set("X-Session-ID", deriveID(uid, "session"))
	}
}

// applyAuthHeaders 写入凭证相关的头。
//
// 凭证缺失时不发空 Bearer（那会被当成"提供了无效凭证"），
// 而是发上游约定的 X-No-* 标记，让它给出更准确的错误。
func applyAuthHeaders(req *http.Request, cred *Credential) {
	if token := cred.AccessTokenValue(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
}

// userAgent 构造三段式 UA。
//
// 平台段在 global 域**必须**是 "WorkBuddy AI"：送成 "WorkBuddy" 会触发
// 上游 403 code=11140 "request illegal"。官方客户端不做任何 UA 随机化，
// 因此这里也保持确定性（随机化反而更容易被风控标记）。
func (c *Client) userAgent(region Region) string {
	if override := strings.TrimSpace(c.opts.UserAgent); override != "" {
		return override
	}
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	cliVersion := c.opts.CLIVersion
	if cliVersion == "" {
		cliVersion = defaultCLIVersion
	}
	platform := GetEndpoints(region).PlatformLabel
	return "WorkBuddy/" + clientVersion + " " + platform + "/" + clientVersion + " CLI/" + cliVersion
}

// applyChatHeaders 写入对话请求专有的头。
func (c *Client) applyChatHeaders(req *http.Request, cred *Credential, meta ChatMeta, clientIP string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/event-stream")

	// 企业与域信息：CN 与 global 的形态不同，写错会被拒。
	if cred.Realm().IsGlobal() {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(RegionGlobal).ChatBase, "/"))
	} else {
		if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if domain := cred.DomainValue(); domain != "" {
			req.Header.Set("X-Domain", domain)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}

	applyAttributionHeaders(req, c)
	applyConversationHeaders(req, meta)

	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
	if c.opts.PassthroughIP && strings.TrimSpace(clientIP) != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Client-IP", clientIP)
	}
}

// applyAttributionHeaders 写入客户端归属头。
//
// 默认四个头一起给，把请求标识成 WorkBuddy 客户端。
// client_name 显式配成 "SaaS" 时还原旧行为（只发 X-Product），保留这个开关
// 是为了在归属头引起风控变化时能快速回退。
func applyAttributionHeaders(req *http.Request, c *Client) {
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	name := strings.TrimSpace(c.opts.ClientName)
	if name == "" {
		name = defaultClientName
	}
	if strings.EqualFold(name, "SaaS") {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", name)
}

// applyConversationHeaders 写入会话头族。
//
// 这一族头看似冗余（多个头取同一个值），但上游后台按它们做请求聚合与链路追踪，
// 缺任意一个都会让会话在后台被拆散。
func applyConversationHeaders(req *http.Request, meta ChatMeta) {
	conversationRequestID := strings.TrimSpace(meta.ConversationRequestID)
	messageID := strings.TrimSpace(meta.MessageID)
	if messageID == "" {
		messageID = newHexID()
	}
	if conversationRequestID == "" {
		conversationRequestID = messageID
	}
	if conversationID := strings.TrimSpace(meta.ConversationID); conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", conversationRequestID)

	traceID := strings.TrimSpace(meta.TraceID)
	if !validTraceID(traceID) {
		traceID = conversationRequestID
	}
	req.Header.Set("X-Trace-ID", traceID)

	b3TraceID := conversationRequestID
	if !validTraceID(b3TraceID) {
		b3TraceID = messageID
	}
	req.Header.Set("X-B3-TraceId", b3TraceID)
	req.Header.Set("X-B3-SpanId", spanFromMessageID(messageID))
	req.Header.Set("X-B3-Sampled", "1")
}

// spanFromMessageID 从消息 ID 截出 16 位作为 B3 span id。
func spanFromMessageID(messageID string) string {
	if len(messageID) >= 16 {
		return messageID[:16]
	}
	return messageID
}

// validTraceID 校验 trace id：必须是 16 或 32 位 hex（大小写均可）。
//
// 上游对非法 trace id 会直接拒绝，因此入站透传的值必须先校验再使用。
func validTraceID(value string) bool {
	if len(value) != 16 && len(value) != 32 {
		return false
	}
	for _, char := range value {
		isDigit := char >= '0' && char <= '9'
		isLower := char >= 'a' && char <= 'f'
		isUpper := char >= 'A' && char <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

// applyBillingHeaders 写入计费域与成长域请求的头。
//
// 与对话头的差别：没有会话头族与归属四头，UA 是单段 "WorkBuddy/<version>"。
func (c *Client) applyBillingHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// 计费域用单段 UA；显式配 SaaS 时不设 UA（旧行为）。
	if name := strings.TrimSpace(c.opts.ClientName); !strings.EqualFold(name, "SaaS") && strings.TrimSpace(c.opts.UserAgent) == "" {
		clientVersion := c.opts.ClientVersion
		if clientVersion == "" {
			clientVersion = defaultClientVersion
		}
		req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion)
	}

	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
}

// applyMPHeaders 在计费头之上叠加小程序指纹头。
//
// 小程序口径的任务列表 / accept / claim 全链路都必须带这组头，
// 缺头时上游会把请求当成默认口径：列表不下发 mp 任务、accept 报 task not found。
func applyMPHeaders(req *http.Request) {
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
}

// applyDesktopHeaders 写入桌面指纹族请求的头。
//
// 桌面族用于成长任务的埋点上报：上游按 UA 与 X-Domain 把事件归属到
// "桌面客户端"，与 CLI 族的事件分开计分。
func (c *Client) applyDesktopHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", requestIDFor(cred))
}

// desktopUA 返回桌面指纹族的 UA。
func desktopUA() string {
	return "WorkBuddy/" + DesktopClientVersion + " WorkBuddy/" + DesktopClientVersion + " CLI/" + defaultCLIVersion
}

// requestIDFor 生成一个带纳秒后缀的请求 ID（桌面族要求每次不同）。
func requestIDFor(cred *Credential) string {
	return deriveID(cred.UIDValue(), "req") + newHexID()[:6]
}

// applyWebHeaders 写入 web 指纹族请求的头（浏览器形态）。
func applyWebHeaders(req *http.Request, cred *Credential, pageURL string) {
	endpoints := GetEndpoints(RegionCN)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", endpoints.WebBase)
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("x-client-platform", "web")
	applyAuthHeaders(req, cred)
	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
}

// applyV3ConfigHeaders 写入 /v3/config 专有的头。
//
// UA 是这里的风控闸门：过旧会拿到精简目录，或直接 400 code=12403。
func (c *Client) applyV3ConfigHeaders(req *http.Request, cred *Credential, ua string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Domain", v3ConfigDomain(cred))
}

// v3ConfigDomain 决定 /v3/config 的 X-Domain 取值。
func v3ConfigDomain(cred *Credential) string {
	domain := strings.TrimSpace(cred.DomainValue())
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")
	if domain != "" {
		return domain
	}
	base := GetEndpoints(cred.Realm()).ChatBase
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	return strings.TrimSuffix(base, "/")
}

// ExtractClientIP 从入站请求头里取客户端 IP（X-Forwarded-For 首段，回落 X-Real-IP）。
//
// 只在 PassthroughIP 开启时使用；透传客户端 IP 会给上游提供额外的关联信息，
// 因此默认关闭。
func ExtractClientIP(header http.Header) string { return core.ExtractClientIP(header) }

// 本文件实现上游错误分类。
//
// 为什么需要它：上游用 HTTP 状态码 + 业务 code + 自然语言消息三种手段混合表达错误，
// 而同一种 HTTP 状态码可能对应完全不同的处置（例如 429 既可能是限流、也可能是余额耗尽）。
// 分类结果决定：是冷却这个账号、禁用这个账号、只避让这个模型，还是根本不罚账号。
//
// 判定顺序是有语义的，不能重排（见 Classify 的注释）。

// Kind 是错误类别。
type Kind = upstream.Kind

const (
	// KindNone 表示成功。
	KindNone Kind = upstream.KindNone
	// KindHardCredit 余额耗尽（402 或余额关键词）→ 长冷却到次日。
	KindHardCredit = upstream.KindHardCredit
	// KindSoftRate 限流（429）→ 短冷却，指数退避。
	KindSoftRate = upstream.KindSoftRate
	// KindSessionDead 会话失效（12153）→ 禁用，需重新登录。
	KindSessionDead = upstream.KindSessionDead
	// KindNotFound 上游偶发 404 → 固定短冷却，不累计熔断。
	KindNotFound = upstream.KindNotFound
	// KindServer 上游 5xx → 计入熔断。
	KindServer = upstream.KindServer
	// KindContentBlocked 内容策略拦截 → 不罚账号，可降级重试。
	KindContentBlocked = upstream.KindContentBlocked
	// KindBadParams 参数非法 → 不罚账号，但仍换号重试。
	KindBadParams = upstream.KindBadParams
	// KindAccountFault 账号级故障（request illegal / trial 未激活）→ 冷却轮换。
	KindAccountFault = upstream.KindAccountFault
	// KindModelBlocked 该后端无此模型（11102）→ 按（账号, 模型）避让。
	KindModelBlocked = upstream.KindModelBlocked
	// KindWAFBlock 上游 WAF 拦截（403 非业务信封）→ 账号软冷却。
	KindWAFBlock = upstream.KindWAFBlock
	// KindPromptTooLong 上下文超限（11115）→ 请求级，不罚号不轮转。
	KindPromptTooLong = upstream.KindPromptTooLong
	// KindImageInvalid 图片无效 → 请求级，不罚号不轮转。
	KindImageInvalid = upstream.KindImageInvalid
	// KindClient 其他 4xx / 业务错误。
	KindClient = upstream.KindClient
)

// Error 是带分类的上游错误。
type Error = upstream.Error

// 关键词表。
//
// 全部小写比对，中文与英文双通道：上游对不同租户/区域返回的文案不一致。
var (
	hardMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	softRateMarkers = []string{
		"rate limit", "rate-limiting", "rate-limited", "too many requests", "too many",
		"usage limit", "请求过于频繁", "限流",
	}
	sessionDeadMarkers  = []string{"offline user session not found", "12153"}
	accountFaultMarkers = []string{
		"request illegal", "trial not activated", "trial version is not yet activated",
	}
	contentBlockedMarkers = []string{
		"blocked by security policy", "unapproved channel", "illegal api invocation",
	}
	invalidImageMarkers = []string{
		"invalid image_url content", "invalid_image_data", "replace the image",
	}
	promptTooLongMarkers = []string{
		`"code":11115`, `"code": 11115`, `"code":"11115"`, "prompt is too long",
	}
	alreadyCheckinMarkers = []string{"已签到", "already"}
)

const (
	badParamsMarkerMsg  = "unmarshal chat params failed"
	badParamsMarkerCode = `"code":11101`
	// modelRateLimitCode 是「模型级」限流：只避让该模型，不惩罚整个账号。
	modelRateLimitCode = "6004"
	// modelBlockCode 是「该后端无此模型」。
	modelBlockCode = "11102"

	// ModelBlockReason 是（账号, 模型）避让的固定原因串。
	ModelBlockReason = "11102 model not available"
	// ModelRateLimitReason 是模型级限流的固定原因串。
	ModelRateLimitReason = "6004 model rate limited"
)

var (
	modelRateLimitRe = regexp.MustCompile(`"code"\s*:\s*"?6004"?`)
	cnResetRe        = regexp.MustCompile(`将在 (.+?) 重置`)
	enResetRe        = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	// softRateResetLoc 是上游重置时刻使用的时区（固定 UTC+8，不依赖系统 tzdata）。
	softRateResetLoc    = time.FixedZone("UTC+8", 8*3600)
	softRateTimeLayout  = "2006-01-02 15:04:05"
	retryAfterSanityCap = 2 * time.Hour
)

// Classify 按状态码与响应体判定错误类别。
//
// 判定顺序是关键，尤其这几条：
//   - 11102 必须最先：它可能是 400 也可能是 404，晚判会被 404 分支吞掉；
//   - 429 必须早于 hardMarkers：429 的正文高频携带 "quota exceeded"/"额度不足"
//     这类跨计费与限流两界的措辞，hardMarkers 先判会把限流误归余额耗尽，
//     白扔一个号约 12 小时。真正的余额耗尽由 402 或业务码 14018 捕获；
//   - 11115（上下文超限）必须早于 404/5xx/WAF/内容策略：它是请求级错误，
//     罚账号是错的；
//   - accountFault 必须早于 429：账号被封的措辞里也可能带限流字样。
func Classify(status int, body string) *Error {
	lower := strings.ToLower(body)

	// 0) 模型级避让：只比对独立字段，绝不做整段文本子串匹配——
	// 否则 "11102" 撞在 requestId 上会误避让一个可用模型。
	if isModelBlocked(status, body) {
		return &Error{Kind: KindModelBlocked, Status: status, Msg: body}
	}
	// 1) 余额耗尽（明确的 402）。
	if status == http.StatusPaymentRequired {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 2) 会话失效。
	if containsAny(lower, sessionDeadMarkers) {
		return &Error{Kind: KindSessionDead, Status: status, Msg: body}
	}
	// 3) 账号级故障（注意：不能按 11140 判定——该 code 也承载模型级限流文案）。
	if containsAny(lower, accountFaultMarkers) {
		return &Error{Kind: KindAccountFault, Status: status, Msg: body}
	}
	// 4) 429 + 业务码 14018 表示余额真的耗尽。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 5) 普通限流。
	if status == http.StatusTooManyRequests {
		return &Error{Kind: KindSoftRate, Status: status, Msg: body}
	}
	// 6) 余额关键词（在 429 之后）。
	if containsAny(lower, hardMarkers) {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 7) 限流关键词。
	if containsAny(lower, softRateMarkers) {
		return &Error{Kind: KindSoftRate, Status: status, Msg: body}
	}
	// 8) 上下文超限（请求级）。
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge) &&
		containsAny(body, promptTooLongMarkers) {
		return &Error{Kind: KindPromptTooLong, Status: status, Msg: body}
	}
	// 9) 偶发 404。
	if status == http.StatusNotFound {
		return &Error{Kind: KindNotFound, Status: status, Msg: body}
	}
	// 10) 上游故障。
	if status >= 500 {
		return &Error{Kind: KindServer, Status: status, Msg: body}
	}
	// 11) WAF 拦截：403 且没有业务信封（说明不是上游业务返回的）。
	if IsWAFBlocked(status, body) {
		return &Error{Kind: KindWAFBlock, Status: status, Msg: body}
	}
	// 12) 图片无效。
	if status == http.StatusBadRequest && (codeMarker(lower, "11135") || containsAny(lower, invalidImageMarkers)) {
		return &Error{Kind: KindImageInvalid, Status: status, Msg: body}
	}
	// 13) 内容策略拦截。
	if status >= 400 && containsAny(lower, contentBlockedMarkers) {
		return &Error{Kind: KindContentBlocked, Status: status, Msg: body}
	}
	// 14) 参数非法。
	if status >= 400 && (strings.Contains(lower, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode)) {
		return &Error{Kind: KindBadParams, Status: status, Msg: body}
	}
	// 15) 其他 4xx。
	if status >= 400 {
		return &Error{Kind: KindClient, Status: status, Msg: body}
	}
	return &Error{Kind: KindNone, Status: status, Msg: body}
}

// isModelBlocked 判断是否为「该后端无此模型」（11102）。
//
// 只比对**独立字段**（code / errCode / error_code 与 msg / message），
// 遍历顶层与 error 子对象两层，绝不做整段文本子串匹配。
func isModelBlocked(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(body, modelBlockMsgMarker) {
		// 轻量预检短路：连标志串都没有就不用解析 JSON 了。
		return false
	}
	var fields map[string]any
	if errUnmarshal := json.Unmarshal([]byte(body), &fields); errUnmarshal != nil {
		return false
	}
	if objectHasModelBlock(fields) {
		return true
	}
	if nested, okNested := fields["error"].(map[string]any); okNested {
		return objectHasModelBlock(nested)
	}
	return false
}

const modelBlockMsgMarker = "service info not found"

// objectHasModelBlock 检查一个 JSON 对象里的 code/msg 字段是否表示模型不可用。
func objectHasModelBlock(fields map[string]any) bool {
	for _, key := range []string{"code", "errCode", "error_code"} {
		if value, okValue := fields[key]; okValue {
			if fmt.Sprintf("%v", value) == modelBlockCode {
				return true
			}
		}
	}
	for _, key := range []string{"msg", "message"} {
		if value, okValue := fields[key].(string); okValue &&
			strings.Contains(strings.ToLower(value), modelBlockMsgMarker) {
			return true
		}
	}
	return false
}

// IsWAFBlocked 判断是否为 WAF 拦截。
//
// 判据：403 且正文里没有业务信封标记（"code": 或 "msg":）。
// 业务错误即使带 403 也会有这两个键。
func IsWAFBlocked(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	return !strings.Contains(body, `"code":`) && !strings.Contains(body, `"msg":`)
}

// IsModelRateLimit 判断响应体是否表示模型级限流（6004）。
func IsModelRateLimit(body string) bool {
	return modelRateLimitRe.MatchString(body)
}

// IsAlreadyCheckin 判断错误是否为「今天已签到」。
//
// 签到接口对重复签到返回业务错误而非成功，调用方需要把它当成幂等成功处理。
func IsAlreadyCheckin(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range alreadyCheckinMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// ParseRateReset 从错误正文里解析上游给出的重置时刻。
//
// 中文文案优先（CN 域更常见），回落英文格式。都取不到返回零值。
func ParseRateReset(body string) time.Time {
	if match := cnResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	if match := enResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ParseRetryAfter 从响应头里解析上游建议的等待时长。
//
// 候选头与语义：Retry-After（秒）、Retry-After-Ms（毫秒）、
// X-Ratelimit-Reset（epoch 秒，≥12 位按毫秒解释）。
// HTTP-Date 形态与超过 2 小时的值一律忽略（上游偶发返回离谱值）。
func ParseRetryAfter(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return 0
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, errAtoi := parseInt64(raw); errAtoi == nil && seconds > 0 {
			return clampRetryAfter(time.Duration(seconds) * time.Second)
		}
	}
	if raw := strings.TrimSpace(header.Get("Retry-After-Ms")); raw != "" {
		if millis, errAtoi := parseInt64(raw); errAtoi == nil && millis > 0 {
			return clampRetryAfter(time.Duration(millis) * time.Millisecond)
		}
	}
	if raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset")); raw != "" {
		if value, errAtoi := parseInt64(raw); errAtoi == nil && value > 0 {
			if len(raw) >= 12 {
				value /= 1000
			}
			resetAt := time.Unix(value, 0)
			if resetAt.After(now) {
				return clampRetryAfter(resetAt.Sub(now))
			}
		}
	}
	return 0
}

// clampRetryAfter 把等待时长钳到合理区间。
func clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > retryAfterSanityCap {
		return retryAfterSanityCap
	}
	return d
}

// parseInt64 解析十进制整数（上游的重试头有时带小数，这里只接受整数）。
func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

// containsAny 报告 text 是否包含任一 marker（text 应已小写）。
func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// codeMarker 判断正文里是否出现某个裸业务码（覆盖多种 JSON 空白/引号形态）。
func codeMarker(lower, code string) bool {
	if strings.Contains(lower, `"code":`+code) ||
		strings.Contains(lower, `"code": `+code) ||
		strings.Contains(lower, `"code":"`+code+`"`) ||
		strings.Contains(lower, `"code": "`+code+`"`) {
		return true
	}
	return strings.Contains(lower, `code=`+code)
}

// hasBusinessCode 递归查找正文里是否有 code 字段精确等于 want。
func hasBusinessCode(body, want string) bool {
	var payload any
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil {
		return false
	}
	return scanBusinessCode(payload, want)
}

func scanBusinessCode(value any, want string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if code, okCode := typed["code"]; okCode {
			if fmt.Sprintf("%v", code) == want {
				return true
			}
		}
		for _, nested := range typed {
			if scanBusinessCode(nested, want) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if scanBusinessCode(item, want) {
				return true
			}
		}
	}
	return false
}
