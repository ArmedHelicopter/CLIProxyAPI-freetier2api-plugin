package workbuddy

// 本文件是 WorkBuddy 的任务中心：动作表、扫描、执行、开学季与定时任务族。
//
// 这些规则完全由 CodeBuddy 上游的活动体系决定（任务码、事件链、计分窗口），
// 因此留在供应商包内；根层只做账号遍历、并发队列与 HTTP 响应。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
)

// taskAction 是一个可自动完成的成长任务。
type taskAction struct {
	Code     string
	Desc     string
	UsesChat bool
	Run      func(ctx context.Context, client *Client, credential *Credential) (string, error)
}

// autoActions 是「一键完成」覆盖的任务表（顺序即依赖序）。
//
// 唯一不做的 Expert_Philanthropy 需要真实捐款（服务端校验捐赠回执）。
var autoActions = []taskAction{
	{Code: "chat_5", Desc: "上报 5 条对话活跃事件", Run: runChatFive},
	{Code: "first_buddy", Desc: "解锁并领养第一只 Buddy", Run: runFirstBuddy},
	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次并对齐上报", Run: runModelChat, UsesChat: true},
	{Code: "RichMeow_Chat", Desc: "桌面指纹对话事件链", Run: runRichMeow},
	{Code: "Buddy_App", Desc: "进入 Buddy 应用事件链", Run: runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "进入企鹅教师助手事件链", Run: runBuddyApp},
	{Code: "automation_1", Desc: "上报定时任务创建事件", Run: runAutomation},
	{Code: "Library_read", Desc: "上报资料库阅读事件", Run: runLibraryRead},
	{Code: "template_5", Desc: "上报模板使用事件组 ×5", Run: runTemplateUse},
	{Code: "playbook_prompt", Desc: "上报灵感案例发送事件组", Run: runPlaybookPrompt},
	{Code: "create_canvas", Desc: "上报设计画布创建事件组", Run: runDesignCanvas},
	{Code: "expert_5", Desc: "真实专家召唤+使用链 ×5", Run: runExpertFive, UsesChat: true},
	{Code: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3", Run: runExpertTeam, UsesChat: true},
	{Code: "Hp_Appearance", Desc: "设置主题 + 皮肤生效事件", Run: runAppearance},
	{Code: "skill_1", Desc: "真实对话 + 技能加载事件", Run: runSkillFresh, UsesChat: true},
	{Code: "Expert_lighthouse", Desc: "轻量云专家召唤+使用链", Run: runExpertLighthouse, UsesChat: true},
	{Code: "black_cat", Desc: "夜猫子：夜间窗口内 glm-5.2 对话", Run: runBlackCat, UsesChat: true},
}

func autoActionFor(code string) *taskAction {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return &autoActions[index]
		}
	}
	return nil
}

func autoActionIndex(code string) int {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return index
		}
	}
	return len(autoActions)
}

// TaskActions 返回本区域暴露的任务动作（控制台展示用）。
func TaskActions(region Region) []core.Task {
	if region.IsGlobal() {
		return nil
	}
	out := make([]core.Task, 0, len(autoActions))
	for _, action := range autoActions {
		out = append(out, core.Task{Code: action.Code, Desc: action.Desc, UsesChat: action.UsesChat})
	}
	return out
}

// ---- 任务扫描 ----

func (v *adapter) InspectTasks(ctx context.Context, cred *core.Credential) (core.TaskInspection, error) {
	if v.region.IsGlobal() {
		return core.TaskInspection{}, nil
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return core.TaskInspection{}, err
	}
	client := UpstreamClient(ctx)
	growth, errGrowth := client.ListTasks(ctx, native)
	if errGrowth != nil {
		return core.TaskInspection{}, errGrowth
	}
	if mpTasks, errMP := client.ListTasksMP(ctx, native); errMP == nil {
		growth = mergeTasks(growth, mpTasks)
	}
	inspection := core.TaskInspection{}
	for _, task := range growth {
		if item, okItem := pendingGrowthItem(task); okItem {
			inspection.Items = append(inspection.Items, item)
		}
	}
	sortItemsByAction(inspection.Items)
	if school, errSchool := client.SchoolTasks(ctx, native); errSchool == nil && school != nil {
		inspection.InPeriod = school.InPeriod
		for _, task := range school.Tasks {
			if item, okItem := pendingSchoolItem(task); okItem {
				inspection.Items = append(inspection.Items, item)
			}
		}
	}
	return inspection, nil
}

