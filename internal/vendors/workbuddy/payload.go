package workbuddy

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"freetier2api-plugin/internal/core"
)

// 本文件实现「OpenAI 请求体 → CodeBuddy 请求体」的改写管线。
//
// **改写顺序有语义，不可调换**。每一步都对应一个曾经真实踩过的坑：
//
//	1. stream 强制 true          —— 上游拒绝非流式请求
//	2. max_completion_tokens 归一 —— 上游只认 max_tokens，别名会被静默忽略
//	3. stream_options 补默认      —— 不补就拿不到 usage（含真实扣费）
//	4. tool_choice 归一           —— 上游该字段是 string，对象形态直接 400
//	5. developer → system         —— 上游 role 白名单不含 developer，命中即 400
//	6. image_url 归一             —— 上游只接受对象形态，字符串会 400
//	7. tool 结果重排              —— 插队消息会让上游判定工具配对失败
//	8. 孤儿 tool_call 裁剪        —— 配对不全上游会顶死整条会话
//	9. 思维链注入                 —— deepseek 系不显式开思考就不返回思维链
//	10. 档位归一                  —— 非法档位 400
//	11. reasoning_content 回填    —— 部分租户要求 assistant 消息带非空 reasoning
//	12. 指纹脱敏                  —— 上游内容审核是逐字匹配

// PrepareOptions 是改写管线的输入。
type PrepareOptions struct {
	// Model 是目标模型裸名。非空时直接改写 payload["model"]。
	Model string
	// PromptMode 提示词模式：passthrough / custom / append。
	PromptMode string
	// PromptText 自定义或追加的提示词内容。
	PromptText string
	// Sanitize 开启指纹脱敏（第 12 步）。
	Sanitize bool
	// DefaultEfforts 是模型名 → 默认档位的表（来自模型目录）。
	DefaultEfforts map[string]string
	// SupportedEfforts 是模型名 → 支持档位列表的表（来自模型目录）。
	SupportedEfforts map[string][]string
	// CacheKey 是 prompt_cache_key；为空时跳过注入。
	CacheKey string
	// Global 表示这是 global 域请求（额外需要 system 兜底）。
	Global bool
}

// defaultDeepSeekEffort 是 deepseek 系模型缺失档位时的兜底默认档。
const defaultDeepSeekEffort = "high"

// effortRank 是档位的强度排序，用于「取不高于请求档的最高支持档」。
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// PrepareBody 执行完整改写管线，返回可直接发给上游的请求体。
//
// 解析失败时原样返回输入：宁可不改写也不要让一次对话因为改写失败而中断，
// 上游的真实报错比本地的解析错误更有诊断价值。
func PrepareBody(body []byte, opts PrepareOptions) []byte {
	payload := decodeBodyMap(body)
	if payload == nil {
		return body
	}

	// 0) 出站 model 设为裸名（单 pass 改写，无需二次序列化）。
	if trimmedModel := strings.TrimSpace(opts.Model); trimmedModel != "" {
		payload["model"] = trimmedModel
	}

	// 1) 上游拒绝非流式。
	payload["stream"] = true

	// 2) max_completion_tokens → max_tokens。
	translateMaxCompletionTokens(payload)

	// 3) stream_options 缺省补 include_usage（不补就拿不到 usage）。
	if _, okOptions := payload["stream_options"]; !okOptions {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4) tool_choice 归一。
	normalizeToolChoice(payload)

	// 5) developer → system。
	normalizeRoles(payload)

	// 5.5) 系统提示词处理（透传 / 替换 / 追加）。
	applyPromptMode(payload, opts.PromptMode, opts.PromptText)

	// 6) image_url 归一。
	normalizeImageURL(payload)

	if messages, okMessages := payload["messages"].([]any); okMessages {
		// 7) 工具结果重排。
		messages = repackToolResultBlocks(messages)
		// 8) 孤儿 tool_call 裁剪。
		messages = cleanupOrphanToolCalls(messages)
		payload["messages"] = messages
	}

	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)

	// 9) 思维链注入。
	injectThinking(payload, defaultEffortFor(opts.DefaultEfforts, model))

	// 10) 档位归一。
	normalizeReasoningEffort(payload, opts.SupportedEfforts[model])

	// 11) reasoning_content 回填。
	backfillReasoningContent(payload)

	// global 域的兜底 system：上游某些路径要求首条消息必须是 system。
	if opts.Global {
		ensureConsoleSystem(payload)
	}

	// prompt_cache_key：单项最大的费用优化，必须在最后注入（前面的步骤不依赖它）。
	if strings.TrimSpace(opts.CacheKey) != "" {
		if _, okKey := payload["prompt_cache_key"]; !okKey {
			payload["prompt_cache_key"] = opts.CacheKey
		}
	}

	// 12) 指纹脱敏（放最后：前面步骤可能引入需要净化的文本）。
	if opts.Sanitize {
		if messages, okMessages := payload["messages"].([]any); okMessages {
			sanitizeMessages(messages)
		}
	}

	encoded, errEncode := encodeBodyMap(payload)
	if errEncode != nil {
		return body
	}
	return encoded
}

