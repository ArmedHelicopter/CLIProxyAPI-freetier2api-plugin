package minimaxcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

var (
	contentKeys  = []string{"msg_content", "msgContent", "content", "text", "delta", "answer"}
	thinkingKeys = []string{
		"reasoning_content", "thinking_content", "reason_content",
		"think_content", "reasoning", "thinking",
	}
	// upstreamErrTextRe 识别以「数字错误码:」开头的整段消息（如 50110:insufficient balance）。
	// 只用于完整消息节点，不用于流式增量——增量的切分点是任意的，锚定开头会误伤。
	upstreamErrTextRe = regexp.MustCompile(`^\d{5,8}:\s*[A-Za-z]`)
)

// ChatRequest 内部解析结构。
type chatPayload struct {
	Model      string `json:"model"`
	Messages   []any  `json:"messages"`
	Stream     bool   `json:"stream"`
	Tools      []any  `json:"tools"`
	ToolChoice any    `json:"tool_choice"`
}

// toolsEnabledFor 判断本次请求是否携带工具（决定是否启用标记协议）。
func (cp *chatPayload) toolsEnabled() bool {
	return len(cp.Tools) > 0 && !isToolChoiceNone(cp.ToolChoice)
}

// frameAggregator 汇总上游一整条 SSE 流的帧内容。
//
// 上游有三类携带文本的帧，处理方式不同：
//   - agent_message_chunk：流式增量，可直接向下游发射；
//   - agent_message：完整消息节点，其中 role=user 的是上游对用户输入的回显
//     （必须丢弃——否则会把 prompt 原样当回复吐给客户端），role=assistant 的
//     是整条回复的完整拷贝，与 chunk 增量重复，仅当没有任何 chunk 时才使用；
//   - 其它未知帧：深搜兜底，优先级最低。
type frameAggregator struct {
	chunkText, chunkThinking strings.Builder
	msgText, msgThinking     strings.Builder
	miscText, miscThinking   strings.Builder
	finishReason             string
	err                      string
}

func (a *frameAggregator) add(payload map[string]any) {
	if a.err != "" {
		return
	}
	if errMsg := extractUpstreamError(payload); errMsg != "" {
		a.err = errMsg
		return
	}
	if fr := findDeepString(payload, "finish_reason", "finishReason"); fr != "" {
		a.finishReason = fr
	}

	chunkNode, hasChunk := payload["agent_message_chunk"].(map[string]any)
	msgNode, hasMsg := payload["agent_message"].(map[string]any)

	switch {
	case hasChunk:
		a.chunkText.WriteString(findDeepString(chunkNode, contentKeys...))
		a.chunkThinking.WriteString(findDeepString(chunkNode, thinkingKeys...))
	case hasMsg:
		if isUserRoleNode(msgNode) {
			// 上游回显的用户消息：既不是回复也不是增量，直接丢弃。
			return
		}
		text := findDeepString(msgNode, contentKeys...)
		a.msgText.WriteString(text)
		a.msgThinking.WriteString(findDeepString(msgNode, thinkingKeys...))
		// 完整消息节点上的「错误码:」开头文本按上游错误处理。
		if errMsg := looksLikeUpstreamErrorText(a.msgText.String()); errMsg != "" {
			a.err = errMsg
		}
	default:
		text := findDeepStringSkipUser(payload, contentKeys...)
		a.miscText.WriteString(text)
		a.miscThinking.WriteString(findDeepStringSkipUser(payload, thinkingKeys...))
		if errMsg := looksLikeUpstreamErrorText(a.miscText.String()); errMsg != "" {
			a.err = errMsg
		}
	}
}

// chunkContent 报告是否有 chunk 增量文本。
func (a *frameAggregator) hasChunkContent() bool {
	return a.chunkText.Len() > 0
}

