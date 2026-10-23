package qoder

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 本文件是 Qoder 的传输与对话层：HTTP/会话基建（cosy 签名、Bearer 客户端）、
// 上游消息与增量转换、chat-completions 的出入口。

// 本文件提供「以载荷为输入输出」的对话入口。
//
// 上游 bridge 的 handler 直接写 http.ResponseWriter（它是从网关服务移植来的），
// 而本插件是 CPA 的 executor：不产生 HTTP 响应，只产出载荷交给宿主。
//
// 这里刻意**不**复用 HandleChatCompletions，而是把它的组装逻辑按载荷形态
// 重写一遍：那套 handler 把 SSE 帧直接 `fmt.Fprintf(w, "data: %s\n\n", ...)`，
// 与宿主的分帧职责重复（宿主会按声明格式自己加 `data: `）。因此这里只产出
// 裸 JSON 载荷，帧包装交给宿主。

// RunChatCompletions 执行一次非流式 chat-completions，把响应体写进 w。
func RunChatCompletions(b *Bridge, payload []byte, w io.Writer) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var content strings.Builder
	var toolCallBuf []interface{}
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		if d.Content != "" {
			content.WriteString(d.Content)
		}
		if d.ToolCalls != nil {
			toolCallBuf = mergeToolCallDeltas(toolCallBuf, d.ToolCalls)
		}
	})
	if errCall != nil {
		return errCall
	}

	toolCallBuf = filterValidToolCalls(toolCallBuf)

	finishReason := "stop"
	message := map[string]interface{}{"role": "assistant", "content": content.String()}
	if len(toolCallBuf) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = toolCallBuf
		if content.Len() == 0 {
			// OpenAI 语义：只有工具调用时 content 必须是 null，不是空串。
			message["content"] = nil
		}
	}
	response := map[string]interface{}{
		"id": reqID, "object": "chat.completion",
		"created": created, "model": req.Model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return fmt.Errorf("encode chat response: %w", errMarshal)
	}
	_, errWrite := w.Write(encoded)
	return errWrite
}

// StreamChatCompletions 执行一次流式 chat-completions，逐帧交给 emit。
//
// emit 收到的是裸 JSON 载荷，外层由 hostStreamSink.Emit 统一格式化为合法的 SSE 格式。
func StreamChatCompletions(b *Bridge, payload []byte, emit func([]byte) error) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var hasToolCalls bool
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		chunk := MakeChatChunk(reqID, created, req.Model)
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) > 0 {
			entry, _ := choices[0].(map[string]interface{})
			delta, _ := entry["delta"].(map[string]interface{})
			if delta != nil {
				if d.Content != "" {
					delta["role"] = "assistant"
					delta["content"] = d.Content
				}
				if d.Reasoning != "" {
					// reasoning_content 是 DeepSeek 系客户端的约定字段。
					delta["reasoning_content"] = d.Reasoning
				}
				if d.ToolCalls != nil {
					delta["tool_calls"] = d.ToolCalls
					hasToolCalls = true
				}
			}
		}
		encoded, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			return
		}
		_ = emit(encoded)
	})
	if errCall != nil {
		return errCall
	}

	// 收尾帧：finish_reason 与 usage。
	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	}
	done := MakeChatChunk(reqID, created, req.Model)
	choices, _ := done["choices"].([]interface{})
	if len(choices) > 0 {
		entry, _ := choices[0].(map[string]interface{})
		entry["finish_reason"] = finishReason
		entry["delta"] = map[string]interface{}{}
	}
	if inputTokens > 0 || outputTokens > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
	}
	encoded, errMarshal := json.Marshal(done)
	if errMarshal != nil {
		return errMarshal
	}
	return emit(encoded)
}

