package codearts

import (
	"freetier2api-plugin/internal/upstream"

	"encoding/json"
	"freetier2api-plugin/internal/core"
	"net/http"
	"strings"
)

// 常量与端点定义：华为云 CodeArts Agent 配置。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/const.go

const (
	VendorID   = "codearts"
	VendorName = "CodeArts"

	// 华为云 CodeArts API 网关
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	SnapManagerHost   = SnapEngineApiHost + "/snap-manager"
	PortalHost        = "https://codearts.huaweicloud.com/portal"
	BenefitHost       = "https://opengw.developer.huaweicloud.com"

	ClientID = "codearts-agent"

	// 端点路径
	EpChatV2        = "/api/v2/chat/completions"
	EpModelBuiltin  = "/v1/model/builtin"
	EpBenefitConfig = "/api/v1/gateway/config"
	EpBenefitClaim  = "/api/v1/benefit/claim"
	EpLoginTicket   = "/v1/login/ticket"
	EpOAuthTokens   = "/v1/oauth2/tokens"
)

// 本文件实现华为云 CodeArts 的上游错误分类。

type ErrorKind = upstream.Kind

const (
	KindCredential = upstream.KindCredential
	KindRateLimit  = upstream.KindRateLimit
	KindTransient  = upstream.KindTransient
	KindClient     = upstream.KindClient
)

type Error = upstream.Error

func Classify(status int, body string) error {
	var payload struct {
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.ErrorMessage, payload.Message, payload.Error, body)
	code := core.FirstNonEmpty(payload.ErrorCode, payload.Error)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(code, "ExpiredToken") || strings.Contains(code, "InvalidToken"):
		return &Error{Kind: KindCredential, Status: status, Code: code, Msg: msg}
	case strings.Contains(body, "benefit not found") || strings.Contains(code, "InferHub.4004.200"):
		return &Error{Kind: KindCredential, Status: status, Code: code, Msg: "未领取免费福利模型配额，请在管理端领取或开启自动领取"}
	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &Error{Kind: KindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &Error{Kind: KindClient, Status: status, Code: code, Msg: msg}
	}
}