// finalize 在流结束后取出正文与思考内容（chunk 优先，完整消息兜底）。
func (a *frameAggregator) finalize() (text, thinking string) {
	text = a.chunkText.String()
	if text == "" {
		text = a.msgText.String()
	}
	if text == "" {
		text = a.miscText.String()
	}
	thinking = a.chunkThinking.String()
	if thinking == "" {
		thinking = a.msgThinking.String()
	}
	if thinking == "" {
		thinking = a.miscThinking.String()
	}
	return text, thinking
}

// errorText 返回结构化或文本形态的上游错误（若有）。
func (a *frameAggregator) errorText() string {
	if a.err != "" {
		return a.err
	}
	return ""
}

// isUserRoleNode 判断节点是否标记为用户角色（回显帧）。
func isUserRoleNode(node map[string]any) bool {
	role, _ := node["role"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "user")
}

// findDeepStringSkipUser 深搜字符串，跳过 role=user 的节点（回显保护）。
func findDeepStringSkipUser(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		if isUserRoleNode(v) {
			return ""
		}
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepStringSkipUser(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepStringSkipUser(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// looksLikeUpstreamErrorText 判断一条完整消息是否是上游错误文本。
func looksLikeUpstreamErrorText(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || len(trimmed) > 300 {
		return ""
	}
	if upstreamErrTextRe.MatchString(trimmed) {
		return trimmed
	}
	return ""
}

// openAIToolCalls 把解析结果转成 OpenAI tool_calls 数组。
func openAIToolCalls(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%s", RandomUUID()[:8])
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
			"index": i,
		})
	}
	return out
}

// normalizeFinishReason 只放行客户端认识的取值。
//
// 上游会给出 "error" 这类非标准值（错误场景下错误文本已被拦截，
// 但防御性兜底），透传未知值会让协议转换端行为不确定。
func normalizeFinishReason(reason string) string {
	switch reason {
	case "stop", "length":
		return reason
	default:
		return "stop"
	}
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) ([]byte, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, promptTokens, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return nil, fmt.Errorf("minimax upstream error: %s", errMsg)
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return nil, fmt.Errorf("read stream: %w", errScan)
	}

	if errMsg := agg.errorText(); errMsg != "" {
		return nil, fmt.Errorf("minimax upstream error: %s", errMsg)
	}

	contentStr, reasoningStr := agg.finalize()
	var calls []ToolCall
	if toolsEnabled {
		contentStr, calls = parseToolCalls(contentStr)
	}
	if contentStr == "" && reasoningStr == "" && len(calls) == 0 {
		return nil, fmt.Errorf("minimax upstream returned empty response")
	}

	finishReason := normalizeFinishReason(agg.finishReason)

	msgMap := map[string]any{
		"role":    "assistant",
		"content": contentStr,
	}
	if reasoningStr != "" {
		msgMap["reasoning_content"] = reasoningStr
	}
	if len(calls) > 0 {
		finishReason = "tool_calls"
		msgMap["tool_calls"] = openAIToolCalls(calls)
		if contentStr == "" {
			// OpenAI 语义：只有工具调用时 content 是 null，不是空串。
			msgMap["content"] = nil
		}
	}

	completionTokens := EstimateTokens(contentStr + reasoningStr)

	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msgMap,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}

	return json.Marshal(out)
}

