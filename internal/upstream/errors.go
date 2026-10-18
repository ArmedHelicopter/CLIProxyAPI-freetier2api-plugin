// Package upstream 提供供应商无关的上游错误分类。
//
// 各供应商只负责把上游响应翻译成这里的 Kind/Error；宿主状态码、可重试
// 语义与错误码字符串统一由本包给出。这样供应商之间不会互相 import，
// 也不会各自复制一份 HTTPStatus/Retryable 映射。
package upstream

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind 是上游错误的可处置分类。
type Kind string

const (
	KindNone           Kind = "none"
	KindHardCredit     Kind = "hard_credit"
	KindSoftRate       Kind = "soft_rate"
	KindSessionDead    Kind = "session_dead"
	KindNotFound       Kind = "not_found"
	KindServer         Kind = "server"
	KindContentBlocked Kind = "content_blocked"
	KindBadParams      Kind = "bad_params"
	KindAccountFault   Kind = "account_fault"
	KindModelBlocked   Kind = "model_blocked"
	KindWAFBlock       Kind = "waf_block"
	KindPromptTooLong  Kind = "prompt_too_long"
	KindImageInvalid   Kind = "image_invalid"
	KindClient         Kind = "client"

	// 兼容通用供应商的短分类。
	KindCredential Kind = "credential"
	KindRateLimit  Kind = "rate_limit"
	KindTransient  Kind = "transient"
	KindQuota      Kind = "quota"
	KindBusy       Kind = "busy"
)

// String 返回稳定的类别名（用于日志与错误码）。
func (k Kind) String() string {
	if k == "" {
		return "unknown"
	}
	return string(k)
}

// Error 是带分类的上游错误。
//
// Msg 保持上游原文；Code 是上游业务码的可读形式（可为空）。RetryAfter /
// ResetAt 是上游明示的等待信息，零值表示未给出。
type Error struct {
	Kind       Kind
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration
	ResetAt    time.Time

	// RetryableOverride 允许供应商覆盖通用的可重试判定（nil 表示按 Kind 推导）。
	RetryableOverride *bool
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream %s [%d/%s]: %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s [%d]: %s", e.Kind, e.Status, e.Msg)
}

// HTTPStatus 返回映射给宿主的 HTTP 状态码。
//
// 上游状态码通常已经是宿主认得的（401/402/429/5xx），优先透传；只有
// Status==0（网络层失败或 200 包体里的业务错误）才按 Kind 推导。
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status > 0 {
		return e.Status
	}
	switch e.Kind {
	case KindHardCredit, KindQuota:
		return http.StatusPaymentRequired
	case KindSoftRate, KindRateLimit:
		return http.StatusTooManyRequests
	case KindSessionDead, KindCredential:
		return http.StatusUnauthorized
	case KindNotFound, KindModelBlocked:
		return http.StatusNotFound
	case KindServer, KindTransient:
		return http.StatusBadGateway
	case KindWAFBlock:
		return http.StatusForbidden
	case KindBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// Retryable 报告宿主是否应该换一个凭证重试同一请求。
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	if e.RetryableOverride != nil {
		return *e.RetryableOverride
	}
	switch e.Kind {
	case KindContentBlocked, KindPromptTooLong, KindImageInvalid, KindBadParams,
		KindClient, KindCredential:
		return false
	default:
		return true
	}
}

// UpstreamKind 返回错误分类标识。
func (e *Error) UpstreamKind() string { return e.Kind.String() }

// UpstreamMessage 返回上游原始正文（实现 core.UpstreamDetail）。
func (e *Error) UpstreamMessage() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// New 构造一个上游错误。
func New(kind Kind, status int, code, message string) *Error {
	return &Error{Kind: kind, Status: status, Code: code, Msg: message}
}

// Bool 返回 bool 指针（构造 RetryableOverride 用）。
func Bool(value bool) *bool { return &value }

// ClassifyStatus 是通用 HTTP 状态分类：调用方可在此基础上追加业务码规则。
func ClassifyStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindCredential
	case status == http.StatusPaymentRequired:
		return KindHardCredit
	case status == http.StatusTooManyRequests:
		return KindRateLimit
	case status >= 500:
		return KindTransient
	case status >= 400:
		return KindClient
	default:
		return KindTransient
	}
}

// Truncate 截断过长的上游正文，避免错误信息淹没日志。
func Truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
