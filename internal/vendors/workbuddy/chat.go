package workbuddy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件是 CodeBuddy 协议的客户端核心：请求发送、统一信封处理、凭证刷新、
// 设备指纹派生与出站 HTTP 客户端构造。
//
// 所有出站请求都经宿主 HTTP 桥（internal/hostbridge），因此宿主的代理、
// 账号级代理与请求日志对这些请求同样生效。

const (
	// shortTimeout 是短 RPC（刷新、签到、余额、模型清单、任务上报）的总时长上限。
	shortTimeout = 120 * time.Second
	// chatHeaderTimeout 是对话首字节前的等待上限。
	chatHeaderTimeout = 120 * time.Second
	// refreshTimeout 是凭证刷新请求的上限。
	refreshTimeout = 30 * time.Second
	// apiBodyLimit 是短 RPC 响应体的读取上限。
	apiBodyLimit = 1 << 20
	// configBodyLimit 是 /v3/config 的读取上限（响应体明显更大）。
	configBodyLimit = 2 << 20

	// deriveSaltPrefix 是设备指纹派生的固定盐前缀。
	//
	// 固定前缀保证指纹跨重启稳定（换了就是新设备）；真正的部署隔离
	// 由 installSalt（机器盐）提供：不同部署的指纹空间互不相同。
	deriveSaltPrefix = "wb2a:"

	// authRefreshSource 是刷新请求的 X-Auth-Refresh-Source 取值。
	authRefreshSource = "plugin"

	// maxRefreshTokenExpiresIn 是刷新响应里 expiresIn 的合理上限。
	// 超过它视为脏数据，保留原有的 expiresAt 而不是写成一个离谱的未来时刻。
	maxRefreshTokenExpiresIn = int64(10 * 365 * 24 * 60 * 60)

	// deviceTokenFileTTL 是 device token 文件的读取缓存时长。
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen 是 device token 文件的长度上限（超过视为异常）。
	deviceTokenFileMaxLen = 1024
)

// Options 是构造 Client 所需的全部输入。
//
// 刻意用一个结构体而不是长参数列表：字段会随上游变化持续增加，
// 位置参数很快就会不可读。
type Options struct {
	// Context 绑定宿主回调 ID，使出站请求进宿主的请求日志。
	Context context.Context
	// EnabledRealms 是启用的域（cn / global 子集）。
	EnabledRealms []string
	// PromptMode / PromptText 控制系统提示词处理。
	PromptMode string
	PromptText string
	// OnContentBlocked 是内容审核拦截时的回调（触发提示词降级）。
	OnContentBlocked func()
	// SanitizeFingerprints 开启出站请求体黑名单脱敏。
	SanitizeFingerprints bool
	// PassthroughIP 决定是否把客户端 IP 透传给上游。
	PassthroughIP bool
	// UserAgent 显式覆盖出站 UA。
	UserAgent string
	// ClientVersion / CLIVersion 覆盖 UA 的两段版本号。
	ClientVersion string
	CLIVersion    string
	// ClientName 覆盖归属头取值。
	ClientName string
	// DeviceToken / DeviceTokenFile 是设备令牌的两级兜底。
	DeviceToken     string
	DeviceTokenFile string
	// StateDir 用于存放模型清单缓存。
	StateDir string
}

// Client 是 CodeBuddy 上游客户端。
//
// 每个请求构造一个实例：它很轻（只是配置快照 + 少许缓存），
// 但必须绑定请求级 context 才能让出站流量进宿主的请求日志。
type Client struct {
	rt   *runtime
	opts Options
}

// NewClient 构造一个上游客户端。
func NewClient(rt *runtime, opts Options) *Client {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	return &Client{rt: rt, opts: opts}
}

// enabledRealm 报告某个域是否被配置启用。
//
// 关闭的域上的账号一律不发起上游请求：判断放在客户端层而不是散落各处，
// 保证「关掉 global」是真的一个请求都不发。
func (c *Client) enabledRealm(region Region) bool {
	if len(c.opts.EnabledRealms) == 0 {
		return true
	}
	for _, item := range c.opts.EnabledRealms {
		if item == string(region) {
			return true
		}
	}
	return false
}

