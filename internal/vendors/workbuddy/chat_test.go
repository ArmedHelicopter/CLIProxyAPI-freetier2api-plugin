package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggregateMergesToolCallDeltas 验证流式工具调用分片的合并。
func TestAggregateMergesToolCallDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	aggregated, errAggregate := Aggregate(strings.NewReader(stream))
	if errAggregate != nil {
		t.Fatalf("Aggregate: %v", errAggregate)
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(aggregated, &response); errUnmarshal != nil {
		t.Fatalf("decode aggregate: %v", errUnmarshal)
	}
	if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one merged tool call, got %+v", response.Choices)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "Bash" {
		t.Fatalf("tool call identity lost: %+v", call)
	}
	if call.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments not concatenated: %q", call.Function.Arguments)
	}
}

// TestAggregateEmptyStreamIsRecognizable 验证空流可被识别。
//
// 空流是上游缺陷，需要与「客户端断连」区分开。
func TestAggregateEmptyStreamIsRecognizable(t *testing.T) {
	_, errAggregate := Aggregate(strings.NewReader("data: [DONE]\n\n"))
	if errAggregate == nil {
		t.Fatal("empty stream must return an error")
	}
	if !IsEmptyStreamError(errAggregate) {
		t.Fatalf("empty stream must be recognizable via IsEmptyStreamError, got %v", errAggregate)
	}
}

// TestStreamNormalizesFrames 验证流式帧的白名单重建。
func TestStreamNormalizesFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","service_tier":"x","private_field":"leak","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"private_choice":1}],"extra":{}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	stats, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
	})
	if errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if stats.Frames != 1 || !stats.SawDone {
		t.Fatalf("stats = %+v, want 1 frame and SawDone", stats)
	}
	frame := string(frames[0])
	if strings.Contains(frame, "private_field") || strings.Contains(frame, "private_choice") {
		t.Fatalf("whitelist rebuild must drop unknown fields: %s", frame)
	}
	if !strings.Contains(frame, `"service_tier":"x"`) {
		t.Fatalf("known top-level fields must survive: %s", frame)
	}
}

// TestStreamPassesThroughErrorFrames 验证错误帧原样透传。
//
// 上游的错误信息比我们重建的更有诊断价值，不该被白名单改写。
func TestStreamPassesThroughErrorFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream blew up","code":"500"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
		Hint: func(string) string { return "try again later" },
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	frame := string(frames[0])
	if !strings.Contains(frame, "upstream blew up") {
		t.Fatalf("error message must be preserved verbatim: %s", frame)
	}
	if !strings.Contains(frame, "gateway_hint") {
		t.Fatalf("hint must be attached to the error frame: %s", frame)
	}
}

// TestNormalizeFrameDropsUnknownFields 验证帧白名单在非流式路径也生效。
func TestNormalizeFrameDropsUnknownFields(t *testing.T) {
	frame := map[string]any{
		"id": "c1", "secret": "leak",
		"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "hi", "private": 1}}},
	}
	normalized := normalizeFrame(frame)
	if _, okSecret := normalized["secret"]; okSecret {
		t.Fatal("unknown top-level field must be dropped")
	}
	choices := normalized["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if _, okPrivate := delta["private"]; okPrivate {
		t.Fatal("unknown delta field must be dropped")
	}
	if delta["content"] != "hi" {
		t.Fatal("known delta fields must survive")
	}
}