// mergeToolCallDeltas 按 index 累加流式工具分片，将 arguments 增量拼成完整工具调用。
func mergeToolCallDeltas(accumulated []interface{}, incoming []interface{}) []interface{} {
	for _, raw := range incoming {
		call, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		idx := core.IntFromAny(call["index"])
		var target map[string]interface{}
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]interface{}); okMap {
				if core.IntFromAny(existing["index"]) == idx {
					target = existing
					break
				}
			}
		}
		if target == nil {
			target = cloneToolCall(call)
			target["index"] = idx
			accumulated = append(accumulated, target)
			continue
		}
		if id, okID := call["id"].(string); okID && id != "" {
			target["id"] = id
		}
		if tp, okTp := call["type"].(string); okTp && tp != "" {
			target["type"] = tp
		}
		if fnIncoming, okFn := call["function"].(map[string]interface{}); okFn {
			fnTarget, _ := target["function"].(map[string]interface{})
			if fnTarget == nil {
				fnTarget = map[string]interface{}{}
				target["function"] = fnTarget
			}
			if name, okName := fnIncoming["name"].(string); okName && name != "" {
				fnTarget["name"] = name
			}
			if args, okArgs := fnIncoming["arguments"].(string); okArgs && args != "" {
				prevArgs, _ := fnTarget["arguments"].(string)
				fnTarget["arguments"] = prevArgs + args
			}
		}
	}
	return accumulated
}

// filterValidToolCalls 剔除没有有效 function.name 的残缺工具调用，并按 index 升序排序。
func filterValidToolCalls(calls []interface{}) []interface{} {
	var valid []interface{}
	for _, item := range calls {
		call, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]interface{})
		if !okFn {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		valid = append(valid, call)
	}
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0; j-- {
			left, _ := valid[j-1].(map[string]interface{})
			right, _ := valid[j].(map[string]interface{})
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]interface{}); ok {
				fnCopy := make(map[string]interface{}, len(fn))
				for fk, fv := range fn {
					fnCopy[fk] = fv
				}
				dst[k] = fnCopy
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// chatRequest 是一次对话请求的关键字段。
type chatRequest struct {
	Model     string
	Stream    bool
	Messages  []interface{}
	Tools     interface{}
	ToolsUsed bool
	Prompt    string
}

// decodeChatRequest 解析请求体。
func decodeChatRequest(payload []byte) (chatRequest, error) {
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(payload, &raw); errUnmarshal != nil {
		return chatRequest{}, fmt.Errorf("decode chat request: %w", errUnmarshal)
	}
	messages, _ := raw["messages"].([]interface{})
	tools := raw["tools"]
	stream, _ := raw["stream"].(bool)
	return chatRequest{
		Model:     StrValDefault(raw, "model", "auto"),
		Stream:    stream,
		Messages:  messages,
		Tools:     tools,
		ToolsUsed: tools != nil,
		Prompt:    ExtractLatestUserPrompt(messages),
	}, nil
}

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return rebuilt
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" {
		return nil
	}

	if role == "user" {
		return BuildUserMessage(text)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

func BuildUserMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"role":    "user",
		"content": "",
		"contents": []interface{}{
			map[string]interface{}{"type": "text", "text": text},
		},
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}

type Delta struct {
	Role         string
	Content      string
	Reasoning    string // 上游推理过程（reasoning_content）
	ToolCalls    []interface{}
	InputTokens  int
	OutputTokens int
	Err          error // 上游返回业务错误时非 nil
}

func (d Delta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil && d.InputTokens == 0 && d.OutputTokens == 0 && d.Err == nil
}

