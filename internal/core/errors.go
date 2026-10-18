package core

// 本文件定义供应商错误的中立契约。
//
// 背景：插件把「上游错误」翻译成宿主认得的 HTTP 状态码，宿主据此决定
// 冷却/禁用/轮转。此前根层只认 workbuddy.Error，其它供应商的错误全部退化成
// 502 retryable（401 不会让凭证失效、429 不会触发限流处置）。现在每个供应商
// 的分类错误都实现本接口，由 ToPluginError 统一翻译。

import (
	"errors"
	"net/http"
	"strings"
)

// UpstreamError 是供应商分类后的上游错误。
//
// 方法名刻意避开 Kind / Status / Message：各家的 Error 结构体已经用了这些
// 字段名，而同名字段与方法在 Go 里不能共存。
type UpstreamError interface {
	error
	// HTTPStatus 返回映射给宿主的 HTTP 状态码。
	HTTPStatus() int
	// Retryable 报告宿主是否应该换一个凭证重试同一请求。
	Retryable() bool
	// UpstreamKind 返回错误分类标识（用于日志与错误码）。
	UpstreamKind() string
}

// UpstreamDetail 由能提供上游原始响应正文的错误实现。
//
// 用途：流式错误帧与日志里优先给用户看上游原文（诊断价值更高），
// 而不是插件重写的分类描述。
type UpstreamDetail interface {
	error
	UpstreamMessage() string
}

// ToPluginError 把供应商错误翻译成宿主契约错误。
//
// 非分类错误按 502 可重试处理：那通常是本地问题（编解码、连接），
// 换号重试是安全的。
func ToPluginError(err error) error {
	if err == nil {
		return nil
	}
	var upstreamErr UpstreamError
	if errors.As(err, &upstreamErr) {
		status := upstreamErr.HTTPStatus()
		if status <= 0 {
			status = http.StatusBadGateway
		}
		code := upstreamCode(upstreamErr.UpstreamKind())
		if upstreamErr.Retryable() {
			return RetryablePluginError(code, upstreamErr.Error(), status)
		}
		return NewPluginError(code, upstreamErr.Error(), status)
	}
	return RetryablePluginError("upstream_request_failed", err.Error(), http.StatusBadGateway)
}

// upstreamCode 生成错误码：upstream_<kind>。
func upstreamCode(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "upstream_error"
	}
	return "upstream_" + kind
}
