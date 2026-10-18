package trae

// 模型目录：提供 Trae 国内版、国际版与 TRAE SOLO 的模型列表。

import (
	"context"
	"encoding/json"
	"freetier2api-plugin/internal/hostbridge"
	"io"
	"net/http"
	"time"
)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	SupportsTools bool   `json:"supports_tools"`
	IsReasoning   bool   `json:"is_reasoning"`
}

// DefaultModelsFor 返回某 profile 的默认模型列表。
func DefaultModelsFor(cfg Config) []Model {
	switch {
	case cfg.Solo:
		return []Model{
			{ID: "glm-5.3", Name: "GLM-5.3", Description: "TRAE SOLO GLM-5.3 代码模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
			{ID: "glm-5.2", Name: "GLM-5.2", Description: "TRAE SOLO GLM-5.2 默认推理模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
			{ID: "deepseek-v3", Name: "DeepSeek-V3", Description: "TRAE SOLO DeepSeek-V3 模型", ContextWindow: 65536, MaxTokens: 8192, SupportsTools: true},
			{ID: "claude-3-5-sonnet", Name: "Claude 3.5 Sonnet", Description: "TRAE SOLO Claude 3.5 Sonnet 模型", ContextWindow: 200000, MaxTokens: 8192, SupportsTools: true},
		}
	case cfg.Region == RegionGlobal:
		return []Model{
			{ID: "gpt-5", Name: "GPT-5", Description: "Trae 国际版 GPT-5 模型", ContextWindow: 200000, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
			{ID: "claude-3-5-sonnet", Name: "Claude 3.5 Sonnet", Description: "Trae 国际版 Claude 3.5 Sonnet 模型", ContextWindow: 200000, MaxTokens: 8192, SupportsTools: true},
			{ID: "gemini-1.5-pro", Name: "Gemini 1.5 Pro", Description: "Trae 国际版 Gemini 1.5 Pro 模型", ContextWindow: 1000000, MaxTokens: 8192, SupportsTools: true},
			{ID: "deepseek-v3", Name: "DeepSeek-V3", Description: "Trae 国际版 DeepSeek-V3 模型", ContextWindow: 65536, MaxTokens: 8192, SupportsTools: true},
		}
	default:
		return []Model{
			{ID: "glm-5.3", Name: "GLM-5.3", Description: "Trae 国内版 GLM-5.3 代码模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
			{ID: "glm-5.2", Name: "GLM-5.2", Description: "Trae 国内版 GLM-5.2 默认推理模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
			{ID: "kimi-k3", Name: "Kimi-K3", Description: "Trae 国内版 Kimi-K3 长文本模型", ContextWindow: 200000, MaxTokens: 8192, SupportsTools: true},
			{ID: "doubao-seed", Name: "Doubao-Seed", Description: "Trae 国内版 豆包模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true},
			{ID: "deepseek-v4", Name: "DeepSeek-V4", Description: "Trae 国内版 DeepSeek-V4 深度推理模型", ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true, IsReasoning: true},
		}
	}
}

// FetchModels 返回本实例可用模型清单；SOLO 尝试探测上游，失败时回退默认表。
func FetchModels(ctx context.Context, cred *Credential, cfg Config) ([]Model, error) {
	fallback := DefaultModelsFor(cfg)
	if !cfg.Solo || cred == nil || cred.AccessToken == "" {
		return fallback, nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ChatHost+EpModels, nil)
	if errReq != nil {
		return fallback, nil
	}
	ApplyTraeHeaders(req, cred, cfg, false)

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return fallback, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fallback, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Data struct {
			Models []struct {
				Name string `json:"model_name"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.Data.Models) > 0 {
		out := make([]Model, 0, len(result.Data.Models))
		for _, m := range result.Data.Models {
			if m.Name == "" {
				continue
			}
			out = append(out, Model{
				ID: m.Name, Name: m.Name, Description: "TRAE SOLO 上游模型",
				ContextWindow: 131072, MaxTokens: 8192, SupportsTools: true,
			})
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return fallback, nil
}
