package qoder

// 本文件实现 Qoder 的错误分类与插件错误构造。
//
// 分类结果决定宿主对凭证的处置：401 禁用账号、502 换号重试、503 稍后再试。

import (
	"net/http"
	"strings"

	"freetier2api-plugin/internal/upstream"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
)

// PluginError 是 Qoder 的上游错误（与其它供应商共用同一类型）。
type PluginError = upstream.Error

// NewError 构造一个 Qoder 插件错误。
func NewError(code, message string, status int) *PluginError {
	kind := upstream.ClassifyStatus(status)
	switch {
	case strings.Contains(code, "credential"):
		kind = upstream.KindCredential
	case strings.Contains(code, "busy"):
		kind = upstream.KindBusy
	case strings.Contains(code, "transient"):
		kind = upstream.KindTransient
	}
	retryable := kind == upstream.KindTransient && strings.Contains(code, "transient")
	return &upstream.Error{
		Kind:              kind,
		Status:            status,
		Code:              code,
		Msg:               message,
		RetryableOverride: upstream.Bool(retryable),
	}
}

// ClassifyCredentialError 把上游错误分类成插件错误。
func ClassifyCredentialError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	lower := strings.ToLower(message)
	if _, queued := bridge.ParseQueueSignal(message); queued {
		return NewError("qoder_model_busy", message, http.StatusServiceUnavailable)
	}
	switch {
	case strings.Contains(message, "HTTP 401"), strings.Contains(message, "HTTP 403"),
		strings.Contains(lower, "unauthorized"), strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "invalid token"), strings.Contains(lower, "token expired"):
		return NewError("qoder_credential_invalid", message, http.StatusUnauthorized)
	case bridge.IsTransientTransport(err):
		return NewError("qoder_upstream_transient", message, http.StatusBadGateway)
	default:
		return NewError("qoder_upstream_error", message, http.StatusBadGateway)
	}
}

// classifyCredentialError 是 ClassifyCredentialError 的包内别名。
func classifyCredentialError(err error) error { return ClassifyCredentialError(err) }