// translateMaxCompletionTokens 把 max_completion_tokens 翻译成 max_tokens。
//
// 背景：OpenAI 规范里 max_tokens 已废弃、max_completion_tokens 是新字段，
// 但上游只认 max_tokens——透传别名会被忽略，然后回落到默认输出上限，
// 长任务被静默截断。
//
// 规则：
//   - 别名**无论是否翻译都删除**（减少体积与排障噪音）；
//   - 显式 max_tokens 存在时只删不译（显式优先）；
//   - 别名是正整数值才翻译，写成 int64 避免科学计数法（1.28e5）；
//   - 0/null/负数/非数值不翻译（0/null 语义是「未设置」，负数是非法值）。
func translateMaxCompletionTokens(payload map[string]any) {
	alias, hasAlias := payload["max_completion_tokens"]
	if !hasAlias {
		return
	}
	delete(payload, "max_completion_tokens")

	if _, hasExplicit := payload["max_tokens"]; hasExplicit {
		return
	}
	if value, okValue := positiveInt64(alias); okValue {
		payload["max_tokens"] = value
	}
}

// positiveInt64 把 JSON 数值转成正整数。
func positiveInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		if typed <= 0 {
			return 0, false
		}
		return typed, true
	case int:
		if typed <= 0 {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}

// normalizeToolChoice 把 tool_choice 归一成上游接受的形态。
//
// 上游该字段是 Go string，对象形态会 400 code=11101。
func normalizeToolChoice(payload map[string]any) {
	raw, hasChoice := payload["tool_choice"]
	if !hasChoice {
		return
	}
	if text, okText := raw.(string); okText {
		if strings.EqualFold(strings.TrimSpace(text), "none") {
			// "none" 表示不使用工具：删掉选择器与工具定义本身。
			delete(payload, "tool_choice")
			delete(payload, "tools")
			delete(payload, "functions")
			return
		}
		return
	}
	object, okObject := raw.(map[string]any)
	if !okObject {
		delete(payload, "tool_choice")
		return
	}
	choiceType, _ := object["type"].(string)
	switch strings.ToLower(strings.TrimSpace(choiceType)) {
	case "none":
		delete(payload, "tool_choice")
		delete(payload, "tools")
		delete(payload, "functions")
	case "auto", "required":
		payload["tool_choice"] = strings.ToLower(strings.TrimSpace(choiceType))
	case "function":
		// 两种形态：{"function":{"name":"x"}} 与 {"name":"x"}。
		name := ""
		if function, okFunction := object["function"].(map[string]any); okFunction {
			name, _ = function["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			name, _ = object["name"].(string)
		}
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			payload["tool_choice"] = trimmed
			return
		}
		delete(payload, "tool_choice")
	default:
		delete(payload, "tool_choice")
	}
}

// normalizeRoles 把 developer 归一为 system。
//
// 上游的 role 白名单不含 developer，命中即 HTTP 400 code=11-128。
// 只归一这一个值，其余 role 原样保留（不合并、不重排、不删除）。
func normalizeRoles(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, okRole := message["role"].(string)
		if !okRole {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			message["role"] = "system"
		}
	}
}

