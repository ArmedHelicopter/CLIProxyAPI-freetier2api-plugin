package raccoon

// 本文件定义小浣熊的上游端点与请求头。
//
// 协议依据：xiaohuanxiong2api（haoxinyu1）与 xiaohuanxiong2api-panel
// （misakano7545，v1.1.0 抓包）两份逆向实现，Issue #24 指定为实现参考。
//
// 要点：
//   - 单一部署 https://xiaohuanxiong.com（无国内/国际之分）；
//   - 四个域：LLM 网关 / 鉴权 / 积分 / 主站 origin（签到挂在 origin）；
//   - 业务信封 {"code","message","data"}，code=0 成功；
//   - 出站带 v1.1.0 桌面客户端识别头（x-client-name 等）。

import (
	"net/http"
	"strings"
)

// 上游基址（可被配置覆盖）。
const (
	DefaultLLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	DefaultAuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	DefaultPointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	DefaultOrigin     = "https://xiaohuanxiong.com"
)

// 业务路径（分别挂在对应域下）。
const (
	ChatPath        = "/chat/completions"
	CatalogPath     = "/model_catalog"
	RefreshPath     = "/refresh"
	LoginCodePath   = "/login_with_authorization_code"
	BalancePath     = "/balance"
	UserInfoPath    = "/user_info"
	EntitlementPath = "/entitlement_info"
	PointsGrantPath = "/login/points/grant"
	PointsBillsPath = "/bills"
	DesktopBasePath = "/api/web/desktop/v1"
	AuthorizePath   = "/code/authorize"
	// DefaultConfigName 是上游默认对话模型（v1.1.0 抓包：目录 default_model）。
	DefaultConfigName = "raccoon-8c4485"
	// DefaultClientVersion 是出站 x-client-version 头的内置值（抓包实测）。
	DefaultClientVersion = "v1.1.0"
)

// BaseURL 归一化基址：override 优先（末尾无斜杠），否则默认值。
func BaseURL(override, fallback string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return fallback
}

// ApplyClientHeaders 写入小浣熊桌面客户端识别头（抓包实测：上游按客户端
// 渠道做风控，带上最贴近真实流量）。
func ApplyClientHeaders(req *http.Request) {
	req.Header.Set("x-client-name", "@raccoon/office-desktop")
	req.Header.Set("x-client-platform", "desktop-windows-x64")
	req.Header.Set("x-client-version", DefaultClientVersion)
	req.Header.Set("x-client-channel", "official")
}

// ApplyChatHeaders 写入 chat 请求所需的全部头。
func ApplyChatHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

// ApplyAPIHeaders 写入鉴权 / 积分域 JSON 请求的头（Accept + 客户端识别 + Bearer）。
func ApplyAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}
