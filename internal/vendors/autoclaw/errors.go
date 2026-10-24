package autoclaw

// 本文件实现 AutoClaw 的上游错误分类。

import (
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

// Classify 按 HTTP 状态码与响应体分类一个上游错误。
//
// 骨架阶段只有通用状态码分类；拿到上游资料后按业务错误码细化
// （额度、限流、内容风控等）。
func Classify(status int, body string) *core.Failure {
	kind := core.ClassifyStatus(status)
	// 402/429 之外的额度语义（如业务码里的 quota/exhausted）待实测后补充。
	if strings.Contains(strings.ToLower(body), "insufficient") && status == http.StatusPaymentRequired {
		kind = core.FailureKindHardCredit
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}

