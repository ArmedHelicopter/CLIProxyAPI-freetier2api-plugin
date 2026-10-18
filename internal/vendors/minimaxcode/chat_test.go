package minimaxcode

import (
	"strings"
	"testing"
)

func TestFrameAggregatorSkipsUserEcho(t *testing.T) {
	agg := &frameAggregator{}
	agg.add(map[string]any{
		"type": float64(3),
		"agent_message": map[string]any{
			"msg_content": "[系统指令] secret prompt",
			"role":        "user",
		},
	})
	agg.add(map[string]any{
		"agent_message_chunk": map[string]any{
			"msg_content": "真实回复",
		},
	})
	text, _ := agg.finalize()
	if text != "真实回复" {
		t.Fatalf("echo must be dropped, got %q", text)
	}
}

func TestFrameAggregatorPrefersChunkOverFullMessage(t *testing.T) {
	agg := &frameAggregator{}
	agg.add(map[string]any{
		"agent_message_chunk": map[string]any{"msg_content": "增量"},
	})
	agg.add(map[string]any{
		"agent_message": map[string]any{
			"msg_content": "增量+完整拷贝",
			"role":        "assistant",
		},
	})
	text, _ := agg.finalize()
	if text != "增量" {
		t.Fatalf("chunk must win, got %q", text)
	}
}

func TestFrameAggregatorFallsBackToFullMessage(t *testing.T) {
	agg := &frameAggregator{}
	agg.add(map[string]any{
		"agent_message": map[string]any{
			"msg_content": "只有完整消息",
			"role":        "assistant",
		},
	})
	text, _ := agg.finalize()
	if text != "只有完整消息" {
		t.Fatalf("full message fallback, got %q", text)
	}
}

func TestFrameAggregatorDetectsErrorCodeText(t *testing.T) {
	agg := &frameAggregator{}
	agg.add(map[string]any{
		"agent_message": map[string]any{
			"msg_content": "50110:insufficient balance (1008)",
			"role":        "assistant",
		},
	})
	if errMsg := agg.errorText(); !strings.Contains(errMsg, "insufficient balance") {
		t.Fatalf("error text must be detected, got %q", errMsg)
	}
	// 长正文不应误判
	agg2 := &frameAggregator{}
	agg2.add(map[string]any{
		"agent_message": map[string]any{
			"msg_content": strings.Repeat("很长的正文。", 60),
			"role":        "assistant",
		},
	})
	if errMsg := agg2.errorText(); errMsg != "" {
		t.Fatalf("long text must not be error, got %q", errMsg)
	}
}

func TestSignatureIsMD5OverSaltAndBody(t *testing.T) {
	got := XSignature(1700000000, `{"a":1}`)
	expected := MD5Hex("1700000000" + SignatureSalt + `{"a":1}`)
	if got != expected {
		t.Fatalf("got %s, want %s", got, expected)
	}
}

func TestYYCoversTheEncodedURL(t *testing.T) {
	url := "https://agent.minimax.io/x?token=ab&client=web"
	expected := MD5Hex(
		EncodeURIComponent(url) +
			"_" +
			"{}" +
			MD5Hex("1700000000000") +
			"ooui",
	)
	got := YYSignature(url, "{}", 1700000000000)
	if got != expected {
		t.Fatalf("got %s, want %s", got, expected)
	}
}

func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	unreserved := "!'()*-._~"
	for i := 0; i < len(unreserved); i++ {
		char := string(unreserved[i])
		if got := EncodeURIComponent(char); got != char {
			t.Fatalf("EncodeURIComponent(%q) = %q, want %q", char, got, char)
		}
	}
	if got := EncodeURIComponent(" "); got != "%20" {
		t.Fatalf("EncodeURIComponent(' ') = %q, want %%20", got)
	}
	if got := EncodeURIComponent("中"); got != "%E4%B8%AD" {
		t.Fatalf("EncodeURIComponent('中') = %q, want %%E4%%B8%%AD", got)
	}
	if got := EncodeURIComponent("a+b"); got != "a%2Bb" {
		t.Fatalf("EncodeURIComponent('a+b') = %q, want 'a%%2Bb'", got)
	}
	if got := EncodeURIComponent("a/b"); got != "a%2Fb" {
		t.Fatalf("EncodeURIComponent('a/b') = %q, want 'a%%2Fb'", got)
	}
}

func TestFormEncodeUsesPlusForSpace(t *testing.T) {
	if got := FormEncode("a b"); got != "a+b" {
		t.Fatalf("FormEncode('a b') = %q, want 'a+b'", got)
	}
	if got := FormEncode("a+b"); got != "a%2Bb" {
		t.Fatalf("FormEncode('a+b') = %q, want 'a%%2Bb'", got)
	}
}