// ChatStream 执行一次流式对话，分片通过 onChunk 回调发射。
func ChatStream(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, onChunk func([]byte) error) error {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, _, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}
	parser := &toolStreamParser{}

	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	emitDelta := func(delta map[string]any) error {
		chunk := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		}
		chunkBytes, _ := json.Marshal(chunk)
		return onChunk(chunkBytes)
	}
	emitContent := func(text string) error {
		if text == "" {
			return nil
		}
		return emitDelta(map[string]any{"content": text})
	}

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		// 结构化错误、回显过滤与错误码文本识别都在 add 内完成。
		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return fmt.Errorf("minimax upstream error: %s", errMsg)
		}

		// chunk 增量立即发射；完整消息节点的文本到流结束再按优先级取舍。
		if chunkNode, ok := payload["agent_message_chunk"].(map[string]any); ok {
			thinking := findDeepString(chunkNode, thinkingKeys...)
			if thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
			text := findDeepString(chunkNode, contentKeys...)
			if text != "" {
				outText := text
				if toolsEnabled {
					outText = parser.push(text)
				}
				if errEmit := emitContent(outText); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return fmt.Errorf("read stream: %w", errScan)
	}
	if errMsg := agg.errorText(); errMsg != "" {
		return fmt.Errorf("minimax upstream error: %s", errMsg)
	}

	// 没有任何 chunk 增量时，完整消息就是回复正文（错误检测已在 add 内触发）。
	var calls []ToolCall
	if !agg.hasChunkContent() {
		finalText, _ := agg.finalize()
		if finalText != "" {
			if toolsEnabled {
				if errEmit := emitContent(parser.push(finalText)); errEmit != nil {
					return errEmit
				}
			} else if errEmit := emitContent(finalText); errEmit != nil {
				return errEmit
			}
		}
		// chunk 没有思考增量时，用完整消息上的思考兜底。
		if agg.chunkThinking.Len() == 0 {
			if thinking := agg.msgThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			} else if thinking := agg.miscThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if toolsEnabled {
		rest, parsedCalls := parser.flush()
		if errEmit := emitContent(rest); errEmit != nil {
			return errEmit
		}
		calls = parsedCalls
		for i, call := range calls {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%s", RandomUUID()[:8])
			}
			// 一次性发出完整调用：宿主按 index 累加 arguments，
			// 分多帧发送会造成重复拼接。
			errEmit := emitDelta(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": i,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					},
				},
			})
			if errEmit != nil {
				return errEmit
			}
		}
	}

	// 最终结束帧
	finish := normalizeFinishReason(agg.finishReason)
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	finalChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
			},
		},
	}
	finalBytes, _ := json.Marshal(finalChunk)
	return onChunk(finalBytes)
}

func doMessageRequest(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) (io.ReadCloser, string, int, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}

	modelID := req.Model
	if modelID == "" {
		modelID = cp.Model
	}
	modelCfg, _ := findModel(modelID)

	var turns []*Turn
	for _, m := range cp.Messages {
		if t := ParseTurn(m); t != nil {
			turns = append(turns, t)
		}
	}

	prompt := BuildPromptWithTools(turns, cp.Tools, cp.ToolChoice)
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("empty prompt")
	}
	promptTokens := EstimateTokens(prompt)

	sessionID, errSession := createSession(ctx, cred, baseURLOverride)
	if errSession != nil {
		return nil, "", 0, fmt.Errorf("create minimax session: %w", errSession)
	}

	bodyMap := map[string]any{
		"content":      prompt,
		"turn_id":      RandomUUID(),
		"worktreeMode": false,
	}
	if modelCfg.UpstreamModel != "" {
		sel := map[string]any{
			"model_id":    modelCfg.UpstreamModel,
			"provider_id": "minimax",
		}
		if modelCfg.Variant != "" {
			sel["variant"] = modelCfg.Variant
		}
		bodyMap["model"] = sel
	}

	bodyBytes, _ := json.Marshal(bodyMap)

	streamBase := StreamHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpMessage, sessionID)
	target := buildAgentTarget(cred, streamBase, path, http.MethodPost, string(bodyBytes), true)

	resp, errDo := doRequest(ctx, cred, target, streamBase, 120*time.Second)
	if errDo != nil {
		return nil, "", 0, fmt.Errorf("send message request: %w", errDo)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		return nil, "", 0, core.NewPluginError("minimax_unauthorized", fmt.Sprintf("minimax HTTP %d", resp.StatusCode), http.StatusUnauthorized)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, "", 0, fmt.Errorf("minimax HTTP %d: %s", resp.StatusCode, string(raw))
	}

	return resp.Body, modelCfg.ID, promptTokens, nil
}

