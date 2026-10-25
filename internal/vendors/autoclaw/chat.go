package autoclaw

// 本文件实现 AutoClaw 的协议端点、请求签名/请求头与 chat-completions 转发。
//
// 协议依据：Agent2API（docs/reverse-notes.md，2026-09 对 Web 前端 bundle 与
// 桌面端 1.18.5 的静态逆向结论），Issue #24 指定为本供应商的实现参考。
//
// 通道：region 基址下的 /autoclaw-proxy/proxy/autoclaw/v1/chat/completions
// （Agent2API 的 chat_proxy 通道，模型经 X-Request-Model 头声明）。
// 上游返回 OpenAI 兼容的 JSON / SSE，插件原样透传。
//
// 说明：Agent2API 还有「沙箱 relay」通道（wss 设备上线 + agent/send +
// /api/events），依赖 per-account 云端沙箱与 WebSocket 长连接。本插件按
// 「凭证驻留宿主进程、按请求出站」的形态选择 proxy 通道；relay 通道需要
// 的常驻会话状态与插件模型不符，暂不实现（凭证里保留 sandbox 字段以备扩展）。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 区域默认基地址（userapi REST 基址，cn/global 双部署）。
const (
	DefaultBaseURLCN     = "https://autoglm-acceleration-api.zhipuai.cn"
	DefaultBaseURLGlobal = "https://autoglm-api.autoglm.ai"
)

// 业务路径（挂在 region 基址下）。
const (
	ChatPath         = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	ModelsConfigPath = "/autoclaw-proxy/proxy/autoclaw-model-config"
	RefreshPath      = "/userapi/v1/refresh"
	WalletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	CheckinPath      = "/autoclaw-proxy/proxy/autoclaw-task-complete"
)

// 签名与客户端常量（逆向自 Web 前端 bundle）。
const (
	authAppID     = "100003"
	authSecret    = "38d2391985e2369a5fb8227d8e6cd5e5"
	clientVer     = "1.18.5"
	errorCodeDead = 410000 // 未登录/会话失效
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// DefaultBaseURLFor 返回区域的默认基地址。
func DefaultBaseURLFor(region Region) string {
	if region.IsGlobal() {
		return DefaultBaseURLGlobal
	}
	return DefaultBaseURLCN
}

// BaseURL 返回上游基地址（末尾无斜杠）：override 优先，否则区域默认值。
func BaseURL(override string, region Region) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return DefaultBaseURLFor(region)
}

// AuthSign 计算请求签名：MD5("<appid>&<ts>&<secret>")。
func AuthSign(ts string) string {
	sum := md5.Sum([]byte(authAppID + "&" + ts + "&" + authSecret))
	return hex.EncodeToString(sum[:])
}

// bearer 归一化 token：已带 Bearer 前缀时幂等。
func bearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		return trimmed
	}
	return "Bearer " + trimmed
}

// ApplyUserAPIHeaders 写入 userapi REST 请求所需的全部头（业务信封接口）。
func ApplyUserAPIHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Tm", "web")
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "web")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("X-Lang", "en")
	// 不能留空：Go 默认的 "Go-http-client/1.1" 会被阿里云 WAF 以 405 HTML 拦截
	// （实测同一请求只换 UA 就恢复；OAuth 的 405 就是这么来的）。
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// ApplyOAuthHeaders 写入国际版 OAuth 两跳（oauth-url / oauth-login）的请求头。
//
// 与 ApplyUserAPIHeaders 的差别（别合并）：OAuth 走的是官方**桌面端**链路，
// 客户端指纹是 pc / zcode / linux / zh-CN（Agent2API signed_auth_headers 同口径）。
// User-Agent 尤其不能留空：浏览器里生成的阿里云验证码令牌由服务端原样转发，
// 而 Go 默认 UA 会被阿里云 WAF 判成机器人并回 405 HTML（现象是「Google 能过、
// Zai 一直失败」——WAF 规则按路径命中，不是端点形状问题）。
func ApplyOAuthHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Harness-Type", "zcode")
	// 与参考实现一致：非 Windows 构建发 linux。WAF 与业务层都不按它区分
	// 浏览器所在的桌面系统，改 mac 没有任何收益（agent2api 在 macOS 上也发 linux）。
	req.Header.Set("X-Tm", "linux")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// ApplyChatHeaders 写入 proxy chat 请求所需的全部头。
//
// 与 userapi 头的差异：走桌面端形态（X-Tm: mac / X-Client-Type: pc），
// 模型经 X-Request-Model 与 X-Authorization 双通道声明。
func ApplyChatHeaders(req *http.Request, token, model string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Tm", "mac")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("x_trace_id", "freetier2api")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
		req.Header.Set("X-Authorization", bearer(token))
	}
	if strings.TrimSpace(model) != "" {
		req.Header.Set("X-Request-Model", model)
	}
}

// ApplyModelsHeaders 写入模型配置请求的头（userapi 信封接口）。
func ApplyModelsHeaders(req *http.Request, token string) {
	ApplyUserAPIHeaders(req, token)
	req.Header.Set("Accept", "application/json")
}

// DeadCode 报告业务码是否为「会话失效」（410000）。
func DeadCode(code int64) bool { return code == errorCodeDead }

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// Token 是出站凭证（access token）。
	Token string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Model 是剥掉前缀后的裸模型名。
	Model string
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	// 业务信封形态（{"code","msg","data"}）：非 0 业务码按错误处理。
	if code, msg, ok := envelopeError(body); ok {
		return nil, Classify(resp.StatusCode, code, msg)
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, 0, string(payload))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// prepareBody 是请求体出站前的钩子：把 model 改写为上游路由名并去掉 user 字段
// （与 Agent2API 的 _prepare_proxy_body 一致；user 字段会被上游拒绝）。
func prepareBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if strings.TrimSpace(model) != "" {
		payload["model"] = model
	}
	delete(payload, "user")
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// envelopeError 检查业务信封 {"code","msg"}：code 存在且非 0 时返回 (code, msg, true)。
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	msg := probe.Msg
	if msg == "" {
		msg = probe.Message
	}
	return *probe.Code, msg, true
}