// 三档出站 HTTP 客户端。

// shortClient 返回短 RPC 客户端。
func (c *Client) shortClient() *http.Client {
	return hostbridge.Client(c.opts.Context, shortTimeout)
}

// refreshClient 返回凭证刷新客户端。
func (c *Client) refreshClient() *http.Client {
	return hostbridge.Client(c.opts.Context, refreshTimeout)
}

// chatClient 返回对话客户端。
//
// 总时长设为 0：流式对话的时长不可预期，首字节与空闲分别由
// headerTimeout 与流内空闲监控负责。用总超时会误杀长回答。
func (c *Client) chatClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = chatHeaderTimeout
	}
	// 宿主桥不支持 ResponseHeaderTimeout（由宿主侧决定），
	// 这里用 context 超时表达首字节上限：httpx 的 transport 会把
	// 请求 context 传给 host.http.do_stream。
	return hostbridge.StreamClient(c.opts.Context, 0)
}

// apiEnvelope 是上游的统一响应信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发起一次短 RPC 并解开统一信封。
//
// 错误处理：HTTP ≥400 走 Classify；HTTP 成功但业务 code 非 0 也走 Classify
// （把 msg 当正文）。返回的是信封里的 data 部分。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		// 半截 body 不进 Classify：网络问题不该被当成账号问题罚号。
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// doJSONWithLimit 与 doJSON 相同，但使用更大的读取上限（/v3/config 需要）。
func (c *Client) doJSONWithLimit(req *http.Request, limit int64) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, limit))
	if errRead != nil {
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// unwrapEnvelope 把「状态码 + 正文」解成 data 或分类错误。
func unwrapEnvelope(status int, body string, header http.Header) (json.RawMessage, error) {
	if status >= 400 {
		return nil, enrichClassifyError(status, body, header)
	}
	var envelope apiEnvelope
	if errUnmarshal := json.Unmarshal([]byte(body), &envelope); errUnmarshal != nil {
		// 响应不是 JSON：说明上游返回了非预期内容（HTML 错误页等）。
		return nil, &core.Failure{Kind: core.FailureKindClient, Status: status, Msg: core.Truncate(body, 512)}
	}
	if envelope.Code != 0 {
		return nil, enrichClassifyError(status, body, header)
	}
	return envelope.Data, nil
}

// enrichClassifyError 在分类结果上补齐上游给出的等待信息。
//
// 上游有两种表达等待的方式：响应头（Retry-After 系列）与正文文案（"将在 X 重置"）。
// 两者都解析出来交给调用方，由它决定冷却到哪个时刻。
func enrichClassifyError(status int, body string, header http.Header) *core.Failure {
	classified := Classify(status, body)
	classified.RetryAfter = ParseRetryAfter(header, time.Now())
	classified.ResetAt = ParseRateReset(body)
	return classified
}

// refreshTokenResponse 是刷新端点的响应。
type refreshTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// DeviceToken 按「账号凭证 > 插件配置 > 文件」的优先级解析设备令牌。
func (c *Client) DeviceToken(cred *Credential) string {
	if token := cred.DeviceTokenValue(); token != "" {
		return token
	}
	if token := strings.TrimSpace(c.opts.DeviceToken); token != "" {
		return token
	}
	return c.rt.readDeviceTokenFile(c.opts.DeviceTokenFile)
}

// deriveIDWithSalt 派生一个跨重启稳定、账号间互异的标识。
func deriveIDWithSalt(uid, purpose, salt string) string {
	seed := deriveSaltPrefix + purpose + ":" + uid + ":" + salt
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:18])
}