func extractUpstreamError(payload map[string]any) string {
	if errObj, ok := payload["error"].(map[string]any); ok {
		if msg := core.GetString(errObj, "message"); msg != "" {
			return msg
		}
	}
	if errStr, ok := payload["error"].(string); ok && errStr != "" {
		return errStr
	}
	for _, key := range []string{"base_resp", "statusInfo"} {
		if node, ok := payload[key].(map[string]any); ok {
			code := core.GetInt64(node, "status_code", "code")
			if code != 0 {
				msg := core.FirstNonEmpty(core.GetString(node, "status_msg"), core.GetString(node, "message"))
				return fmt.Sprintf("status %d: %s", code, msg)
			}
		}
	}
	for _, key := range []string{"error_msg", "error_message", "err_msg", "status_msg"} {
		if msg := core.GetString(payload, key); msg != "" && !strings.EqualFold(msg, "success") && !strings.EqualFold(msg, "ok") {
			return msg
		}
	}
	return ""
}

// Target 是一次签名的 MiniMax 上游请求。
type Target struct {
	Method  string
	URL     string
	SignURL string
	UnixMS  int64
	Body    string
	Checkin bool
}

// AgentInfo 描述 MiniMax 账号下的一个 Agent。
type AgentInfo struct {
	ID            string `json:"name"`
	Role          string `json:"agent_role"`
	RootSessionID string `json:"root_session_id"`
}

// buildAgentQuery 构建 agent 族接口的查询字符串。
func buildAgentQuery(cred *Credential, unixMS int64) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	if cred.AuthMode == "oauth" {
		pairs := [][2]string{
			{"device_platform", "web"},
			{"biz_id", "3"},
			{"app_id", "3001"},
			{"version_code", "22201"},
			{"unix", strconv.FormatInt(unixMS, 10)},
			{"timezone_offset", "28800"},
			{"is_desktop", "1"},
			{"desktop_version", "3.0.73"},
			{"sys_language", "en"},
			{"lang", "en"},
			{"uuid", cred.UUID},
			{"device_id", cred.DeviceID},
			{"os_name", "macOS"},
			{"browser_name", "Chrome"},
			{"device_memory", "16"},
			{"cpu_core_num", "8"},
			{"browser_language", "zh-CN"},
			{"browser_platform", "MacIntel"},
			{"user_id", cred.UserID},
			{"screen_width", strconv.Itoa(width)},
			{"screen_height", strconv.Itoa(height)},
			{"client", "desktop"},
			{"region", "en"},
		}
		var parts []string
		for _, p := range pairs {
			// desktop_version 即使为空也保留
			if p[1] != "" || p[0] == "desktop_version" {
				parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
			}
		}
		return strings.Join(parts, "&")
	}

	// Web JWT 模式
	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "zh-CN"},
		{"browser_platform", "Win32"},
		{"user_id", cred.UserID},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", cred.AccessToken},
		{"client", "web"},
		{"region", "en"},
	}
	var parts []string
	for _, p := range pairs {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
		}
	}
	return strings.Join(parts, "&")
}

// buildSigninQuery 构建签到、额度与账号信息接口的查询字符串。
func buildSigninQuery(cred *Credential, unixMS int64, forSignature bool) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	userID := cred.UserID
	if userID == "" {
		userID = "0"
	}

	tokenParam := ""
	if cred.AuthMode != "oauth" {
		tokenParam = cred.AccessToken
	}

	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "en-US"},
		{"browser_platform", "Win32"},
		{"user_id", userID},
		{"op_ticket", "undefined"},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", tokenParam},
		{"client", "web"},
	}

	var parts []string
	for _, p := range pairs {
		if p[0] == "op_ticket" && !forSignature {
			continue
		}
		if p[1] == "" && p[0] != "op_ticket" {
			continue
		}
		parts = append(parts, p[0]+"="+FormEncode(p[1]))
	}
	return strings.Join(parts, "&")
}

