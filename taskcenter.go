package main

// 本文件是任务中心的通用编排层：账号扫描、执行队列、单任务/一键完成、
// 开学季状态汇总。
//
// 供应商隔离靠 core 的可选能力接口：
//   - TaskInspector：扫描待办；
//   - TaskRunner：单任务执行与一键完成；
//   - TaskPreparer：执行前批量报名；
//   - SchoolStatusProvider：开学季状态。
//
// 不支持的供应商不建文件、不写占位方法，调度时自然跳过。

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
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

const (
	// queueMaxConcurrency 是账号间并发的上限。
	queueMaxConcurrency = 4
	// queueAutoAllTimeout 是「一键完成」的总时长上限。
	queueAutoAllTimeout = 5 * time.Minute
)

// 账号级互斥锁：同一账号的任务动作必须串行。
var (
	taskLockMu sync.Mutex
	taskLocks  = map[string]*sync.Mutex{}
)

func tryLockAccount(uid string) bool {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return false
	}
	taskLockMu.Lock()
	lock, okLock := taskLocks[trimmed]
	if !okLock {
		lock = &sync.Mutex{}
		taskLocks[trimmed] = lock
	}
	taskLockMu.Unlock()
	return lock.TryLock()
}

func unlockAccount(uid string) {
	trimmed := strings.TrimSpace(uid)
	if trimmed == "" {
		return
	}
	taskLockMu.Lock()
	lock, okLock := taskLocks[trimmed]
	taskLockMu.Unlock()
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
func handleTasksScan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	scans := scanAllAccounts(ctx, callbackID)
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
func scanAllAccounts(ctx context.Context, callbackID string) []accountScan {
	accounts := collectAccounts(ctx, callbackID)
	scans := make([]accountScan, len(accounts))
	var waitGroup sync.WaitGroup
	for index := range accounts {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			scans[slot] = scanOneAccount(ctx, accounts[slot])
		}(index)
	}
	waitGroup.Wait()
	sort.Slice(scans, func(i, j int) bool { return scans[i].Label < scans[j].Label })
	return scans
}

// scanOneAccount 扫描单个账号。
func scanOneAccount(ctx context.Context, account *accountContext) accountScan {
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

var currentQueue = &queueState{}

func taskQueueSnapshot() map[string]any {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	items := make([]queueItem, len(currentQueue.items))
	copy(items, currentQueue.items)

	done := 0
	for _, item := range items {
		switch item.Status {
		case "done", "error", "skipped":
			done++
		}
	}
	snapshot := map[string]any{
		"running": currentQueue.running,
		"seq":     currentQueue.seq,
		"conc":    currentQueue.conc,
		"total":   len(items),
		"done":    done,
		"items":   items,
	}
	if !currentQueue.startedAt.IsZero() {
		snapshot["started_at"] = currentQueue.startedAt.Format(time.RFC3339)
	}
	return snapshot
}

type taskRunRequest struct {
	Concurrency int  `json:"concurrency"`
	Growth      bool `json:"growth"`
	School      bool `json:"school"`
}

func handleTasksRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
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

	currentQueue.mu.Lock()
	if currentQueue.running {
		currentQueue.mu.Unlock()
		return jsonResponse(http.StatusConflict, map[string]any{"ok": false, "error": "队列正在执行中"})
	}
	currentQueue.mu.Unlock()

	ctx, cancel := managementContext(req)
	callbackID := hostCallbackID(req)
	scans := scanAllAccounts(ctx, callbackID)
	cancel()

	items := buildQueueItems(scans, runGrowth, runSchool)
	if len(items) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "started": false, "message": "全部账号没有待办任务",
		})
	}

	currentQueue.mu.Lock()
	currentQueue.running = true
	currentQueue.seq++
	currentQueue.conc = concurrency
	currentQueue.startedAt = time.Now()
	currentQueue.items = items
	seq := currentQueue.seq
	currentQueue.mu.Unlock()

	go runQueueItems(callbackID, concurrency, seq)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "total": len(items), "seq": seq, "concurrency": concurrency,
	})
}

// buildQueueItems 把扫描结果摊平成队列项（账号内按依赖序排列）。
func buildQueueItems(scans []accountScan, runGrowth, runSchool bool) []queueItem {
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
func runQueueItems(callbackID string, concurrency int, seq int64) {
	defer func() {
		currentQueue.mu.Lock()
		if currentQueue.seq == seq {
			currentQueue.running = false
		}
		currentQueue.mu.Unlock()
	}()

	ctx := context.Background()
	accounts := collectAccounts(ctx, callbackID)
	byUID := make(map[string]*accountContext, len(accounts))
	for _, account := range accounts {
		byUID[account.uid()] = account
	}

	grouped := map[string][]int{}
	order := make([]string, 0, len(byUID))
	for index, item := range queueSnapshotItems() {
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
			markQueueItems(grouped[uid], "skipped", "账号已不可用", 0, 0)
			continue
		}
		waitGroup.Add(1)
		go func(target *accountContext, indices []int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if !tryLockAccount(target.uid()) {
				markQueueItems(indices, "skipped", "该账号有其它任务动作在执行", 0, 0)
				return
			}
			defer unlockAccount(target.uid())

			if preparer, okPreparer := core.As[core.TaskPreparer](target.vendor); okPreparer {
				if accepted := preparer.PrepareTasks(ctx, target.credential); accepted > 0 {
					logger.Debug("queue: accepted %d task(s) for %s", accepted, target.label())
					time.Sleep(1050 * time.Millisecond)
				}
			}
			for _, index := range indices {
				item := queueItemAt(index)
				markQueueItem(index, "running", "")
				result, errRun := runTaskForAccount(ctx, target, item.Code)
				if errRun != nil {
					markQueueItem(index, "error", errRun.Error())
				} else {
					setQueueItemResult(index, result.Status, result.Message, result.Credit, result.Energy)
				}
				time.Sleep(1050 * time.Millisecond)
			}
		}(account, grouped[uid])
	}
	waitGroup.Wait()
	logger.Info("task queue finished (seq=%d)", seq)
}