func sortItemsByAction(items []core.TaskItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && autoActionIndex(items[j].Code) < autoActionIndex(items[j-1].Code); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func pendingGrowthItem(task Task) (core.TaskItem, bool) {
	if task.Claimed || task.Locked {
		return core.TaskItem{}, false
	}
	action := autoActionFor(task.Code)
	if action == nil {
		return core.TaskItem{}, false
	}
	return core.TaskItem{
		Kind:     "growth",
		Code:     task.Code,
		Title:    core.FirstNonEmpty(task.Title, action.Desc, task.Code),
		Current:  task.Current,
		Target:   task.Target,
		UsesChat: action.UsesChat,
	}, true
}

func pendingSchoolItem(task SchoolTask) (core.TaskItem, bool) {
	switch task.Code {
	case "task_student_verify":
		return core.TaskItem{}, false
	case "desktop_chat_1_time":
		if task.Status == "claimed" {
			return core.TaskItem{}, false
		}
	default:
		if task.Status == "claimed" || task.Status == "completed" {
			return core.TaskItem{}, false
		}
	}
	return core.TaskItem{
		Kind:     "school",
		Code:     task.Code,
		Title:    schoolTaskTitle(task.Code),
		Current:  task.Progress,
		Target:   task.TargetCount,
		UsesChat: task.Code == "desktop_chat_1_time",
	}, true
}

func schoolTaskTitle(code string) string {
	switch code {
	case "share_invite":
		return "分享活动"
	case "desktop_chat_1_time":
		return "桌面端体验"
	case "chat_3_times":
		return "和 AI 对话 3 次"
	case "expert_use":
		return "召唤开学季专家"
	}
	return code
}

func mergeTasks(primary, extra []Task) []Task {
	seen := make(map[string]bool, len(primary))
	out := make([]Task, 0, len(primary)+len(extra))
	for _, task := range primary {
		seen[task.Code] = true
		out = append(out, task)
	}
	for _, task := range extra {
		if seen[task.Code] {
			continue
		}
		out = append(out, task)
	}
	return out
}

// ---- 任务执行 ----

func (v *adapter) RunTask(ctx context.Context, cred *core.Credential, code string) (core.TaskRunResult, error) {
	if v.region.IsGlobal() {
		return core.TaskRunResult{Status: "skipped", Message: "国际版账号没有成长任务"}, nil
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return core.TaskRunResult{}, err
	}
	client := UpstreamClient(ctx)
	if strings.TrimSpace(code) == "school_daily" {
		message, errRun := RunSchool(ctx, client, native)
		if errRun != nil {
			return core.TaskRunResult{}, errRun
		}
		if message == "" {
			message = "开学季无需动作（活动期外或已完成）"
		}
		return core.TaskRunResult{Status: "done", Message: message}, nil
	}
	message, credit, energy, errRun := runGrowthTask(ctx, client, native, code)
	if errRun != nil {
		return core.TaskRunResult{}, errRun
	}
	return core.TaskRunResult{Status: "done", Message: message, Credit: credit, Energy: energy}, nil
}

// PrepareTasks 批量报名未接受的任务（队列与一键完成的前置）。
func (v *adapter) PrepareTasks(ctx context.Context, cred *core.Credential) int {
	if v.region.IsGlobal() {
		return 0
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return 0
	}
	return acceptPendingTasks(ctx, UpstreamClient(ctx), native)
}

func (v *adapter) RunAllTasks(ctx context.Context, cred *core.Credential) []core.AutoTaskResult {
	if v.region.IsGlobal() {
		return nil
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return []core.AutoTaskResult{{Code: "(账号)", Status: "error", Message: err.Error()}}
	}
	client := UpstreamClient(ctx)
	results := make([]core.AutoTaskResult, 0, len(autoActions)+1)
	if accepted := acceptPendingTasks(ctx, client, native); accepted > 0 {
		results = append(results, core.AutoTaskResult{
			Code: "(批量报名)", Status: "done",
			Message: fmt.Sprintf("报名 %d 个任务", accepted),
		})
		sleepContext(ctx, 1050*time.Millisecond)
	}
	for _, action := range autoActions {
		task, errTask := findGrowthTask(ctx, client, native, action.Code)
		if errTask != nil {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "error", Message: errTask.Error()})
			continue
		}
		if task == nil {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "skipped", Message: "该账号无此任务"})
			continue
		}
		if task.Claimed {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "skipped", Message: "已领取过奖励"})
			continue
		}
		if action.Code == "black_cat" && !InNightWindow(time.Now()) {
			results = append(results, core.AutoTaskResult{
				Code: action.Code, Status: "skipped",
				Message: "当前不在 23:00-08:00 计分窗口，排程会在夜间自动补足",
			})
			continue
		}
		message, credit, energy, errRun := runGrowthTask(ctx, client, native, action.Code)
		result := core.AutoTaskResult{Code: action.Code, Credit: credit, Energy: energy}
		if errRun != nil {
			result.Status = "error"
			result.Message = errRun.Error()
		} else {
			result.Status = "done"
			result.Message = message
		}
		if latest, errLatest := findGrowthTask(ctx, client, native, action.Code); errLatest == nil && latest != nil {
			result.Progress = fmt.Sprintf("%d/%d", latest.Current, latest.Target)
		}
		results = append(results, result)
		sleepContext(ctx, 1050*time.Millisecond)
	}
	return results
}

