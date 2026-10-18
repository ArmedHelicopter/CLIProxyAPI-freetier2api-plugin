package hostabi

// 本文件提供管理请求的通用上下文与回调 ID 解析。
// admin、tasks 与 app 共用，避免各自复制一份。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// ManagementContext 为管理接口里的上游调用构造带超时与回调 ID 的上下文。
func ManagementContext(req pluginapi.ManagementRequest, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return WithCallbackID(ctx, RequestCallbackID(req)), cancel
}

// RequestCallbackID 读取宿主传入的回调 ID。
func RequestCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// ErrorStatus 返回错误对应的 HTTP 状态码（默认 500）。
func ErrorStatus(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	type statusCoder interface{ StatusCode() int }
	if coded, ok := err.(statusCoder); ok {
		if status := coded.StatusCode(); status > 0 {
			return status
		}
	}
	return http.StatusInternalServerError
}
