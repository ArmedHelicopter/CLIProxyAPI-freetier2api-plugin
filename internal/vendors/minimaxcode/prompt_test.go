package minimaxcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPromptInlinesSystemAndNumbersHistory(t *testing.T) {
	turns := []*Turn{
		{Role: "system", Text: "be brief"},
		{Role: "user", Text: "one"},
		{Role: "assistant", Text: "two"},
		{Role: "user", Text: "three"},
	}
	expected := "[系统指令] be brief\n\n 用户：1. one\n\n助手：two\n\n 用户：2. three"
	got := BuildPrompt(turns)
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestSingleQuestionIsSentBare(t *testing.T) {
	turns := []*Turn{
		{Role: "user", Text: "hello"},
	}
	expected := "hello"
	got := BuildPrompt(turns)
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestImagesFollowTheTextAndKeepTheirURL(t *testing.T) {
	message := ParseTurn(map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "这是什么"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/x.png"}},
		},
	})
	text := BuildPrompt([]*Turn{message})
	if !strings.HasPrefix(text, "这是什么") {
		t.Fatalf("expected prefix '这是什么', got %q", text)
	}
	if !strings.HasSuffix(strings.TrimSpace(text), "(https://cdn/x.png)") {
		t.Fatalf("expected suffix '(https://cdn/x.png)', got %q", text)
	}
}

func TestImageOnlyTurnSaysSomething(t *testing.T) {
	message := ParseTurn(map[string]any{
		"role":    "user",
		"content": "",
		"images":  []any{"https://cdn/x.png"},
	})
	expected := "看图\n![已忽略的图片 0](https://cdn/x.png)"
	got := BuildPrompt([]*Turn{message})
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestToolAndUnknownRolesAreRenderedAsUserTurns(t *testing.T) {
	tool := ParseTurn(map[string]any{"role": "tool", "content": "42"})
	user := ParseTurn(map[string]any{"role": "user", "content": "so?"})
	expected := " 用户：1. 42\n\n 用户：2. so?"
	got := BuildPrompt([]*Turn{tool, user})
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
	if ParseTurn(map[string]any{"content": "no role"}).Role != "user" {
		t.Fatalf("expected role 'user'")
	}
	if ParseTurn(nil) != nil {
		t.Fatalf("expected nil")
	}
}

func TestRenderToolsSectionEmptyAndChoice(t *testing.T) {
	if got := renderToolsSection(nil, nil); got != "" {
		t.Fatalf("no tools must render nothing, got %q", got)
	}
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "Bash",
			"description": "run a command",
			"parameters":  map[string]any{"type": "object"},
		},
	}}
	got := renderToolsSection(tools, nil)
	for _, want := range []string{"[可用工具]", `"Bash"`, "run a command", toolCallStart, toolCallEnd} {
		if !strings.Contains(got, want) {
			t.Fatalf("section missing %q:\n%s", want, got)
		}
	}
	// tool_choice=none → 不渲染
	if gotNone := renderToolsSection(tools, "none"); gotNone != "" {
		t.Fatalf("tool_choice none must render nothing, got %q", gotNone)
	}
	// required → 带强制指令
	gotRequired := renderToolsSection(tools, "required")
	if !strings.Contains(gotRequired, "必须至少输出一个") {
		t.Fatalf("required choice must add force instruction:\n%s", gotRequired)
	}
	// 指定工具
	gotSpecific := renderToolsSection(tools, map[string]any{
		"type": "function", "function": map[string]any{"name": "Bash"},
	})
	if !strings.Contains(gotSpecific, "必须调用工具 Bash") {
		t.Fatalf("specific choice must name the tool:\n%s", gotSpecific)
	}
}

func TestParseToolCallsPlainAndBareObject(t *testing.T) {
	plain := "普通回答，没有工具。"
	gotText, calls := parseToolCalls(plain)
	if gotText != plain || len(calls) != 0 {
		t.Fatalf("plain text must pass through, got %q calls=%d", gotText, len(calls))
	}

	objText := `{"name":"get_weather","arguments":{"city":"北京"}}`
	gotText, calls = parseToolCalls(objText)
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("bare object must parse, got %q calls=%+v", gotText, calls)
	}
	if gotText != "" {
		t.Fatalf("bare object consumes the text, got %q", gotText)
	}
}

