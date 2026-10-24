package raccoon

// 本文件实现小浣熊的模型目录获取（GET {llm_base}/model_catalog）。
//
// 上游形态 {"code":0,"data":{"categories":[{type,models:[…]}]}}，
// 条目 {name, description, visible, context_window, max_tokens,
// billing_multiplier, params:{...}}：context_window / max_tokens 可能是
// 字符串或数字，也可能嵌在 params 里（两处都认）；同名首次出现者胜；
// visible 缺省 true。
//
// 目录拉不到时回落到静态单条（DefaultConfigName），保证 /v1/models 不空表。

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

// modelsHTTPTimeout 是目录请求的上限。
const modelsHTTPTimeout = 15 * time.Second

// Model 是一条归一化后的目录条目。
type Model struct {
	ID              string
	Name            string
	Description     string
	ContextWindow   int64
	MaxOutputTokens int64
	IsReasoning     bool
	SupportsTools   bool
}

// FetchModels 拉取模型目录。
func FetchModels(ctx context.Context, llmBase, token string) ([]Model, error) {
	if strings.TrimSpace(llmBase) == "" {
		return nil, fmt.Errorf("upstream LLM base URL is not configured")
	}
	endpoint := llmBase + CatalogPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build catalog request: %w", errReq)
	}
	ApplyAPIHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read model catalog response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	models, errParse := parseCatalog(body)
	if errParse != nil {
		return nil, errParse
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model catalog is empty")
	}
	return models, nil
}

// parseCatalog 解析目录响应（root 无 data 时把根当 data；平铺 categories[].models）。
func parseCatalog(body []byte) ([]Model, error) {
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model catalog: %w", errUnmarshal)
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, ClassifyBusiness(http.StatusOK, *envelope.Code, envelope.Message)
	}
	root := json.RawMessage(body)
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		root = envelope.Data
	}
	var parsed struct {
		Categories []struct {
			Models []catalogEntry `json:"models"`
		} `json:"categories"`
	}
	if errUnmarshal := json.Unmarshal(root, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model catalog: %w", errUnmarshal)
	}
	out := make([]Model, 0)
	seen := map[string]bool{}
	for _, category := range parsed.Categories {
		for _, entry := range category.Models {
			model, okModel := entry.normalize()
			if !okModel || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			out = append(out, model)
		}
	}
	return out, nil
}

// catalogEntry 是目录条目的原始形态。
type catalogEntry struct {
	Name               string   `json:"name"`
	ModelName          string   `json:"model_name"`
	Description        string   `json:"description"`
	DisplayDescription string   `json:"display_description"`
	Visible            *bool    `json:"visible"`
	ContextWindow      *flexInt `json:"context_window"`
	MaxTokens          *flexInt `json:"max_tokens"`
	Params             *struct {
		ContextWindow *flexInt `json:"context_window"`
		MaxTokens     *flexInt `json:"max_tokens"`
	} `json:"params"`
}

// flexInt 兼容数字与字符串两种数值形态（上游两种都发过）。
type flexInt float64

// UnmarshalJSON 兼容 JSON 数字与字符串。
func (f *flexInt) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		return nil
	}
	trimmed = strings.Trim(trimmed, "\"")
	if trimmed == "" {
		return nil
	}
	var value float64
	if _, err := fmt.Sscanf(trimmed, "%g", &value); err != nil {
		return fmt.Errorf("flexInt: %w", err)
	}
	*f = flexInt(value)
	return nil
}

// normalize 把原始条目归一成 Model（缺 id 丢弃；visible 缺省 true）。
func (e *catalogEntry) normalize() (Model, bool) {
	id := strings.TrimSpace(core.FirstNonEmpty(e.Name, e.ModelName))
	if id == "" {
		return Model{}, false
	}
	visible := true
	if e.Visible != nil {
		visible = *e.Visible
	}
	if !visible {
		return Model{}, false
	}
	contextWindow := firstFloat(e.ContextWindow, e.nestedContext())
	maxTokens := firstFloat(e.MaxTokens, e.nestedMaxTokens())
	if contextWindow == 0 {
		contextWindow = core.DefaultContextWindow(id)
	}
	if maxTokens == 0 {
		maxTokens = core.DefaultMaxOutputTokens
	}
	return Model{
		ID:              id,
		Name:            core.FirstNonEmpty(e.DisplayDescription, id),
		Description:     core.FirstNonEmpty(e.DisplayDescription, e.Description, "小浣熊上游模型"),
		ContextWindow:   contextWindow,
		MaxOutputTokens: maxTokens,
		IsReasoning:     true,
		SupportsTools:   true,
	}, true
}

func (e *catalogEntry) nestedContext() *flexInt {
	if e.Params == nil {
		return nil
	}
	return e.Params.ContextWindow
}

func (e *catalogEntry) nestedMaxTokens() *flexInt {
	if e.Params == nil {
		return nil
	}
	return e.Params.MaxTokens
}

// firstFloat 返回第一个非 nil 的数值（取整）。
func firstFloat(values ...*flexInt) int64 {
	for _, value := range values {
		if value != nil && *value > 0 {
			return int64(*value)
		}
	}
	return 0
}

// StaticFallbackModels 是目录拉不到时的兜底单条（保证 /v1/models 不空表）。
func StaticFallbackModels() []Model {
	return []Model{{
		ID:              DefaultConfigName,
		Name:            "Raccoon Chat",
		Description:     "小浣熊默认模型（目录拉取失败时的兜底）",
		ContextWindow:   180000,
		MaxOutputTokens: 80000,
		IsReasoning:     true,
		SupportsTools:   true,
	}}
}