// buildAgentTarget 构建对 Agent 族端点的签名目标。
func buildAgentTarget(cred *Credential, baseURL, path, method, body string, streamHost bool) Target {
	unixMS := time.Now().UnixMilli()
	query := buildAgentQuery(cred, unixMS)
	wire := baseURL + path
	if strings.Contains(wire, "?") {
		wire += "&" + query
	} else {
		wire += "?" + query
	}

	var signURL string
	if streamHost {
		signURL = wire
	} else {
		signURL = path
		if strings.Contains(signURL, "?") {
			signURL += "&" + query
		} else {
			signURL += "?" + query
		}
	}

	return Target{
		Method:  method,
		URL:     wire,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: false,
	}
}

// buildSigninTarget 构建对 Signin/Quota/UserInfo 端点的签名目标。
func buildSigninTarget(cred *Credential, baseURL, path, method, body string) Target {
	unixMS := time.Now().UnixMilli()
	signQuery := buildSigninQuery(cred, unixMS, true)
	wireQuery := buildSigninQuery(cred, unixMS, false)

	signURL := path + "?" + signQuery
	wireURL := baseURL + path + "?" + wireQuery

	return Target{
		Method:  method,
		URL:     wireURL,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: true,
	}
}

// buildHeaders 组装出站 HTTP 请求头。
func buildHeaders(cred *Credential, target Target, origin string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")

	unixSec := target.UnixMS / 1000
	h.Set("X-Timestamp", strconv.FormatInt(unixSec, 10))
	h.Set("X-Signature", XSignature(unixSec, target.Body))

	yyBody := target.Body
	if target.Checkin {
		yyBody = "{}"
		h.Set("Accept", "*/*")
	} else if cred.AuthMode == "oauth" {
		h.Set("Accept", "application/json, text/plain, */*")
	} else {
		h.Set("Accept", "text/event-stream, application/json, */*")
	}

	h.Set("YY", YYSignature(target.SignURL, yyBody, target.UnixMS))

	if cred.AuthMode == "oauth" {
		h.Set("User-Agent", DesktopUA)
		h.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		h.Set("User-Agent", WebUA)
		if cred.AccessToken != "" {
			h.Set("Token", cred.AccessToken)
		}
		h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	}
	return h
}

// doRequest 发起一次已签名的 HTTP 请求。
func doRequest(ctx context.Context, cred *Credential, target Target, origin string, timeout time.Duration) (*http.Response, error) {
	var bodyReader io.Reader
	if target.Body != "" {
		bodyReader = bytes.NewReader([]byte(target.Body))
	}
	req, errNew := http.NewRequestWithContext(ctx, target.Method, target.URL, bodyReader)
	if errNew != nil {
		return nil, fmt.Errorf("create request: %w", errNew)
	}
	req.Header = buildHeaders(cred, target, origin)
	return hostbridge.Client(ctx, timeout).Do(req)
}

// fetchUserInfo 获取当前账号 realUserID 和展示标识。
func fetchUserInfo(ctx context.Context, cred *Credential, baseURLOverride string) (userID, name, email string, err error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildSigninTarget(cred, base, EpUserInfo, http.MethodGet, "")
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", "", "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("fetch user info HTTP %d", resp.StatusCode)
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", "", "", errDec
	}

	profile := raw
	if data, ok := raw["data"].(map[string]any); ok {
		profile = data
	}
	if ui, ok := profile["userInfo"].(map[string]any); ok {
		profile = ui
	}

	realUserID := core.GetString(profile, "realUserID", "real_user_id", "userID", "user_id")
	accName := core.GetString(profile, "name", "username")
	accEmail := core.GetString(profile, "email", "phone")

	if realUserID != "" {
		cred.UserID = realUserID
	}
	if accEmail != "" && cred.Email == "" {
		cred.Email = accEmail
	}
	if accName != "" && cred.Label == "" {
		cred.Label = accName
	}

	return realUserID, accName, accEmail, nil
}