// normalizeImageURL 把字符串形态的 image_url 转成对象形态。
//
// 上游只接受对象，字符串会 400 code=11101 cannot unmarshal string into ImageContent。
// 已是对象的一律原样保留（不补 detail/mime_type 默认值：让上游返回真实错误，
// 而不是被我们补的默认值掩盖）。
func normalizeImageURL(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); !okType || partType != "image_url" {
				continue
			}
			if url, okURL := partMap["image_url"].(string); okURL {
				partMap["image_url"] = map[string]any{"url": url}
			}
		}
	}
}

// ensureConsoleSystem 在 global 域缺少首条 system 时补一条兜底。
//
// 上游 global 的某些路径要求 messages[0] 是 system，缺失时行为不确定。
func ensureConsoleSystem(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return
	}
	if first, okFirst := messages[0].(map[string]any); okFirst {
		if role, okRole := first["role"].(string); okRole && strings.EqualFold(strings.TrimSpace(role), "system") {
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, messages...)
}

// defaultEffortFor 查模型的默认档位。
func defaultEffortFor(table map[string]string, model string) string {
	if len(table) == 0 {
		return ""
	}
	return strings.TrimSpace(table[strings.TrimSpace(model)])
}

// applyPromptMode 处理系统提示词：custom 替换全部 system/developer；append 在开头连续 system 块后追加。
func applyPromptMode(payload map[string]any, mode, text string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	text = strings.TrimSpace(text)
	if mode == "" || mode == "passthrough" || text == "" {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	if mode == "custom" {
		kept := make([]any, 0, len(messages)+1)
		kept = append(kept, map[string]any{"role": "system", "content": text})
		for _, item := range messages {
			msgMap, okMap := item.(map[string]any)
			if !okMap {
				kept = append(kept, item)
				continue
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				continue
			}
			kept = append(kept, item)
		}
		payload["messages"] = kept
	} else if mode == "append" {
		insertAt := 0
		for insertAt < len(messages) {
			msgMap, okMap := messages[insertAt].(map[string]any)
			if !okMap {
				break
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				insertAt++
				continue
			}
			break
		}
		out := make([]any, 0, len(messages)+1)
		out = append(out, messages[:insertAt]...)
		out = append(out, map[string]any{"role": "system", "content": text})
		out = append(out, messages[insertAt:]...)
		payload["messages"] = out
	}
}

// decodeBodyMap 把请求体解析成 map。解析失败返回 nil（调用方按"无法解析"处理）。
func decodeBodyMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}
	return payload
}

// encodeBodyMap 把 map 序列化回请求体。
func encodeBodyMap(payload map[string]any) ([]byte, error) {
	return json.Marshal(payload)
}

// 本文件处理 DeepSeek 系的思维链。
//
// 上游对 deepseek 系模型标记了 thinkingFormat:"deepseek" 与
// requiresReasoningContentOnAssistantMessages，带来两个硬性要求：
//   - 开启思考必须显式带 thinking:{type:"enabled"} **且有档位**，
//     否则上游默认按「不思考」应答（思维链完全不返回）；
//   - assistant 消息必须带非空 reasoning，部分租户校验 len(reasoning) > 0，
//     缺失/null/空串一律 400（空白串可以，说明校验的是长度而非内容）。

// isDeepSeekModel 判断是否为 deepseek 系模型。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 为 deepseek 系模型注入思维链开关。
//
//	输入                           行为
//	thinking.type == "disabled"    尊重，删除档位字段
//	thinking.type == "enabled"     补默认档（若缺）
//	无 thinking / 非对象 / type 空  注入 enabled 并补默认档
func injectThinking(payload map[string]any, defaultEffort string) {
	model, _ := payload["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking {
		payload["thinking"] = map[string]any{"type": "enabled"}
		ensureDeepSeekEffort(payload, defaultEffort)
		return
	}
	thinkingType, _ := thinking["type"].(string)
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "disabled":
		// 用户明确要关：尊重它，同时删掉档位（开着档位却关思考是矛盾组合）。
		delete(payload, "reasoning_effort")
		delete(payload, "reasoningEffort")
	case "enabled":
		ensureDeepSeekEffort(payload, defaultEffort)
	default:
		thinking["type"] = "enabled"
		ensureDeepSeekEffort(payload, defaultEffort)
	}
}