func runGrowthTask(ctx context.Context, client *Client, credential *Credential, code string) (string, int64, int64, error) {
	action := autoActionFor(code)
	if action == nil {
		return "", 0, 0, fmt.Errorf("该任务没有对应的自动动作（可能需要客户端内交互）")
	}
	task, errTask := findGrowthTask(ctx, client, credential, code)
	if errTask == nil && task != nil {
		if task.Claimed {
			return "任务已完成且已领奖", 0, 0, nil
		}
		if task.Done() {
			credit, energy := claimGrowthReward(ctx, client, credential, code)
			msg := "任务已达标（跳过重复执行）"
			if credit > 0 || energy > 0 {
				msg = fmt.Sprintf("任务已达标，已领奖 +%d 积分 +%d 能量", credit, energy)
			}
			return msg, credit, energy, nil
		}
	}
	message, errRun := action.Run(ctx, client, credential)
	if errRun != nil {
		return "", 0, 0, errRun
	}
	if waitGrowthClaimable(ctx, client, credential, code) {
		credit, energy := claimGrowthReward(ctx, client, credential, code)
		if credit > 0 || energy > 0 {
			message += fmt.Sprintf("（领奖 +%d 积分 +%d 能量）", credit, energy)
		}
		return message, credit, energy, nil
	}
	return message, 0, 0, nil
}

func waitGrowthClaimable(ctx context.Context, client *Client, credential *Credential, code string) bool {
	mp := IsMPTaskCode(code)
	attempts := 4
	if mp {
		attempts = 2
	}
	for attempt := 1; attempt < attempts; attempt++ {
		if !sleepContext(ctx, 3*time.Second) {
			return false
		}
		task, errTask := findGrowthTask(ctx, client, credential, code)
		if errTask != nil {
			continue
		}
		if task == nil || task.Claimed {
			return false
		}
		if task.Claimable() || task.Done() {
			return true
		}
	}
	return false
}

func findGrowthTask(ctx context.Context, client *Client, credential *Credential, code string) (*Task, error) {
	var list []Task
	var errList error
	if IsMPTaskCode(code) {
		list, errList = client.ListTasksMP(ctx, credential)
	} else {
		list, errList = client.ListTasks(ctx, credential)
	}
	if errList != nil {
		return nil, errList
	}
	for index := range list {
		if list[index].Code == code {
			return &list[index], nil
		}
	}
	return nil, nil
}

