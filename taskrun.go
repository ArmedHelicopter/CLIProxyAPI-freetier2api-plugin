package main

// 本文件是通用任务调度器：整点小时列表 + 账号遍历。
//
// 调度器只认 core 的任务族标识与可选能力接口；具体做什么由供应商的
// core.ScheduledRunner 决定。不支持的供应商在遍历时自然跳过。

import (
	"context"
	"sort"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
)

// 默认排程（本地时区整点）。
var defaultSchedule = map[string][]int{
	core.TaskKindCheckin:   {9, 21},
	core.TaskKindTravel:    {9, 21},
	core.TaskKindActivity:  {10},
	core.TaskKindKeepalive: {22},
	core.TaskKindBlackcat:  {23},
}

// 调度循环参数。
const (
	// schedulerWakeGrace 是迟到唤醒的宽限等待（休眠恢复后网络栈需要时间）。
	schedulerWakeGrace = 5 * time.Second
	// schedulerLateThreshold 是判定「迟到」的阈值。
	schedulerLateThreshold = 1 * time.Second
	// schedulerReconfigure 是配置变更信号缓冲。
	schedulerReconfigure = "reconfigure"
	// accountGap 是账号之间的操作间隔。
	accountGap = 800 * time.Millisecond
)

var (
	schedulerMu      sync.Mutex
	schedulerPoke    = make(chan string, 4)
	schedulerRunning bool
)

// runTaskScheduler 是后台调度循环。
func runTaskScheduler(ctx context.Context) {
	logger.Info("task scheduler started")
	schedulerMu.Lock()
	schedulerRunning = true
	schedulerMu.Unlock()
	defer func() {
		schedulerMu.Lock()
		schedulerRunning = false
		schedulerMu.Unlock()
		logger.Info("task scheduler stopped")
	}()
	for {
		kinds, next := nextScheduledWake(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case signal := <-schedulerPoke:
			timer.Stop()
			if signal == schedulerReconfigure {
				logger.Debug("task scheduler reconfigured")
			}
			continue
		case <-timer.C:
			if delay := time.Since(next); delay > schedulerLateThreshold {
				logger.Debug("task scheduler woke %s late, waiting grace period", delay.Round(time.Millisecond))
				if !sleepCtx(ctx, schedulerWakeGrace) {
					return
				}
			}
			runTaskBatch(ctx, kinds)
		}
	}
}

// runTaskBatch 并行派发同一时刻的多个任务族。
func runTaskBatch(ctx context.Context, kinds []string) {
	var waitGroup sync.WaitGroup
	for _, kind := range kinds {
		waitGroup.Add(1)
		go func(name string) {
			defer waitGroup.Done()
			runScheduledTaskContext(ctx, name)
		}(kind)
	}
	waitGroup.Wait()
}

// nextScheduledWake 返回下一次唤醒时刻与要执行的任务族。
func nextScheduledWake(now time.Time) ([]string, time.Time) {
	schedule := effectiveSchedule()
	byHour := map[int][]string{}
	hours := make([]int, 0, len(schedule))
	for kind, list := range schedule {
		for _, hour := range list {
			if hour < 0 || hour > 23 {
				continue
			}
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
			return byHour[hour], candidate
		}
	}
	first := hours[0]
	tomorrow := now.AddDate(0, 0, 1)
	return byHour[first], time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), first, 0, 0, 0, now.Location())
}

// effectiveSchedule 返回生效的排程（页面设置优先于默认值）。
func effectiveSchedule() map[string][]int {
	state := snapshotState()
	out := make(map[string][]int, len(defaultSchedule))
	for kind, fallback := range defaultSchedule {
		out[kind] = fallback
	}
	for kind, hours := range map[string][]int{
		core.TaskKindCheckin:   state.CheckinHours,
		core.TaskKindTravel:    state.TravelHours,
		core.TaskKindActivity:  state.ActivityHours,
		core.TaskKindKeepalive: state.KeepaliveHours,
		core.TaskKindBlackcat:  state.BlackcatHours,
	} {
		if len(hours) > 0 {
			out[kind] = hours
		}
	}
	return out
}

