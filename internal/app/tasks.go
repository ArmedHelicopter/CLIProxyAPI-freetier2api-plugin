package app

// 本文件是通用任务编排：
//   - 调度器：整点小时列表唤醒，按任务族并行派发；
//   - 任务中心：账号扫描、执行队列、单任务/一键完成、开学季状态汇总。
//
// 具体任务动作与上游协议在供应商包（如 workbuddy），这里只认 core 能力接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/tasks"
)

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

// runTaskScheduler 是后台调度循环。
func (a *App) runTaskScheduler(ctx context.Context) {
	logger.Info("task scheduler started")
	a.schedulerMu.Lock()
	a.schedulerRunning = true
	a.schedulerMu.Unlock()
	defer func() {
		a.schedulerMu.Lock()
		a.schedulerRunning = false
		a.schedulerMu.Unlock()
		logger.Info("task scheduler stopped")
	}()
	for {
		kinds, next := a.nextScheduledWake(a.now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case signal := <-a.schedulerPoke:
			timer.Stop()
			if signal == schedulerReconfigure {
				logger.Debug("task scheduler reconfigured")
			}
			continue
		case <-timer.C:
			if delay := time.Since(next); delay > schedulerLateThreshold {
				logger.Debug("task scheduler woke %s late, waiting grace period", delay.Round(time.Millisecond))
				if !tasks.SleepContext(ctx, schedulerWakeGrace) {
					return
				}
			}
			a.runTaskBatch(ctx, kinds)
		}
	}
}

// runTaskBatch 并行派发同一时刻的多个任务族。
func (a *App) runTaskBatch(ctx context.Context, kinds []string) {
	var waitGroup sync.WaitGroup
	for _, kind := range kinds {
		waitGroup.Add(1)
		go func(name string) {
			defer waitGroup.Done()
			a.runScheduledTaskContext(ctx, name)
		}(kind)
	}
	waitGroup.Wait()
}

// nextScheduledWake 返回下一次唤醒时刻与要执行的任务族。
func (a *App) nextScheduledWake(now time.Time) ([]string, time.Time) {
	return tasks.NextWake(now, a.effectiveSchedule())
}

// effectiveSchedule 返回生效的排程（页面设置优先于默认值）。
func (a *App) effectiveSchedule() map[string][]int {
	return a.taskCatalog.EffectiveSchedule(a.SnapshotState().TaskSchedules)
}

// pokeScheduler 唤醒调度循环重新计算下次执行时刻。
func (a *App) pokeScheduler() {
	select {
	case a.schedulerPoke <- schedulerReconfigure:
	default:
	}
}

// runScheduledTask 在后台执行一个任务族（供管理接口手动触发）。
func (a *App) runScheduledTask(kind string) {
	a.runScheduledTaskContext(context.Background(), kind)
}

// runScheduledTaskContext 执行一个任务族。
func (a *App) runScheduledTaskContext(ctx context.Context, kind string) {
	if kind == core.TaskKindCheckin {
		a.runCheckinAll(ctx)
		return
	}
	a.runScheduledFamily(ctx, kind)
}

// schedulerStatus 返回调度器状态（供管理页展示）。
func (a *App) schedulerStatus() map[string]any {
	a.schedulerMu.Lock()
	running := a.schedulerRunning
	a.schedulerMu.Unlock()

	schedule := a.effectiveSchedule()
	kinds, next := a.nextScheduledWake(a.now())
	readable := make(map[string][]int, len(schedule))
	for kind, hours := range schedule {
		readable[kind] = hours
	}
	return map[string]any{
		"running":    running,
		"schedule":   readable,
		"next_at":    next.Format(time.RFC3339),
		"next_tasks": kinds,
		"auto_tasks": a.effectiveAutoTasks(),
		"task_specs": a.taskCatalog.Specs(),
	}
}

// effectiveAutoTasks 返回是否启用任务自动闭环（页面设置优先）。
func (a *App) effectiveAutoTasks() bool {
	state := a.SnapshotState()
	if state.AutoTasks != nil {
		return *state.AutoTasks
	}
	return a.Config().AutoTasks
}

// effectiveAutoCheckin 返回是否启用自动签到（页面设置优先）。
func (a *App) effectiveAutoCheckin() bool {
	state := a.SnapshotState()
	if state.AutoCheckin != nil {
		return *state.AutoCheckin
	}
	return a.Config().AutoCheckin
}

