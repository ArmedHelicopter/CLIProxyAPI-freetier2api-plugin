// Package modelconfig 管理模型的注册视图：禁用集合、别名、额外模型、
// 静态模型缓存与同名合并。
//
// 这些规则与 ABI 无关，属于领域逻辑；根层只保留请求解码与响应封装。
package modelconfig

import (
	"sort"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// Settings 是模型层关心的全局配置快照。
type Settings struct {
	EnabledRealms  []string
	ExtraModels    []string
	DisabledModels []string
	// ContextWindowFor 返回额外模型的上下文窗口兜底（缺省 0 表示不填）。
	ContextWindowFor func(modelID string) int64
}

// Manager 持有模型相关的进程级缓存。
type Manager struct {
	store    *state.Store
	registry *core.Registry

	mu       sync.RWMutex
	settings Settings
	disabled map[string]bool
	aliases  map[string]string
	// cached 是最近一次拿到的**官方 ID** 静态清单（控制台回读用）。
	cached map[string][]pluginapi.ModelInfo
}

// New 创建 Manager。
func New(store *state.Store, registry *core.Registry) *Manager {
	return &Manager{
		store:    store,
		registry: registry,
		disabled: map[string]bool{},
		aliases:  map[string]string{},
		cached:   map[string][]pluginapi.ModelInfo{},
	}
}

// Configure 更新全局设置并重算禁用集合（配置声明 ∪ 状态记录）。
func (m *Manager) Configure(settings Settings) {
	snap := settings
	snap.ExtraModels = append([]string(nil), settings.ExtraModels...)
	snap.DisabledModels = append([]string(nil), settings.DisabledModels...)
	snap.EnabledRealms = append([]string(nil), settings.EnabledRealms...)
	m.mu.Lock()
	m.settings = snap
	m.mu.Unlock()
	m.reloadDisabled()
}

// ReloadFromState 用状态快照重算禁用集合与别名表。
func (m *Manager) ReloadFromState(snapshot state.State) {
	m.mu.Lock()
	m.reloadAliasesLocked(snapshot.ModelAliases)
	m.mu.Unlock()
	m.reloadDisabledWith(snapshot.DisabledModels)
}

// RefreshFromState 直接读取存储快照并重算。
func (m *Manager) RefreshFromState() {
	if m.store == nil {
		return
	}
	m.ReloadFromState(m.store.Snapshot())
}

func (m *Manager) reloadDisabled() {
	if m.store == nil {
		m.reloadDisabledWith(nil)
		return
	}
	m.reloadDisabledWith(m.store.Snapshot().DisabledModels)
}

func (m *Manager) reloadDisabledWith(stateDisabled []string) {
	m.mu.Lock()
	merged := make([]string, 0, len(m.settings.DisabledModels)+len(stateDisabled))
	merged = append(merged, m.settings.DisabledModels...)
	merged = append(merged, stateDisabled...)
	set := make(map[string]bool, len(merged)*2)
	for _, item := range merged {
		for _, key := range m.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	m.disabled = set
	m.mu.Unlock()
}

// RealmEnabled 报告区域是否启用（空区域视为启用）。
func (m *Manager) RealmEnabled(region string) bool {
	m.mu.RLock()
	enabled := append([]string(nil), m.settings.EnabledRealms...)
	m.mu.RUnlock()
	return core.RealmEnabled(enabled, region)
}

// ExpandKeys 把模型标识展开成查询键。
//
//   - 裸名：原样返回（对所有供应商生效）；
//   - 供应商 ID 前缀：规范 vendorKey；
//   - 区域别名：展开成该区域每家供应商的 vendorKey。
func (m *Manager) ExpandKeys(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := m.registry.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := m.registry.ByID(prefix); okVendor {
		return []string{m.registry.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range m.registry.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, m.registry.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

// ExpandDisabledList 展开配置里的禁用项并稳定排序。
func (m *Manager) ExpandDisabledList(items []string) []string {
	set := make(map[string]bool, len(items)*2)
	for _, item := range items {
		for _, key := range m.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// IsDisabled 判断某供应商下的模型是否被禁用。
func (m *Manager) IsDisabled(vendorID, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	if prefix, rest := m.registry.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := m.vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = rest
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.disabled[bare] {
		return true
	}
	return m.disabled[m.registry.ModelKeyFor(vendorID, bare)]
}

// vendorForPrefix 把限定前缀（供应商 ID 或区域别名）解析成供应商实例。
func (m *Manager) vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := m.registry.ByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range m.registry.Vendors() {
		if vendor.Region() == prefix {
			return vendor, true
		}
	}
	return nil, false
}

// FilterForVendor 剔除某供应商下被禁用的模型。
func (m *Manager) FilterForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if m.IsDisabled(vendorID, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// SetDisabled 批量设置禁用状态，落盘并返回归一后的清单。
func (m *Manager) SetDisabled(targets []string, disabled bool) []string {
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		normalized = append(normalized, m.ExpandKeys(target)...)
	}
	if m.store == nil {
		return normalized
	}
	var next []string
	m.store.Mutate(func(s *state.State) {
		current := make(map[string]bool, len(s.DisabledModels))
		for _, id := range s.DisabledModels {
			current[strings.TrimSpace(id)] = true
		}
		for _, key := range normalized {
			if key == "" {
				continue
			}
			if disabled {
				current[key] = true
			} else {
				delete(current, key)
			}
		}
		next = make([]string, 0, len(current))
		for id := range current {
			next = append(next, id)
		}
		sort.Strings(next)
		s.DisabledModels = next
		s.RestartPending = true
	})
	m.reloadDisabledWith(next)
	return next
}

// RestartPending 报告是否存在待宿主重载的模型变更。
func (m *Manager) RestartPending() bool {
	if m.store == nil {
		return false
	}
	return m.store.Snapshot().RestartPending
}

// ClearRestartPending 在宿主重建模型注册表后清除待生效标记。
func (m *Manager) ClearRestartPending() {
	if m.store == nil {
		return
	}
	if !m.store.Snapshot().RestartPending {
		return
	}
	m.store.Mutate(func(s *state.State) { s.RestartPending = false })
}

// CacheStatic 记录某供应商的官方模型清单（控制台回读用）。
func (m *Manager) CacheStatic(vendorID string, models []pluginapi.ModelInfo) {
	copied := append([]pluginapi.ModelInfo(nil), models...)
	m.mu.Lock()
	m.cached[vendorID] = copied
	m.mu.Unlock()
}

// Registered 返回某供应商最近一次的官方模型清单。
func (m *Manager) Registered(vendorID string) []pluginapi.ModelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]pluginapi.ModelInfo(nil), m.cached[vendorID]...)
}

// LoadCachedState 从 state.json 恢复全部供应商的模型缓存，并通知供应商预热。
func (m *Manager) LoadCachedState(cached []state.CachedModel) {
	byVendor := map[string][]pluginapi.ModelInfo{}
	for _, item := range cached {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		vendorID := strings.TrimSpace(item.VendorID)
		if vendorID == "" {
			// 旧状态没有 vendor_id：当时只有 WorkBuddy 写模型缓存。
			switch item.Realm {
			case "global":
				vendorID = "workbuddyglobal"
			default:
				vendorID = "workbuddycn"
			}
		}
		byVendor[vendorID] = append(byVendor[vendorID], pluginapi.ModelInfo{
			ID: item.ID, Name: item.Name, DisplayName: item.Name,
			Description: item.Description, ContextLength: item.ContextLength,
			InputTokenLimit: item.ContextLength, MaxCompletionTokens: item.MaxOutputTokens,
			OutputTokenLimit: item.MaxOutputTokens, Thinking: thinkingFromCached(item),
		})
	}
	m.mu.Lock()
	for vendorID, models := range byVendor {
		m.cached[vendorID] = models
	}
	m.mu.Unlock()

	for _, vendor := range m.registry.Vendors() {
		loader, okLoader := core.As[core.ModelCacheLoader](vendor)
		if !okLoader {
			continue
		}
		loader.LoadModelCache(cachedForVendor(cached, vendor.ID()))
	}
}

// CachedState 把当前各供应商的模型缓存拍平成 state.json 结构。
func (m *Manager) CachedState() []state.CachedModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]state.CachedModel, 0, 64)
	for vendorID, models := range m.cached {
		for _, model := range models {
			entry := state.CachedModel{
				VendorID: vendorID, ID: model.ID, Name: model.Name,
				Description: model.Description, ContextLength: model.ContextLength,
				MaxOutputTokens: model.MaxCompletionTokens,
			}
			if vendor, okVendor := m.registry.ByID(vendorID); okVendor {
				entry.Realm = vendor.Region()
			}
			if model.Thinking != nil {
				entry.Efforts = append([]string(nil), model.Thinking.Levels...)
				entry.DefaultEffort = model.Thinking.Levels[0]
			}
			entry.SupportsImages = core.ModelSupportsImages(model)
			entry.SupportsTool = core.ModelSupportsTools(model)
			out = append(out, entry)
		}
	}
	return out
}

func cachedForVendor(cached []state.CachedModel, vendorID string) []state.CachedModel {
	out := make([]state.CachedModel, 0, len(cached))
	for _, item := range cached {
		id := item.VendorID
		if id == "" {
			switch item.Realm {
			case "global":
				id = "workbuddyglobal"
			default:
				id = "workbuddycn"
			}
		}
		if id == vendorID {
			out = append(out, item)
		}
	}
	return out
}

func thinkingFromCached(item state.CachedModel) *pluginapi.ThinkingSupport {
	if len(item.Efforts) == 0 && item.DefaultEffort == "" {
		return nil
	}
	return &pluginapi.ThinkingSupport{Levels: append([]string(nil), item.Efforts...), DynamicAllowed: item.DefaultEffort != ""}
}
