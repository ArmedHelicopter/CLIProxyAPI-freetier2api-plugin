package qoder

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadQoderResponseBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("x", int(shortResponseMaxBytes)+1))
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if _, err = readQoderResponseBody(resp.Body); err == nil {
		t.Fatal("expected oversized response to be rejected")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected oversized response error: %v", err)
	}
}

func TestReadQoderResponseBodyAllowsNormalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := readQoderResponseBody(resp.Body)
	if err != nil {
		t.Fatalf("normal response rejected: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestMergeToolCallDeltas(t *testing.T) {
	delta1 := []interface{}{
		map[string]interface{}{
			"index": 0,
			"id":    "call_bash_1",
			"type":  "function",
			"function": map[string]interface{}{
				"name":      "Bash",
				"arguments": "{\"command\": \"",
			},
		},
	}
	delta2 := []interface{}{
		map[string]interface{}{
			"index": 0,
			"function": map[string]interface{}{
				"arguments": "ls -la\"}",
			},
		},
	}
	delta3Bad := []interface{}{
		map[string]interface{}{
			"index": 1,
			"id":    "call_unnamed",
			"type":  "function",
			"function": map[string]interface{}{
				"name":      "",
				"arguments": "{}",
			},
		},
	}

	var accumulated []interface{}
	accumulated = mergeToolCallDeltas(accumulated, delta1)
	accumulated = mergeToolCallDeltas(accumulated, delta2)
	accumulated = mergeToolCallDeltas(accumulated, delta3Bad)

	valid := filterValidToolCalls(accumulated)
	if len(valid) != 1 {
		t.Fatalf("expected 1 valid tool call after filtering unnamed calls, got %d", len(valid))
	}

	call := valid[0].(map[string]interface{})
	if call["id"] != "call_bash_1" {
		t.Errorf("expected id 'call_bash_1', got %v", call["id"])
	}
	fn := call["function"].(map[string]interface{})
	if fn["name"] != "Bash" {
		t.Errorf("expected name 'Bash', got %v", fn["name"])
	}
	if fn["arguments"] != `{"command": "ls -la"}` {
		t.Errorf("expected merged arguments '{\"command\": \"ls -la\"}', got %v", fn["arguments"])
	}
}

// ==================== 专有协议测试 ====================

func TestMapModel(t *testing.T) {
	// MapModel 只处理「索引未命中」的兜底：本插件自己注册的模型由
	// Bridge.resolveModel 先走 modelIndex（见 TestBridgeResolveModel）。
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "gmodel", "gmodel"},
		{"codex", "kmodel_latest", "kmodel_latest"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
		{"codex", "Performance", "performance"},   // 大小写归一
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModel(t *testing.T) {
	var index modelIndex
	index.replace(testModels())
	bridge := &Bridge{models: &index}

	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		// 对外 slug → 上游 key
		{"codex", "deepseek-v4-pro", "dmodel"},
		{"codex", "qwen3.8-max", "qmodel_38max"},
		{"codex", "glm-5.3", "gmodel"},
		// 大小写不敏感
		{"codex", "DeepSeek-V4-Pro", "dmodel"},
		// 旧上游 key 仍可直传（向后兼容）
		{"codex", "dmodel", "dmodel"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		// 索引未命中 → 内置映射兜底
		{"claude", "claude-sonnet-4-6", "gmodel"},
	}
	for _, c := range cases {
		if got := bridge.resolveModel(c.agent, c.model); got != c.expected {
			t.Errorf("resolveModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModelWithoutIndex(t *testing.T) {
	// 没有索引时（测试/独立构造）退回 MapModel，行为与变更前一致。
	bridge := &Bridge{}
	if got := bridge.resolveModel("claude", "claude-sonnet-4-6"); got != "gmodel" {
		t.Errorf("resolveModel without index = %q; want gmodel", got)
	}
}
