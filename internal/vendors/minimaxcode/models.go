package minimaxcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
)

// Model 是 MiniMax Code 支持的模型元数据。
type Model struct {
	ID            string
	Name          string
	Description   string
	ContextWindow int64
	MaxOutput     int64
	SupportsTools bool
	IsReasoning   bool
	UpstreamModel string
	Variant       string
}

// bundledModels 返回 MiniMax Code 的内置模型清单（探测失败时的兜底）。
func bundledModels() []Model {
	return []Model{
		{
			ID:            "minimax-agent",
			Name:          "MiniMax Agent",
			Description:   "通用 Agent，自动规划并调用工具",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "",
		},
		{
			ID:            "minimax-m3",
			Name:          "MiniMax M3",
			Description:   "对话模式，响应更快",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3",
		},
		{
			ID:            "minimax-m3-thinking",
			Name:          "MiniMax M3 Thinking",
			Description:   "深度思考模式，推理内容走 reasoning_content",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m3.1-flash",
			Name:          "MiniMax M3.1 Flash Preview",
			Description:   "新一代 Flash 预览版，512K/1M 上下文，响应更快",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3.1-Flash",
		},
		{
			ID:            "minimax-m3.1-flash-thinking",
			Name:          "MiniMax M3.1 Flash Preview Thinking",
			Description:   "M3.1 Flash 预览版的深度思考模式",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3.1-Flash",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m2.7",
			Name:          "MiniMax M2.7",
			Description:   "上一代对话模型",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M2.7",
		},
	}
}

// FindModel 根据客户端请求的模型 ID 查找模型配置。
func findModel(modelID string) (Model, bool) {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	// 如果带有前缀，剥离前缀
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	for _, m := range bundledModels() {
		if strings.EqualFold(m.ID, clean) {
			return m, true
		}
	}
	// 默认回落到 minimax-agent
	return bundledModels()[0], false
}

// FetchModels 返回供宿主注册的模型清单。
func FetchModels(ctx context.Context, region Region) ([]Model, error) {
	_ = ctx
	_ = region
	return bundledModels(), nil
}

// 本文件实现「按账号工作区解析上游模型 ID」。
//
// 为什么需要它：上游会改名/下线模型（实测 2026-10-25：订阅工作区里的
// M3.1-Flash 已从 MiniMax-M3.1-Flash-Preview 更名为 MiniMax-M3.1-Flash），
// 而内置目录是编译期固化的——继续发旧 ID 会被 Agent 会话以
// 1406010011 "explicit model is not registered" 拒绝。
// 权威来源是 account 域的 /config（签名请求），它按账号列出当前可用的
// model_id；解析失败时回落内置目录（旧行为），不拖垮对话。

const workspaceModelsTTL = 10 * time.Minute

// workspaceModel 是工作区配置里的一个可用模型。
type workspaceModel struct {
	ModelID      string
	DisplayName  string
	ContextLimit int64
	Variants     []string
	Efforts      []string
}

type workspaceCacheEntry struct {
	models    []workspaceModel
	fetchedAt time.Time
}

var (
	workspaceMu    sync.Mutex
	workspaceCache = map[string]workspaceCacheEntry{}
)

// workspaceModelsFor 返回该账号工作区当前可用的模型清单（带 TTL 缓存）。
// 拉取失败返回 nil（调用方回落内置目录）。
func workspaceModelsFor(ctx context.Context, cred *Credential, baseURLOverride string) []workspaceModel {
	if cred == nil {
		return nil
	}
	key := cred.AccessToken
	workspaceMu.Lock()
	if entry, ok := workspaceCache[key]; ok && time.Since(entry.fetchedAt) < workspaceModelsTTL {
		models := entry.models
		workspaceMu.Unlock()
		return models
	}
	workspaceMu.Unlock()

	models, errFetch := fetchWorkspaceModels(ctx, cred, baseURLOverride)
	workspaceMu.Lock()
	defer workspaceMu.Unlock()
	if errFetch != nil || len(models) == 0 {
		// 拉取失败：保留旧缓存（如有），但不写空值——瞬时失败不该清掉已知清单。
		if entry, ok := workspaceCache[key]; ok {
			return entry.models
		}
		return nil
	}
	workspaceCache[key] = workspaceCacheEntry{models: models, fetchedAt: time.Now()}
	return models
}

// fetchWorkspaceModels 拉取并解析 /config 的 models 列表（签名 account 域请求）。
func fetchWorkspaceModels(ctx context.Context, cred *Credential, baseURLOverride string) ([]workspaceModel, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpConfig, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 20*time.Second)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, core.NewPluginError("minimax_config_unavailable",
			"config endpoint returned "+resp.Status, resp.StatusCode)
	}
	var body struct {
		Models []struct {
			ModelID           string   `json:"model_id"`
			DisplayName       string   `json:"display_name"`
			ContextLimit      int64    `json:"context_limit"`
			SupportedVariants []string `json:"supported_variants"`
			EffortOptions     []string `json:"effort_options"`
		} `json:"models"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); errDecode != nil {
		return nil, errDecode
	}
	models := make([]workspaceModel, 0, len(body.Models))
	for _, item := range body.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, workspaceModel{
			ModelID:      id,
			DisplayName:  item.DisplayName,
			ContextLimit: item.ContextLimit,
			Variants:     item.SupportedVariants,
			Efforts:      item.EffortOptions,
		})
	}
	return models, nil
}

// resolveWorkspaceModel 把客户端请求的模型解析成工作区当前注册的 model_id。
//
// 匹配顺序：请求 ID（去 -thinking 后缀再试）→ 内置目录的 UpstreamModel。
// 命中工作区模型时同时校对 variant（工作区声明支持才带上）。
// 未命中返回 ok=false，调用方沿用内置目录的 UpstreamModel（旧行为）。
func resolveWorkspaceModel(ctx context.Context, cred *Credential, baseURLOverride string, modelID string, bundled Model) (string, string, bool) {
	models := workspaceModelsFor(ctx, cred, baseURLOverride)
	if len(models) == 0 {
		return "", "", false
	}
	variant := bundled.Variant
	base := normalizeModelProbe(modelID)
	if strings.HasSuffix(base, "thinking") {
		// thinking 变体在请求侧以后缀表达；工作区侧以 supported_variants 表达。
		if variant == "" {
			variant = "thinking"
		}
		base = strings.TrimSuffix(strings.TrimSuffix(base, "thinking"), "-")
	}
	for _, item := range models {
		if !strings.EqualFold(item.ModelID, base) && !strings.EqualFold(item.ModelID, bundled.UpstreamModel) {
			continue
		}
		resolved := item.ModelID
		if variant != "" && !workspaceSupportsVariant(item, variant) {
			// 工作区不支持该变体：按无变体处理（宁可直接出话也不发非法参数）。
			variant = ""
		}
		return resolved, variant, true
	}
	return "", "", false
}

// workspaceSupportsVariant 报告工作区模型是否声明支持某变体；
// 未声明 supported_variants 的模型视为支持（保守沿用现状）。
func workspaceSupportsVariant(item workspaceModel, variant string) bool {
	if len(item.Variants) == 0 {
		return true
	}
	for _, v := range item.Variants {
		if strings.EqualFold(strings.TrimSpace(v), variant) {
			return true
		}
	}
	return false
}

// normalizeModelProbe 归一化请求 ID 用于工作区匹配：剥供应商前缀、小写。
func normalizeModelProbe(modelID string) string {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	return clean
}