// fetchAgents 获取账号名下的 Agent 列表并挑出适合对话的 AgentID。
func fetchAgents(ctx context.Context, cred *Credential, baseURLOverride string) (string, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpAgentList, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch agent list HTTP %d", resp.StatusCode)
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", errDec
	}

	var agents []AgentInfo
	profile := raw
	if data, ok := raw["data"].(map[string]any); ok {
		profile = data
	}
	if rawAgents, ok := profile["agents"].([]any); ok {
		for _, a := range rawAgents {
			if am, ok := a.(map[string]any); ok {
				agents = append(agents, AgentInfo{
					ID:            core.GetString(am, "name", "id"),
					Role:          core.GetString(am, "agent_role", "role"),
					RootSessionID: core.GetString(am, "root_session_id"),
				})
			}
		}
	}

	// 优选 mavis，其次 general，最后任意有 ID 的 agent
	for _, a := range agents {
		if a.Role == "mavis" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.Role == "general" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.ID != "" {
			return a.ID, nil
		}
	}
	return "", nil
}

// prepare 执行 Agent 启动握手（/config、/agent、/channel/connections）。
func prepare(ctx context.Context, cred *Credential, baseURLOverride string) error {
	base := AgentHostFor(cred.Region, baseURLOverride)

	// 1. /config 接口
	targetCfg := buildAgentTarget(cred, base, EpConfig, http.MethodGet, "", false)
	if resp, err := doRequest(ctx, cred, targetCfg, base, 20*time.Second); err == nil {
		_ = resp.Body.Close()
	}

	// 2. /agent 列表解析
	if cred.AgentID == "" {
		if agentID, err := fetchAgents(ctx, cred, baseURLOverride); err == nil && agentID != "" {
			cred.AgentID = agentID
		}
	}

	// 3. /channel/connections 接口
	targetConn := buildAgentTarget(cred, base, EpConnections, http.MethodGet, "", false)
	if resp, err := doRequest(ctx, cred, targetConn, base, 20*time.Second); err == nil {
		_ = resp.Body.Close()
	}

	return nil
}