// buildCacheKey 构造 prompt_cache_key。
//
// 这是单项最大的费用优化：同一段长前缀命中缓存后费用可降一个数量级。
// uid 前 8 位是**硬隔离因子**——跨账号复用同一 cache key 会让上游
// 命中错账号的前缀缓存，泄露对方对话内容。
func buildCacheKey(uid, conversation string) string {
	uidPrefix := uid
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	if uidPrefix == "" {
		uidPrefix = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uidPrefix + "-" + hex.EncodeToString(sum[:16])
}

// closeBody 关闭响应体并忽略错误（响应体关闭失败不影响业务结果）。
func closeBody(body io.Closer) {
	if body != nil {
		_ = body.Close()
	}
}

// 本文件实现对话调用：请求构造、出站、错误分类与流式读取。

// ChatRequest 是一次对话调用的输入。
type ChatRequest struct {
	// Credential 是被选中的账号凭证。
	Credential *Credential
	// Body 是客户端原始请求体（OpenAI 形态）。
	Body []byte
	// Model 是上游模型名（已剥离 realm 前缀）。
	Model string
	// ClientIP 是客户端 IP（仅在 PassthroughIP 开启时才会上送）。
	ClientIP string
	// Meta 是会话头族。
	Meta ChatMeta
}

// ChatResult 是一次对话调用的结果。
type ChatResult struct {
	// Response 是非流式聚合后的响应体（流式调用时为空）。
	Response []byte
	// Stats 是流式统计（非流式调用时为零值）。
	Stats StreamStats
	// Credential 是实际使用的凭证（可能被刷新过）。
	Credential *Credential
}

// ChatStream 发起一次流式对话，返回可供读取的响应体。
//
// 调用方负责关闭返回的 ReadCloser。上游强制流式（请求体里的 stream 恒为 true），
// 因此这是唯一的对话出口：非流式需求由 Aggregate 在本侧聚合。
func (c *Client) ChatStream(req ChatRequest) (io.ReadCloser, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := req.Credential.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// 改写管线：加 stream、归一字段、注入思维链、脱敏。
	conversationKey := ConversationKey(req.Body)
	cacheKey := buildCacheKey(req.Credential.UIDValue(), core.FirstNonEmpty(conversationKey, req.Meta.ConversationRequestID))
	defaultEfforts, supportedEfforts := c.rt.effortTables()
	prepared := PrepareBody(req.Body, PrepareOptions{
		Model:            model,
		PromptMode:       c.opts.PromptMode,
		PromptText:       c.opts.PromptText,
		Sanitize:         c.opts.SanitizeFingerprints,
		DefaultEfforts:   defaultEfforts,
		SupportedEfforts: supportedEfforts,
		CacheKey:         cacheKey,
		Global:           region.IsGlobal(),
	})

	endpoints := GetEndpoints(region)
	httpReq, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, strings.NewReader(string(prepared)))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	c.applyChatHeaders(httpReq, req.Credential, req.Meta, req.ClientIP)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(httpReq)
	if errDo != nil {
		// 传输层错误：不是账号问题（不产生分类副作用），但换号重试有意义。
		return nil, transientError(errDo)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		closeBody(resp.Body)
		errClassified := enrichClassifyError(resp.StatusCode, string(body), resp.Header)
		if errClassified != nil && errClassified.Kind == core.FailureKindContentBlocked && c.opts.OnContentBlocked != nil {
			c.opts.OnContentBlocked()
		}
		return nil, errClassified
	}
	return resp.Body, nil
}

// transientError 把传输层错误包装成一个「可重试但不罚号」的错误。
//
// 与分类错误的区别：Status 为 0，宿主按 502 处理并换号重试，
// 但不会因它把凭证标记为坏（半截响应不该让账号背锅）。
func transientError(err error) *core.Failure {
	if err == nil {
		return nil
	}
	return &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: err.Error()}
}

