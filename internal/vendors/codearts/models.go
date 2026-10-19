package codearts

// 模型目录：提供 CodeArts 的内置与福利模型列表。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/models.go

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
	"freetier2api-plugin/internal/logger"
)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	SupportsTools bool   `json:"supports_tools"`
	IsReasoning   bool   `json:"is_reasoning"`
	// Benefit 标记该模型走每日免费福利额度（请求头 maas_type: benefit）。
	Benefit bool `json:"benefit,omitempty"`
}

// gatewayEnvelope 是福利网关接口的统一信封。
type gatewayEnvelope struct {
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Result    json.RawMessage `json:"result"`
}

// FetchBenefitModels 拉取每日免费福利通道的模型清单。
//
// 上游把「每日免费额度」与「订阅积分」分成两条通道：同一个模型名在两条通道里
// 都可能出现，区别只在请求头 maas_type。哪些模型能用免费额度完全由这份清单决定
// （积分通道的模型不在本插件支持范围内）。
func FetchBenefitModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, nil
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitConfig, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit models failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope gatewayEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit gateway config: %w", err)
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit gateway %s: %s", envelope.ErrorCode, envelope.ErrorMsg)
	}
	var config struct {
		Models []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ContextWindow int    `json:"context_window"`
			MaxTokens     int    `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(envelope.Result, &config); err != nil {
		return nil, fmt.Errorf("decode benefit model list: %w", err)
	}

	models := make([]Model, 0, len(config.Models))
	for _, item := range config.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:            id,
			Name:          core.FirstNonEmpty(item.ModelName, id),
			Description:   "CodeArts 每日免费福利额度模型",
			ContextWindow: item.ContextWindow,
			MaxTokens:     item.MaxTokens,
			SupportsTools: true,
			Benefit:       true,
		})
	}
	return models, nil
}

// DefaultModels 返回 CodeArts 的默认模型清单。
func bundledModels() []Model {
	return []Model{
		{
			ID:            "GLM-5.2",
			Name:          "GLM-5.2",
			Description:   "CodeArts 盘古代码大模型 GLM-5.2",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "glm-5.2",
			Name:          "GLM-5.2 (小写别名)",
			Description:   "CodeArts 盘古代码大模型 GLM-5.2",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "Qwen3-VL-235B",
			Name:          "Qwen3-VL-235B",
			Description:   "CodeArts 通义千问超大视觉模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "snap-chat",
			Name:          "Snap Chat",
			Description:   "CodeArts 默认代码对话模型",
			ContextWindow: 65536,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
	}
}

// FetchModels 尝试从上游拉取模型列表。
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return bundledModels(), nil
	}

	// 福利通道清单只在有凭证时拉取：它是「哪些模型能走每日免费额度」的唯一判据。
	benefitModels, errBenefit := FetchBenefitModels(ctx, cred)
	if errBenefit != nil {
		logger.Debug("codearts: 拉取福利模型清单失败: %v", errBenefit)
		benefitModels = nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpModelBuiltin, nil)
	if errReq != nil {
		return bundledModels(), nil
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return bundledModels(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return bundledModels(), nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Models []struct {
			ModelName string `json:"model_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.Models) > 0 {
		var out []Model
		for _, m := range result.Models {
			if m.ModelName == "" {
				continue
			}
			out = append(out, Model{
				ID:            m.ModelName,
				Name:          m.ModelName,
				Description:   "CodeArts 内置模型",
				ContextWindow: 131072,
				MaxTokens:     8192,
				SupportsTools: true,
			})
		}
		if len(out) > 0 {
			return mergeBenefitModels(out, benefitModels), nil
		}
	}

	return mergeBenefitModels(bundledModels(), benefitModels), nil
}

// mergeBenefitModels 把福利通道模型并进清单，已存在的 ID 不重复添加。
func mergeBenefitModels(base, benefit []Model) []Model {
	seen := make(map[string]bool, len(base))
	for _, model := range base {
		seen[model.ID] = true
	}
	for _, model := range benefit {
		if seen[model.ID] {
			continue
		}
		base = append(base, model)
	}
	return base
}