// ensureDeepSeekEffort 保证 deepseek 请求带一个档位。
//
// snake_case 优先、camelCase 兜底；两者都没有才写默认档。
func ensureDeepSeekEffort(payload map[string]any, defaultEffort string) {
	if _, okSnake := payload["reasoning_effort"]; okSnake {
		return
	}
	if _, okCamel := payload["reasoningEffort"]; okCamel {
		return
	}
	effort := strings.TrimSpace(defaultEffort)
	if effort == "" {
		effort = defaultDeepSeekEffort
	}
	payload["reasoning_effort"] = effort
}

// normalizeReasoningEffort 把档位降到模型支持的范围。
//
// 规则（支持表为空时一律透传，不做任何猜测）：
//   - 请求档位在支持列表里 → 原样透传；
//   - 否则取**不高于**请求档的最高支持档；
//   - 若所有支持档都高于请求档 → 取最低支持档（偏离最小）。
//
// 上游收到非法档位直接 400，而降级到最近的合法档位能让请求成功。
func normalizeReasoningEffort(payload map[string]any, supported []string) {
	if len(supported) == 0 {
		return
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		raw, okRaw := payload[key].(string)
		if !okRaw {
			continue
		}
		requested := strings.ToLower(strings.TrimSpace(raw))
		if requested == "" {
			continue
		}
		if containsString(supported, requested) {
			continue
		}
		payload[key] = nearestEffort(supported, effortRank[requested])
	}
}

// nearestEffort 在支持列表里找与请求档位最接近的档位。
func nearestEffort(supported []string, requestedRank int) string {
	lowest := ""
	lowestRank := 99
	nearest := ""
	nearestRank := -1
	for _, item := range supported {
		rank, okRank := effortRank[item]
		if !okRank {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = item
		}
		// 不高于请求档的最高者。
		if rank <= requestedRank && rank > nearestRank {
			nearestRank = rank
			nearest = item
		}
	}
	if nearest != "" {
		return nearest
	}
	// 所有支持档都高于请求档：取最低支持档（偏离最小）。
	if lowest != "" {
		return lowest
	}
	return supported[0]
}

// backfillReasoningContent 为 assistant 消息补齐 reasoning 字段。
//
// 门控：请求开了思考，或历史里已经有思维链痕迹。
// 只处理 assistant 消息：
//   - reasoning_content 已是字符串 → 不覆盖；
//   - 否则若 reasoning 是字符串 → 复制其值；
//   - 否则补空串。
//
// 同时镜像保证 reasoning 字段非空：缺失/null/空串归一为单空格
// （实测部分租户校验 len(reasoning) > 0，空白串可通过）。
func backfillReasoningContent(payload map[string]any) {
	if !reasoningGateOpen(payload) {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}

		if _, okString := message["reasoning_content"].(string); !okString {
			if reasoning, okReasoning := message["reasoning"].(string); okReasoning {
				message["reasoning_content"] = reasoning
			} else {
				message["reasoning_content"] = ""
			}
		}

		// 镜像字段：非空优先，皆无补一个空格。
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			if filled, okFilled := message["reasoning_content"].(string); okFilled && strings.TrimSpace(filled) != "" {
				message["reasoning"] = filled
			} else {
				message["reasoning"] = " "
			}
		}
	}
}

