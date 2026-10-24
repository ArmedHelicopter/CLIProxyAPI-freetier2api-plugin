package raccoon

// 本文件实现小浣熊的上游错误分类。
//
// 分类依据 xiaohuanxiong2api-panel 的 Classify()（HTTP 与流内共用同一套
// 信号判据，避免两条路径漂移）：
//   - 401/403 → 会话失效；
//   - 429 → 限流；
//   - 额度文案（积分/额度/余额 + 不足/耗尽，或业务码 14018）→ hard_credit；
//   - 排队信封 → busy（换号不罚号）。
//
// 方向：宁可漏判（不罚号、只换号重试），不可误伤可用账号。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

// quotaBusinessCode 是「额度不足」的专用业务码（抓包实测）。
const quotaBusinessCode = 14018

// queueUnavailable 报告响应体是否为上游排队信封。
func queueUnavailable(body string) bool {
	var probe struct {
		Data struct {
			ServiceAvailable bool `json:"serviceAvailable"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return false
	}
	return !probe.Data.ServiceAvailable
}

// quotaSignal 报告文案是否确实在说额度/余额不足。
//
// `insufficient` 单独出现太宽（"context window is insufficient" 也会命中），
// 必须搭配额度词才算数；`token` 同理不进会话失效判据。
func quotaSignal(lower string) bool {
	if strings.Contains(lower, "14018") || strings.Contains(lower, "credits exhausted") {
		return true
	}
	if strings.Contains(lower, "insufficient") {
		for _, word := range []string{"point", "credit", "balance", "quota", "余额", "积分", "额度"} {
			if strings.Contains(lower, word) {
				return true
			}
		}
	}
	for _, pair := range [][2]string{
		{"积分", "不足"}, {"积分", "耗尽"}, {"额度", "不足"}, {"额度", "耗尽"},
		{"余额", "不足"}, {"余额", "耗尽"}, {"quota", "exceeded"}, {"quota", "exhausted"},
	} {
		if strings.Contains(lower, pair[0]) && strings.Contains(lower, pair[1]) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态码与响应体分类一个上游错误。
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)
	kind := classifyKind(status, lower, body)
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(errorMessage(body), 300),
		Kind:   kind,
	}
}

// ClassifyBusiness 按 HTTP 2xx 包体里的业务码分类（code != 0）。
func ClassifyBusiness(status int, code int64, message string) *core.Failure {
	kind := core.FailureKindClient
	switch {
	case code == quotaBusinessCode:
		kind = core.FailureKindHardCredit
	case strings.Contains(message, "失效") || strings.Contains(message, "重新登录"):
		kind = core.FailureKindSessionDead
	}
	return &core.Failure{
		Status: status,
		Msg:    core.Truncate(message, 300),
		Kind:   kind,
	}
}

// classifyKind 把 HTTP 状态与文案归一成 FailureKind。
func classifyKind(status int, lower, body string) core.FailureKind {
	switch {
	// 认证类先判：403 可能只是权限问题，落到额度文案会误封号。
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return core.FailureKindSessionDead
	case status == http.StatusTooManyRequests:
		return core.FailureKindSoftRate
	case status >= 500:
		return core.FailureKindServer
	case status == http.StatusNotFound:
		return core.FailureKindNotFound
	case status == http.StatusPaymentRequired:
		return core.FailureKindHardCredit
	}
	if queueUnavailable(body) {
		return core.FailureKindBusy
	}
	if quotaSignal(lower) {
		return core.FailureKindHardCredit
	}
	if status >= 400 {
		return core.FailureKindClient
	}
	return core.FailureKindTransient
}

// errorMessage 从错误体里取人类可读文案（message / msg / detail / error.message）。
func errorMessage(body string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return strings.TrimSpace(body)
	}
	if msg := core.GetString(payload, "message", "msg", "detail"); msg != "" {
		return msg
	}
	if inner, okInner := payload["error"].(map[string]any); okInner {
		return core.GetString(inner, "message")
	}
	return strings.TrimSpace(body)
}
