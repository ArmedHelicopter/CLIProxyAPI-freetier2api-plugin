package trae

// 本文件提供 Trae 家族的模型目录解析。
//
// TraeCode 网关有两个命名空间（参考 Trae2api-cn）：
//   - config_name：目录里的配置名（如 glm-5.2），请求体 config_name 字段；
//   - wire id：目录 model_detail_list[].model_name（如 glm-5.2__dev），
//     请求体 model_name 字段，且按大小写敏感匹配。
//
// 目录唯一来源：POST /api/ide/v1/get_detail_param（function=solo_work_lite，
// 其响应含 chat_v3 与 solo_agent 共享条目）。按账号缓存；缓存过期后重新
// 拉取，拉取失败时沿用旧条目（stale-while-error），没有旧条目才报错。
// usage=custom_model/summary 的条目是路由配置，不作为可选对话模型。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/hostbridge"
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

// ResolvedModel 是一次对话请求使用的模型标识对。
type ResolvedModel struct {
	ConfigName string // 请求体 config_name
	WireName   string // 请求体 model_name（上游 wire id）
}

// ---- 上游目录（按账号缓存） ----

type catalogEntry struct {
	models    map[string]ResolvedModel // config_name（小写）-> 解析结果
	metadata  map[string]Model         // config_name -> 元数据
	fetchedAt time.Time
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalogEntry{}
)

const catalogTTL = 10 * time.Minute

// resolveModelWithCatalog 用账号目录把请求模型名解析成 (config_name, wire id)。
//
// 模型名原样透传（不做别名归一）：目录里有什么就点什么，点不到即报错，
// 避免猜一个错的 wire id 换来上游 4023。
func resolveModelWithCatalog(ctx context.Context, model string, cfg Config, cred *Credential) (ResolvedModel, error) {
	requested := strings.TrimSpace(model)
	if requested == "" {
		requested = cfg.DefaultModel
	}
	entry, okEntry := loadCatalog(ctx, cred, cfg)
	if !okEntry {
		return ResolvedModel{}, fmt.Errorf("trae 模型目录暂不可用，请稍后重试")
	}
	if resolved, okResolved := entry.models[strings.ToLower(requested)]; okResolved {
		return resolved, nil
	}
	return ResolvedModel{}, fmt.Errorf("模型 %s 不在该账号的可用目录中", requested)
}

// loadCatalog 读取账号的模型目录缓存；过期则重新拉取，拉取失败沿用旧条目。
func loadCatalog(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	key := cfg.VendorID + "|" + cred.UID
	catalogMu.Lock()
	entry, okCached := catalogCache[key]
	catalogMu.Unlock()
	if okCached && time.Since(entry.fetchedAt) < catalogTTL {
		return entry, true
	}

	fresh, okFresh := fetchCatalog(ctx, cred, cfg)
	if okFresh {
		catalogMu.Lock()
		catalogCache[key] = fresh
		catalogMu.Unlock()
		return fresh, true
	}
	// 上游暂时不可用时沿用旧条目；从未拉到过才失败。
	if okCached {
		return entry, true
	}
	return catalogEntry{}, false
}

// catalogResponse 是 get_detail_param 的响应结构（只取需要的字段）。
type catalogResponse struct {
	ConfigInfoList []catalogItem `json:"config_info_list"`
}

type catalogItem struct {
	ConfigName    string `json:"config_name"`
	ConfigSwitch  bool   `json:"config_switch"`
	Usage         string `json:"usage"`
	CustomModels  any    `json:"custom_models"`
	ContextWindow struct {
		Dev int `json:"dev"`
	} `json:"context_window_tokens"`
	DisplayConfig struct {
		DisplayName     string `json:"display_name"`
		ModelCapability string `json:"model_capability"`
	} `json:"display_config"`
	ExtraConfig     string          `json:"extra_config"`
	ModelDetailList []catalogDetail `json:"model_detail_list"`
}

