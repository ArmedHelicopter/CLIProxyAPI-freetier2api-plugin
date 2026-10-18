package modelconfig

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
)

// StaticModels 返回所有启用且有凭证的供应商的模型并集（同名合并）。
func (m *Manager) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	var all []pluginapi.ModelInfo
	var firstErr error
	active := hostabi.ActiveVendorSet(ctx, "")
	for _, vendor := range core.Vendors() {
		if !m.RealmEnabled(vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		// 缓存官方 ID 清单供管理页回读（注册路径随后会换成别名）。
		m.CacheStatic(vendor.ID(), models)
		all = append(all, m.ApplyAliases(vendor.ID(), m.FilterForVendor(vendor.ID(), models))...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	for _, extra := range m.ExtraModels() {
		all = append(all, m.ApplyAliases(extra.OwnedBy, []pluginapi.ModelInfo{extra})...)
	}
	return MergeSameNameModels(all), nil
}

// RefreshFromUpstream 主动刷新所有启用且有凭证的供应商模型清单，返回条目数。
func (m *Manager) RefreshFromUpstream(ctx context.Context) (int, error) {
	total := 0
	var firstErr error
	active := hostabi.ActiveVendorSet(ctx, "")
	for _, vendor := range core.Vendors() {
		if !m.RealmEnabled(vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		m.CacheStatic(vendor.ID(), models)
		total += len(models)
	}
	if total == 0 && firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// ExtraModels 把配置里的额外模型名转成模型条目。
func (m *Manager) ExtraModels() []pluginapi.ModelInfo {
	m.mu.RLock()
	settings := m.settings
	m.mu.RUnlock()
	out := make([]pluginapi.ModelInfo, 0, len(settings.ExtraModels))
	for _, name := range settings.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		if vendorID, bareModel := core.SplitModelID(bare); vendorID != "" {
			if vendor, okVendor := core.VendorByID(vendorID); okVendor &&
				m.RealmEnabled(vendor.Region()) && !m.IsDisabled(vendor.ID(), bareModel) {
				out = append(out, m.extraModelInfo(vendor.ID(), bareModel, settings))
			}
			continue
		}
		for _, vendor := range core.Vendors() {
			if !m.RealmEnabled(vendor.Region()) || m.IsDisabled(vendor.ID(), bare) {
				continue
			}
			out = append(out, m.extraModelInfo(vendor.ID(), bare, settings))
		}
	}
	return out
}

func (m *Manager) extraModelInfo(vendorID, bareModel string, settings Settings) pluginapi.ModelInfo {
	var contextLength int64
	if settings.ContextWindowFor != nil {
		contextLength = settings.ContextWindowFor(bareModel)
	}
	return core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
		ID:            bareModel,
		Name:          bareModel,
		ContextLength: contextLength,
	})
}

// MergeSameNameModels 按裸模型名合并同名条目，参数取并集最优。
//
// 宿主按 EqualsFold 匹配模型 ID，同名条目在它眼里就是同一个模型；
// 不合并会让 /v1/models 列出重复项。窗口/输出取大，能力取或，档位取并集。
func MergeSameNameModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	merged := make([]pluginapi.ModelInfo, 0, len(models))
	seen := make(map[string]int, len(models))
	for _, model := range models {
		bare := strings.TrimSpace(model.ID)
		if bare == "" {
			continue
		}
		key := strings.ToLower(bare)
		index, okSeen := seen[key]
		if !okSeen {
			seen[key] = len(merged)
			merged = append(merged, model)
			continue
		}
		target := &merged[index]
		if model.ContextLength > target.ContextLength {
			target.ContextLength = model.ContextLength
			target.InputTokenLimit = model.InputTokenLimit
		}
		if model.MaxCompletionTokens > target.MaxCompletionTokens {
			target.MaxCompletionTokens = model.MaxCompletionTokens
			target.OutputTokenLimit = model.OutputTokenLimit
		}
		if target.Description == "" {
			target.Description = model.Description
		}
		if core.ModelSupportsImages(model) {
			target.SupportedInputModalities = appendUnique(target.SupportedInputModalities, "image")
		}
		if core.ModelSupportsTools(model) {
			target.SupportedGenerationMethods = appendUnique(target.SupportedGenerationMethods, "tool_calls")
		}
		target.Thinking = mergeThinking(target.Thinking, model.Thinking)
	}
	return merged
}

func mergeThinking(a, b *pluginapi.ThinkingSupport) *pluginapi.ThinkingSupport {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	levels := append([]string(nil), a.Levels...)
	for _, level := range b.Levels {
		levels = appendUnique(levels, level)
	}
	return &pluginapi.ThinkingSupport{
		Levels:         levels,
		DynamicAllowed: a.DynamicAllowed || b.DynamicAllowed,
		ZeroAllowed:    a.ZeroAllowed || b.ZeroAllowed,
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