// Chat 发起一次对话并聚合为非流式响应。
func (c *Client) Chat(req ChatRequest) (*ChatResult, error) {
	reader, errStream := c.ChatStream(req)
	if errStream != nil {
		return nil, errStream
	}
	defer closeBody(reader)

	aggregated, errAggregate := Aggregate(reader)
	if errAggregate != nil {
		if IsEmptyStreamError(errAggregate) {
			return nil, &core.Failure{Kind: core.FailureKindServer, Status: http.StatusBadGateway,
				Msg: "upstream stream contained no valid data events (upstream_parse)"}
		}
		return nil, errAggregate
	}
	return &ChatResult{Response: aggregated, Credential: req.Credential}, nil
}

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
		req.Header.Set("X-Machine-ID", c.rt.deriveID(uid, "machine"))
		req.Header.Set("X-Session-ID", c.rt.deriveID(uid, "session"))
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
	req.Header.Set("X-Request-ID", requestIDFor(cred, c.rt))
}

// desktopUA 返回桌面指纹族的 UA。
func desktopUA() string {
	return "WorkBuddy/" + DesktopClientVersion + " WorkBuddy/" + DesktopClientVersion + " CLI/" + defaultCLIVersion
}

// requestIDFor 生成一个带纳秒后缀的请求 ID（桌面族要求每次不同）。
func requestIDFor(cred *Credential, rt *runtime) string {
	return rt.deriveID(cred.UIDValue(), "req") + newHexID()[:6]
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

// 本文件实现上游 SSE 流的处理：流式透传与非流式聚合。
//
// 上游返回的是 OpenAI 形态的 SSE（`data: {...}` 帧 + 结尾 `data: [DONE]`），
// 但直接原样转发给客户端是不安全的：
//   - 上游会在 `choices[].message` 里放整条消息（而非 delta），累加型客户端会重复拼接；
//   - 上游的 tool_call 分片每片都带 function.name，累加型客户端会拼成 "BashBash"；
//   - 上游可能中途漏掉 id/model 字段，客户端会把它当成新消息。
//
// 因此采用**白名单重建**：只保留已知字段，其余丢弃。

// errEmptyStream 表示上游流里没有任何有效数据帧。
//
// 单独定义是为了让调用方区分「上游空流」（上游缺陷，该报错）
// 与「客户端断连导致的写失败」（正常现象，不该报错）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为上游空流。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// StreamStats 是一次流式转发的统计。
type StreamStats struct {
	// Frames 是转发的有效帧数。
	Frames int
	// Usage 是上游给出的 usage（可能为空）。
	Usage map[string]any
	// Model 是上游回答里的模型名。
	Model string
	// Credit 是上游给出的本次真实扣费积分（可能为 nil）。
	Credit *float64
	// FinishReason 是最后一个 finish_reason。
	FinishReason string
	// SawDone 表示是否收到了 [DONE]。
	SawDone bool
}

// StreamOptions 是流式转发的选项。
type StreamOptions struct {
	// Emit 收到一帧可下发的载荷时调用。载荷是**裸 JSON**（不含 `data: ` 前缀），
	// 因为宿主会为 chat-completions 补上前缀与结尾的 [DONE]。
	Emit func(payload []byte) error
	// Hint 为错误帧附加值（可为 nil）。
	Hint func(code string) string
}

// Stream 读取上游 SSE 并把规范化后的帧交给 emit。
//
// 行为要点：
//   - 收到 `[DONE]` 后停止读取，之后的数据一律忽略；
//   - 含 error 键的帧原样透传（错误帧不该被白名单改写）；
//   - 白名单重建其余帧；
//   - 全程零有效数据帧 → 返回 errEmptyStream。
func Stream(body io.Reader, opts StreamOptions) (StreamStats, error) {
	var stats StreamStats
	if body == nil {
		return stats, errEmptyStream
	}

	scanner := bufio.NewScanner(body)
	// 上游的单帧可能很长（工具参数、长文本），默认 64KB 会截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	firstID := ""
	seenToolName := map[int]bool{}
	sawAny := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// 非 data: 行（注释、event: 等）原样下发，保持上游的帧结构。
		if !strings.HasPrefix(trimmed, "data:") {
			if errEmit := opts.emit([]byte(line)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			stats.SawDone = true
			break
		}
		if payload == "" {
			continue
		}

		// 错误帧：原样透传，可选附加提示。上游的错误信息比我们重建的更有价值。
		if isErrorFrame(payload) {
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(attachHint(payload, opts.Hint))); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			// 无法解析的帧原样透传（可能是上游新增的帧类型）。
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(payload)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		// tool_call 的 name 只保留在首片。
		StripToolCallNames(frame, seenToolName)

		// id 续传：上游偶发在后续帧漏 id，客户端会把它当成新消息。
		if id, _ := frame["id"].(string); strings.TrimSpace(id) != "" {
			if firstID == "" {
				firstID = id
			}
		} else if firstID != "" {
			frame["id"] = firstID
		}

		collectStats(frame, &stats)
		normalized := normalizeFrame(frame)

		encoded, errEncode := json.Marshal(normalized)
		if errEncode != nil {
			continue
		}
		stats.Frames++
		sawAny = true
		if errEmit := opts.emit(encoded); errEmit != nil {
			return stats, errEmit
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		// 扫描错误（含上游断流）：已转发的帧仍然有效，把错误交给调用方判断。
		return stats, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if !sawAny {
		return stats, errEmptyStream
	}
	return stats, nil
}

func (o StreamOptions) emit(payload []byte) error {
	if o.Emit == nil {
		return nil
	}
	return o.Emit(payload)
}

// isErrorFrame 判断帧是否为错误帧。
func isErrorFrame(payload string) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// attachHint 给错误帧附加提示文案（只加不改）。
func attachHint(payload string, hint func(code string) string) string {
	if hint == nil {
		return payload
	}
	var frame map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
		return payload
	}
	errorField, okError := frame["error"].(map[string]any)
	if !okError {
		return payload
	}
	if _, okHint := errorField["gateway_hint"]; okHint {
		return payload
	}
	code, _ := errorField["code"].(string)
	text := hint(code)
	if strings.TrimSpace(text) == "" {
		return payload
	}
	errorField["gateway_hint"] = text
	encoded, errEncode := json.Marshal(frame)
	if errEncode != nil {
		return payload
	}
	return string(encoded)
}

// collectStats 从帧里提取统计信息。
func collectStats(frame map[string]any, stats *StreamStats) {
	if usage, okUsage := frame["usage"].(map[string]any); okUsage && len(usage) > 0 {
		stats.Usage = usage
		if credit, okCredit := toFloat64(usage["credit"]); okCredit {
			stats.Credit = &credit
		}
	}
	if model, okModel := frame["model"].(string); okModel && strings.TrimSpace(model) != "" {
		stats.Model = model
	}
	choices, okChoices := frame["choices"].([]any)
	if !okChoices || len(choices) == 0 {
		return
	}
	choice, okChoice := choices[0].(map[string]any)
	if !okChoice {
		return
	}
	if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
		stats.FinishReason = reason
	}
}

func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	}
	return 0, false
}