// createSession 打开一个独立的对话 session。
func createSession(ctx context.Context, cred *Credential, baseURLOverride string) (string, error) {
	if cred.UserID == "" {
		_, _, _, _ = fetchUserInfo(ctx, cred, baseURLOverride)
	}
	if cred.AgentID == "" {
		_ = prepare(ctx, cred, baseURLOverride)
	}
	if cred.AgentID == "" {
		return "", fmt.Errorf("minimax agent_id is unknown and could not be discovered")
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpSession, cred.AgentID)
	bodyBytes, _ := json.Marshal(map[string]any{
		"agent_id":     cred.AgentID,
		"worktreeMode": false,
	})

	target := buildAgentTarget(cred, base, path, http.MethodPost, string(bodyBytes), false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("minimax session HTTP %d: token invalid or expired", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("minimax session HTTP %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", fmt.Errorf("decode create session response: %w", errDec)
	}

	sessionID := findDeepString(raw, "session_id", "sessionId", "id", "sessionID")
	if sessionID == "" {
		rawBytes, _ := json.Marshal(raw)
		return "", fmt.Errorf("no session_id in response: %s", string(rawBytes))
	}

	return sessionID, nil
}

func findDeepString(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepString(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepString(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// 本文件解析 MiniMax Code 的供应商专属配置。

// isUnreserved 判断是否为 JavaScript encodeURIComponent 不转义的字符集。
// JS 规范字符集：A-Z a-z 0-9 - _ . ! ~ * ' ( )
func isUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' ||
		b == '!' || b == '~' || b == '*' ||
		b == '\'' || b == '(' || b == ')'
}

// EncodeURIComponent 模拟 JavaScript 的 encodeURIComponent 行为。
// 对非 ASCII 字符按 UTF-8 字节编码为大写 %XX 形式。
func EncodeURIComponent(value string) string {
	var buf strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if isUnreserved(b) {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

// FormEncode 模拟 URLSearchParams 的表单键值编码。
// 字符集与 EncodeURIComponent 一致，但空格转义为 +。
func FormEncode(value string) string {
	return strings.ReplaceAll(EncodeURIComponent(value), "%20", "+")
}

// MD5Hex 计算字符串的小写 32 位 MD5 哈希。
func MD5Hex(value string) string {
	h := md5.Sum([]byte(value))
	return hex.EncodeToString(h[:])
}

// XSignature 计算 x-signature 请求头：MD5(unix_seconds + salt + body)。
func XSignature(unixSeconds int64, body string) string {
	return MD5Hex(fmt.Sprintf("%d%s%s", unixSeconds, SignatureSalt, body))
}

// YYSignature 计算 yy 请求头：MD5(encodeURIComponent(sign_url) + "_" + body + MD5(ms) + "ooui")。
func YYSignature(signURL, body string, unixMS int64) string {
	msMD5 := MD5Hex(strconv.FormatInt(unixMS, 10))
	input := EncodeURIComponent(signURL) + "_" + body + msMD5 + SignatureSuffix
	return MD5Hex(input)
}

// Region 是 MiniMax Code 服务部署区域。
type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "minimaxcodecn"
	VendorNameCN     = "MiniMax Code 国内版"
	VendorIDGlobal   = "minimaxcodeglobal"
	VendorNameGlobal = "MiniMax Code 国际版"

	// 国内版 (CN)
	AccountHostCN = "https://account.minimax.cn"
	AgentHostCN   = "https://agent.minimaxi.com"
	StreamHostCN  = "https://agent-stream.minimaxi.com"

	// 国际版 (Global)
	AccountHostGlobal = "https://account.minimax.io"
	AgentHostGlobal   = "https://agent.minimax.io"
	StreamHostGlobal  = "https://agent-stream.minimax.io"

	// OAuth2 设备码配置（与桌面客户端对齐）
	ClientID         = "mcode-public"
	OAuthScope       = "agent.default"
	OAuthAudience    = "agent-backend"
	DeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrantType = "refresh_token"

	// 官方签名协议盐值与后缀
	SignatureSalt   = "I*7Cf%WZ#S&%1RlZJ&C2"
	SignatureSuffix = "ooui"

	// 客户端 UA 指纹
	DesktopUA = "MiniMaxAgent Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) MiniMaxAgent/3.0.73 Chrome/148.0.7778.280 Safari/537.36"
	WebUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

	// 端点路径
	EpDeviceCode    = "/oauth2/device/code"
	EpToken         = "/oauth2/token"
	EpUserInfo      = "/v1/api/user/info"
	EpConfig        = "/minimax-cloud/api/v1/config"
	EpAgentList     = "/minimax-cloud/api/v1/agent"
	EpConnections   = "/minimax-cloud/api/v1/channel/connections"
	EpSession       = "/minimax-cloud/api/v1/agent/%s/session"
	EpMessage       = "/minimax-cloud/api/v1/session/%s/message"
	EpCheckinStatus = "/minimax-cloud/api/v1/signin/status"
	EpCheckinClaim  = "/minimax-cloud/api/v1/signin/claim"
	EpMembership    = "/matrix/api/v1/commerce/get_membership_info"
	EpCreditDetails = "/minimax-cloud/api/v1/credit/details"
)

// VendorIDFor 返回某区域对应的 VendorID。
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor 返回某区域对应的展示名。
func VendorNameFor(r Region) string {
	if r == RegionGlobal {
		return VendorNameGlobal
	}
	return VendorNameCN
}

// AccountHostFor 返回账号服务基础地址。
func AccountHostFor(r Region) string {
	if r == RegionCN {
		return AccountHostCN
	}
	return AccountHostGlobal
}

// AgentHostFor 返回 Agent 业务基础地址。
func AgentHostFor(r Region, override string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(override), "/"); trimmed != "" {
		return trimmed
	}
	if r == RegionCN {
		return AgentHostCN
	}
	return AgentHostGlobal
}

// StreamHostFor 返回流式对话消息基础地址。
func StreamHostFor(r Region, override string) string {
	base := AgentHostFor(r, override)
	// 如果是 agent.xxx 转换为 agent-stream.xxx
	if strings.Contains(base, "://agent.") {
		return strings.Replace(base, "://agent.", "://agent-stream.", 1)
	}
	if r == RegionCN {
		return StreamHostCN
	}
	return StreamHostGlobal
}
