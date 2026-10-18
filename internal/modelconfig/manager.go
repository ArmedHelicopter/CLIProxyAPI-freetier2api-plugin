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
	store *state.Store

	mu       sync.RWMutex
	settings Settings
	disabled map[string]bool
	aliases  map[string]string
	// cached 是最近一次拿到的**官方 ID** 静态清单（控制台回读用）。
	cached map[string][]pluginapi.ModelInfo
}

// New 创建 Manager。
func New(store *state.Store) *Manager {
	return &Manager{
		store:    store,
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
		for _, key := range ExpandKeys(item) {
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
	if strings.TrimSpace(region) == "" {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.settings.EnabledRealms) == 0 {
		return true
	}
	for _, item := range m.settings.EnabledRealms {
		if item == region {
			return true
		}
	}
	return false
}

// SnapshotSettings 返回当前设置副本。
func (m *Manager) SnapshotSettings() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// ExpandKeys 把模型标识展开成查询键。
//
//   - 裸名：原样返回（对所有供应商生效）；
//   - 供应商 ID 前缀：规范 vendorKey；
//   - 区域别名：展开成该区域每家供应商的 vendorKey。
func ExpandKeys(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := core.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := core.VendorByID(prefix); okVendor {
		return []string{core.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range core.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, core.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

// ExpandDisabledList 展开配置里的禁用项并稳定排序。
func ExpandDisabledList(items []string) []string {
	set := make(map[string]bool, len(items)*2)
	for _, item := range items {
		for _, key := range ExpandKeys(item) {
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
	if prefix, rest := core.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = rest
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.disabled[bare] {
		return true
	}
	return m.disabled[core.ModelKeyFor(vendorID, bare)]
}

// vendorForPrefix 把限定前缀（供应商 ID 或区域别名）解析成供应商实例。
func vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := core.VendorByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range core.Vendors() {
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
		normalized = append(normalized, ExpandKeys(target)...)
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

// SnapshotCached 返回全部缓存的副本（管理页批量展示用）。
func (m *Manager) SnapshotCached() map[string][]pluginapi.ModelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]pluginapi.ModelInfo, len(m.cached))
	for vendorID, models := range m.cached {
		out[vendorID] = append([]pluginapi.ModelInfo(nil), models...)
	}
	return out
}

// ResetDisabledForTest 清空禁用集合（测试隔离用）。
func (m *Manager) ResetDisabledForTest() {
	m.mu.Lock()
	m.disabled = map[string]bool{}
	m.mu.Unlock()
}

// ResetForTest 清空全部进程级缓存（测试隔离用）。
func (m *Manager) ResetForTest() {
	m.mu.Lock()
	m.disabled = map[string]bool{}
	m.aliases = map[string]string{}
	m.cached = map[string][]pluginapi.ModelInfo{}
	m.mu.Unlock()
}