// normalizeFrame 用白名单重建帧。
//
// 顶层保留：id / object / created / model / system_fingerprint / service_tier / choices / usage。
// delta 只保留：role / content / reasoning_content / refusal / tool_calls /
// function_call（空占位剔除）。
//
// 丢弃上游的私有字段（session_id、trace 等）：它们对客户端无用，
// 而且会把上游内部的会话标识暴露给下游。
func normalizeFrame(frame map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if value, okValue := frame[key]; okValue {
			out[key] = value
		}
	}
	if _, okObject := out["object"]; !okObject {
		out["object"] = "chat.completion.chunk"
	}
	if _, okID := out["id"]; !okID {
		out["id"] = "chatcmpl-wb2api"
	}

	if choices, okChoices := frame["choices"].([]any); okChoices {
		out["choices"] = normalizeChoices(choices)
	} else {
		out["choices"] = []any{}
	}

	// usage 缺失时显式给 null：客户端普遍会读这个键判断是否结束。
	if usage, okUsage := frame["usage"]; okUsage {
		out["usage"] = usage
	} else {
		out["usage"] = nil
	}
	return out
}

// normalizeChoices 重建 choices。
func normalizeChoices(choices []any) []any {
	out := make([]any, 0, len(choices))
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		normalized := map[string]any{}
		if index, okIndex := choice["index"]; okIndex {
			normalized["index"] = index
		}
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			normalized["delta"] = normalizeDelta(delta)
		}
		// finish_reason 空串归一为 null（客户端靠它判断结束）。
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			normalized["finish_reason"] = reason
		} else {
			normalized["finish_reason"] = nil
		}
		out = append(out, normalized)
	}
	return out
}