type catalogDetail struct {
	ModelName string `json:"model_name"`
	MaxTokens int    `json:"max_tokens"`
}

// selectWireModel 从 model_detail_list 里挑 wire id：优先 __dev 档位，
// 没有 __dev 的配置（如 DeepSeek-V4-Pro -> deepseek_v4_pro__dev）取第一条。
func selectWireModel(details []catalogDetail) (string, int) {
	fallback := ""
	fallbackTokens := 0
	for _, detail := range details {
		name := strings.TrimSpace(detail.ModelName)
		if name == "" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), "__dev") {
			return name, detail.MaxTokens
		}
		if fallback == "" {
			fallback = name
			fallbackTokens = detail.MaxTokens
		}
	}
	return fallback, fallbackTokens
}

// fetchCatalog 调用上游 get_detail_param 拉取账号可用模型目录。
//
// 请求方法与头集合对齐 Trae2api-cn 的 _fetch_solo_model_configs：POST +
// function=solo_work_lite + SOLO 版头。
func fetchCatalog(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	body, errBody := json.Marshal(map[string]any{
		"function":            "solo_work_lite",
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if errBody != nil {
		return catalogEntry{}, false
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ChatHost+EpModels, strings.NewReader(string(body)))
	if errReq != nil {
		return catalogEntry{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("X-Cloudide-Token", cred.AccessToken)
	req.Header.Set("X-Ide-Token", cred.AccessToken)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", cfg.AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-App-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version", cfg.IDEVersion)
	req.Header.Set("X-Ide-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Brand", cfg.DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("Request-Traffic-Type", "prod")

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return catalogEntry{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return catalogEntry{}, false
	}

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if errRead != nil {
		return catalogEntry{}, false
	}
	var parsed catalogResponse
	if errJSON := json.Unmarshal(raw, &parsed); errJSON != nil {
		return catalogEntry{}, false
	}

	entry := catalogEntry{
		models:    map[string]ResolvedModel{},
		metadata:  map[string]Model{},
		fetchedAt: time.Now(),
	}
	for _, item := range parsed.ConfigInfoList {
		name := strings.TrimSpace(item.ConfigName)
		if name == "" || !item.ConfigSwitch {
			continue
		}
		// usage=custom_model/summary 的条目是路由配置或内部摘要，不是可选对话模型。
		if usage := strings.ToLower(strings.TrimSpace(item.Usage)); usage != "" && usage != "chat_completion" {
			continue
		}
		if item.CustomModels != nil {
			continue
		}
		wire, maxTokens := selectWireModel(item.ModelDetailList)
		if wire == "" {
			continue
		}
		display := strings.TrimSpace(item.DisplayConfig.DisplayName)
		if display == "" {
			display = name
		}
		entry.models[strings.ToLower(name)] = ResolvedModel{ConfigName: name, WireName: wire}
		entry.metadata[name] = Model{
			ID:            name,
			Name:          display,
			Description:   "Trae 上游模型",
			ContextWindow: item.ContextWindow.Dev,
			MaxTokens:     maxTokens,
			SupportsTools: strings.Contains(item.ExtraConfig, `"native_function_call":true`),
			IsReasoning:   item.DisplayConfig.ModelCapability == "reasoning_model",
		}
	}
	if len(entry.models) == 0 {
		return catalogEntry{}, false
	}
	return entry, true
}

// FetchModels 返回该凭证账号可用模型清单（直接来自上游目录）。
func FetchModels(ctx context.Context, cred *Credential, cfg Config) ([]Model, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("trae: 无可用凭证，无法拉取模型目录")
	}
	entry, okEntry := loadCatalog(ctx, cred, cfg)
	if !okEntry {
		return nil, fmt.Errorf("trae: 模型目录拉取失败")
	}
	out := make([]Model, 0, len(entry.metadata))
	for _, model := range entry.metadata {
		out = append(out, model)
	}
	return out, nil
}
