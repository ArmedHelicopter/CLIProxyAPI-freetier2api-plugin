package trae

// 本文件提供 Trae 家族的模型目录解析。
//
// TraeCode 网关有两个命名空间（参考 Trae2api-cn）：
//   - config_name：目录里的配置名（如 glm-5.2），请求体 config_name 字段；
//   - wire id：目录 model_detail_list[].model_name（如 glm-5.2__dev），
//     请求体 model_name 字段，且按大小写敏感匹配。
//
// 目录唯一来源：POST /api/ide/v1/get_detail_param（function 与对话侧一致，
// 见 fetchCatalog）。按账号缓存；缓存过期后重新拉取，拉取失败时沿用旧条目
// （stale-while-error），没有旧条目才报错。
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
//
// 缓存键带上 function 与构建号：目录内容由这两者共同决定，只按账号缓存会在
// 版本号被配置改写后继续命中旧清单（最长 catalogTTL）。
func loadCatalog(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	key := strings.Join([]string{cfg.VendorID, cred.UID, cfg.Function, cfg.IDEVersionCode}, "|")
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
// 没有 __dev 的配置取第一条。注意 wire id 属于某个 function 的命名空间，
// 返回值只在该 function 下有效（见 fetchCatalog）。
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
// 关键约束：**function 必须与对话侧一致**（cfg.Function）。上游按 function
// 维护两套命名空间，同一个模型在不同 function 下的 wire id 不同：
//   - chat_v3：        DeepSeek-V4-Pro -> DeepSeek-V4-Pro__dev
//   - solo_work_lite： DeepSeek-V4-Pro -> deepseek_v4_pro__dev
//
// 目录取 solo_work_lite 的 wire id 再去 chat_v3 对话会被上游判成
// 4023 model is unknown。因此请求体、X-App-Function、X-Ide-Function 三处
// 都下发 cfg.Function。
func fetchCatalog(ctx context.Context, cred *Credential, cfg Config) (catalogEntry, bool) {
	body, errBody := json.Marshal(map[string]any{
		"function":            cfg.Function,
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
	req.Header.Set("X-App-Function", cfg.Function)
	req.Header.Set("X-Ide-Function", cfg.Function)
	req.Header.Set("X-Device-Brand", cfg.DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("Request-Traffic-Type", "prod")

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		logger.Info("trae catalog: %s request failed: %v", cfg.VendorID, errDo)
		return catalogEntry{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		logger.Info("trae catalog: %s status=%d body=%s", cfg.VendorID, resp.StatusCode, core.Truncate(string(raw), 512))
		return catalogEntry{}, false
	}
	if errRead != nil {
		logger.Info("trae catalog: %s read body failed: %v", cfg.VendorID, errRead)
		return catalogEntry{}, false
	}
	var parsed catalogResponse
	if errJSON := json.Unmarshal(raw, &parsed); errJSON != nil {
		logger.Info("trae catalog: %s decode failed: %v body=%s", cfg.VendorID, errJSON, core.Truncate(string(raw), 512))
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
		// 上游偶尔会整体降级（例如构建号低于门控时只回旧清单）。把条目摘要
		// 打出来，才能区分「目录结构变了」和「版本号被降级」。
		logger.Info("trae catalog: %s 过滤后为空（function=%s code=%s，原始 %d 条）: %s",
			cfg.VendorID, cfg.Function, cfg.IDEVersionCode, len(parsed.ConfigInfoList),
			summarizeCatalog(parsed.ConfigInfoList))
		return catalogEntry{}, false
	}
	return entry, true
}

// summarizeCatalog 生成原始条目摘要，只含分类字段、不涉及凭证。
func summarizeCatalog(items []catalogItem) string {
	const limit = 20
	parts := make([]string, 0, min(len(items), limit))
	for index, item := range items {
		if index >= limit {
			parts = append(parts, fmt.Sprintf("...(+%d)", len(items)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s[usage=%s,switch=%t,custom=%t]",
			item.ConfigName, item.Usage, item.ConfigSwitch, item.CustomModels != nil))
	}
	return strings.Join(parts, ", ")
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