// normalizeDelta 用白名单重建 delta。
func normalizeDelta(delta map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"role", "content", "reasoning_content", "refusal"} {
		if value, okValue := delta[key]; okValue {
			out[key] = value
		}
	}
	if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
		out["tool_calls"] = calls
	}
	// function_call 是旧字段：空对象占位会被客户端当成一次调用，必须剔除。
	if functionCall, okFunctionCall := delta["function_call"].(map[string]any); okFunctionCall && len(functionCall) > 0 {
		out["function_call"] = functionCall
	}
	return out
}

// Aggregate 把上游 SSE 流聚合成一个非流式响应。
//
// 上游强制流式，因此非流式客户端必须由我们在网关侧聚合。
func Aggregate(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errEmptyStream
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		id           string
		model        string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []any
		usage        map[string]any
		finishReason string
		sawDone      bool
		validEvents  int
		gotContent   bool
	)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if payload == "" {
			continue
		}
		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			continue
		}
		if isErrorFrame(payload) {
			return nil, fmt.Errorf("upstream error: %s", core.Truncate(payload, 512))
		}
		validEvents++

		if got := core.StringField(frame, "id"); got != "" && id == "" {
			id = got
		}
		if got := core.StringField(frame, "model"); got != "" {
			model = got
		}
		if value, okValue := toFloat64(frame["created"]); okValue {
			created = int64(value)
		}
		if payloadUsage, okUsage := frame["usage"].(map[string]any); okUsage && len(payloadUsage) > 0 {
			usage = payloadUsage
		}

		choices, okChoices := frame["choices"].([]any)
		if !okChoices || len(choices) == 0 {
			continue
		}
		choice, okChoice := choices[0].(map[string]any)
		if !okChoice {
			continue
		}
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			finishReason = reason
		}

		// 部分上游帧把内容放在 message（而非 delta）里，两种都要处理。
		// gotContent 闩锁防止同一份内容被 delta 与 message 各算一次。
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			appendContent(&content, delta["content"])
			appendContent(&reasoning, delta["reasoning_content"])
			if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, calls)
				gotContent = true
			}
		}
		if message, okMessage := choice["message"].(map[string]any); okMessage && !gotContent {
			if text, okText := message["content"].(string); okText && text != "" {
				content.WriteString(text)
			}
			if text, okText := message["reasoning_content"].(string); okText && text != "" {
				reasoning.WriteString(text)
			}
		}
		if content.Len() > 0 {
			gotContent = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if validEvents == 0 {
		return nil, errEmptyStream
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// 输出被截断或流没正常结束时，工具参数必然不完整，丢弃它们。
		if finishReason == "length" || !sawDone {
			toolCalls = DropTruncatedToolCalls(toolCalls)
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = sortToolCalls(toolCalls)
		}
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "stop"
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		response["usage"] = ensureUsageTotal(usage)
	}
	return json.Marshal(response)
}

func appendContent(builder *strings.Builder, value any) {
	if text, okText := value.(string); okText && text != "" {
		builder.WriteString(text)
	}
}