// reasoningGateOpen 判断是否需要回填思维链字段。
//
// 开思考时必须回填（上游会把历史里的 assistant 消息一起校验）；
// 历史里已经出现过思维链痕迹时也要回填，否则后续轮次会突然缺失字段。
func reasoningGateOpen(payload map[string]any) bool {
	if thinking, okThinking := payload["thinking"].(map[string]any); okThinking {
		if thinkingType, okType := thinking["type"].(string); okType &&
			strings.EqualFold(strings.TrimSpace(thinkingType), "enabled") {
			return true
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		if reasoning, okReasoning := message["reasoning"].(string); okReasoning && strings.TrimSpace(reasoning) != "" {
			return true
		}
		if _, okContent := message["reasoning_content"]; okContent {
			return true
		}
	}
	return false
}

// 本文件处理 tool_call 与 tool 结果的配对问题。
//
// 上游对工具配对的校验很严：调用与结果必须**双侧齐全**，否则返回
// 11148 "tool calls and tool results do not match" 并顶死整条会话。
//
// 两个真实来源的破坏：
//   - 客户端（如 Codex）会在并行调用的两份结果之间插入非 tool 消息；
//   - 客户端会发送「调用没有结果」或「结果没有调用」的残缺历史。

// repackToolResultBlocks 把插在 tool_calls 与其结果之间的非 tool 消息挪到整组之后。
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 只调顺序不改内容。无插入消息时返回原 slice（零改动零分配）。
func repackToolResultBlocks(messages []any) []any {
	if len(messages) < 3 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false

	for index := 0; index < len(messages); index++ {
		message, okMessage := messages[index].(map[string]any)
		if !okMessage {
			out = append(out, messages[index])
			continue
		}
		if !hasToolCalls(message) {
			out = append(out, messages[index])
			continue
		}

		// 收集这一组：调用消息本身 + 紧随其后的 tool 结果（中间可能夹着别的消息）。
		group := []any{messages[index]}
		var displaced []any
		cursor := index + 1
		for ; cursor < len(messages); cursor++ {
			next, okNext := messages[cursor].(map[string]any)
			if !okNext {
				break
			}
			role, _ := next["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "tool" {
				group = append(group, messages[cursor])
				continue
			}
			// 遇到下一组带 tool_calls 的 assistant 必须交还外层循环：
			// 否则它那批结果永远得不到重排（真实会话里正是这样漏掉的）。
			if hasToolCalls(next) {
				break
			}
			displaced = append(displaced, messages[cursor])
		}
		if len(displaced) > 0 {
			changed = true
			out = append(out, group...)
			out = append(out, displaced...)
		} else {
			out = append(out, group...)
		}
		index = cursor - 1
	}
	if !changed {
		return messages
	}
	return out
}

// cleanupOrphanToolCalls 裁剪配对不全的工具调用与结果。
//
// 规则：调用与结果的交集才保留。
//   - assistant.tool_calls 按交集对称裁剪；裁空则删除该键；
//   - role=tool 只在对应调用被保留时保留，否则整条删除。
//
// 历史上的错误做法是「批内全齐才整批保留，否则删掉整个 tool_calls 键」，
// 那会留下「无 tool_calls 的 assistant + 孤儿 tool」，正好命中上游的 11148。
func cleanupOrphanToolCalls(messages []any) []any {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}

	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			for _, id := range toolCallIDs(message) {
				callIDs[id] = true
			}
		case "tool":
			if id, okID := message["tool_call_id"].(string); okID && strings.TrimSpace(id) != "" {
				resultIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages
	}

	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}

	out := make([]any, 0, len(messages))
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			out = append(out, item)
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			if !hasToolCalls(message) {
				out = append(out, message)
				continue
			}
			filtered := filterToolCalls(message, keep)
			if len(filtered) == 0 {
				delete(message, "tool_calls")
				changed = true
			} else {
				message["tool_calls"] = filtered
			}
			out = append(out, message)
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if !keep[strings.TrimSpace(id)] {
				changed = true
				continue
			}
			out = append(out, message)
		default:
			out = append(out, message)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// toolCallIDs 取出 assistant 消息里的全部 tool_call id。
func toolCallIDs(message map[string]any) []string {
	calls, okCalls := message["tool_calls"].([]any)
	if !okCalls {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		if id, okID := call["id"].(string); okID && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

// hasToolCalls 报告消息是否带非空 tool_calls。
func hasToolCalls(message map[string]any) bool {
	calls, okCalls := message["tool_calls"].([]any)
	return okCalls && len(calls) > 0
}

// filterToolCalls 按 keep 集合裁剪 tool_calls。
func filterToolCalls(message map[string]any, keep map[string]bool) []any {
	calls, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		id, _ := call["id"].(string)
		if keep[strings.TrimSpace(id)] {
			out = append(out, call)
		}
	}
	return out
}

// IsTruncatedArguments 判断工具调用参数是否为残缺 JSON。
//
// 空串是合法的（无参工具）；非空但解析不了说明响应被截断。
func IsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var value any
	return json.Unmarshal([]byte(trimmed), &value) != nil
}

