// Package tasks 提供供应商无关的任务目录、排程与调度基础设施。
package tasks

import (
	"sort"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

// Catalog 汇总已注册供应商的任务族。
type Catalog struct{ registry *core.Registry }

// NewCatalog 构造任务目录。
func NewCatalog(registry *core.Registry) *Catalog { return &Catalog{registry: registry} }

// Specs 汇总全部启用任务族的规格（按 kind 去重，先注册者优先）。
func (c *Catalog) Specs() []core.TaskSpec {
	out := make([]core.TaskSpec, 0, 8)
	seen := map[string]bool{}
	if c.anyCheckin() {
		seen[core.TaskKindCheckin] = true
		out = append(out, core.TaskSpec{
			Kind:         core.TaskKindCheckin,
			Label:        "每日签到",
			Description:  "对所有支持签到的账号执行签到",
			DefaultHours: []int{9, 21},
			Manual:       true,
		})
	}
	for _, vendor := range c.registry.Vendors() {
		catalog, okCatalog := core.As[core.TaskCatalogProvider](vendor)
		if !okCatalog {
			continue
		}
		for _, spec := range catalog.TaskSpecs() {
			kind := strings.TrimSpace(spec.Kind)
			if kind == "" || seen[kind] {
				continue
			}
			spec.Kind = kind
			spec.DefaultHours = NormalizeHours(spec.DefaultHours)
			seen[kind] = true
			out = append(out, spec)
		}
	}
	return out
}

// SpecByKind 返回某个任务族的规格。
func (c *Catalog) SpecByKind(kind string) (core.TaskSpec, bool) {
	kind = strings.TrimSpace(kind)
	for _, spec := range c.Specs() {
		if spec.Kind == kind {
			return spec, true
		}
	}
	return core.TaskSpec{}, false
}

// ManualSpecs 返回需要管理端手动入口的任务族。
func (c *Catalog) ManualSpecs() []core.TaskSpec {
	all := c.Specs()
	out := make([]core.TaskSpec, 0, len(all))
	for _, spec := range all {
		if spec.Manual {
			out = append(out, spec)
		}
	}
	return out
}

// DefaultSchedule 从任务规格生成默认整点排程。
func (c *Catalog) DefaultSchedule() map[string][]int {
	out := map[string][]int{}
	for _, spec := range c.Specs() {
		if len(spec.DefaultHours) > 0 {
			out[spec.Kind] = append([]int(nil), spec.DefaultHours...)
		}
	}
	return out
}

// EffectiveSchedule 合并默认值、状态文件中的用户覆盖值与任务规格。
func (c *Catalog) EffectiveSchedule(overrides map[string][]int) map[string][]int {
	out := c.DefaultSchedule()
	for kind, hours := range overrides {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		if _, known := c.SpecByKind(kind); !known {
			continue
		}
		if len(hours) > 0 {
			out[kind] = NormalizeHours(hours)
		}
	}
	return out
}

// NormalizeHours 去重、排序并过滤非法小时。
func NormalizeHours(hours []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 || seen[hour] {
			continue
		}
		seen[hour] = true
		out = append(out, hour)
	}
	sort.Ints(out)
	return out
}

// NextWake 返回下一次唤醒时刻与要执行的任务族。
func NextWake(now time.Time, schedule map[string][]int) ([]string, time.Time) {
	byHour := map[int][]string{}
	hours := make([]int, 0, len(schedule))
	for kind, list := range schedule {
		for _, hour := range NormalizeHours(list) {
			if _, okHour := byHour[hour]; !okHour {
				hours = append(hours, hour)
			}
			byHour[hour] = append(byHour[hour], kind)
		}
	}
	if len(hours) == 0 {
		return nil, now.Add(time.Hour)
	}
	sort.Ints(hours)
	for _, hour := range hours {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if candidate.After(now) {
			sort.Strings(byHour[hour])
			return byHour[hour], candidate
		}
	}
	first := hours[0]
	tomorrow := now.AddDate(0, 0, 1)
	sort.Strings(byHour[first])
	return byHour[first], time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), first, 0, 0, 0, now.Location())
}

func (c *Catalog) anyCheckin() bool {
	for _, vendor := range c.registry.Vendors() {
		if core.SupportsCheckin(vendor) {
			return true
		}
	}
	return false
}
