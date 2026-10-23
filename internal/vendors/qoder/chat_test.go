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
		_, _ = io.WriteString(w, strings.Repeat("x", int(qoderMaxResponseBytes)+1))
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