// effectiveSettings 返回页面设置与配置合并后的可读视图。
func (a *App) effectiveSettings() map[string]any {
	state := a.SnapshotState()
	checkinAt := state.AutoCheckinAt
	if checkinAt == "" {
		checkinAt = a.Config().AutoCheckinAt
	}
	return map[string]any{
		"auto_checkin":    a.effectiveAutoCheckin(),
		"auto_checkin_at": checkinAt,
		"auto_tasks":      a.effectiveAutoTasks(),
		"schedule":        a.effectiveSchedule(),
		"task_specs":      a.taskCatalog.Specs(),
		"vendor_settings": a.SnapshotState().VendorSettings,
		"fields":          a.runtimeFields(),
	}
}

// sleepCtx 可取消的睡眠。返回 false 表示上下文已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	return tasks.SleepContext(ctx, d)
}

// forEachAccount 遍历本插件名下可用的账号。
func (a *App) forEachAccount(ctx context.Context, callbackID string, fn func(context.Context, *accountContext) error) {
	cfg := a.Config()
	accounts, failures := a.pool.Load(ctx, callbackID, accounts.Options{
		RealmEnabled: func(region string) bool { return realmEnabled(cfg, region) },
	})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	for _, account := range accounts {
		if errRun := fn(ctx, &accountContext{
			entry:      account.Entry,
			vendor:     account.Vendor,
			credential: account.Credential,
			callbackID: callbackID,
		}); errRun != nil {
			logger.Debug("account %s task failed: %v", core.FirstNonEmpty(account.Entry.Label, account.Entry.Name), errRun)
		}
		if !tasks.SleepContext(ctx, accountGap) {
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

// ---- 定时任务族（原 tasks_schedule.go） ----

// runCheckinAll 执行全账号签到，并在启用自动任务时驱动从属闭环。
func (a *App) runCheckinAll(ctx context.Context) {
	if !a.effectiveAutoCheckin() {
		logger.Debug("checkin skipped: auto checkin is disabled")
		return
	}
	logger.Info("checkin: starting for all accounts")
	count := 0
	a.forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if !core.SupportsCheckin(account.vendor) {
			return nil
		}
		checkin, okCheckin := core.As[core.CheckinProvider](account.vendor)
		if !okCheckin {
			return nil
		}
		result, errCheckin := checkin.Checkin(ctx, account.credential)
		if errCheckin != nil {
			logger.Error("checkin %s: %v", account.label(), errCheckin)
			return nil
		}
		if result == nil {
			return nil
		}
		a.recordCheckinResult(account.credential, result)
		if result.Already {
			logger.Info("checkin %s: already checked in today", account.label())
		} else {
			logger.Info("checkin %s: %s", account.label(), result.Message)
		}
		count++
		return nil
	})
	logger.Info("checkin: finished for %d account(s)", count)

	if a.effectiveAutoTasks() {
		for _, spec := range a.taskCatalog.Specs() {
			if spec.AfterCheckin {
				a.runScheduledFamily(ctx, spec.Kind)
			}
		}
	}
}

// runScheduledFamily 遍历账号，把任务族交给供应商的可选实现。
func (a *App) runScheduledFamily(ctx context.Context, kind string) {
	logger.Info("%s: starting for all accounts", kind)
	count := 0
	a.forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		runner, okRunner := core.As[core.ScheduledRunner](account.vendor)
		if !okRunner {
			return nil
		}
		message, errRun := runner.RunScheduled(ctx, account.credential, kind)
		if errRun != nil {
			logger.Error("%s %s: %v", kind, account.label(), errRun)
			return nil
		}
		if message != "" {
			logger.Info("%s %s: %s", kind, account.label(), message)
			count++
		}
		return nil
	})
	logger.Info("%s: finished for %d account(s)", kind, count)
}

const (
	// queueMaxConcurrency 是账号间并发的上限。
	queueMaxConcurrency = 4
	// queueAutoAllTimeout 是「一键完成」的总时长上限。
	queueAutoAllTimeout = 5 * time.Minute
)