func claimGrowthReward(ctx context.Context, client *Client, credential *Credential, code string) (int64, int64) {
	if IsMPTaskCode(code) {
		credit, energy, errClaim := client.ClaimRewardMP(ctx, credential, code)
		if errClaim != nil {
			logger.Debug("claim mp reward for %s failed: %v", code, errClaim)
			return 0, 0
		}
		return credit, energy
	}
	credit, energy, errClaim := client.ClaimReward(ctx, credential, code)
	if errClaim != nil {
		logger.Debug("claim reward for %s failed: %v", code, errClaim)
		return 0, 0
	}
	return credit, energy
}

func acceptPendingTasks(ctx context.Context, client *Client, credential *Credential) int {
	tasksList, errTasks := client.ListTasks(ctx, credential)
	if errTasks != nil {
		return 0
	}
	var codes []string
	for _, task := range tasksList {
		if task.Claimed || task.Locked {
			continue
		}
		status := strings.ToLower(task.AcceptStatus)
		if status == "accepted" || status == "completed" || status == "claimed" {
			continue
		}
		if autoActionFor(task.Code) == nil && !IsMPTaskCode(task.Code) {
			continue
		}
		codes = append(codes, task.Code)
	}
	if len(codes) == 0 {
		return 0
	}
	accepted := 0
	for start := 0; start < len(codes); start += 20 {
		end := start + 20
		if end > len(codes) {
			end = len(codes)
		}
		if errAccept := client.AcceptTasks(ctx, credential, codes[start:end]); errAccept == nil {
			accepted += end - start
		}
		sleepContext(ctx, 1050*time.Millisecond)
	}
	return accepted
}

// ---- 开学季状态 ----

func (v *adapter) SchoolStatus(ctx context.Context, cred *core.Credential) core.SchoolStatusResult {
	if v.region.IsGlobal() {
		return core.SchoolStatusResult{Error: "global realm（无开学季活动）"}
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return core.SchoolStatusResult{Error: err.Error()}
	}
	client := UpstreamClient(ctx)
	result := core.SchoolStatusResult{}
	status, errStatus := client.SchoolTasks(ctx, native)
	if errStatus != nil {
		result.Error = errStatus.Error()
	} else if status != nil {
		result.InPeriod = status.InPeriod
		for _, task := range status.Tasks {
			result.Tasks = append(result.Tasks, core.SchoolTaskView{
				Code: task.Code, Title: schoolTaskTitle(task.Code),
				Status: task.Status, Progress: task.Progress, Target: task.TargetCount,
			})
		}
	}
	if chances, errChances := client.SchoolChances(ctx, native); errChances == nil {
		result.Chances = chances
	}
	return result
}

// ---- 定时任务族 ----

func (v *adapter) RunScheduled(ctx context.Context, cred *core.Credential, kind string) (string, error) {
	if v.region.IsGlobal() {
		return "", nil
	}
	native, err := v.nativeCredential(cred)
	if err != nil {
		return "", err
	}
	client := UpstreamClient(ctx)
	switch kind {
	case core.TaskKindActivity:
		conversationID := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if errReport := client.ReportChatActivity(ctx, native, conversationID, "", "", ""); errReport != nil {
			return "", errReport
		}
		return checkActivityStreak(ctx, client, native), nil
	case core.TaskKindKeepalive:
		return "", refreshAndSaveCredential(ctx, client, cred, native)
	case core.TaskKindTravel:
		return RunTravel(ctx, client, native), nil
	case core.TaskKindBlackcat:
		if !InNightWindow(time.Now()) {
			return "", nil
		}
		return RunBlackcat(ctx, client, native)
	case core.TaskKindSchool:
		return RunSchool(ctx, client, native)
	case core.TaskKindStreak:
		return RunStreakBonus(ctx, client, native)
	}
	return "", fmt.Errorf("unknown scheduled task kind %q", kind)
}

