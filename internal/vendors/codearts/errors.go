package codearts

// 本文件实现华为云 CodeArts 的上游错误分类。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

// Classify 把 CodeArts 的上游响应分类成共享的上游错误。
func Classify(status int, body string) *core.Failure {
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
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: msg}
	case strings.Contains(body, "benefit not found") || strings.Contains(code, "InferHub.4004.200"):
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: "未领取免费福利模型配额，请在管理端领取或开启自动领取"}
	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &core.Failure{Kind: core.FailureKindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &core.Failure{Kind: core.FailureKindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Code: code, Msg: msg}
	}
}
