package autoclaw

// 本文件定义 AutoClaw 的上游端点与请求头。
//
// **骨架说明（2026-10）**：截至本文件编写时，AutoClaw 国内版与国际版的
// 上游 API 契约（域名、鉴权头、请求/响应体）在仓库内没有任何实测依据，
// Issue #24 也未附接口文档。为了不臆造端点，这里只落实两件确定的事：
//
//  1. 两个区域各有独立的可配置基地址（默认值待上游资料补充后回填）；
//  2. chat-completions 与模型清单走 OpenAI 兼容路径（autoclaw_key 的
//     申请形态与 OpenAI 风格控制台一致；真实路径以实测为准）。
//
// 待上游资料确认后需要回填的位置都以 TODO(upstream) 标出：
//   - DefaultBaseURLCN / DefaultBaseURLGlobal
//   - ApplyChatHeaders / ApplyModelsHeaders 的专有头
//
// 运维路径：管理员可通过 autoclawcn_base_url / autoclawglobal_base_url
// 配置项指向已知的上游（或自建网关），无需改代码。

import (
	"net/http"
	"strings"
)

// 区域默认基地址。**占位值**：上游域名未经实测确认，留空并在运行期
// 报「未配置上游地址」错误，而不是把猜测的域名当成可用端点。
const (
	// DefaultBaseURLCN 是国内版默认基地址（待上游资料回填）。
	DefaultBaseURLCN = ""
	// DefaultBaseURLGlobal 是国际版默认基地址（待上游资料回填）。
	DefaultBaseURLGlobal = ""
)

// OpenAI 兼容路径。真实路径若与实测不符，只需改这里。
const (
	// ChatPath 是 chat-completions 的路径。
	ChatPath = "/v1/chat/completions"
	// ModelsPath 是模型清单的路径。
	ModelsPath = "/v1/models"
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

// ApplyChatHeaders 写入 chat-completions 请求所需的全部头。
//
// 目前只有 OpenAI 兼容的最小集（Bearer 认证）。AutoClaw 专有的客户端
// 标识头（UA、版本头等，若上游有要求）待实测后补齐 —— TODO(upstream)。
func ApplyChatHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// ApplyModelsHeaders 写入模型清单请求的头（只需认证）。
func ApplyModelsHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

