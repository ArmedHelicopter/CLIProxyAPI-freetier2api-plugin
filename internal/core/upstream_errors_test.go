package core

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToPluginErrorPreservesWrappedMessageAndClassification(t *testing.T) {
	for _, kind := range []FailureKind{FailureKindCredential, FailureKindTransient} {
		upstream := &Failure{Kind: kind, Msg: "gateway rejected"}
		wrapped := fmt.Errorf("积分签到成功；福利领取失败: %w", upstream)
		converted, ok := ToPluginError(wrapped).(*PluginError)
		if !ok || converted.Message != wrapped.Error() || converted.HTTPStatus != upstream.HTTPStatus() || converted.Retryable != upstream.Retryable() {
			t.Fatalf("lost context or classification: %+v", converted)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        *Failure
		wantStatus int
		wantRetry  bool
	}{
		{"credential", &Failure{Kind: FailureKindCredential}, http.StatusUnauthorized, false},
		{"rate", &Failure{Kind: FailureKindRateLimit}, http.StatusTooManyRequests, true},
		{"quota", &Failure{Kind: FailureKindHardCredit}, http.StatusPaymentRequired, true},
		{"bad params", &Failure{Kind: FailureKindBadParams}, http.StatusBadRequest, false},
		{"channel rejected", &Failure{Kind: FailureKindChannelRejected}, http.StatusBadRequest, false},
		{"status passthrough", &Failure{Kind: FailureKindClient, Status: http.StatusConflict}, http.StatusConflict, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.HTTPStatus(); got != tc.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tc.wantStatus)
			}
			if got := tc.err.Retryable(); got != tc.wantRetry {
				t.Fatalf("Retryable() = %v, want %v", got, tc.wantRetry)
			}
		})
	}
}

// TestTrimToBytesKeepsRuneBoundary 守住字节上限截断不切坏 UTF-8。
//
// 上游错误正文多为中文；按字节直切会把最后一个字符劈成半个序列，
// 日志与面板显示成 U+FFFD。
func TestTrimToBytesKeepsRuneBoundary(t *testing.T) {
	body := "签到失败：活动未开启，请稍后再试"
	for limit := 1; limit <= len(body)+2; limit++ {
		got := TrimToBytes(body, limit)
		if len(got) > limit {
			t.Fatalf("limit=%d 超长: %q", limit, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("limit=%d 切坏 UTF-8: %q", limit, got)
		}
		if !strings.HasPrefix(body, got) {
			t.Fatalf("limit=%d 不是前缀: %q", limit, got)
		}
	}
}

// TestTruncateKeepsRuneBoundary 守住 Truncate 的口径：超长才加省略号，且不切坏字符。
func TestTruncateKeepsRuneBoundary(t *testing.T) {
	body := "上游返回：额度不足"
	if got := Truncate(body, 300); got != body {
		t.Fatalf("未超长不应改动: %q", got)
	}
	for limit := 1; limit < len(body); limit++ {
		got := Truncate(body, limit)
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("limit=%d 缺少省略号: %q", limit, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("limit=%d 切坏 UTF-8: %q", limit, got)
		}
	}
}