// runTaskForAccount 调用供应商的任务执行能力并记录结果。
func runTaskForAccount(ctx context.Context, account *accountContext, code string) (core.TaskRunResult, error) {
	runner, okRunner := core.As[core.TaskRunner](account.vendor)
	if !okRunner {
		return core.TaskRunResult{}, fmt.Errorf("该供应商不支持任务执行")
	}
	result, errRun := runner.RunTask(ctx, account.credential, code)
	if errRun != nil {
		recordTaskResult(account.uid(), code, errRun.Error(), "error", 0, 0)
		return core.TaskRunResult{}, errRun
	}
	recordTaskResult(account.uid(), code, result.Message, result.Status, result.Credit, result.Energy)
	return result, nil
}

// 队列状态访问器（避免在 goroutine 里长时间持锁）。

func queueSnapshotItems() []queueItem {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	items := make([]queueItem, len(currentQueue.items))
	copy(items, currentQueue.items)
	return items
}

func queueItemAt(index int) queueItem {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return queueItem{}
	}
	return currentQueue.items[index]
}

func markQueueItem(index int, status, message string) {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return
	}
	currentQueue.items[index].Status = status
	if message != "" {
		currentQueue.items[index].Message = message
	}
}

func setQueueItemResult(index int, status, message string, credit, energy int64) {
	currentQueue.mu.Lock()
	defer currentQueue.mu.Unlock()
	if index < 0 || index >= len(currentQueue.items) {
		return
	}
	currentQueue.items[index].Status = status
	currentQueue.items[index].Message = message
	currentQueue.items[index].Credit = credit
	currentQueue.items[index].Energy = energy
}

func markQueueItems(indices []int, status, message string, credit, energy int64) {
	for _, index := range indices {
		setQueueItemResult(index, status, message, credit, energy)
	}
}

// recordTaskResult 记录任务执行结果到状态文件。
func recordTaskResult(uid, code, message, status string, credit, energy int64) {
	if strings.TrimSpace(uid) == "" {
		return
	}
	key := taskStateKey(uid, code)
	mutateState(func(state *pluginState) {
		state.Tasks[key] = taskRecord{
			LastRunAt:   nowRFC3339(),
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

func handleTasksAuto(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body taskAutoRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authID := core.FirstNonEmpty(body.AuthID, body.UID, body.AccountID)
	code := strings.TrimSpace(body.TaskCode)

	ctx, cancel := managementContext(req)
	defer cancel()
	account := findAccount(ctx, hostCallbackID(req), authID)
	if account == nil {
		return jsonResponse(http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"})
	}
	if !tryLockAccount(account.uid()) {
		return jsonResponse(http.StatusConflict, map[string]any{"ok": false, "error": "该账号有任务动作正在执行中"})
	}
	defer unlockAccount(account.uid())

	if code == "" {
		results := runAutoAll(ctx, account)
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}

	result, errRun := runTaskForAccount(ctx, account, code)
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

func handleTasksAutoAll(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	accounts := collectAccounts(ctx, callbackID)
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
		if !tryLockAccount(account.uid()) {
			entry.Skipped = "该账号有其它任务动作在执行"
			out = append(out, entry)
			continue
		}
		entry.Results = runAutoAll(ctx, account)
		unlockAccount(account.uid())
		out = append(out, entry)
		if !sleepCtx(ctx, accountGap) {
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
func runAutoAll(ctx context.Context, account *accountContext) []autoAllResult {
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
		recordTaskResult(account.uid(), item.Code, item.Message, item.Status, item.Credit, item.Energy)
		results = append(results, result)
	}
	return results
}

// collectAccounts 收集本插件名下的可用账号。
func collectAccounts(ctx context.Context, callbackID string) []*accountContext {
	accounts := make([]*accountContext, 0, 8)
	forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
		accounts = append(accounts, account)
		return nil
	})
	return accounts
}

// findAccount 按标识查找账号（支持 auth_id / uid / auth_index）。
func findAccount(ctx context.Context, callbackID, identifier string) *accountContext {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return nil
	}
	var found *accountContext
	forEachAccount(ctx, callbackID, func(_ context.Context, account *accountContext) error {
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
func schoolStatusForAccounts(ctx context.Context, callbackID string) []map[string]any {
	accounts := collectAccounts(ctx, callbackID)
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
