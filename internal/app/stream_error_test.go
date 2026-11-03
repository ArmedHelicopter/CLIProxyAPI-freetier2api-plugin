package app

// 本文件锁住「流式失败也要把状态码带过桥」的契约。
//
// 背景（2026-11-03 线上）：runStreamExecution 原先只把错误文本交给
// host.stream.close，没有 error_detail，宿主因此把每次流式失败都当「无状态码
// 的插件错误」——客户端一律 500 internal_server_error，凭证也不进冷却，
// 被封账号每 60-90 秒被重新选中。streamErrorDetail 是这条链的唯一出口。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestStreamErrorDetailCarriesStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantRetry  bool
	}{
		{
			name:       "账号级故障 403",
			err:        &core.Failure{Kind: core.FailureKindAccountFault, Status: http.StatusForbidden, Msg: `{"code":11140,"msg":"request illegal"}`},
			wantStatus: 403,
			wantRetry:  true,
		},
		{
			name:       "上游限流 429",
			err:        &core.Failure{Kind: core.FailureKindSoftRate, Status: http.StatusTooManyRequests, Msg: "rate limited"},
			wantStatus: 429,
			wantRetry:  true,
		},
		{
			name:       "宿主桥 429 文本",
			err:        fmt.Errorf(`Post "https://www.workbuddy.ai/v2/chat/completions": host.http.do_stream: host_call_failed: Too Many Requests`),
			wantStatus: 429,
			wantRetry:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := streamFailureMessage(tt.err)
			detail := streamErrorDetail(tt.err, message)
			if detail == nil {
				t.Fatalf("streamErrorDetail = nil，状态码丢失")
			}
			if detail.HTTPStatus != tt.wantStatus {
				t.Fatalf("HTTPStatus = %d, want %d", detail.HTTPStatus, tt.wantStatus)
			}
			if detail.Retryable != tt.wantRetry {
				t.Fatalf("Retryable = %v, want %v", detail.Retryable, tt.wantRetry)
			}
			if strings.TrimSpace(detail.Message) == "" {
				t.Fatalf("Message 为空")
			}
			if strings.Contains(detail.Message, "host_call_failed") {
				t.Fatalf("消息里仍带内部前缀：%s", detail.Message)
			}
		})
	}
}

func TestStreamErrorDetailKeepsUpstreamBody(t *testing.T) {
	// 上游原文比插件重写的描述更有诊断价值，必须优先透传。
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号存在异常行为，已被限制使用"}}`
	detail := streamErrorDetail(&core.Failure{Kind: core.FailureKindAccountFault, Status: http.StatusForbidden, Msg: body}, streamFailureMessage(&core.Failure{Kind: core.FailureKindAccountFault, Status: http.StatusForbidden, Msg: body}))
	if detail == nil {
		t.Fatalf("streamErrorDetail = nil")
	}
	if detail.Message != body {
		t.Fatalf("Message = %q, want 上游原文", detail.Message)
	}
}
