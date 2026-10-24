package autoclaw

// 本文件定义 AutoClaw 的上游端点、请求头与请求签名。
//
// 协议依据：Agent2API（docs/reverse-notes.md，2026-09 对
// Web 前端 bundle 与桌面端 1.18.5 的静态逆向结论），Issue #24 指定为本
// 供应商的实现参考。
//
// 要点：
//   - userapi REST 基址分地区（cn=智谱加速域名 / global=autoglm.ai）；
//   - 业务信封 {"code","msg","data"}，code=0 成功，410000=会话失效；
//   - 请求头带 X-Auth-Appid / X-Auth-TimeStamp / X-Auth-Sign
//     （MD5("100003&<ts>&<secret>")）；
//   - chat 走 /autoclaw-proxy/proxy/autoclaw/v1/chat/completions，
//     模型经 X-Request-Model 头指定（body 的 model 同步改写）。

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
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