func (a *App) tryLockAccount(uid string) bool {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return false
	}
	a.taskLockMu.Lock()
	lock, okLock := a.taskLocks[trimmed]
	if !okLock {
		lock = &sync.Mutex{}
		a.taskLocks[trimmed] = lock
	}
	a.taskLockMu.Unlock()
	return lock.TryLock()
}

func (a *App) unlockAccount(uid string) {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return
	}
	a.taskLockMu.Lock()
	lock, okLock := a.taskLocks[trimmed]
	a.taskLockMu.Unlock()
	if okLock {
		lock.Unlock()
	}
}

// taskItem 是扫描结果里的一条待办（JSON 契约由 core.TaskItem 定义）。
type taskItem = core.TaskItem

// accountScan 是单账号的扫描结果。
type accountScan struct {
	UID      string     `json:"uid"`
	Label    string     `json:"label"`
	Realm    string     `json:"realm"`
	Growth   []taskItem `json:"growth"`
	School   []taskItem `json:"school"`
	InPeriod bool       `json:"in_period"`
	Error    string     `json:"error,omitempty"`
}

// handleTasksScan 扫描全部账号的待办任务。
func (a *App) handleTasksScan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	scans := a.scanAllAccounts(ctx, callbackID)
	total := 0
	for _, scan := range scans {
		total += len(scan.Growth) + len(scan.School)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":            true,
		"accounts":      scans,
		"pending_count": total,
	})
}

// scanAllAccounts 并发扫描每个账号的待办任务。
func (a *App) scanAllAccounts(ctx context.Context, callbackID string) []accountScan {
	accounts := a.collectAccounts(ctx, callbackID)
	scans := make([]accountScan, len(accounts))
	var waitGroup sync.WaitGroup
	for index := range accounts {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			scans[slot] = a.scanOneAccount(ctx, accounts[slot])
		}(index)
	}
	waitGroup.Wait()
	sort.Slice(scans, func(i, j int) bool { return scans[i].Label < scans[j].Label })
	return scans
}

// scanOneAccount 扫描单个账号。
func (a *App) scanOneAccount(ctx context.Context, account *accountContext) accountScan {
	scan := accountScan{
		UID:   account.uid(),
		Label: account.label(),
		Realm: account.vendor.Region(),
	}
	inspector, okInspector := core.As[core.TaskInspector](account.vendor)
	if !okInspector {
		return scan
	}
	inspection, errInspect := inspector.InspectTasks(ctx, account.credential)
	if errInspect != nil {
		scan.Error = errInspect.Error()
		return scan
	}
	scan.InPeriod = inspection.InPeriod
	for _, item := range inspection.Items {
		switch item.Kind {
		case "growth":
			scan.Growth = append(scan.Growth, item)
		case "school":
			scan.School = append(scan.School, item)
		}
	}
	return scan
}

// 执行队列。

