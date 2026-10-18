package tests

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// TestExecutorNonStreamingAggregatesUpstream 验证非流式执行的完整链路。
func TestExecutorNonStreamingAggregatesUpstream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			// 上游强制流式：非流式由插件侧聚合。
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
				"",
				`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"，世界"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		// 国际版每日活跃奖励：热力图未点亮 → 上报信标 → 回读仍为 0 也算成功。
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecute, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
	})
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode executor response: %v", errUnmarshal)
	}

	var aggregated struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Payload, &aggregated); errUnmarshal != nil {
		t.Fatalf("decode aggregated payload: %v (raw=%s)", errUnmarshal, response.Payload)
	}
	if aggregated.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", aggregated.Object)
	}
	if len(aggregated.Choices) != 1 || aggregated.Choices[0].Message.Content != "你好，世界" {
		t.Fatalf("content not aggregated as expected: %+v", aggregated.Choices)
	}
	if aggregated.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop (default when upstream omits it)", aggregated.Choices[0].FinishReason)
	}
	// total_tokens 缺失时应由 prompt + completion 补齐。
	if aggregated.Usage.TotalTokens != 7 {
		t.Fatalf("total_tokens = %d, want 7 (derived)", aggregated.Usage.TotalTokens)
	}
}

// TestExecutorStreamEmitsNormalizedFrames 验证流式转发把帧规范化后投递。
func TestExecutorStreamEmitsNormalizedFrames(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"A"}}],"session_id":"leak-me"}`,
				"",
				`data: {"choices":[{"index":0,"delta":{"content":"B"}}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		// 国际版每日活跃奖励：热力图未点亮 → 上报信标 → 回读仍为 0 也算成功。
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
		StreamID: "s1",
	})
	var response app.ExecutorStreamResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode stream response: %v", errUnmarshal)
	}
	if response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream content-type = %q", response.Headers.Get("Content-Type"))
	}

	if !waitFor(t, 3e9, func() bool { return len(host.emittedChunks()) >= 2 }) {
		t.Fatalf("expected at least 2 emitted chunks, got %v", host.emittedChunks())
	}
	chunks := host.emittedChunks()
	for _, chunk := range chunks {
		if strings.Contains(chunk, "session_id") {
			t.Fatalf("upstream private field leaked to downstream: %s", chunk)
		}
		if !json.Valid([]byte(chunk)) {
			// 宿主负责 SSE 信封，executor 只投递裸 JSON。
			t.Fatalf("chat-completions chunk must be bare JSON, got %q", chunk)
		}
	}
	// 第二帧缺 id，应被续传为第一帧的 id。
	if !strings.Contains(chunks[1], `"id":"c1"`) {
		t.Fatalf("missing id must be backfilled from the first frame: %s", chunks[1])
	}
	// 流必须以一次 close 收尾。
	if !waitFor(t, 3e9, func() bool { return len(host.closedStreams()) == 1 }) {
		t.Fatalf("expected exactly one stream close, got %d", len(host.closedStreams()))
	}
	if len(host.streamErrors()) != 0 {
		t.Fatalf("successful stream must not report an error: %v", host.streamErrors())
	}
}

// TestExecutorStreamReportsUpstreamError 验证上游错误被上报为流失败。
func TestExecutorStreamReportsUpstreamError(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{
				Status: http.StatusTooManyRequests,
				Body:   `{"code":6004,"msg":"The model provider is rate-limiting requests."}`,
			}
		}
		// 国际版每日活跃奖励：热力图未点亮 → 上报信标 → 回读仍为 0 也算成功。
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[]}`),
		},
		StreamID: "s1",
	})

	if !waitFor(t, 3e9, func() bool { return len(host.streamErrors()) > 0 }) {
		t.Fatal("upstream 429 must surface as a stream failure")
	}
	if !strings.Contains(host.streamErrors()[0], "rate-limiting") {
		t.Fatalf("stream error should carry the upstream message verbatim: %v", host.streamErrors())
	}
}

// TestExecutorRejectsUnsupportedFormat 验证格式声明不一致时明确报错。
func TestExecutorRejectsUnsupportedFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := testApp.HandleMethod(pluginabi.MethodExecutorExecute, mustMarshal(app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "1", Model: "cn:x", Format: "gemini"},
	}))
	if errHandle == nil {
		t.Fatalf("unsupported format must produce an error envelope, got %s", raw)
	}

	_ = workbuddy.RegionCN
	_ = http.StatusOK
}

// TestQuotaResetDomesticAndGlobal 验证国内版走 daily-checkin 签到、
// 国际版走每日活跃奖励（活跃信标），两条路径都能成功重置。
func TestQuotaResetDomesticAndGlobal(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.addAccount("2", "global-account", sampleCredentialJSON("global", "g1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "daily-checkin") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"credit":10,"energy":5,"streak":3}}`,
			}
		}
		// 国际版每日活跃奖励：热力图未点亮 → 上报信标 → 回读仍为 0 也算成功。
		if strings.Contains(url, "/activity/growth/heatmap") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"cells":[{"date":"2026-10-18","score":0}]}}`,
			}
		}
		if strings.Contains(url, "/v2/report") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// 国际版账号重置执行每日活跃奖励（活跃信标已上报）
	resGlobal := callMethod(t, pluginabi.MethodQuotaReset, pluginapi.QuotaResetRequest{AuthID: "2"})
	var respGlobal pluginapi.QuotaResetResponse
	if errUnmarshal := json.Unmarshal(resGlobal, &respGlobal); errUnmarshal != nil {
		t.Fatalf("decode global quota reset: %v", errUnmarshal)
	}
	if !respGlobal.Success {
		t.Fatalf("global account quota reset should run the daily bonus, message: %s", respGlobal.Message)
	}
	if !strings.Contains(respGlobal.Message, "活跃信标") {
		t.Fatalf("global quota reset message should mention the beacon, got: %s", respGlobal.Message)
	}

	// 国内版账号重置应成功执行签到
	resCN := callMethod(t, pluginabi.MethodQuotaReset, pluginapi.QuotaResetRequest{AuthID: "1"})
	var respCN pluginapi.QuotaResetResponse
	if errUnmarshal := json.Unmarshal(resCN, &respCN); errUnmarshal != nil {
		t.Fatalf("decode cn quota reset: %v", errUnmarshal)
	}
	if !respCN.Success {
		t.Fatalf("cn account quota reset should succeed, message: %s", respCN.Message)
	}
}

// TestFormatSSEChunk 验证流式分片格式化确保满足 SSE 标准规范（以 data: 开头，以 \n\n 结尾）。
func TestFormatSSEChunk(t *testing.T) {
	// 1. 裸 JSON 分片自动补全 data: 与 \n\n
	out := app.FormatSSEChunk([]byte(`{"id":"1"}`))
	if string(out) != "data: {\"id\":\"1\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 2. 已有 data: 前缀的分片确保末尾有 \n\n
	out = app.FormatSSEChunk([]byte("data: {\"id\":\"2\"}\n\n"))
	if string(out) != "data: {\"id\":\"2\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 3. 裸 [DONE] 标记补全为 data: [DONE]\n\n
	out = app.FormatSSEChunk([]byte("[DONE]"))
	if string(out) != "data: [DONE]\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 4. 空分片返回 nil
	if out = app.FormatSSEChunk(nil); out != nil {
		t.Errorf("expected nil for empty payload, got %q", string(out))
	}
	if out = app.FormatSSEChunk([]byte("   \n\r  ")); out != nil {
		t.Errorf("expected nil for whitespace payload, got %q", string(out))
	}
}
