// 本文件是模型目录（catalog）：静态清单缓存、别名、禁用集合、同名合并与刷新。
//
// 放在 admin 包内是因为它只服务于管理页与模型注册这条链：目录的写入全部
// 来自管理接口或注册回包，读写都要经过宿主 RPC 与 state.json，没有第二个
// 调用方。为此单开一个包只会让 admin 与它互相 import。
package admin

import (
	"context"
	"sort"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// 模型目录（Catalog）管理模型的注册视图：禁用集合、别名、额外模型、
// 静态模型缓存与同名合并。
//
// 这些规则与传输层无关；管理接口只做请求解码与响应封装，规则全在这里。

// ModelSettings 是模型层关心的全局配置快照。
type ModelSettings struct {
	EnabledRealms  []string
	ExtraModels    []string
	DisabledModels []string
	// ContextWindowFor 返回额外模型的上下文窗口兜底（缺省 0 表示不填）。
	ContextWindowFor func(modelID string) int64
}

// Catalog 持有模型相关的进程级缓存。
type Catalog struct {
	store    *state.Store
	registry *core.Registry

	mu       sync.RWMutex
	settings ModelSettings
	disabled map[string]bool
	aliases  map[string]string
	// cached 是最近一次拿到的**官方 ID** 静态清单（控制台回读用）。
	cached map[string][]pluginapi.ModelInfo
}

// New 创建 Catalog。
func NewCatalog(store *state.Store, registry *core.Registry) *Catalog {
	return &Catalog{
		store:    store,
		registry: registry,
		disabled: map[string]bool{},
		aliases:  map[string]string{},
		cached:   map[string][]pluginapi.ModelInfo{},
	}
}

// Configure 更新全局设置并重算禁用集合（配置声明 ∪ 状态记录）。
func (c *Catalog) Configure(settings ModelSettings) {
	snap := settings
	snap.ExtraModels = append([]string(nil), settings.ExtraModels...)
	snap.DisabledModels = append([]string(nil), settings.DisabledModels...)
	snap.EnabledRealms = append([]string(nil), settings.EnabledRealms...)
	c.mu.Lock()
	c.settings = snap
	c.mu.Unlock()
	c.reloadDisabled()
}

// ReloadFromState 用状态快照重算禁用集合与别名表。
func (c *Catalog) ReloadFromState(snapshot state.State) {
	c.mu.Lock()
	c.reloadAliasesLocked(snapshot.ModelAliases)
	c.mu.Unlock()
	c.reloadDisabledWith(snapshot.DisabledModels)
}

// RefreshFromState 直接读取存储快照并重算。
func (c *Catalog) RefreshFromState() {
	if c.store == nil {
		return
	}
	c.ReloadFromState(c.store.Snapshot())
}

func (c *Catalog) reloadDisabled() {
	if c.store == nil {
		c.reloadDisabledWith(nil)
		return
	}
	c.reloadDisabledWith(c.store.Snapshot().DisabledModels)
}

func (c *Catalog) reloadDisabledWith(stateDisabled []string) {
	c.mu.Lock()
	merged := make([]string, 0, len(c.settings.DisabledModels)+len(stateDisabled))
	merged = append(merged, c.settings.DisabledModels...)
	merged = append(merged, stateDisabled...)
	set := make(map[string]bool, len(merged)*2)
	for _, item := range merged {
		for _, key := range c.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	c.disabled = set
	c.mu.Unlock()
}

// RealmEnabled 报告区域是否启用（空区域视为启用）。
func (c *Catalog) RealmEnabled(region string) bool {
	c.mu.RLock()
	enabled := append([]string(nil), c.settings.EnabledRealms...)
	c.mu.RUnlock()
	return core.RealmEnabled(enabled, region)
}

// ExpandKeys 把模型标识展开成查询键。
//
//   - 裸名：原样返回（对所有供应商生效）；
//   - 供应商 ID 前缀：规范 vendorKey；
//   - 区域别名：展开成该区域每家供应商的 vendorKey。
func (c *Catalog) ExpandKeys(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := c.registry.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := c.registry.ByID(prefix); okVendor {
		return []string{c.registry.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, c.registry.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

// ExpandDisabledList 展开配置里的禁用项并稳定排序。
func (c *Catalog) ExpandDisabledList(items []string) []string {
	set := make(map[string]bool, len(items)*2)
	for _, item := range items {
		for _, key := range c.ExpandKeys(item) {
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
func (c *Catalog) IsDisabled(vendorID, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	if prefix, rest := c.registry.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := c.vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = rest
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.disabled[bare] {
		return true
	}
	return c.disabled[c.registry.ModelKeyFor(vendorID, bare)]
}

// vendorForPrefix 把限定前缀（供应商 ID 或区域别名）解析成供应商实例。
func (c *Catalog) vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := c.registry.ByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			return vendor, true
		}
	}
	return nil, false
}

// FilterForVendor 剔除某供应商下被禁用的模型。
func (c *Catalog) FilterForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if c.IsDisabled(vendorID, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// SetDisabled 批量设置禁用状态，落盘并返回归一后的清单。
func (c *Catalog) SetDisabled(targets []string, disabled bool) []string {
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		normalized = append(normalized, c.ExpandKeys(target)...)
	}
	if c.store == nil {
		return normalized
	}
	var next []string
	c.store.Mutate(func(s *state.State) {
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
	c.reloadDisabledWith(next)
	return next
}

// RestartPending 报告是否存在待宿主重载的模型变更。
func (c *Catalog) RestartPending() bool {
	if c.store == nil {
		return false
	}
	return c.store.Snapshot().RestartPending
}

// ClearRestartPending 在宿主重建模型注册表后清除待生效标记。
func (c *Catalog) ClearRestartPending() {
	if c.store == nil {
		return
	}
	if !c.store.Snapshot().RestartPending {
		return
	}
	c.store.Mutate(func(s *state.State) { s.RestartPending = false })
}

// CacheStatic 记录某供应商的官方模型清单（控制台回读用）。
func (c *Catalog) CacheStatic(vendorID string, models []pluginapi.ModelInfo) {
	copied := append([]pluginapi.ModelInfo(nil), models...)
	c.mu.Lock()
	c.cached[vendorID] = copied
	c.mu.Unlock()
}

// Registered 返回某供应商最近一次的官方模型清单。
func (c *Catalog) Registered(vendorID string) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]pluginapi.ModelInfo(nil), c.cached[vendorID]...)
}

// LoadCachedState 从 state.json 恢复全部供应商的模型缓存，并通知供应商预热。
func (c *Catalog) LoadCachedState(cached []core.CachedModel) {
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
	c.mu.Lock()
	for vendorID, models := range byVendor {
		c.cached[vendorID] = models
	}
	c.mu.Unlock()

	for _, vendor := range c.registry.Vendors() {
		loader, okLoader := core.As[core.ModelCacheLoader](vendor)
		if !okLoader {
			continue
		}
		loader.LoadModelCache(cachedForVendor(cached, vendor.ID()))
	}
}

// CachedState 把当前各供应商的模型缓存拍平成 state.json 结构。
func (c *Catalog) CachedState() []core.CachedModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]core.CachedModel, 0, 64)
	for vendorID, models := range c.cached {
		for _, model := range models {
			entry := core.CachedModel{
				VendorID: vendorID, ID: model.ID, Name: model.Name,
				Description: model.Description, ContextLength: model.ContextLength,
				MaxOutputTokens: model.MaxCompletionTokens,
			}
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor {
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

func cachedForVendor(cached []core.CachedModel, vendorID string) []core.CachedModel {
	out := make([]core.CachedModel, 0, len(cached))
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

func thinkingFromCached(item core.CachedModel) *pluginapi.ThinkingSupport {
	if len(item.Efforts) == 0 && item.DefaultEffort == "" {
		return nil
	}
	return &pluginapi.ThinkingSupport{Levels: append([]string(nil), item.Efforts...), DynamicAllowed: item.DefaultEffort != ""}
}

// aliasScopedKey 是别名表的键：<vendorID>:<官方模型 ID>。
func aliasScopedKey(vendorID, modelID string) string {
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(modelID)
}

func (c *Catalog) reloadAliasesLocked(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		trimmedKey := strings.TrimSpace(key)
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedKey == "" || trimmedAlias == "" {
			continue
		}
		next[trimmedKey] = trimmedAlias
	}
	c.aliases = next
}

// AliasFor 返回某供应商某模型设置的别名；未设置时返回空串。
func (c *Catalog) AliasFor(vendorID, modelID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.aliases[aliasScopedKey(vendorID, modelID)]
}

// ApplyAliases 给某供应商的模型清单换上别名（对外 ID）。
func (c *Catalog) ApplyAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(models) == 0 || len(c.aliases) == 0 {
		return models
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if alias := c.aliases[prefix+strings.TrimSpace(model.ID)]; alias != "" {
			model.ID = alias
			model.Name = alias
			model.DisplayName = alias
		}
		out = append(out, model)
	}
	return out
}

// ResolveOutboundModel 把客户端请求的模型名还原成上游的官方 ID。
func (c *Catalog) ResolveOutboundModel(vendorID, model string) string {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return model
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, alias := range c.aliases {
		if strings.EqualFold(alias, requested) && strings.HasPrefix(key, prefix) {
			return strings.TrimPrefix(key, prefix)
		}
	}
	return model
}

// SetAlias 设置或清除某模型的别名。
//
// 同一供应商内别名唯一：写入时清掉占用同名别名的旧模型，否则反查会命中两条。
func (c *Catalog) SetAlias(vendorID, bare, alias string) {
	if c.store == nil {
		return
	}
	scoped := aliasScopedKey(vendorID, bare)
	alias = strings.TrimSpace(alias)
	var next map[string]string
	c.store.Mutate(func(s *state.State) {
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
	c.mu.Lock()
	c.reloadAliasesLocked(next)
	c.mu.Unlock()
}

// VendorForAliasScope 解析 <vendor>:<model> 形式，供管理接口校验。
func (c *Catalog) VendorForAliasScope(scope string) (string, string) {
	return c.registry.SplitModelID(strings.TrimSpace(scope))
}

// StaticModels 返回所有启用且有凭证的供应商的模型并集（同名合并）。
//
// 诊断约定：某供应商没出现在结果里时，这里必须留下可读的原因（域未启用 /
// 没有可用凭证 / 探测失败）。否则用户只看到「模型列表是空的」，无从下手。
func (c *Catalog) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	var all []pluginapi.ModelInfo
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) {
			logger.Debug("models: skip %s (realm %q not enabled)", vendor.ID(), vendor.Region())
			continue
		}
		if !active[vendor.ID()] {
			logger.Debug("models: skip %s (no usable credential)", vendor.ID())
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: %s probe failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: %s registered %d model(s)", vendor.ID(), len(models))
		// 缓存官方 ID 清单供管理页回读（注册路径随后会换成别名）。
		c.CacheStatic(vendor.ID(), models)
		all = append(all, c.ApplyAliases(vendor.ID(), c.FilterForVendor(vendor.ID(), models))...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	for _, extra := range c.ExtraModels() {
		all = append(all, c.ApplyAliases(extra.OwnedBy, []pluginapi.ModelInfo{extra})...)
	}
	return MergeSameNameModels(all), nil
}

// RefreshFromUpstream 主动刷新所有启用且有凭证的供应商模型清单，返回条目数。
func (c *Catalog) RefreshFromUpstream(ctx context.Context) (int, error) {
	total := 0
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) || !active[vendor.ID()] {
			logger.Debug("models: refresh skip %s (realm enabled=%t, credential=%t)",
				vendor.ID(), c.RealmEnabled(vendor.Region()), active[vendor.ID()])
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: refresh %s failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: refresh %s got %d model(s)", vendor.ID(), len(models))
		c.CacheStatic(vendor.ID(), models)
		total += len(models)
	}
	if total == 0 && firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// ExtraModels 把配置里的额外模型名转成模型条目。
func (c *Catalog) ExtraModels() []pluginapi.ModelInfo {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	out := make([]pluginapi.ModelInfo, 0, len(settings.ExtraModels))
	for _, name := range settings.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		if vendorID, bareModel := c.registry.SplitModelID(bare); vendorID != "" {
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor &&
				c.RealmEnabled(vendor.Region()) && !c.IsDisabled(vendor.ID(), bareModel) {
				out = append(out, c.extraModelInfo(vendor.ID(), bareModel, settings))
			}
			continue
		}
		for _, vendor := range c.registry.Vendors() {
			if !c.RealmEnabled(vendor.Region()) || c.IsDisabled(vendor.ID(), bare) {
				continue
			}
			out = append(out, c.extraModelInfo(vendor.ID(), bare, settings))
		}
	}
	return out
}

func (c *Catalog) extraModelInfo(vendorID, bareModel string, settings ModelSettings) pluginapi.ModelInfo {
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