type queueItem struct {
	UID      string `json:"uid"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	seq       int64
	conc      int
	items     []queueItem
}

func (a *App) taskQueueSnapshot() map[string]any {
	a.taskQueue.mu.Lock()
	defer a.taskQueue.mu.Unlock()
	items := make([]queueItem, len(a.taskQueue.items))
	copy(items, a.taskQueue.items)

	done := 0
	for _, item := range items {
		switch item.Status {
		case "done", "error", "skipped":
			done++
		}
	}
	snapshot := map[string]any{
		"running": a.taskQueue.running,
		"seq":     a.taskQueue.seq,
		"conc":    a.taskQueue.conc,
		"total":   len(items),
		"done":    done,
		"items":   items,
	}
	if !a.taskQueue.startedAt.IsZero() {
		snapshot["started_at"] = a.taskQueue.startedAt.Format(time.RFC3339)
	}
	return snapshot
}

type taskRunRequest struct {
	Concurrency int  `json:"concurrency"`
	Growth      bool `json:"growth"`
	School      bool `json:"school"`
}

func (a *App) handleTasksRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := taskRunRequest{Concurrency: 1, Growth: true, School: true}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	concurrency := body.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > queueMaxConcurrency {
		concurrency = queueMaxConcurrency
	}
	runGrowth := body.Growth
	runSchool := body.School
	if !runGrowth && !runSchool {
		runGrowth, runSchool = true, true
	}

	a.taskQueue.mu.Lock()
	if a.taskQueue.running {
		a.taskQueue.mu.Unlock()
		return jsonResponse(http.StatusConflict, map[string]any{"ok": false, "error": "队列正在执行中"})
	}
	a.taskQueue.mu.Unlock()

	ctx, cancel := managementContext(req)
	callbackID := hostCallbackID(req)
	scans := a.scanAllAccounts(ctx, callbackID)
	cancel()

	items := a.buildQueueItems(scans, runGrowth, runSchool)
	if len(items) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "started": false, "message": "全部账号没有待办任务",
		})
	}

	a.taskQueue.mu.Lock()
	a.taskQueue.running = true
	a.taskQueue.seq++
	a.taskQueue.conc = concurrency
	a.taskQueue.startedAt = time.Now()
	a.taskQueue.items = items
	seq := a.taskQueue.seq
	a.taskQueue.mu.Unlock()

	go a.runQueueItems(callbackID, concurrency, seq)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "total": len(items), "seq": seq, "concurrency": concurrency,
	})
}

// buildQueueItems 把扫描结果摊平成队列项（账号内按依赖序排列）。
func (a *App) buildQueueItems(scans []accountScan, runGrowth, runSchool bool) []queueItem {
	items := make([]queueItem, 0, 64)
	for _, scan := range scans {
		if runGrowth {
			for _, task := range scan.Growth {
				items = append(items, queueItem{
					UID: scan.UID, Label: scan.Label, Kind: "growth",
					Code: task.Code, Status: "pending",
					Progress: fmt.Sprintf("%d/%d", task.Current, task.Target),
				})
			}
		}
		if runSchool && len(scan.School) > 0 {
			items = append(items, queueItem{
				UID: scan.UID, Label: scan.Label, Kind: "school",
				Code: "school_daily", Status: "pending",
			})
		}
	}
	return items
}

// runQueueItems 执行队列：账号内串行、账号间并发。
func (a *App) runQueueItems(callbackID string, concurrency int, seq int64) {
	defer func() {
		a.taskQueue.mu.Lock()
		if a.taskQueue.seq == seq {
			a.taskQueue.running = false
		}
		a.taskQueue.mu.Unlock()
	}()

	ctx := context.Background()
	accounts := a.collectAccounts(ctx, callbackID)
	byUID := make(map[string]*accountContext, len(accounts))
	for _, account := range accounts {
		byUID[account.uid()] = account
	}

	grouped := map[string][]int{}
	order := make([]string, 0, len(byUID))
	for index, item := range a.queueSnapshotItems() {
		if _, okGroup := grouped[item.UID]; !okGroup {
			order = append(order, item.UID)
		}
		grouped[item.UID] = append(grouped[item.UID], index)
	}

	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for _, uid := range order {
		account, okAccount := byUID[uid]
		if !okAccount {
			a.markQueueItems(grouped[uid], "skipped", "账号已不可用", 0, 0)
			continue
		}
		waitGroup.Add(1)
		go func(target *accountContext, indices []int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if !a.tryLockAccount(target.uid()) {
				a.markQueueItems(indices, "skipped", "该账号有其它任务动作在执行", 0, 0)
				return
			}
			defer a.unlockAccount(target.uid())

			if preparer, okPreparer := core.As[core.TaskPreparer](target.vendor); okPreparer {
				if accepted := preparer.PrepareTasks(ctx, target.credential); accepted > 0 {
					logger.Debug("queue: accepted %d task(s) for %s", accepted, target.label())
					time.Sleep(1050 * time.Millisecond)
				}
			}
			for _, index := range indices {
				item := a.queueItemAt(index)
				a.markQueueItem(index, "running", "")
				result, errRun := a.runTaskForAccount(ctx, target, item.Code)
				if errRun != nil {
					a.markQueueItem(index, "error", errRun.Error())
				} else {
					a.setQueueItemResult(index, result.Status, result.Message, result.Credit, result.Energy)
				}
				time.Sleep(1050 * time.Millisecond)
			}
		}(account, grouped[uid])
	}
	waitGroup.Wait()
	logger.Info("task queue finished (seq=%d)", seq)
}

// runTaskForAccount 调用供应商的任务执行能力并记录结果。
func (a *App) runTaskForAccount(ctx context.Context, account *accountContext, code string) (core.TaskRunResult, error) {
	runner, okRunner := core.As[core.TaskRunner](account.vendor)
	if !okRunner {
		return core.TaskRunResult{}, fmt.Errorf("该供应商不支持任务执行")
	}
	result, errRun := runner.RunTask(ctx, account.credential, code)
	if errRun != nil {
		a.recordTaskResult(account.uid(), code, errRun.Error(), "error", 0, 0)
		return core.TaskRunResult{}, errRun
	}
	a.recordTaskResult(account.uid(), code, result.Message, result.Status, result.Credit, result.Energy)
	return result, nil
}

// 队列状态访问器（避免在 goroutine 里长时间持锁）。

func (a *App) queueSnapshotItems() []queueItem {
	a.taskQueue.mu.Lock()
	defer a.taskQueue.mu.Unlock()
	items := make([]queueItem, len(a.taskQueue.items))
	copy(items, a.taskQueue.items)
	return items
}

func (a *App) queueItemAt(index int) queueItem {
	a.taskQueue.mu.Lock()
	defer a.taskQueue.mu.Unlock()
	if index < 0 || index >= len(a.taskQueue.items) {
		return queueItem{}
	}
	return a.taskQueue.items[index]
}

func (a *App) markQueueItem(index int, status, message string) {
	a.taskQueue.mu.Lock()
	defer a.taskQueue.mu.Unlock()
	if index < 0 || index >= len(a.taskQueue.items) {
		return
	}
	a.taskQueue.items[index].Status = status
	if message != "" {
		a.taskQueue.items[index].Message = message
	}
}

func (a *App) setQueueItemResult(index int, status, message string, credit, energy int64) {
	a.taskQueue.mu.Lock()
	defer a.taskQueue.mu.Unlock()
	if index < 0 || index >= len(a.taskQueue.items) {
		return
	}
	a.taskQueue.items[index].Status = status
	a.taskQueue.items[index].Message = message
	a.taskQueue.items[index].Credit = credit
	a.taskQueue.items[index].Energy = energy
}

func (a *App) markQueueItems(indices []int, status, message string, credit, energy int64) {
	for _, index := range indices {
		a.setQueueItemResult(index, status, message, credit, energy)
	}
}

// recordTaskResult 记录任务执行结果到状态文件。
func (a *App) recordTaskResult(uid, code, message, status string, credit, energy int64) {
	if strings.TrimSpace(uid) == "" {
		return
	}
	key := taskStateKey(uid, code)
	a.MutateState(func(state *pluginState) {
		state.Tasks[key] = taskRecord{
			LastRunAt:   a.nowRFC3339(),
			LastStatus:  status,
			LastMessage: message,
			Credit:      credit,
			Energy:      energy,
		}
	})
}

type taskAutoRequest struct {
	AuthID    string `json:"auth_id"`
	UID       string `json:"uid"`
	AccountID string `json:"account_id"`
	TaskCode  string `json:"task_code"`
}

func (a *App) handleTasksAuto(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body taskAutoRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authID := core.FirstNonEmpty(body.AuthID, body.UID, body.AccountID)
	code := strings.TrimSpace(body.TaskCode)

	ctx, cancel := managementContext(req)
	defer cancel()
	account := a.findAccount(ctx, hostCallbackID(req), authID)
	if account == nil {
		return jsonResponse(http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"})
	}
	if !a.tryLockAccount(account.uid()) {
		return jsonResponse(http.StatusConflict, map[string]any{"ok": false, "error": "该账号有任务动作正在执行中"})
	}
	defer a.unlockAccount(account.uid())

	if code == "" {
		results := a.runAutoAll(ctx, account)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}

	result, errRun := a.runTaskForAccount(ctx, account, code)
	if errRun != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": false, "error": errRun.Error(), "task_code": code,
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "task_code": code, "message": result.Message,
		"credit": result.Credit, "energy": result.Energy,
	})
}

func (a *App) handleTasksAutoAll(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	accounts := a.collectAccounts(ctx, callbackID)
	if len(accounts) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "accounts": 0, "message": "没有可用账号"})
	}

	type accountResult struct {
		Label   string          `json:"label"`
		UID     string          `json:"uid"`
		Realm   string          `json:"realm"`
		Skipped string          `json:"skipped,omitempty"`
		Results []autoAllResult `json:"results,omitempty"`
	}
	out := make([]accountResult, 0, len(accounts))
	for _, account := range accounts {
		entry := accountResult{Label: account.label(), UID: account.uid(), Realm: account.vendor.Region()}
		if _, okRunner := core.As[core.TaskRunner](account.vendor); !okRunner {
			entry.Skipped = "该供应商没有成长任务"
			out = append(out, entry)
			continue
		}
		if !a.tryLockAccount(account.uid()) {
			entry.Skipped = "该账号有其它任务动作在执行"
			out = append(out, entry)
			continue
		}
		entry.Results = a.runAutoAll(ctx, account)
		a.unlockAccount(account.uid())
		out = append(out, entry)
		if !tasks.SleepContext(ctx, accountGap) {
			break
		}
	}

	done := 0
	for _, entry := range out {
		for _, result := range entry.Results {
			if result.Status == "done" {
				done++
			}
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "accounts": len(out), "tasks_done": done, "results": out,
	})
}

type autoAllResult struct {
	Code     string `json:"task_code"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Progress string `json:"progress,omitempty"`
}