func ExtractDelta(dataLine string) Delta {
	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &wrapper); err != nil {
		logger.Debug("[delta] unmarshal wrapper failed: %v, raw=%s", err, dataLine)
		return Delta{}
	}
	// ---- 信封状态检查（第 3 步，hub iter_inner_sse 形态） ----
	// 上游帧：data:{"headers":{...},"body":"<内层 chunk>","statusCodeValue":200}
	// 关键形态：上游 HTTP200 建流后，把 provider 故障包在信封里投递
	// statusCodeValue=418/5xx（access log 记 200 而业务错的根因）。
	// 与内层业务 code（body 里的 {"code":"115"}）严格区分：信封错误带真实
	// HTTP 状态，可参与瞬时重试判定；重开闸门在 CallQoderWithOpts。
	if raw, ok := wrapper["statusCodeValue"]; ok {
		if status := toEnvelopeStatus(raw); status != 200 {
			ue := NewUpstreamError(status, envelopeDetail(wrapper))
			logger.Error("[delta] envelope statusCodeValue=%v -> %s", raw, ue.Message)
			return Delta{Err: ue}
		}
	}
	inner, _ := wrapper["body"].(string)
	if inner == "" {
		logger.Debug("[delta] no body field, keys=%v, raw=%s", mapKeys(wrapper), dataLine)
		return Delta{}
	}
	var innerJSON map[string]interface{}
	if err := json.Unmarshal([]byte(inner), &innerJSON); err != nil {
		logger.Debug("[delta] unmarshal inner failed: %v", err)
		return Delta{}
	}
	// 先捕获顶层 usage（上游可能在最后一个 chunk 中与 choices 一起返回），
	// 不提前 return：同帧若还带 choices 内容，合并进同一个 Delta，避免内容丢失
	var usageIn, usageOut int
	if usage, ok := innerJSON["usage"].(map[string]interface{}); ok {
		usageIn = int(FloatVal(usage, "prompt_tokens"))
		usageOut = int(FloatVal(usage, "completion_tokens"))
	}
	choices, _ := innerJSON["choices"].([]interface{})
	for _, ch := range choices {
		chMap, _ := ch.(map[string]interface{})
		delta, _ := chMap["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		role, _ := delta["role"].(string)
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		var toolCalls []interface{}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls = tc
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil {
			return Delta{Role: role, Content: content, Reasoning: reasoning, ToolCalls: toolCalls,
				InputTokens: usageIn, OutputTokens: usageOut}
		}
	}
	// 上游业务错误：{"code":"115","message":"..."}
	// 流内业务错误（HTTP 200 帧内）：非内容审核不做瞬时重试指引，
	// 内容审核单独映射为 content_policy_rejected。
	// 注意：业务错误检查仍先于 usage-only 返回，错误帧不因带 usage 被误判为正常帧。
	if code, ok := innerJSON["code"].(string); ok && code != "" && code != "0" {
		msg, _ := innerJSON["message"].(string)
		err := NewStreamBusinessError(fmt.Sprintf("upstream error code=%s: %s", code, msg))
		logger.Error("[delta] %v", err)
		return Delta{Err: err}
	}
	// choices 与业务 code 均未命中且 usage > 0：维持返回 usage-only Delta（行为同修复前）
	if usageIn > 0 || usageOut > 0 {
		return Delta{InputTokens: usageIn, OutputTokens: usageOut}
	}
	logger.Debug("[delta] no valid choices found, inner=%s", inner)
	return Delta{}
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// toEnvelopeStatus 信封 statusCodeValue 可能是 int/float64/str，统一成 int
// （解析失败回 502，按瞬时故障处理）。
func toEnvelopeStatus(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 502
		}
		return n
	}
	return 502
}

// envelopeDetail 提取信封错误详情：body 为字符串时直接用（错误详情在内层），
// 否则截断整帧 JSON 便于排查。
func envelopeDetail(wrapper map[string]interface{}) string {
	if body, ok := wrapper["body"].(string); ok && body != "" {
		return body
	}
	b, _ := json.Marshal(wrapper)
	return core.Truncate(string(b), 400)
}

func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]interface{})
		if m["role"] == "user" {
			return NormalizeMessageContent(m)
		}
	}
	return ""
}