// mergeToolCalls 按 index 合并工具调用分片。
//
// 上游的分片形态：首片带 id/name/arguments 开头，后续片只带 arguments 增量。
// 缺 index 的分片需要容错（id 已见过则延续，否则开新序号）。
func mergeToolCalls(existing []any, incoming []any) []any {
	byIndex := make(map[int]map[string]any, len(existing))
	order := make([]int, 0, len(existing)+len(incoming))
	for _, item := range existing {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index := core.IntFromAny(call["index"])
		byIndex[index] = call
		order = append(order, index)
	}

	nextIndex := len(order)
	for _, item := range incoming {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index, okIndex := call["index"]
		var key int
		if okIndex {
			key = core.IntFromAny(index)
		} else {
			// 缺 index：延续最近一次调用（上游偶发行为）。
			if len(order) > 0 {
				key = order[len(order)-1]
			} else {
				key = nextIndex
				nextIndex++
			}
		}
		target, okTarget := byIndex[key]
		if !okTarget {
			target = map[string]any{"index": key}
			byIndex[key] = target
			order = append(order, key)
		}
		mergeToolCallFields(target, call)
	}

	out := make([]any, 0, len(order))
	for _, key := range order {
		if call, okCall := byIndex[key]; okCall {
			out = append(out, call)
		}
	}
	return out
}

// mergeToolCallFields 把分片字段合并进目标调用。
func mergeToolCallFields(target, incoming map[string]any) {
	if id, okID := incoming["id"].(string); okID && strings.TrimSpace(id) != "" {
		target["id"] = id
	}
	if callType, okType := incoming["type"].(string); okType && strings.TrimSpace(callType) != "" {
		target["type"] = callType
	}
	incomingFunction, okFunction := incoming["function"].(map[string]any)
	if !okFunction {
		return
	}
	targetFunction, okTargetFunction := target["function"].(map[string]any)
	if !okTargetFunction {
		targetFunction = map[string]any{}
		target["function"] = targetFunction
	}
	if name, okName := incomingFunction["name"].(string); okName && strings.TrimSpace(name) != "" {
		targetFunction["name"] = name
	}
	if arguments, okArgs := incomingFunction["arguments"].(string); okArgs && arguments != "" {
		previous, _ := targetFunction["arguments"].(string)
		targetFunction["arguments"] = previous + arguments
	}
}

// sortToolCalls 按 index 排序输出（客户端依赖顺序）。
func sortToolCalls(calls []any) []any {
	out := append([]any(nil), calls...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			left, _ := out[j-1].(map[string]any)
			right, _ := out[j].(map[string]any)
			if core.IntFromAny(left["index"]) <= core.IntFromAny(right["index"]) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ensureUsageTotal 补齐缺失的 total_tokens。
//
// 不修改原 map：上游的 usage 可能被其他路径复用，就地改会有隐性耦合。
func ensureUsageTotal(usage map[string]any) map[string]any {
	if _, okTotal := usage["total_tokens"]; okTotal {
		return usage
	}
	prompt, okPrompt := toFloat64(usage["prompt_tokens"])
	completion, okCompletion := toFloat64(usage["completion_tokens"])
	if !okPrompt || !okCompletion {
		return usage
	}
	out := make(map[string]any, len(usage)+1)
	for key, value := range usage {
		out[key] = value
	}
	out["total_tokens"] = int64(prompt + completion)
	return out
}

// ChatMetaFromBody 从请求体与请求头派生会话头族。
func ChatMetaFromBody(body []byte, headers http.Header) ChatMeta {
	conversationKey := ConversationKey(body)
	turnKey := TurnKey(body)

	// 入站透传的链路 ID 优先（调用方可能已经建好了链路）。
	traceID := ""
	if headers != nil {
		traceID = strings.TrimSpace(headers.Get("X-Trace-ID"))
	}

	conversationRequestID := ""
	switch {
	case conversationKey != "" && turnKey != "":
		conversationRequestID = TurnRequestID(conversationKey + ":" + turnKey)
	case turnKey != "":
		conversationRequestID = TurnRequestID(turnKey)
	case conversationKey != "":
		conversationRequestID = RequestIDForKey(conversationKey)
	default:
		conversationRequestID = TurnRequestID("")
	}

	return ChatMeta{
		ConversationID:        conversationKey,
		ConversationRequestID: conversationRequestID,
		TraceID:               traceID,
	}
}
