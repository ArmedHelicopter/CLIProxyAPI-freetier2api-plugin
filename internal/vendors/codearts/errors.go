package codearts

// 错误分类：解析华为云 CodeArts 错误码。

import (
	"encoding/json"
	"fmt"
	"freetier2api-plugin/internal/core"
	"net/http"
	"strings"
)

type ErrorKind string

const (
	KindCredential ErrorKind = "credential"
	KindRateLimit  ErrorKind = "rate_limit"
	KindTransient  ErrorKind = "transient"
	KindClient     ErrorKind = "client"
)

type Error struct {
	Kind    ErrorKind `json:"kind"`
	Status  int       `json:"status"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("codearts upstream error [%d/%s]: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("codearts upstream error [%d]: %s", e.Status, e.Message)
}

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
		return &Error{Kind: KindCredential, Status: status, Code: code, Message: msg}

	case strings.Contains(body, "benefit not found") || strings.Contains(code, "InferHub.4004.200"):
		return &Error{Kind: KindCredential, Status: status, Code: code, Message: "未领取免费福利模型配额，请在管理端领取或开启自动领取"}

	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &Error{Kind: KindRateLimit, Status: status, Code: code, Message: msg}

	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: code, Message: msg}

	default:
		return &Error{Kind: KindClient, Status: status, Code: code, Message: msg}
	}
}

// HTTPStatus 返回映射给宿主的 HTTP 状态码。
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status >= 400 {
		return e.Status
	}
	switch e.Kind {
	case KindCredential:
		return http.StatusUnauthorized
	case KindRateLimit:
		return http.StatusTooManyRequests
	case KindTransient:
		return http.StatusBadGateway
	case KindClient:
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// Retryable 报告宿主是否应换一个凭证重试同一请求。
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	return e.Kind != KindCredential && e.Kind != KindClient
}

// UpstreamKind 返回错误分类标识。
func (e *Error) UpstreamKind() string {
	if e == nil || e.Kind == "" {
		return "unknown"
	}
	return string(e.Kind)
}
