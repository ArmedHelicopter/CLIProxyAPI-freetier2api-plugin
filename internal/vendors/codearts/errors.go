package codearts

// 本文件实现华为云 CodeArts 的上游错误分类。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/upstream"
)

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