func MakeChatChunk(id string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func (b *Bridge) templateMessages() []interface{} {
	if msgs, ok := b.templateBase["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

type BearerClient struct {
	sess *SessionContext
}

func NewBearerClient(sess *SessionContext) *BearerClient {
	return &BearerClient{sess: sess}
}

func (c *BearerClient) buildHeaders(pathSig, body, accept string, extra map[string]string) (map[string]string, error) {
	payloadB64, err := BuildPayloadB64(c.sess.Info)
	if err != nil {
		return nil, err
	}
	date := fmt.Sprintf("%d", UnixSec())
	sig := SignRequest(payloadB64, c.sess.CosyKey, date, body, pathSig)
	bearer := ComposeBearer(payloadB64, sig)

	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-machinetype":      c.sess.MachineType,
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             c.sess.Identity.Uid,
		"cosy-key":              c.sess.CosyKey,
		"cache-control":         "no-cache",
		"accept":                accept,
		"authorization":         bearer,
		"cosy-version":          Version,
		"cosy-machineid":        c.sess.MachineId,
		"cosy-machinetoken":     c.sess.MachineToken,
		"login-version":         "v2",
		"user-agent":            "Go-http-client/2.0",
		"cosy-scene":            "assistant",
		"cosy-business-product": "ide",
		"cosy-business-type":    "agent",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
}

func PathSigFrom(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		p = p[len("/algo"):]
	}
	return p, nil
}

const qoderMaxResponseBytes int64 = 1 << 20

// readQoderResponseBody 读取 Qoder 短响应，并拒绝超过上限的正文。
// 读取 limit+1 字节用于区分「恰好达到上限」和「被截断」，避免静默解析半截 JSON。
func readQoderResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, qoderMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > qoderMaxResponseBytes {
		return nil, fmt.Errorf("qoder response body exceeds %d bytes", qoderMaxResponseBytes)
	}
	return data, nil
}

// callGet 用 cosy 签名发送 GET 请求，body 部分参与签名时为空字符串。
// 用于 /algo/api/v2/model/list 之类的纯查询接口。
func (c *BearerClient) callGet(ctx context.Context, fullURL string) (map[string]interface{}, error) {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return nil, err
	}
	headers, err := c.buildHeaders(pathSig, "", "application/json", nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, NewUpstreamError(resp.StatusCode, string(data))
	}
	preview := string(data)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	logger.Info("callGet %s response (%d bytes): %s", fullURL, len(data), preview)
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

// openStreamLines sends a POST, reads SSE lines, calls onLine for each non-empty line.
// onLine 返回 false 表示消费方要求停止读取（如 deltaDispatcher 已捕获信封错误帧），
// 此时立即结束本次流读取并返回，由上层（CallQoderWithOpts 信封重开闸门）决定是否重开。
// ctx 可用于取消流式读取（例如客户端断开连接时）。
//
// 连接阶段（建流之前）带同账号瞬时重试：418/5xx/传输抖动 → 1s/2s 退避最多
// TransientMaxRetries 次。建流成功后的流内错误由上层（第 3 步信封重试）处理。
func (c *BearerClient) openStreamLines(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string) bool) error {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(jsonBody)
	if err != nil {
		return err
	}
	bodyStr, err := Encode(plain)
	if err != nil {
		return err
	}
	// 不设整体 Timeout，改由 context 控制生命周期，避免长流式响应被截断
	client := hostbridge.StreamClient(ctx, 0)

	var lastErr error
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			if !core.SleepContext(ctx, RetryBackoff(attempt)) {
				return ctx.Err()
			}
		}
		headers, err := c.buildHeaders(pathSig, bodyStr, "text/event-stream", extra)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if IsTransientTransport(err) && attempt < TransientMaxRetries {
				logger.Info("transient transport error opening stream %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, err.Error(), attempt+1)
				continue
			}
			return WrapTransportError(err)
		}
		if resp.StatusCode != 200 {
			body, errRead := readQoderResponseBody(resp.Body)
			resp.Body.Close()
			if errRead != nil {
				return fmt.Errorf("read qoder stream response: %w", errRead)
			}
			detail := string(body)
			if signal, queued := ParseQueueSignal(detail); queued && queueWaits < maxQueueWaits && ctx.Err() == nil {
				wait := queueWaitFor(signal)
				queueWaits++
				logger.Info("upstream queues model %s opening stream %s (%s); waiting %s before retry %d/%d",
					signal.ModelKey, fullURL, signal.QueueType, wait, queueWaits, maxQueueWaits)
				if !core.SleepContext(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			if signal, queued := ParseQueueSignal(detail); queued {
				return QueueSignalError(signal)
			}
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d opening stream %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, attempt+1)
				continue
			}
			return NewUpstreamError(resp.StatusCode, detail)
		}

		// 建流成功：进入流式读取，不再在本层重试
		defer resp.Body.Close()
		lineCh := make(chan string)
		errCh := make(chan error, 1)
		// done：主循环提前退出（onLine 返回 false）时关闭，解锁 scanner goroutine，
		// 避免其阻塞在 lineCh 发送处、仅剩 ctx.Done 一条出路造成 goroutine 泄漏
		done := make(chan struct{})
		defer close(done)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			for scanner.Scan() {
				select {
				case lineCh <- scanner.Text():
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				case <-done:
					return
				}
			}
			if err := scanner.Err(); err != nil {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				logger.Debug("stream read complete")
				return err
			case line := <-lineCh:
				if line != "" {
					// onLine 返回 false：消费方已置位 upstreamErr（信封错误帧），
					// 立即停止读取，避免上游不关流时请求挂起；
					// 错误由 CallQoderWithOpts 的 emitted 闸门 + isRetryableStreamError 完成重开
					if !onLine(line) {
						return nil
					}
				}
			}
		}
	}
	// 防御性兜底：所有失败分支均在循环内直接 return，正常不可达
	return WrapTransportError(lastErr)
}

