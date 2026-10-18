package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// consoleModel 是控制台页展示用的模型条目。
//
// 刻意自定义而不直接回传 pluginapi.ModelInfo：后者没有 json tag，
// 字段名会变成 Go 字段名，页面按小写键读会全部拿到 undefined。
type consoleModel struct {
	ID              string   `json:"id"`
	ScopeID         string   `json:"scope_id"`
	VendorID        string   `json:"vendor_id"`
	VendorName      string   `json:"vendor_name"`
	Realm           string   `json:"realm"`
	Name            string   `json:"name,omitempty"`
	Alias           string   `json:"alias,omitempty"`
	Description     string   `json:"description,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTools   bool     `json:"supports_tools,omitempty"`
	Credits         string   `json:"credits,omitempty"`
	Disabled        bool     `json:"disabled,omitempty"`
}

func (s *Service) handleModelsList(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)
	cfg := s.deps.Config()
	manager := s.deps.Model()
	models := make([]consoleModel, 0, 128)

	active := s.hostActiveVendors(ctx, callbackID)

	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		models = append(models, s.consoleModelsForVendor(manager, vendor)...)
	}

	for _, extra := range manager.ExtraModels() {
		vendorID := extra.OwnedBy
		if vendorID != "" {
			if _, okVendor := s.registry.ByID(vendorID); !okVendor || !active[vendorID] {
				continue
			}
		}
		models = append(models, consoleModel{
			ID:            extra.ID,
			ScopeID:       s.registry.ModelKeyFor(vendorID, extra.ID),
			VendorID:      vendorID,
			VendorName:    s.vendorNameFor(vendorID),
			Realm:         s.vendorRegionFor(vendorID),
			Name:          core.FirstNonEmpty(extra.DisplayName, extra.Name),
			ContextLength: extra.ContextLength,
		})
	}

	for index := range models {
		models[index].Disabled = manager.IsDisabled(models[index].VendorID, models[index].ID)
		models[index].Alias = manager.AliasFor(models[index].VendorID, models[index].ID)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].VendorName != models[j].VendorName {
			return models[i].VendorName < models[j].VendorName
		}
		return models[i].ID < models[j].ID
	})

	payload := map[string]any{"ok": true, "models": models, "count": len(models)}
	if len(models) == 0 {
		payload["error"] = "本地暂无模型缓存，正在从上游获取；也可点「从上游刷新」立即拉取。"
	}
	return JSONResponse(http.StatusOK, payload)
}

func (s *Service) consoleModelsForVendor(manager *Catalog, vendor core.Vendor) []consoleModel {
	if provider, okProvider := core.As[core.ConsoleModelProvider](vendor); okProvider {
		return s.consoleModelsFromProvider(vendor, provider.ConsoleModels())
	}
	infos := manager.Registered(vendor.ID())
	out := make([]consoleModel, 0, len(infos))
	for _, info := range infos {
		entry := consoleModel{
			ID:              info.ID,
			ScopeID:         s.registry.ModelKeyFor(vendor.ID(), info.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            core.FirstNonEmpty(info.DisplayName, info.Name),
			Description:     info.Description,
			ContextLength:   info.ContextLength,
			MaxOutputTokens: info.MaxCompletionTokens,
			SupportsImages:  core.ModelSupportsImages(info),
			SupportsTools:   core.ModelSupportsTools(info),
		}
		if info.Thinking != nil {
			entry.Efforts = info.Thinking.Levels
		}
		out = append(out, entry)
	}
	return out
}

func (s *Service) consoleModelsFromProvider(vendor core.Vendor, models []core.ConsoleModel) []consoleModel {
	out := make([]consoleModel, 0, len(models))
	for _, model := range models {
		out = append(out, consoleModel{
			ID:              model.ID,
			ScopeID:         s.registry.ModelKeyFor(vendor.ID(), model.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxOutputTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsTools,
			Credits:         model.Credits,
		})
	}
	return out
}

func (s *Service) vendorNameFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Name()
	}
	return ""
}

func (s *Service) vendorRegionFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Region()
	}
	return ""
}

// ModelToggle 批量禁用/启用模型（导出给根层兼容包装）。
func (s *Service) ModelToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelToggleHandler(req)
}

// ModelAlias 设置/清除别名（导出给根层兼容包装）。
func (s *Service) ModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelAliasHandler(req)
}

// modelToggleHandler 批量禁用/启用模型。
func (s *Service) modelToggleHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Models   []string `json:"models"`
		Disabled bool     `json:"disabled"`
	}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}
	targets := make([]string, 0, len(body.Models))
	for _, item := range body.Models {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请先选择要操作的模型"})
	}
	s.deps.Model().SetDisabled(targets, body.Disabled)
	verb := "启用"
	if body.Disabled {
		verb = "禁用"
	}
	logger.Info("models %s: %d 个（%s）", verb, len(targets), strings.Join(targets, ", "))
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": s.deps.State().DisabledModels,
		"message":         "已" + verb + " " + strconv.Itoa(len(targets)) + " 个模型（调用即时生效）",
	})
}

// modelAliasHandler 设置或清除某个模型的别名。
func (s *Service) modelAliasHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Model string `json:"model"`
		Alias string `json:"alias"`
	}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}
	vendorID, bare := s.deps.Model().VendorForAliasScope(body.Model)
	if vendorID == "" || strings.TrimSpace(bare) == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "model 必须是 <vendor>:<模型 ID> 形式",
		})
	}
	alias := strings.TrimSpace(body.Alias)
	if strings.ContainsAny(alias, " \t\r\n") {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "别名不能包含空白字符",
		})
	}
	s.deps.Model().SetAlias(vendorID, bare, alias)
	return JSONResponse(http.StatusOK, map[string]any{"ok": true})
}
