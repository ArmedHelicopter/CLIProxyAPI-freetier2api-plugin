package raccoon

// 本文件实现小浣熊的模型清单获取（骨架）。
//
// 清单完全动态：上游 GET {base}/v1/models 返回可用模型。本插件不编
// 硬编码清单——编出来的清单一旦与上游不一致，用户会看到调用必失败的
// 模型。探测需要一把有效 key；探测失败时返回空清单而不是兜底表。
//
// **骨架说明**：真实路径与响应形态待上游资料确认（TODO(upstream)）；
// 当前按 OpenAI 风格 {"data":[{"id":...}]} 解析，兼容裸数组形态。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// modelsHTTPTimeout 是模型清单请求的上限。
const modelsHTTPTimeout = 30 * time.Second

// Model 是一个小浣熊上游模型。
type Model struct {
	// ID 是模型标识（上游原样）。
	ID string
	// Name 是展示名（上游没给时用 ID）。
	Name string
	// ContextWindow 是上下文窗口（上游给了才有）。
	ContextWindow int64
	// MaxOutputTokens 是输出上限（上游给了才有）。
	MaxOutputTokens int64
	// SupportsTools 报告是否支持工具调用。
	SupportsTools bool
	// IsReasoning 报告是否是推理模型。
	IsReasoning bool
}

// FetchModels 拉取上游模型清单。
func FetchModels(ctx context.Context, baseURL string, apiKey string) ([]Model, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("api key is required to list models")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured (set raccoon_base_url or fill DefaultBaseURL after upstream research)")
	}
	endpoint := baseURL + ModelsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models request: %w", errReq)
	}
	ApplyModelsHeaders(req, apiKey)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read models response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return parseModels(body)
}

// parseModels 解析上游模型清单（OpenAI 风格，兼容裸数组）。
func parseModels(body []byte) ([]Model, error) {
	type rawModel struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		ContextLength int64  `json:"context_length"`
		ContextWindow int64  `json:"context_window"`
		MaxOutput     int64  `json:"max_output_tokens"`
		MaxTokens     int64  `json:"max_tokens"`
		SupportsTools *bool  `json:"supports_tools"`
		ToolCall      *bool  `json:"tool_call"`
		IsReasoning   *bool  `json:"is_reasoning"`
		Reasoning     *bool  `json:"reasoning"`
	}

	var entries []rawModel
	var wrapper struct {
		Data   []rawModel `json:"data"`
		Models []rawModel `json:"models"`
	}
	if errWrapper := json.Unmarshal(body, &wrapper); errWrapper == nil {
		switch {
		case len(wrapper.Data) > 0:
			entries = wrapper.Data
		case len(wrapper.Models) > 0:
			entries = wrapper.Models
		}
	}
	if len(entries) == 0 {
		if errArray := json.Unmarshal(body, &entries); errArray != nil && !json.Valid(body) {
			return nil, fmt.Errorf("models response is not valid JSON: %w", errArray)
		}
	}

	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		contextWindow := entry.ContextWindow
		if contextWindow == 0 {
			contextWindow = entry.ContextLength
		}
		maxOutput := entry.MaxOutput
		if maxOutput == 0 {
			maxOutput = entry.MaxTokens
		}
		out = append(out, Model{
			ID:              id,
			Name:            core.FirstNonEmpty(entry.DisplayName, entry.Name, id),
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxOutput,
			SupportsTools:   firstBool(entry.SupportsTools, entry.ToolCall),
			IsReasoning:     firstBool(entry.IsReasoning, entry.Reasoning),
		})
	}
	return out, nil
}

// firstBool 返回第一个非 nil 的布尔值（全 nil 时 false）。
func firstBool(values ...*bool) bool {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return false
}