// DropTruncatedToolCalls 丢弃参数残缺的工具调用。
//
// 在 finish_reason 为 length（输出被截断）或流没有正常结束时调用：
// 那种情况下的工具参数必然是不完整的 JSON，交给客户端只会得到解析错误。
// 能解析但类型不对的（标量/数组）不丢，留给客户端的 schema 校验处理。
func DropTruncatedToolCalls(calls []any) []any {
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			out = append(out, item)
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			out = append(out, call)
			continue
		}
		arguments, _ := function["arguments"].(string)
		if IsTruncatedArguments(arguments) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// StripToolCallNames 让每个 tool_call 的 function.name 只出现在首片。
//
// 累加型客户端会做 name += delta.name，若每片都带 name 就会拼成
// "BashBashBash"；覆盖型客户端（name ?? state.name）则不会被空值清空。
// 因此「首片保留、后续删除该键」是同时满足两类客户端的做法。
func StripToolCallNames(frame map[string]any, seen map[int]bool) {
	choices, okChoices := frame["choices"].([]any)
	if !okChoices {
		return
	}
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		delta, okDelta := choice["delta"].(map[string]any)
		if !okDelta {
			continue
		}
		calls, okCalls := delta["tool_calls"].([]any)
		if !okCalls {
			continue
		}
		for _, callItem := range calls {
			call, okCall := callItem.(map[string]any)
			if !okCall {
				continue
			}
			index := core.IntFromAny(call["index"])
			function, okFunction := call["function"].(map[string]any)
			if !okFunction {
				continue
			}
			if seen[index] {
				delete(function, "name")
				continue
			}
			if _, okName := function["name"]; okName {
				seen[index] = true
			}
		}
	}
}

// 本文件实现出站请求体的指纹脱敏。
//
// 背景：客户端（Claude Code 类 CLI）会在 system prompt 里注入若干固定模板句，
// 上游的内容审核是**逐字精确匹配**（不是语义审核），一字改动即可绕过。
//
// 策略分两层：
//   - 键值/header 型指纹：整段剥离（它本身就是注入痕迹，不含用户语义）；
//   - 承载语义的模板句：最小改写（换一个词），语义不变。
//
// 规则表从 catalog 同级的 sanitize_rules.json 读取（内嵌），
// 便于审计「到底改了哪些字面量」，也避免 Go 源码里出现大量易被误改的临界字符串。

//go:embed assets/sanitize_rules.json
var sanitizeRulesRaw []byte

// sanitizeRules 是脱敏规则。
type sanitizeRules struct {
	// Features 是特征预检串：任一命中才进入净化（普通请求零开销）。
	Features []string `json:"features"`
	// Rewrites 是 [原文, 改写] 对。
	Rewrites [][2]string `json:"rewrites"`
	// Regexes 是整段剥离用的正则（按名字取用）。
	Regexes map[string]string `json:"regexes"`
}

var (
	sanitizeOnce    sync.Once
	sanitizeRuleSet sanitizeRules
	sanitizeRegexes map[string]*regexp.Regexp
)

func loadSanitizeRules() (sanitizeRules, map[string]*regexp.Regexp) {
	sanitizeOnce.Do(func() {
		var rules sanitizeRules
		if errUnmarshal := json.Unmarshal(sanitizeRulesRaw, &rules); errUnmarshal != nil {
			// 内嵌资源损坏是构建期问题：没有规则就退化为「不脱敏」，
			// 而不是让插件启动失败（脱敏是加分项，不是必需项）。
			rules = sanitizeRules{}
		}
		compiled := map[string]*regexp.Regexp{}
		for name, pattern := range rules.Regexes {
			if strings.TrimSpace(pattern) == "" {
				continue
			}
			expr, errCompile := regexp.Compile(pattern)
			if errCompile != nil {
				// 规则写坏时跳过该条，而不是 panic。
				continue
			}
			compiled[name] = expr
		}
		sanitizeRuleSet = rules
		sanitizeRegexes = compiled
	})
	return sanitizeRuleSet, sanitizeRegexes
}