func TestParseToolCallsBareMarkersMultiple(t *testing.T) {
	text := "先查天气。\n" +
		toolCallStart + "\n" +
		`{"name":"get_weather","arguments":{"city":"北京"}}` + "\n" +
		toolCallEnd + "\n" +
		"再查湿度。\n" +
		toolCallStart + "\n" +
		`{"name":"get_humidity","arguments":{"city":"上海"}}` + "\n" +
		toolCallEnd + "\n" +
		"结束。"
	gotText, calls := parseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "get_weather" || calls[1].Name != "get_humidity" {
		t.Fatalf("wrong names: %+v", calls)
	}
	if !strings.Contains(gotText, "先查天气") || !strings.Contains(gotText, "结束") {
		t.Fatalf("plain text must survive, got %q", gotText)
	}
	if strings.Contains(gotText, toolCallStart) || strings.Contains(gotText, toolCallEnd) {
		t.Fatalf("markers must be stripped, got %q", gotText)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil || args["city"] != "北京" {
		t.Fatalf("arguments must be object JSON: %q err=%v", calls[0].Arguments, err)
	}
}

func TestParseToolCallsFenceVariantAndId(t *testing.T) {
	text := "调用：\n```tool_call\n" +
		`{"name":"Read","arguments":{"file_path":"/a"},"id":"call_123"}` + "\n" +
		"```\n完成。"
	gotText, calls := parseToolCalls(text)
	if len(calls) != 1 || calls[0].Name != "Read" || calls[0].ID != "call_123" {
		t.Fatalf("fence form must parse: %+v", calls)
	}
	if strings.Contains(gotText, "tool_call") {
		t.Fatalf("fence must be stripped, got %q", gotText)
	}
}

func TestParseToolCallsBrokenBlockStaysInText(t *testing.T) {
	text := "看这个：" + toolCallStart + "\nnot json\n" + toolCallEnd + " 完"
	gotText, calls := parseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("broken block must not parse: %+v", calls)
	}
	if !strings.Contains(gotText, "not json") || !strings.Contains(gotText, "看这个") {
		t.Fatalf("broken block must stay in text, got %q", gotText)
	}
}

func TestToolStreamParserPassthroughHoldBack(t *testing.T) {
	p := &toolStreamParser{}
	var out strings.Builder
	deltas := strings.Split("hello world, this is a plain answer without markers.", "")
	for _, d := range deltas {
		out.WriteString(p.push(d))
	}
	rest, calls := p.flush()
	out.WriteString(rest)
	if out.String() != "hello world, this is a plain answer without markers." {
		t.Fatalf("passthrough mismatch: %q", out.String())
	}
	if len(calls) != 0 {
		t.Fatalf("no calls expected: %+v", calls)
	}
}

func TestToolStreamParserMarkerSplitAcrossChunks(t *testing.T) {
	p := &toolStreamParser{}
	var out strings.Builder
	full := "说明文字" + toolCallStart + "\n" +
		`{"name":"Bash","arguments":{"command":"ls"}}` + "\n" +
		toolCallEnd + "收尾"
	// 逐字节送入，强制标记跨分片。
	for _, r := range full {
		out.WriteString(p.push(string(r)))
	}
	rest, calls := p.flush()
	out.WriteString(rest)
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("want Bash call, got %+v", calls)
	}
	got := out.String()
	if strings.Contains(got, toolCallStart) || strings.Contains(got, "tool_call") {
		t.Fatalf("marker leaked into output: %q", got)
	}
	if !strings.HasPrefix(got, "说明文字") || !strings.HasSuffix(got, "收尾") {
		t.Fatalf("text around marker must survive: %q", got)
	}
}

func TestToolStreamParserUnterminatedBlockFlush(t *testing.T) {
	p := &toolStreamParser{}
	var out strings.Builder
	out.WriteString(p.push("文字" + toolCallStart + "\n" + `{"name":"Bash","arguments":{}}`))
	rest, calls := p.flush()
	out.WriteString(rest)
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("unterminated but parseable block must count as call: %+v", calls)
	}
	if strings.Contains(out.String(), toolCallStart) {
		t.Fatalf("marker leaked: %q", out.String())
	}
}

func TestBuildPromptWithToolsHistory(t *testing.T) {
	turns := []*Turn{
		{Role: "user", Text: "查天气"},
		{
			Role: "assistant",
			Text: "我来查询。",
			ToolCalls: []map[string]any{
				{"id": "call_1", "function": map[string]any{
					"name": "get_weather", "arguments": `{"city":"北京"}`,
				}},
			},
		},
		{Role: "tool", Text: "晴 25°C", ToolCallID: "call_1", ToolName: "get_weather"},
		{Role: "user", Text: "回复用户"},
	}
	tools := []any{map[string]any{"function": map[string]any{
		"name": "get_weather", "description": "查天气",
		"parameters": map[string]any{"type": "object"},
	}}}
	prompt := BuildPromptWithTools(turns, tools, nil)
	for _, want := range []string{
		toolCallStart,
		`"name":"get_weather"`,
		toolCallEnd,
		"[工具结果 id=call_1 name=get_weather]",
		"晴 25°C",
		"[可用工具]",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	// 无工具时与旧版一致
	plain := BuildPromptWithTools(turns, nil, nil)
	if strings.Contains(plain, "[可用工具]") {
		t.Fatalf("no tools must not render section")
	}
}
