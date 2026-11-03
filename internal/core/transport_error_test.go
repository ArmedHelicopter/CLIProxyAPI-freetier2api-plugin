package core

// 本文件锁住「宿主 HTTP 桥传输错误 → 分类错误」的还原规则。
//
// 背景（2026-11-03 线上）：workbuddyglobal 上游返回 429 时，宿主
// host.http.do_stream 把整个响应折叠成一条 RPC 错误文本，插件拿不到
// http.Response，于是 429 落进 502 兜底分支——客户端收到 500
// internal_server_error，宿主也不做限流冷却。这里守住还原结果。

import (
	"fmt"
	"strings"
	"testing"
)

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantStatus   int
		wantRetry    bool
		wantContains string
		wantNil      bool
	}{
		{
			name: "上游 429",
			raw: `Post "https://www.workbuddy.ai/v2/chat/completions": host.http.do_stream: ` +
				`host_call_failed: execute host http request: Post "https://www.workbuddy.ai/v2/chat/completions": Too Many Requests`,
			wantStatus:   429,
			wantRetry:    true,
			wantContains: "上游 HTTP 429 Too Many Requests（www.workbuddy.ai）",
		},
		{
			name:       "上游 403",
			raw:        `Post "https://api.example.com/v1/chat": host_call_failed: execute host http request: Forbidden`,
			wantStatus: 403,
			wantRetry:  true,
		},
		{
			name:       "上游 401",
			raw:        `Get "https://api.example.com/v1/models": host.http.do: host_call_failed: Unauthorized`,
			wantStatus: 401,
			wantRetry:  false,
		},
		{
			name:       "上游 400",
			raw:        `Post "https://api.example.com/v1/chat": host_call_failed: Bad Request`,
			wantStatus: 400,
			wantRetry:  false,
		},
		{
			name:       "上游 413 不被 400 吃掉",
			raw:        `Post "https://api.example.com/v1/chat": host_call_failed: Request Entity Too Large`,
			wantStatus: 413,
			wantRetry:  false,
		},
		{
			name:       "上游 404",
			raw:        `Get "https://api.example.com/v1/x": host_call_failed: Not Found`,
			wantStatus: 404,
			wantRetry:  true,
		},
		{
			name:       "上游 503",
			raw:        `Post "https://api.example.com/v1/chat": host_call_failed: Service Unavailable`,
			wantStatus: 503,
			wantRetry:  true,
		},
		{
			name:    "无状态短语不猜",
			raw:     `Post "https://api.example.com/v1/chat": host_call_failed: dial tcp: i/o timeout`,
			wantNil: true,
		},
		{
			name:    "非桥错误不误判",
			raw:     "open /tmp/model.json: no such file or directory",
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := classifyTransportError(fmt.Errorf("%s", tt.raw))
			if tt.wantNil {
				if failure != nil {
					t.Fatalf("classifyTransportError = %+v, want nil", failure)
				}
				return
			}
			if failure == nil {
				t.Fatalf("classifyTransportError = nil, want status %d", tt.wantStatus)
			}
			if got := failure.HTTPStatus(); got != tt.wantStatus {
				t.Fatalf("HTTPStatus = %d, want %d", got, tt.wantStatus)
			}
			if got := failure.Retryable(); got != tt.wantRetry {
				t.Fatalf("Retryable = %v, want %v", got, tt.wantRetry)
			}
			if tt.wantContains != "" && failure.Msg != tt.wantContains {
				t.Fatalf("Msg = %q, want %q", failure.Msg, tt.wantContains)
			}
		})
	}
}

func TestToPluginErrorTransport(t *testing.T) {
	// 线上原文：修复前这条会变成 502 upstream_request_failed 且客户端拿 500。
	raw := `Post "https://www.workbuddy.ai/v2/chat/completions": host.http.do_stream: ` +
		`host_call_failed: execute host http request: Post "https://www.workbuddy.ai/v2/chat/completions": Too Many Requests`
	pluginErr, ok := ToPluginError(fmt.Errorf("%s", raw)).(*PluginError)
	if !ok || pluginErr == nil {
		t.Fatalf("ToPluginError 未返回 *PluginError")
	}
	if pluginErr.HTTPStatus != 429 {
		t.Fatalf("HTTPStatus = %d, want 429", pluginErr.HTTPStatus)
	}
	if !pluginErr.Retryable {
		t.Fatalf("Retryable = false, want true")
	}
	if pluginErr.Code != "upstream_soft_rate" {
		t.Fatalf("Code = %q, want upstream_soft_rate", pluginErr.Code)
	}
	for _, leak := range []string{"host.http.do_stream", "host_call_failed", "execute host http request"} {
		if strings.Contains(pluginErr.Message, leak) {
			t.Fatalf("消息里仍带内部前缀 %q：%s", leak, pluginErr.Message)
		}
	}
}

func TestHTTPStatusCredentialKindsOverride(t *testing.T) {
	// 上游把账号级故障包在 400 / 200 里时，仍要按凭证侧结论上报：
	// 400 命中不了宿主的冷却分支，被封账号会每轮被重新选中。
	tests := []struct {
		name string
		fail *Failure
		want int
	}{
		{name: "account_fault 400 → 403", fail: &Failure{Kind: FailureKindAccountFault, Status: 400}, want: 403},
		{name: "account_fault 200 → 403", fail: &Failure{Kind: FailureKindAccountFault, Status: 200}, want: 403},

		{name: "普通 400 原样", fail: &Failure{Kind: FailureKindClient, Status: 400}, want: 400},
		{name: "429 原样", fail: &Failure{Kind: FailureKindSoftRate, Status: 429}, want: 429},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fail.HTTPStatus(); got != tt.want {
				t.Fatalf("HTTPStatus = %d, want %d", got, tt.want)
			}
		})
	}
}