// sanitizeMessages 净化请求体里的全部文本。
//
// 净化范围：content（字符串或 parts 里的文本）、reasoning/reasoning_content、
// tool_calls 的参数（它是字符串化 JSON，按文本走）。**图片 part 不动**。
//
// 返回值报告是否发生了改动。
func sanitizeMessages(messages []any) bool {
	rules, regexes := loadSanitizeRules()
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		// 注意：这里**不能**在 content 缺失时 continue。
		// 工具调用消息的 content 通常是 null，早期版本因此在 content 缺失时跳过，
		// 导致 tool_calls 里的参数完全不被净化——写进工具参数（文件名、命令、
		// 写入内容）的被拦字符串会原样漏出。
		if content, okContent := message["content"]; okContent {
			if updated, didChange := sanitizeContent(content, rules, regexes); didChange {
				message["content"] = updated
				changed = true
			}
		}
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if text, okText := message[key].(string); okText {
				if updated := sanitizeText(text, rules, regexes); updated != text {
					message[key] = updated
					changed = true
				}
			}
		}
		if calls, okCalls := message["tool_calls"].([]any); okCalls {
			if sanitizeToolCalls(calls, rules, regexes) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeContent 净化消息内容（字符串或 parts 数组）。
func sanitizeContent(content any, rules sanitizeRules, regexes map[string]*regexp.Regexp) (any, bool) {
	switch typed := content.(type) {
	case string:
		updated := sanitizeText(typed, rules, regexes)
		return updated, updated != typed
	case []any:
		changed := false
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			// 图片 part 不动：净化文本字段之外的内容会破坏图片。
			if partType, okType := part["type"].(string); okType && partType == "image_url" {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if updated := sanitizeText(text, rules, regexes); updated != text {
				part["text"] = updated
				changed = true
			}
		}
		return typed, changed
	}
	return content, false
}

// sanitizeToolCalls 净化工具调用的参数与名称。
func sanitizeToolCalls(calls []any, rules sanitizeRules, regexes map[string]*regexp.Regexp) bool {
	changed := false
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			continue
		}
		if arguments, okArgs := function["arguments"].(string); okArgs {
			if updated := sanitizeText(arguments, rules, regexes); updated != arguments {
				function["arguments"] = updated
				changed = true
			}
		}
	}
	return changed
}

// hasFingerprint 特征预检：快速判断文本是否需要净化。
//
// 用 strings.Contains 走快路径：绝大多数请求全不命中，
// 直接跳过后面所有正则与改写（零分配）。
func hasFingerprint(text string, rules sanitizeRules) bool {
	for _, feature := range rules.Features {
		if strings.Contains(text, feature) {
			return true
		}
	}
	return false
}

// sanitizeText 对一段文本执行完整脱敏。
//
// 执行顺序：
//  1. 特征预检（不命中直接返回原串）；
//  2. 改写表（承载语义的模板句，逐词替换）；
//  3. header 键值段整段剥离；
//  4. cc_ 键值段循环清理（直到稳定）；
//  5. 裸 header 键名缩写。
func sanitizeText(text string, rules sanitizeRules, regexes map[string]*regexp.Regexp) string {
	if text == "" || !hasFingerprint(text, rules) {
		return text
	}
	for _, pair := range rules.Rewrites {
		if len(pair) != 2 || pair[0] == "" {
			continue
		}
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if expr := regexes["sanitizeHdrRe"]; expr != nil {
		text = expr.ReplaceAllString(text, "")
	}
	if expr := regexes["sanitizeKvRe"]; expr != nil {
		// 循环清理直到稳定：一条规则剥掉前缀后可能让下一条匹配露出来。
		for previous := ""; previous != text; {
			previous = text
			text = expr.ReplaceAllString(text, "")
		}
	}
	if expr := regexes["sanitizeBareHdrRe"]; expr != nil {
		text = expr.ReplaceAllStringFunc(text, func(match string) string {
			// 把裸键名缩写成短形式，保留可读性同时不再是原字面量。
			if len(match) > 4 {
				return match[:len(match)-3] + "hdr"
			}
			return match
		})
	}
	return strings.TrimSpace(text)
}