func checkActivityStreak(ctx context.Context, client *Client, credential *Credential) string {
	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil {
		return ""
	}
	if streak.Days == 0 {
		logger.Error("activity: report succeeded but streak.days=0 (event may have been silently dropped)")
		return ""
	}
	return fmt.Sprintf("连登天数 %d", streak.Days)
}

// refreshAndSaveCredential 保活：刷新 token 并写回宿主与磁盘。
func refreshAndSaveCredential(ctx context.Context, client *Client, cred *core.Credential, native *Credential) error {
	if errRefresh := client.RefreshToken(native); errRefresh != nil {
		return errRefresh
	}
	callbackID := hostabi.CallbackID(ctx)
	raw, okRaw := hostabi.AuthJSON(ctx, callbackID, cred.FileIDValue())
	if okRaw {
		if updated, errMerge := MergeStorageJSON(raw, native); errMerge == nil {
			if _, errSave := hostabi.Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
				Name: cred.FileIDValue(), JSON: updated,
			}); errSave != nil {
				logger.Debug("keepalive: host.auth.save failed: %v", errSave)
			}
		}
	}
	if errSave := SaveCredentialFile(cred.FilePath, native); errSave != nil {
		logger.Debug("keepalive: save credential failed: %v", errSave)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
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

// ---- 动作实现 ----

func runChatFive(ctx context.Context, client *Client, credential *Credential) (string, error) {
	for index := 0; index < 5; index++ {
		conversationID := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), index)
		if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "", ""); errReport != nil {
			return "", errReport
		}
		if !sleepContext(ctx, 1050*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "上报 5 条活跃事件", nil
}

func runFirstBuddy(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return RunTravel(ctx, client, credential), nil
}

func runModelChat(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-glm-%d", time.Now().UnixMilli())
	if errChat := client.DesktopChat(ctx, credential, "", "glm-5.2", "GLM-5.2", conversationID); errChat != nil {
		return "", errChat
	}
	if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "glm-5.2", "GLM-5.2"); errReport != nil {
		return "", errReport
	}
	return "glm-5.2 对话 + 上报", nil
}

