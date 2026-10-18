package cline

import "freetier2api-plugin/internal/core"

// 本文件实现 Cline 的上游错误分类。
//

// 分类的意义不在于「记录得好看」，而在于决定宿主该怎么处置这个账号：
//   - 凭证失效（401/403）→ 让宿主把账号标记为坏，停止路由；
//   - 限流（429）→ 保留账号，稍后重试；
//   - 上游抖动（5xx / 网络）→ 保留账号，稍后重试；
//   - 请求本身有问题（400/422）→ 保留账号（是这次请求的问题，不是账号的）。

// Error 是共享的上游错误类型。
type Error = core.Failure

// ErrorKind 是错误处置分类。
type ErrorKind = core.FailureKind

const (
	KindCredential = core.FailureKindCredential
	KindRateLimit  = core.FailureKindRateLimit
	KindTransient  = core.FailureKindTransient
	KindClient     = core.FailureKindClient
)

// Classify 按 HTTP 状态码分类一个上游错误。
func Classify(status int, body string) *Error {
	return &Error{
		Status: status,
		Msg:    core.Truncate(body, 300),
		Kind:   core.ClassifyStatus(status),
	}
}
