package opencodezen

// 本文件实现 OpenCode ZEN 的上游错误分类。

import (
	"strings"

	"freetier2api-plugin/internal/core"
)

// Classify 按 HTTP 状态码与响应体分类一个上游错误。
func Classify(status int, body string) *core.Failure {
	kind := core.ClassifyStatus(status)
	// FreeTierError 不是凭证损坏，而是上游对该模型施加的客户端风控；
	// 按 client 处理可保留账号继续尝试其它模型。
	if strings.Contains(body, "FreeTierError") || strings.Contains(body, "free tier can only be used") {
		kind = core.FailureKindClient
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}
