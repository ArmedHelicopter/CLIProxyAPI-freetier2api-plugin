package workbuddy

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyOrderingMatters(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Kind
	}{
		// 11102 必须先判：它可能是 400 也可能是 404，晚判会被 404 分支吞掉。
		{"model blocked on 400", 400, `{"code":11102,"msg":"service info not found"}`, KindModelBlocked},
		{"model blocked on 404", 404, `{"code":11102,"msg":"service info not found"}`, KindModelBlocked},
		// 429 必须早于余额关键词：429 正文常带 "quota exceeded" 这类跨界的措辞，
		// 先判余额会把限流误冷却到次日，白扔一个号。
		{"429 with credit wording stays rate limit", 429, `{"code":1,"msg":"quota exceeded, too many requests"}`, KindSoftRate},
		// 真正的余额耗尽由 402 或业务码 14018 捕获。
		{"402 is hard credit", 402, `{"code":1,"msg":"payment required"}`, KindHardCredit},
		{"429 with 14018 is hard credit", 429, `{"code":14018,"msg":"insufficient"}`, KindHardCredit},
		// 会话失效优先于账号故障。
		{"session dead", 401, `{"code":12153,"msg":"Offline user session not found"}`, KindSessionDead},
		// 账号故障不能按 11140 判定（该 code 同时承载模型级限流文案），只看关键词。
		{"account fault by keyword", 400, `{"msg":"request illegal"}`, KindAccountFault},
		// 11140 带限流文案时必须按文案归为限流，而不是账号故障——
		// 按 code 判定会把一个只是被限流的账号当成被封禁处理。
		{"11140 with rate-limit wording is soft rate", 400, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, KindSoftRate},
		// 上下文超限是请求级错误，必须先于 404/5xx/WAF 判定。
		{"prompt too long on 400", 400, `{"code":11115,"msg":"prompt is too long"}`, KindPromptTooLong},
		{"prompt too long on 413", 413, `{"code":11115,"msg":"prompt is too long"}`, KindPromptTooLong},
		// WAF：403 且没有业务信封。
		{"waf block", 403, `<html>Request blocked by WAF</html>`, KindWAFBlock},
		{"403 with envelope is not waf", 403, `{"code":1000,"msg":"quota exceeded"}`, KindHardCredit},
		// 图片无效。
		{"image invalid", 400, `{"code":11135,"msg":"invalid_image_data"}`, KindImageInvalid},
		// 内容策略拦截。
		{"content blocked", 400, `{"msg":"blocked by security policy"}`, KindContentBlocked},
		// 参数非法。
		{"bad params", 400, `{"msg":"Unmarshal chat params failed"}`, KindBadParams},
		// 其它 4xx。
		{"other client error", 400, `{"code":12345,"msg":"whatever"}`, KindClient},
		// 偶发 404（无 11102 标记）。
		{"plain 404", 404, `{"msg":"not found"}`, KindNotFound},
		// 上游故障。
		{"server error", 500, `{"msg":"internal error"}`, KindServer},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Classify(testCase.status, testCase.body)
			if got.Kind != testCase.want {
				t.Fatalf("Classify(%d, %q).Kind = %s, want %s",
					testCase.status, testCase.body, got.Kind, testCase.want)
			}
		})
	}
}

// TestClassifyModelBlockedNeedsFieldBoundary 验证 11102 只认同独立字段。
//
// 若做整段文本子串匹配，"11102" 出现在 requestId 里就会误避让一个可用模型。
func TestClassifyModelBlockedNeedsFieldBoundary(t *testing.T) {
	// 11102 只出现在 requestId 里：不该被判成模型不可用。
	body := `{"code":12345,"msg":"bad request","requestId":"trace-11102-abc"}`
	got := Classify(http.StatusBadRequest, body)
	if got.Kind == KindModelBlocked {
		t.Fatalf("11102 inside requestId must not trigger model blocking: %s", got.Kind)
	}
	// code 字段真的是 11102：应当命中。
	if got := Classify(http.StatusBadRequest, `{"code":11102}`); got.Kind != KindModelBlocked {
		t.Fatalf("explicit code 11102 must be model blocked, got %s", got.Kind)
	}
}

// TestClassifyRetryableAndStatus 验证可重试判定与状态码映射。
//
// 宿主按 HTTP 状态码决定凭证处置，这张映射表是「账号处置矩阵」的落点。
func TestClassifyRetryableAndStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantStatus    int
	}{
		{"rate limit is retryable", 429, `{"msg":"rate limit"}`, true, 429},
		{"content blocked is not retryable", 400, `{"msg":"blocked by security policy"}`, false, 400},
		{"prompt too long is not retryable", 400, `{"msg":"prompt is too long"}`, false, 400},
		{"image invalid is not retryable", 400, `{"msg":"invalid_image_data"}`, false, 400},
		{"bad params is not retryable", 400, `{"msg":"Unmarshal chat params failed"}`, false, 400},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			classified := Classify(testCase.status, testCase.body)
			if classified.Retryable() != testCase.wantRetryable {
				t.Fatalf("Retryable() = %v, want %v", classified.Retryable(), testCase.wantRetryable)
			}
			if classified.HTTPStatus() != testCase.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", classified.HTTPStatus(), testCase.wantStatus)
			}
		})
	}
}

// TestParseRateResetAndRetryAfter 验证上游等待信息的解析。
func TestParseRateResetAndRetryAfter(t *testing.T) {
	// 中文文案优先。
	reset := ParseRateReset(`{"msg":"额度恢复将在 2026-09-27 04:00:00 重置"}`)
	if reset.IsZero() {
		t.Fatal("chinese rate reset wording must be parsed")
	}
	if reset.Hour() != 4 {
		t.Fatalf("reset hour = %d, want 4", reset.Hour())
	}

	header := http.Header{}
	header.Set("Retry-After", "30")
	if got := ParseRetryAfter(header, time.Now()); got != 30*time.Second {
		t.Fatalf("Retry-After = %v, want 30s", got)
	}
	// 离谱的值被钳制（上游偶发返回极大值）。
	header.Set("Retry-After", "999999")
	if got := ParseRetryAfter(header, time.Now()); got > time.Hour*2 {
		t.Fatalf("Retry-After must be capped, got %v", got)
	}
	// HTTP-Date 形态被忽略（不是纯数字）。
	header.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := ParseRetryAfter(header, time.Now()); got != 0 {
		t.Fatalf("http-date Retry-After must be ignored, got %v", got)
	}
}

// TestClassifyIsModelRateLimit 验证模型级限流的识别。
func TestClassifyIsModelRateLimit(t *testing.T) {
	if !IsModelRateLimit(`{"code":6004,"msg":"rate-limiting"}`) {
		t.Fatal("6004 must be recognized as model-level rate limit")
	}
	if IsModelRateLimit(`{"code":1,"msg":"rate limit"}`) {
		t.Fatal("non-6004 must not be model-level rate limit")
	}
}
