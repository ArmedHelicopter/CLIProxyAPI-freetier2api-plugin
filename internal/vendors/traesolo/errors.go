package traesolo

// 错误分类：解析 TRAE SOLO 上游错误码。

import (
	"encoding/json"
	"fmt"
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
	Code    int       `json:"code,omitempty"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("traesolo upstream error [%d/%d]: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("traesolo upstream error [%d]: %s", e.Status, e.Message)
}

func Classify(status int, body string) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := firstNonEmpty(payload.Message, payload.Msg, body)

	switch {
	case status == http.StatusUnauthorized || payload.Code == 4001 || payload.Code == 4008:
		return &Error{Kind: KindCredential, Status: status, Code: payload.Code, Message: msg}

	case payload.Code == 1005 || strings.Contains(strings.ToLower(msg), "credit") || strings.Contains(msg, "积分不足"):
		return &Error{Kind: KindCredential, Status: status, Code: 1005, Message: "账号积分不足"}

	case status == http.StatusTooManyRequests || payload.Code == 429 || payload.Code == 9074:
		return &Error{Kind: KindRateLimit, Status: status, Code: payload.Code, Message: msg}

	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: payload.Code, Message: msg}

	default:
		return &Error{Kind: KindClient, Status: status, Code: payload.Code, Message: msg}
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