// ExchangeJobToken 将 PAT 或 refresh token 转换为 job token。
// 出站请求经宿主 HTTP 桥（ctx 中携带 host_callback_id）。
func ExchangeJobToken(ctx context.Context, token, machineId, machineToken, machineType string, jobTokenURL ...string) (map[string]interface{}, error) {
	date := CurrentDate()
	sig := SignLegacy(date)

	var personalToken, refreshToken string
	if strings.HasPrefix(token, "drt-") {
		refreshToken = token
		personalToken = ""
	} else {
		personalToken = token
		refreshToken = ""
	}

	inner := map[string]interface{}{
		"personalToken":      personalToken,
		"securityOauthToken": "",
		"refreshToken":       refreshToken,
		"needRefresh":        refreshToken != "",
		"authInfo":           map[string]interface{}{},
	}
	innerJSON, _ := json.Marshal(inner)

	outer := map[string]interface{}{
		"payload":       string(innerJSON),
		"encodeVersion": "1",
	}
	plain, _ := json.Marshal(outer)
	body, err := Encode(plain)
	if err != nil {
		return nil, err
	}

	endpoint := "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1"
	if len(jobTokenURL) > 0 && jobTokenURL[0] != "" {
		endpoint = jobTokenURL[0]
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("cosy-machinetoken", machineToken)
	req.Header.Set("cosy-machinetype", machineType)
	req.Header.Set("login-version", "v2")
	req.Header.Set("appcode", AppCode)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("cosy-version", Version)
	req.Header.Set("cosy-clienttype", "5")
	req.Header.Set("date", date)
	req.Header.Set("signature", sig)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-machineid", machineId)
	req.Header.Set("user-agent", "Go-http-client/2.0")

	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder job token response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d at jobToken body=%s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var c2s [128]int
var s2c [128]int

func init() {
	for i := range c2s {
		c2s[i] = -1
		s2c[i] = -1
	}
	for i := 0; i < 64; i++ {
		c2s[customAlphabet[i]] = int(stdAlphabet[i])
		s2c[stdAlphabet[i]] = int(customAlphabet[i])
	}
	c2s[customPad] = '='
	s2c['='] = customPad
}

func Encode(plaintext []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(rearranged[i])
		if c >= 128 || s2c[c] < 0 {
			return "", fmt.Errorf("char out of alphabet: %d", c)
		}
		out[i] = byte(s2c[c])
	}
	return string(out), nil
}

func Decode(encoded string) ([]byte, error) {
	n := len(encoded)
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(encoded[i])
		if c >= 128 || c2s[c] < 0 {
			return nil, fmt.Errorf("char out of custom alphabet: %d", c)
		}
		mapped[i] = byte(c2s[c])
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}

// saltedSeed 保留接口；本实现不使用部署盐（与既有行为一致）。
func saltedSeed(seed string) string { return seed }

// FingerprintSeed 选择指纹种子：优先账号 UID；
// UID 未知时（jobToken 交换发生在拿到 uid 之前）退回凭证令牌，
// 保证同一凭证跨次调用派生结果一致、不随进程重启漂移。
func FingerprintSeed(uid, credential string) string {
	if uid != "" {
		return uid
	}
	return "cred:" + credential
}

func deriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// DeriveMachineID 派生稳定的 cosy-machineid（32 位十六进制）。
func DeriveMachineID(seed string) string {
	return deriveID(seed, "machine")
}

// DeriveMachineType 派生稳定的 18 位 machine type（与 hub derive_machine_type 同构）。
func DeriveMachineType(seed string) string {
	return deriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken 派生稳定的 machine token。
//
// 形态对齐官方组件输出：88 字符 base64url（65 字节熵）。SHA-512 只有 64
// 字节，因此再追加一轮带不同前缀的摘要凑满——仍是确定性的，同一种子不变。
func DeriveMachineToken(seed string) string {
	primary := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	secondary := sha512.Sum512([]byte("machinetoken2:" + saltedSeed(seed)))
	raw := make([]byte, 66)
	copy(raw, primary[:])
	raw[64] = secondary[0]
	raw[65] = secondary[1]
	return base64.RawURLEncoding.EncodeToString(raw)[:88]
}

const Version = "1.0.10"

const ServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

type AuthIdentity struct {
	Name               string
	Aid                string
	Uid                string
	YxUid              string
	OrganizationId     string
	OrganizationName   string
	UserType           string
	SecurityOauthToken string
	RefreshToken       string
}

type SessionContext struct {
	TempKey      []byte
	CosyKey      string
	Info         string
	Identity     AuthIdentity
	MachineId    string
	MachineToken string
	MachineType  string
}

func NewSession(id AuthIdentity, machineId, machineToken, machineType string) (*SessionContext, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tempKey := []byte(fmt.Sprintf("%x", raw)[:16])

	cosyKeyBytes, err := rsaEncrypt(tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	payloadBytes, err := authPayloadJSON(id)
	if err != nil {
		return nil, err
	}
	encPayload, err := aesEncrypt(payloadBytes, tempKey)
	if err != nil {
		return nil, err
	}
	info := base64.StdEncoding.EncodeToString(encPayload)

	return &SessionContext{
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
		Identity:     id,
		MachineId:    machineId,
		MachineToken: machineToken,
		MachineType:  machineType,
	}, nil
}

func SignRequest(payloadB64, cosyKey, cosyDate, body, pathWithoutAlgo string) string {
	s := payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + pathWithoutAlgo
	return Md5Hex(s)
}

func BuildPayloadB64(info string) (string, error) {
	m := map[string]string{
		"cosyVersion": Version,
		"ideVersion":  "",
		"info":        info,
		"requestId":   NewUUID(),
		"version":     "v1",
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func ComposeBearer(payloadB64, sig string) string {
	return "Bearer COSY." + payloadB64 + "." + sig
}

func authPayloadJSON(id AuthIdentity) ([]byte, error) {
	m := map[string]string{
		"name":                 id.Name,
		"aid":                  id.Aid,
		"uid":                  id.Uid,
		"yx_uid":               id.YxUid,
		"organization_id":      id.OrganizationId,
		"organization_name":    id.OrganizationName,
		"user_type":            id.UserType,
		"security_oauth_token": id.SecurityOauthToken,
		"refresh_token":        id.RefreshToken,
	}
	return json.Marshal(m)
}

func rsaEncrypt(plaintext []byte) ([]byte, error) {
	pemData := strings.ReplaceAll(ServerPubKeyPEM, "\r", "")
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not RSA public key")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plaintext)
}

func aesEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	iv := key[:bs]
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(out, padded)
	return out, nil
}

func NewUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)[:24]
}
func UnixSec() int64 {
	return time.Now().Unix()
}

func UnixMs() int64 {
	return time.Now().UnixMilli()
}

func FloatVal(m map[string]interface{}, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

const (
	AppCode   = "cosy"
	SecretB64 = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" // base64("war, war never changes")
	sep       = "&"
)

func CurrentDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

func SignLegacy(date string) string {
	return Md5Hex(AppCode + sep + SecretB64 + sep + date)
}

func Md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}