// runAutoAll 调用供应商的一键完成，并记录每条结果。
func (a *App) runAutoAll(ctx context.Context, account *accountContext) []autoAllResult {
	runner, okRunner := core.As[core.TaskRunner](account.vendor)
	if !okRunner {
		return nil
	}
	raw := runner.RunAllTasks(ctx, account.credential)
	results := make([]autoAllResult, 0, len(raw))
	for _, item := range raw {
		result := autoAllResult{
			Code: item.Code, Status: item.Status, Message: item.Message,
			Credit: item.Credit, Energy: item.Energy, Progress: item.Progress,
		}
		if !strings.HasPrefix(item.Code, "(") {
			a.recordTaskResult(account.uid(), item.Code, item.Message, item.Status, item.Credit, item.Energy)
		}
		results = append(results, result)
	}
	return results
}

// collectAccounts 收集本插件名下的可用账号。
func (a *App) collectAccounts(ctx context.Context, callbackID string) []*accountContext {
	accounts := make([]*accountContext, 0, 8)
	a.forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
		accounts = append(accounts, account)
		return nil
	})
	return accounts
}

// findAccount 按标识查找账号（支持 auth_id / uid / auth_index）。
func (a *App) findAccount(ctx context.Context, callbackID, identifier string) *accountContext {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return nil
	}
	var found *accountContext
	a.forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
		if found != nil {
			return nil
		}
		if account.uid() == trimmed || account.entry.ID == trimmed ||
			account.entry.Name == trimmed || account.entry.AuthIndex == trimmed ||
			account.entry.Label == trimmed {
			found = account
		}
		return nil
	})
	return found
}