// pokeScheduler 唤醒调度循环重新计算下次执行时刻。
func pokeScheduler() {
	select {
	case schedulerPoke <- schedulerReconfigure:
	default:
	}
}

// runScheduledTask 在后台执行一个任务族（供管理接口手动触发）。
func runScheduledTask(kind string) {
	runScheduledTaskContext(context.Background(), kind)
}

// runScheduledTaskContext 执行一个任务族。
func runScheduledTaskContext(ctx context.Context, kind string) {
	if kind == core.TaskKindCheckin {
		runCheckinAll(ctx)
		return
	}
	runScheduledFamily(ctx, kind)
}

// schedulerStatus 返回调度器状态（供管理页展示）。
func schedulerStatus() map[string]any {
	schedulerMu.Lock()
	running := schedulerRunning
	schedulerMu.Unlock()

	schedule := effectiveSchedule()
	kinds, next := nextScheduledWake(time.Now())
	readable := make(map[string][]int, len(schedule))
	for kind, hours := range schedule {
		readable[kind] = hours
	}
	return map[string]any{
		"running":    running,
		"schedule":   readable,
		"next_at":    next.Format(time.RFC3339),
		"next_tasks": kinds,
		"auto_tasks": effectiveAutoTasks(),
	}
}

// effectiveAutoTasks 返回是否启用任务自动闭环（页面设置优先）。
func effectiveAutoTasks() bool {
	state := snapshotState()
	if state.AutoTasks != nil {
		return *state.AutoTasks
	}
	return loadedConfig().AutoTasks
}

// effectiveAutoCheckin 返回是否启用自动签到（页面设置优先）。
func effectiveAutoCheckin() bool {
	state := snapshotState()
	if state.AutoCheckin != nil {
		return *state.AutoCheckin
	}
	return loadedConfig().AutoCheckin
}

// effectiveSettings 返回页面设置与配置合并后的可读视图。
func effectiveSettings() map[string]any {
	state := snapshotState()
	checkinAt := state.AutoCheckinAt
	if checkinAt == "" {
		checkinAt = loadedConfig().AutoCheckinAt
	}
	return map[string]any{
		"auto_checkin":    effectiveAutoCheckin(),
		"auto_checkin_at": checkinAt,
		"auto_tasks":      effectiveAutoTasks(),
		"schedule":        effectiveSchedule(),
	}
}

// sleepCtx 可取消的睡眠。返回 false 表示上下文已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// forEachAccount 遍历本插件名下可用的账号。
func forEachAccount(ctx context.Context, callbackID string, fn func(context.Context, *accountContext) error) {
	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		logger.Error("list accounts failed: %v", errList)
		return
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, okRaw := hostabi.AuthJSON(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, vendor, errResolve := resolveVendorCredential(ctx, callbackID, raw, hostabi.AccountIdentity(entry), nil)
		if errResolve != nil {
			logger.Debug("skip account %s: %v", entry.Name, errResolve)
			continue
		}
		if !modelCatalog.RealmEnabled(vendor.Region()) {
			continue
		}
		if errRun := fn(ctx, &accountContext{
			entry:      entry,
			vendor:     vendor,
			credential: credential,
			callbackID: callbackID,
		}); errRun != nil {
			logger.Debug("account %s task failed: %v", core.FirstNonEmpty(entry.Label, entry.Name), errRun)
		}
		if !sleepCtx(ctx, accountGap) {
			return
		}
	}
}

// accountContext 是遍历中单个账号的上下文。
type accountContext struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
	callbackID string
}

func (a *accountContext) uid() string { return a.credential.FileIDValue() }

func (a *accountContext) label() string {
	return core.FirstNonEmpty(a.credential.LabelValue(), a.entry.Label, a.entry.Name, a.entry.ID)
}

func (a *accountContext) isGlobal() bool { return a.vendor.Region() == "global" }