func runRichMeow(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-meow-%d", time.Now().UnixMilli())
	requestID := NewHexID()
	events := withDesktopFingerprint(credential,
		DesktopChatSequence(conversationID, requestID, NewHexID(), "fast-model", "fast-model"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "桌面对话事件链", nil
}

func runBuddyApp(ctx context.Context, client *Client, credential *Credential) (string, error) {
	events := withDesktopFingerprint(credential,
		DesktopBuddyAppSequence(DefaultBuddyID, DefaultBuddyName))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "Buddy 应用事件链", nil
}

func runAutomation(ctx context.Context, client *Client, credential *Credential) (string, error) {
	events := withDesktopFingerprint(credential, DesktopAutomationCreateEvent("wb2api 自动化"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "定时任务创建事件", nil
}

func runLibraryRead(ctx context.Context, client *Client, credential *Credential) (string, error) {
	const pageURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if errReport := client.ReportWebElementClick(ctx, credential, pageURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); errReport != nil {
		return "", errReport
	}
	return "资料库阅读事件", nil
}

func runTemplateUse(ctx context.Context, client *Client, credential *Credential) (string, error) {
	templates := []struct{ id, name string }{
		{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
		{"4", "活动策划"}, {"5", "代码评审"},
	}
	for _, template := range templates {
		conversationID := fmt.Sprintf("wb2api-tpl-%d-%s", time.Now().UnixMilli(), template.id)
		requestID := NewHexID()
		events := withDesktopFingerprint(credential,
			DesktopTemplateUseSequence(conversationID, requestID, template.id, template.name))
		if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
			return "", errReport
		}
		if !sleepContext(ctx, 1050*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "模板使用事件 ×5", nil
}

func runPlaybookPrompt(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-pb-%d", time.Now().UnixMilli())
	requestID := fmt.Sprintf("wb2api-pb-req-%d", time.Now().UnixMilli())
	events := withDesktopFingerprint(credential,
		DesktopPlaybookPromptSequence(conversationID, requestID,
			"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "灵感案例事件组", nil
}

func runDesignCanvas(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-canvas-%d", time.Now().UnixMilli())
	requestID := NewHexID()
	events := withDesktopFingerprint(credential,
		DesktopDesignCanvasSequence(conversationID, requestID))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "设计画布事件组", nil
}

func runExpertFive(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "agent", 5)
}

func runExpertTeam(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "team", 3)
}

func runExpertChain(ctx context.Context, client *Client, credential *Credential, expertType string, count int) (string, error) {
	experts, errList := client.MarketExpertList(ctx, credential, expertType)
	if errList != nil {
		return "", errList
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场没有可用的 %s 类型专家", expertType)
	}
	completed := 0
	for index := 0; index < count; index++ {
		expert := experts[index%len(experts)]
		conversationID := fmt.Sprintf("wb2api-expert-%d-%d", time.Now().UnixMilli(), index)
		if errReport := client.ReportDesktopEvents(ctx, credential,
			withDesktopFingerprint(credential, DesktopExpertSummonSequence(expert))); errReport != nil {
			return "", errReport
		}
		if !sleepContext(ctx, 6*time.Second) {
			return "", ctx.Err()
		}
		requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expert.ID, "fast-model", conversationID)
		if errChat != nil {
			logger.Debug("expert chat failed for %s: %v", expert.ID, errChat)
			continue
		}
		if errReport := client.ReportDesktopEvents(ctx, credential,
			withDesktopFingerprint(credential, DesktopExpertActualUseEvent(expert, conversationID, requestID))); errReport != nil {
			return "", errReport
		}
		completed++
		if !sleepContext(ctx, 6*time.Second) {
			return "", ctx.Err()
		}
	}
	if completed == 0 {
		return "", fmt.Errorf("专家使用链一次都没成功")
	}
	return fmt.Sprintf("专家使用链 %d 次", completed), nil
}

func runAppearance(ctx context.Context, client *Client, credential *Credential) (string, error) {
	if errSet := client.DesktopAppearanceSet(ctx, credential, "theme-tkmw7j"); errSet != nil {
		return "", errSet
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode":   "appearance_skin_apply",
		"resourceKey": "theme-tkmw7j", "kind": "theme", "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "主题设置 + 皮肤事件", nil
}

func runSkillFresh(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-skill-%d", time.Now().UnixMilli())
	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, "", "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "skill_info", "skillId": "skill_2097350077599879168",
		"conversationId": conversationID, "requestId": requestID,
		"parentConversationId": conversationID, "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "技能加载事件", nil
}

func runExpertLighthouse(ctx context.Context, client *Client, credential *Credential) (string, error) {
	const expertID = "ex_2cvvUZQhDyeJ"
	const expertName = "轻量云专家"
	conversationID := fmt.Sprintf("wb2api-lighthouse-%d", time.Now().UnixMilli())

	created := DesktopChatSequence(conversationID, NewHexID(), NewHexID(), "fast-model", "fast-model")
	if len(created) > 0 {
		created[0]["has_expert"] = true
		created[0]["expert_id"] = expertID
		created[0]["expert_name"] = expertName
		created[0]["expert_industry_id"] = ""
	}
	if errReport := client.ReportDesktopEvents(ctx, credential, withDesktopFingerprint(credential, created)); errReport != nil {
		return "", errReport
	}
	if !sleepContext(ctx, 6*time.Second) {
		return "", ctx.Err()
	}
	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expertID, "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
		"expertTitle": expertName, "type": "", "expertType": "builtin",
		"source": "builtin", "version": "1.0.0",
		"cost": 0, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tailOf(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "轻量云专家链", nil
}

func runBlackCat(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return RunBlackcat(ctx, client, credential)
}

func withDesktopFingerprint(credential *Credential, events []map[string]any) []map[string]any {
	return ApplyDesktopFingerprint(credential, events)
}

func tailOf(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

// sleepCtx 可取消的睡眠。返回 false 表示上下文已取消。
//
// 任务链上的每个动作之间都要按上游的风控节奏间隔（见 fingerprint.go 的节流
// 参数），而整个闭环必须可被取消——用 time.Sleep 会让取消后仍要等满间隔。
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
