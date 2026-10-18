package modelconfig

import (
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/state"
)

// aliasScopedKey 是别名表的键：<vendorID>:<官方模型 ID>。
func aliasScopedKey(vendorID, modelID string) string {
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(modelID)
}

func (m *Manager) reloadAliasesLocked(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		trimmedKey := strings.TrimSpace(key)
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedKey == "" || trimmedAlias == "" {
			continue
		}
		next[trimmedKey] = trimmedAlias
	}
	m.aliases = next
}

// AliasFor 返回某供应商某模型设置的别名；未设置时返回空串。
func (m *Manager) AliasFor(vendorID, modelID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.aliases[aliasScopedKey(vendorID, modelID)]
}

// ApplyAliases 给某供应商的模型清单换上别名（对外 ID）。
func (m *Manager) ApplyAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(models) == 0 || len(m.aliases) == 0 {
		return models
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if alias := m.aliases[prefix+strings.TrimSpace(model.ID)]; alias != "" {
			model.ID = alias
			model.Name = alias
			model.DisplayName = alias
		}
		out = append(out, model)
	}
	return out
}

// ResolveOutboundModel 把客户端请求的模型名还原成上游的官方 ID。
func (m *Manager) ResolveOutboundModel(vendorID, model string) string {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return model
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	m.mu.RLock()
	defer m.mu.RUnlock()
	for key, alias := range m.aliases {
		if strings.EqualFold(alias, requested) && strings.HasPrefix(key, prefix) {
			return strings.TrimPrefix(key, prefix)
		}
	}
	return model
}

// SetAlias 设置或清除某模型的别名。
//
// 同一供应商内别名唯一：写入时清掉占用同名别名的旧模型，否则反查会命中两条。
func (m *Manager) SetAlias(vendorID, bare, alias string) {
	if m.store == nil {
		return
	}
	scoped := aliasScopedKey(vendorID, bare)
	alias = strings.TrimSpace(alias)
	var next map[string]string
	m.store.Mutate(func(s *state.State) {
		if s.ModelAliases == nil {
			s.ModelAliases = map[string]string{}
		}
		s.RestartPending = true
		if alias == "" {
			delete(s.ModelAliases, scoped)
		} else {
			prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
			for existing, existingAlias := range s.ModelAliases {
				if existing != scoped && strings.EqualFold(existingAlias, alias) && strings.HasPrefix(existing, prefix) {
					delete(s.ModelAliases, existing)
				}
			}
			s.ModelAliases[scoped] = alias
		}
		next = make(map[string]string, len(s.ModelAliases))
		for key, value := range s.ModelAliases {
			next[key] = value
		}
	})
	m.mu.Lock()
	m.reloadAliasesLocked(next)
	m.mu.Unlock()
}

// VendorForAliasScope 解析 <vendor>:<model> 形式，供管理接口校验。
func (m *Manager) VendorForAliasScope(scope string) (string, string) {
	return m.registry.SplitModelID(strings.TrimSpace(scope))
}