// schoolStatusForAccounts 返回开学季活动状态（供管理页展示）。
func (a *App) schoolStatusForAccounts(ctx context.Context, callbackID string) []map[string]any {
	accounts := a.collectAccounts(ctx, callbackID)
	out := make([]map[string]any, 0, len(accounts))
	var waitGroup sync.WaitGroup
	var mu sync.Mutex

	for _, account := range accounts {
		waitGroup.Add(1)
		go func(target *accountContext) {
			defer waitGroup.Done()
			entry := map[string]any{
				"uid":   target.uid(),
				"label": target.label(),
				"realm": target.vendor.Region(),
			}
			provider, okProvider := core.As[core.SchoolStatusProvider](target.vendor)
			if !okProvider {
				entry["in_period"] = false
				entry["error"] = "该供应商没有开学季活动"
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			status := provider.SchoolStatus(ctx, target.credential)
			entry["in_period"] = status.InPeriod
			if status.Error != "" {
				entry["error"] = status.Error
			}
			if len(status.Tasks) > 0 {
				list := make([]map[string]any, 0, len(status.Tasks))
				for _, task := range status.Tasks {
					list = append(list, map[string]any{
						"task_code": task.Code, "title": task.Title,
						"status": task.Status, "progress": task.Progress, "target": task.Target,
					})
				}
				entry["tasks"] = list
			}
			if status.Chances != nil {
				entry["chances"] = status.Chances
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}(account)
	}
	waitGroup.Wait()
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["label"]) < fmt.Sprint(out[j]["label"])
	})
	return out
}
