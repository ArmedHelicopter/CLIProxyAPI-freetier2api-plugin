package workbuddy

import (
	"strings"
	"testing"
)

// TestSanitizeRemovesFingerprints 验证指纹脱敏确实改写了命中文本。
func TestSanitizeRemovesFingerprints(t *testing.T) {
	rules, regexes := loadSanitizeRules()
	if len(rules.Rewrites) == 0 && len(regexes) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	_ = regexes

	// 改写表里的任一条命中都应产生变化。
	if len(rules.Rewrites) > 0 {
		pair := rules.Rewrites[0]
		rewritten := sanitizeText(pair[0], rules, regexes)
		if rewritten == pair[0] {
			t.Fatalf("rewrite rule did not apply: %q", pair[0])
		}
	}
	// 不命中特征串的文本原样返回（快路径）。
	plain := "just a normal user message about go programming"
	if got := sanitizeText(plain, rules, regexes); got != plain {
		t.Fatalf("plain text must pass through unchanged, got %q", got)
	}
}

// TestSanitizeMessagesCoversToolArguments 验证工具参数也被净化。
//
// 早期实现在 content 缺失时跳过整条消息，导致 tool_calls 的参数
// （文件名、命令、写入内容）完全不被净化。
func TestSanitizeMessagesCoversToolArguments(t *testing.T) {
	rules, _ := loadSanitizeRules()
	if len(rules.Rewrites) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	needle := rules.Rewrites[0][0]

	messages := []any{map[string]any{
		// content 为 null（工具调用消息的常见形态）。
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": "c1",
			"function": map[string]any{
				"name":      "Write",
				"arguments": `{"content":"` + needle + `"}`,
			},
		}},
	}}
	sanitizeMessages(messages)

	call := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(arguments, needle) {
		t.Fatal("tool_call arguments must be sanitized even when content is null")
	}
}
