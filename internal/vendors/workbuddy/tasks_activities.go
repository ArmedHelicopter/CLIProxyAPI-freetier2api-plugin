package workbuddy

// 本文件实现成长任务的列表 / 报名 / 领奖接口。
//
// 三条路径的形态差异是这里最容易踩的坑：
//   - 列表与报名在对话域（chatBase），带业务信封；
//   - **领奖在 Web 域**（webBase），task_code 在**路径**里、无 body、带 x-client-platform: web。
//     对话域的 /tasks/reward/claim 路径**不存在**，恒返回 400 "task not completed"，
//     这是早期领奖全部失败的真实原因。
//   - 小程序口径的任务码要走带 X-Client-Platform: miniprogram 的同名接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"freetier2api-plugin/internal/core"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Task 是一条成长任务的状态。
type Task struct {
	// Code 是任务标识。
	Code string
	// Title 是任务名。
	Title string
	// Current / Target 是进度。
	Current int64
	Target  int64
	// Claimed 表示奖励已领取。
	Claimed bool
	// Locked 表示任务处于锁定链上（如每日零点解锁的连续任务）。
	Locked bool
	// AcceptStatus 是报名状态：not_accepted / accepted / claimed。
	AcceptStatus string
}

// Claimable 报告任务是否已达标且未领奖。
//
// 本地推算而非依赖上游字段：上游的 claimable 标记并不总是可信
// （部分形态不下发该字段）。
func (t Task) Claimable() bool {
	if t.Claimed || t.Locked {
		return false
	}
	return t.Target > 0 && t.Current >= t.Target
}

// Done 报告任务是否已完成（无论是否领奖）。
func (t Task) Done() bool { return t.Target > 0 && t.Current >= t.Target }

// growthTaskEntry 是任务条目的原始形态。
//
// progress 字段有两种形态：对象 {current,target} 与平铺的 current/target，
// 两种都要兼容（上游不同接口形态不一致）。
type growthTaskEntry struct {
	TaskCode     string          `json:"task_code"`
	TaskCodeAlt  string          `json:"taskCode"`
	Title        string          `json:"title"`
	TitleAlt     string          `json:"task_name"`
	Claimed      bool            `json:"claimed"`
	Locked       bool            `json:"locked"`
	AcceptStatus string          `json:"accept_status"`
	Progress     json.RawMessage `json:"progress"`
	Current      int64           `json:"current"`
	Target       int64           `json:"target"`
}

// toTask 把原始条目归一化。
func (e growthTaskEntry) toTask() Task {
	task := Task{
		Code:         core.FirstNonEmpty(e.TaskCode, e.TaskCodeAlt),
		Title:        core.FirstNonEmpty(e.Title, e.TitleAlt),
		Claimed:      e.Claimed,
		Locked:       e.Locked,
		AcceptStatus: strings.TrimSpace(e.AcceptStatus),
		Current:      e.Current,
		Target:       e.Target,
	}
	// progress 为对象形态时以它为准。
	if len(e.Progress) > 0 {
		var nested struct {
			Current int64 `json:"current"`
			Target  int64 `json:"target"`
		}
		if errUnmarshal := json.Unmarshal(e.Progress, &nested); errUnmarshal == nil {
			if nested.Current != 0 || nested.Target != 0 {
				task.Current = nested.Current
				task.Target = nested.Target
			}
		}
	}
	return task
}

// ListTasks 拉取成长任务列表（默认口径）。
func (c *Client) ListTasks(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, false)
}

// ListTasksMP 拉取小程序口径的任务列表。
//
// 小程序列表是默认列表的**超集**（含 mp 专属任务），合并时必须按任务码去重。
func (c *Client) ListTasksMP(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, true)
}

func (c *Client) listTasks(ctx context.Context, cred *Credential, mp bool) ([]Task, error) {
	if cred.Realm().IsGlobal() {
		// global 账号没有成长任务体系。
		return nil, nil
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.ChatBase+growthTasksPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build tasks request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	return parseGrowthTasks(data)
}

// parseGrowthTasks 解析任务列表响应。
//
// data 可能是数组，也可能是带 tasks 键的对象。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var entries []growthTaskEntry
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		var wrapper struct {
			Tasks []growthTaskEntry `json:"tasks"`
		}
		if errWrapper := json.Unmarshal(data, &wrapper); errWrapper != nil {
			return nil, fmt.Errorf("decode tasks: %w", errUnmarshal)
		}
		entries = wrapper.Tasks
	}
	out := make([]Task, 0, len(entries))
	for _, entry := range entries {
		task := entry.toTask()
		if task.Code == "" {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

// AcceptTasks 批量报名任务。
//
// 报名本身**不产生进度**（进度靠行为事件点亮），它只是让状态机从
// not_accepted 走到 accepted。但少了它，部分任务的事件不会被计分。
func (c *Client) AcceptTasks(ctx context.Context, cred *Credential, codes []string) error {
	return c.acceptTasks(ctx, cred, codes, false)
}

func (c *Client) acceptTasks(ctx context.Context, cred *Credential, codes []string, mp bool) error {
	if len(codes) == 0 {
		return nil
	}
	body := map[string]any{"task_codes": codes}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode accept request: %w", errEncode)
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+growthTasksAcceptPath, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build accept request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}
	_, errCall := c.doJSON(req)
	return errCall
}

// ClaimReward 领取任务奖励（Web 域）。
//
// 已领取时返回 (0, 0, nil)：上游用 already_claimed 标记表达幂等，
// 把它当错误会让自动任务每次重复执行都报错。
func (c *Client) ClaimReward(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(RegionCN)
	claimURL := endpoints.WebBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build claim request: %w", errReq)
	}
	// 领奖走 web 指纹：Referer 指向成长中心页。
	applyWebHeaders(req, cred, endpoints.WebBase+"/profile/growth-center")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return 0, 0, errCall
	}
	return parseClaimResult(data)
}

// ClaimRewardMP 用小程序口径领奖。
//
// 部分任务/租户在对话域返回 400，此时降级到 Web 域重试。
func (c *Client) ClaimRewardMP(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(cred.Realm())
	claimURL := endpoints.ChatBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build mp claim request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	req.Header.Set("X-Client-Platform", "miniprogram")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		// 对话域失败时降级到 Web 域（上游两个口径的可用性不一致）。
		return c.ClaimReward(ctx, cred, code)
	}
	return parseClaimResult(data)
}

// parseClaimResult 解析领奖响应。
func parseClaimResult(data json.RawMessage) (credit, energy int64, err error) {
	var payload struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		// 结构不认识但请求成功：副作用已发生，按无新增处理。
		return 0, 0, nil
	}
	if payload.AlreadyClaimed {
		return 0, 0, nil
	}
	return payload.Credit, payload.Energy, nil
}

// IsMPTaskCode 报告任务码是否属于小程序口径。
//
// 小程序专属任务不会出现在默认列表里，必须走 mp 口径查询与领奖。
func IsMPTaskCode(code string) bool {
	return mpTaskCodes[strings.TrimSpace(code)]
}

// mpTaskCodes 是小程序口径专属任务码。
//
// 说明：Tasks_4..7 处于「每日零点解锁一环」的锁定链上，
// 其判据为预置推测（解锁后需实测校正）；locked 状态的任务永不入队
// （locked 时报名不落账）。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// 本文件实现需要真实对话的辅助方法。
//
// 部分任务（专家使用、技能尝鲜、夜猫子、桌面开学季）的判据要求
// **真实对话产生的服务端 requestId** —— 自造 UUID 不会被计分。
// 因此这些方法会发一次真实对话，并从 SSE 流里抓出上游返回的 id。

// serverRequestIDRe 校验上游返回的 id 形态（32 位 hex，可带 cmb- 前缀）。
var serverRequestIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// desktopChatSystemPrompt 是辅助对话使用的 system 提示词。
const desktopChatSystemPrompt = "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"

// desktopChatUserPrompt 是辅助对话的用户消息（极短，省额度）。
const desktopChatUserPrompt = "1+1等于几？直接回答。"

// DesktopChat 发一次真实对话但不关心 requestId（夜猫子等场景）。
func (c *Client) DesktopChat(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string) error {
	_, errChat := c.desktopChatInternal(ctx, cred, expertID, modelID, modelName, conversationID, false)
	return errChat
}

// DesktopChatWithRequestID 发一次真实对话并返回上游的服务端 requestId。
//
// 用于需要「真实对话回执」的任务（专家使用、技能尝鲜、桌面开学季）。
func (c *Client) DesktopChatWithRequestID(ctx context.Context, cred *Credential, expertID, modelID, conversationID string) (string, error) {
	return c.desktopChatInternal(ctx, cred, expertID, modelID, modelID, conversationID, true)
}

// desktopChatInternal 是辅助对话的实现。
//
// needRequestID 为 true 时，若流里抓不到合法的服务端 id 会返回错误——
// 因为自造 id 不会被计分，静默继续只会让任务白跑。
func (c *Client) desktopChatInternal(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string, needRequestID bool) (string, error) {
	if strings.TrimSpace(modelID) == "" {
		modelID = "fast-model"
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = modelID
	}
	body := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "system", "content": desktopChatSystemPrompt},
			map[string]any{"role": "user", "content": desktopChatUserPrompt},
		},
		"agent":       "cli",
		"temperature": 1,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return "", fmt.Errorf("encode desktop chat: %w", errEncode)
	}

	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, bytes.NewReader(encoded))
	if errReq != nil {
		return "", fmt.Errorf("build desktop chat request: %w", errReq)
	}
	c.applyDesktopChatHeaders(req, cred, expertID, conversationID, modelID)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(req)
	if errDo != nil {
		return "", transientError(errDo)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		return "", enrichClassifyError(resp.StatusCode, string(raw), resp.Header)
	}

	requestID, errRead := scanServerRequestID(resp.Body)
	if errRead != nil && needRequestID {
		return "", errRead
	}
	return requestID, nil
}

// applyDesktopChatHeaders 写入桌面辅助对话的请求头。
//
// 与常规对话的差别：带 X-Agent-Intent/Type 与桌面 UA，
// 这样上游把它归到「桌面客户端」的行为统计里（部分任务按此判定）。
func (c *Client) applyDesktopChatHeaders(req *http.Request, cred *Credential, expertID, conversationID, modelID string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", DesktopClientVersion)
	if strings.TrimSpace(expertID) != "" {
		req.Header.Set("X-Expert-Id", strings.TrimSpace(expertID))
	}
	if strings.TrimSpace(conversationID) != "" {
		req.Header.Set("X-Conversation-ID", strings.TrimSpace(conversationID))
	}
	req.Header.Set("X-Request-ID", deriveID(cred.UIDValue(), "req")+newHexID()[:6])
}

// scanServerRequestID 从 SSE 流里抓出上游的服务端 requestId。
//
// 判据：第一个形如 "id":"<32hex>"（可带 cmb- 前缀）的值。
// 抓不到返回错误——调用方需要它来构造被计分的事件链。
func scanServerRequestID(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 8*1024), 1<<20)
	deadline := time.Now().Add(30 * time.Second)

	for scanner.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := scanner.Text()
		index := strings.Index(line, `"id":"`)
		if index < 0 {
			continue
		}
		rest := line[index+len(`"id":"`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			continue
		}
		candidate := rest[:end]
		if serverRequestIDRe.MatchString(candidate) {
			// 上游可能返回 cmb- 前缀形态，事件链里用去掉前缀的纯 hex。
			return strings.TrimPrefix(candidate, "cmb-"), nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return "", fmt.Errorf("read desktop chat stream: %w", errScan)
	}
	return "", fmt.Errorf("no server request id found in chat stream (self-generated ids are not scored)")
}

// DesktopAppearanceSet 设置主题外观。
//
// 注意：**单发 set 不计分**，必须叠加 appearance_skin_apply 事件（见任务动作表）。
func (c *Client) DesktopAppearanceSet(ctx context.Context, cred *Credential, resourceKey string) error {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{"kind": "theme", "resource_key": strings.TrimSpace(resourceKey)}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode appearance request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+appearanceSetPath, bytes.NewReader(encoded))
	if errReq != nil {
		return fmt.Errorf("build appearance request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)
	_, errCall := c.doJSON(req)
	return errCall
}

// MarketExpert 是专家市场里的一位专家。
type MarketExpert struct {
	ID         string
	Name       string
	Profession string
	Type       string
	Version    string
	Category   string
}

// MarketExpertList 拉取专家市场列表。
//
// expertType 为 "agent" 取单专家、"team" 取专家团。
// **expert_id 必须是市场里的真实值**：自造 id 不会计入任务进度。
func (c *Client) MarketExpertList(ctx context.Context, cred *Credential, expertType string) ([]MarketExpert, error) {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{
		"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc",
	}
	if trimmed := strings.TrimSpace(expertType); trimmed != "" {
		body["expert_type"] = trimmed
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode expert list request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+expertMarketListPath, bytes.NewReader(encoded))
	if errReq != nil {
		return nil, fmt.Errorf("build expert list request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Experts []struct {
			ExpertID      string   `json:"expert_id"`
			ExpertType    string   `json:"expert_type"`
			DisplayNameZH string   `json:"display_name_zh"`
			ProfessionZH  string   `json:"profession_zh"`
			Version       string   `json:"version"`
			Categories    []string `json:"categories"`
		} `json:"experts"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode expert list: %w", errUnmarshal)
	}
	out := make([]MarketExpert, 0, len(payload.Experts))
	for _, item := range payload.Experts {
		id := strings.TrimSpace(item.ExpertID)
		if id == "" {
			// 空 id 服务端不入账，跳过。
			continue
		}
		expert := MarketExpert{
			ID:         id,
			Name:       strings.TrimSpace(item.DisplayNameZH),
			Profession: strings.TrimSpace(item.ProfessionZH),
			Type:       strings.TrimSpace(item.ExpertType),
			Version:    strings.TrimSpace(item.Version),
		}
		if len(item.Categories) > 0 {
			expert.Category = strings.TrimSpace(item.Categories[0])
		}
		out = append(out, expert)
	}
	return out, nil
}

// 本文件实现成长体系的活动接口：连登、旅行、夜猫子、开学季、礼包与补偿。
//
// 这些接口全部挂在对话域或计费域，形态各异（信封嵌套、幂等码、业务错误表达状态），
// 因此逐个封装成方法，让上层逻辑只管业务语义。

// Streak 是连登状态。
type Streak struct {
	// Days 是连续登录天数。
	Days int64
	// MakeupCards 是补签卡余额。
	MakeupCards int64
	// Tiers 是各兑换档位。
	Tiers []StreakTier
}

// StreakTier 是一个兑换档位。
type StreakTier struct {
	// Tier 是档位标识（7d / 14d / 28d）。
	Tier string
	// Status 是状态：locked / claimed / 空（可兑换）。
	Status string
}

// GrowthStreak 查询连登状态。
func (c *Client) GrowthStreak(ctx context.Context, cred *Credential) (*Streak, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthStreakPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Streak struct {
			Days int64 `json:"days"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int64 `json:"balance"`
		} `json:"makeup_cards"`
		RedemptionStatus struct {
			// 档位状态按 tier_7d_status / tier_14d_status / tier_28d_status 三个键给出。
			Tier7d  string `json:"tier_7d_status"`
			Tier14d string `json:"tier_14d_status"`
			Tier28d string `json:"tier_28d_status"`
		} `json:"redemption_status"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode streak: %w", errUnmarshal)
	}
	streak := &Streak{Days: payload.Streak.Days, MakeupCards: payload.MakeupCards.Balance}
	for _, item := range []struct {
		tier   string
		status string
	}{
		{"7d", payload.RedemptionStatus.Tier7d},
		{"14d", payload.RedemptionStatus.Tier14d},
		{"28d", payload.RedemptionStatus.Tier28d},
	} {
		if strings.TrimSpace(item.status) == "" {
			continue
		}
		streak.Tiers = append(streak.Tiers, StreakTier{Tier: item.tier, Status: item.status})
	}
	return streak, nil
}

// GrowthRedeemTier 兑换连登档位。
//
// 未解锁时上游返回 403「连续登录天数不足」，属预期结果，调用方静默跳过。
func (c *Client) GrowthRedeemTier(ctx context.Context, cred *Credential, tier string) error {
	clientToken := newHexID()
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthRedeemPath, map[string]any{
		"tier": strings.TrimSpace(tier), "client_token": clientToken,
	})
	return errCall
}

// LotteryChances 查询可用抽奖次数。
func (c *Client) LotteryChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthLotterySummary, nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chances int `json:"chances"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode lottery summary: %w", errUnmarshal)
	}
	return payload.Chances, nil
}

// LotteryDraw 执行一次抽奖。
func (c *Client) LotteryDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthLotteryDraw, map[string]any{
		"client_token": newHexID(),
	})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// HeatmapYesterdayMissed 报告昨日是否漏签。
func (c *Client) HeatmapYesterdayMissed(ctx context.Context, cred *Credential) (bool, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthHeatmapPath, nil)
	if errCall != nil {
		return false, errCall
	}
	yesterday := time.Now().AddDate(0, 0, -1).In(cstZone).Format("2006-01-02")
	// 响应形态：cells 数组或日期为键的 map，两种都试。
	var wrapper struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int64  `json:"score"`
		} `json:"cells"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil {
		for _, cell := range wrapper.Cells {
			if cell.Date == yesterday {
				return cell.Score == 0, nil
			}
		}
	}
	var byDate map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &byDate); errUnmarshal == nil {
		if raw, okRaw := byDate[yesterday]; okRaw {
			var cell struct {
				Score int64 `json:"score"`
			}
			if errCell := json.Unmarshal(raw, &cell); errCell == nil {
				return cell.Score == 0, nil
			}
		}
	}
	return false, nil
}

// UseMakeupCard 使用补签卡补签指定日期。
func (c *Client) UseMakeupCard(ctx context.Context, cred *Credential, date string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthMakeupCardsPath, map[string]any{
		"target_date": strings.TrimSpace(date),
	})
	return errCall
}

// cstZone 是上游自然日使用的时区（固定 UTC+8，不依赖系统 tzdata）。
var cstZone = time.FixedZone("CST", 8*3600)

// BuddyInfo 是猫档案。
type BuddyInfo struct {
	// HasBuddy 表示是否已领养。
	HasBuddy bool
}

// BuddyInfo 查询猫档案。
func (c *Client) BuddyInfo(ctx context.Context, cred *Credential) (*BuddyInfo, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyInfoPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode buddy info: %w", errUnmarshal)
	}
	trimmed := strings.TrimSpace(string(payload.Buddy))
	return &BuddyInfo{HasBuddy: trimmed != "" && trimmed != "null"}, nil
}

// BuddyAgreement 同意领养协议（幂等）。
func (c *Client) BuddyAgreement(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return errCall
}

// BuddyFirst 领取第一只猫。
//
// 失败通常因为「当日活跃上报不足」（first_buddy 任务未完成），
// 这不是账号问题，调用方应视为「今天跳过」而不是错误。
func (c *Client) BuddyFirst(ctx context.Context, cred *Credential) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyFirstPath, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// TravelStatus 是旅行状态。
type TravelStatus struct {
	// State 是状态：idle（可派出）/ traveling（在途）/ arrived（可领奖）。
	State string
	// RecordID 是本次旅行的记录标识（领奖必填，为 0 表示无可领）。
	RecordID int64
	// DailyLimitReached 表示今日已派出过。
	DailyLimitReached bool
}

// TravelStatus 查询旅行状态。
func (c *Client) TravelStatus(ctx context.Context, cred *Credential) (*TravelStatus, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyTravelStatusPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		State             string `json:"state"`
		Status            string `json:"status"`
		RecordID          int64  `json:"record_id"`
		DailyLimitReached bool   `json:"daily_limit_reached"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode travel status: %w", errUnmarshal)
	}
	return &TravelStatus{
		State:             core.FirstNonEmpty(payload.State, payload.Status),
		RecordID:          payload.RecordID,
		DailyLimitReached: payload.DailyLimitReached,
	}, nil
}

// TravelDepart 派猫出门旅行。
func (c *Client) TravelDepart(ctx context.Context, cred *Credential, locationID int) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelDepartPath, map[string]any{
		"location_id": locationID,
	})
	return errCall
}

// TravelClaim 领取旅行奖励。
func (c *Client) TravelClaim(ctx context.Context, cred *Credential, recordID int64) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelClaimPath, map[string]any{
		"record_id": recordID,
	})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		RewardCredit int64 `json:"reward_credit"`
		Credit       int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return firstNonZero(payload.RewardCredit, payload.Credit), nil
}

// BlackcatNeed 返回夜猫子任务的缺口次数。
func (c *Client) BlackcatNeed(ctx context.Context, cred *Credential) (int64, error) {
	tasks, errTasks := c.ListTasks(ctx, cred)
	if errTasks != nil {
		return 0, errTasks
	}
	for _, task := range tasks {
		if task.Code != "black_cat" {
			continue
		}
		if task.Claimed || task.Done() {
			return 0, nil
		}
		need := task.Target - task.Current
		if need < 0 {
			need = 0
		}
		return need, nil
	}
	return 0, nil
}

// 礼包与补偿的路径访问器（供上层遍历调用）。
func ClaimGiftPath() string         { return claimGiftPath }
func ClaimCompensationPath() string { return claimCompPath }

// ClaimBillingReward 领取计费域的一次性奖励（礼包 / 补偿）。
//
// 已领过时上游返回业务错误，调用方按「无新增」处理。
func (c *Client) ClaimBillingReward(ctx context.Context, cred *Credential, path string) (int64, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.billingJSON(cred, endpoints.BillingBase+strings.TrimSpace(path), http.MethodPost, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// SchoolStatus 是开学季活动状态。
type SchoolStatus struct {
	// InPeriod 表示当前是否在活动期内。
	InPeriod bool
	// Tasks 是活动任务。
	Tasks []SchoolTask
}

// SchoolTask 是一个开学季任务。
type SchoolTask struct {
	// Code 是任务标识。
	Code string
	// Status 是状态：pending / in_progress / claimed / completed。
	Status string
	// Progress / TargetCount 是进度。
	Progress    int64
	TargetCount int64
}

// SchoolTasks 查询开学季任务。
func (c *Client) SchoolTasks(ctx context.Context, cred *Credential) (*SchoolStatus, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/tasks", nil)
	if errCall != nil {
		return nil, errCall
	}
	_ = endpoints
	var payload struct {
		InPeriod bool `json:"in_period"`
		Tasks    []struct {
			Code        string `json:"task_code"`
			Status      string `json:"status"`
			Progress    int64  `json:"progress"`
			TargetCount int64  `json:"target_count"`
		} `json:"tasks"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode school tasks: %w", errUnmarshal)
	}
	status := &SchoolStatus{InPeriod: payload.InPeriod}
	for _, item := range payload.Tasks {
		status.Tasks = append(status.Tasks, SchoolTask{
			Code: item.Code, Status: item.Status,
			Progress: item.Progress, TargetCount: item.TargetCount,
		})
	}
	return status, nil
}

// SchoolTaskViewed 激活一个开学季任务（pending → in_progress）。
//
// 这是**计数前置**：未激活的任务，后续行为事件不会被计分。
func (c *Client) SchoolTaskViewed(ctx context.Context, cred *Credential, code string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/viewed", map[string]any{})
	return errCall
}

// SchoolTaskClaim 领取开学季任务奖励，返回获得的抽奖次数。
func (c *Client) SchoolTaskClaim(ctx context.Context, cred *Credential, code string) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/claim", map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		ChanceGranted int64 `json:"chance_granted"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.ChanceGranted, nil
}

// SchoolShareComplete 完成分享任务。
//
// 纯前端上报：服务端不校验真实分享回执，直接调用即点亮。
func (c *Client) SchoolShareComplete(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/tasks/share-complete",
		map[string]any{"channel": "wechat"})
	return errCall
}

// SchoolChances 查询开学季抽奖次数余额。
func (c *Client) SchoolChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/config", nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode school config: %w", errUnmarshal)
	}
	return payload.Chance.Balance, nil
}

// SchoolDraw 执行一次开学季抽奖。
func (c *Client) SchoolDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/wheel/draw",
		map[string]any{"draw_uuid": newHexID()})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// SchoolExpertUseEvents 构造开学季专家使用事件链（4 事件）。
func SchoolExpertUseEvents(expertID, expertName, conversationID string) []map[string]any {
	requestID := "wb2api-exp-req-" + randomHexForExport(6)
	return []map[string]any{
		{
			"eventCode": "expert_summon_click", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "position": 0,
		},
		{
			"eventCode": "expert_summoned", "id": expertID, "name": expertName,
			"type": "16-BackToSchool",
		},
		{
			"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "characterCount": 14, "expertType": "builtin",
			"conversationId": conversationID, "requestId": requestID,
		},
		{
			"eventCode": "chat_request_send", "inputLength": 14, "isPlan": false,
			"maxSteps": 500, "expertId": expertID, "expertName": expertName,
			"conversationId": conversationID, "requestId": requestID,
			"parentConversationId": conversationID,
			"codebuddy.session_id": conversationID,
		},
	}
}

// growthJSON 发起一次成长域请求并解开信封。
func (c *Client) growthJSON(ctx context.Context, cred *Credential, method, path string, body any) (json.RawMessage, error) {
	endpoints := GetEndpoints(cred.Realm())
	var reader *strings.Reader
	if body != nil {
		encoded, errEncode := json.Marshal(body)
		if errEncode != nil {
			return nil, fmt.Errorf("encode %s request: %w", path, errEncode)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, errReq := http.NewRequestWithContext(ctx, method, endpoints.ChatBase+path, reader)
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", path, errReq)
	}
	c.applyBillingHeaders(req, cred)
	// 开学季链路走计费域。
	if strings.HasPrefix(path, schoolBasePath) {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, method, bodyOrEmpty(body))
		return data, errCall
	}
	return c.doJSON(req)
}

// bodyOrEmpty 归一化请求体（nil 时给空对象，上游要求 body 是对象）。
func bodyOrEmpty(body any) any {
	if body == nil {
		return map[string]any{}
	}
	return body
}

// firstNonZero 返回第一个非零值。
func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// ReportDesktopEvents 用桌面指纹族上报事件（对话域 /v2/report）。
func (c *Client) ReportDesktopEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.ChatBase+reportPath, events, func(req *http.Request) {
		c.applyDesktopHeaders(req, cred)
	})
}

// ReportBillingEvents 用 CLI/billing 指纹族上报事件（计费域 /v2/report）。
func (c *Client) ReportBillingEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
	})
}

// ReportWebEvents 用 web 指纹族上报事件（Web 域 /v2/report）。
func (c *Client) ReportWebEvents(ctx context.Context, cred *Credential, pageURL string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.WebBase+reportPath, events, func(req *http.Request) {
		applyWebHeaders(req, cred, pageURL)
	})
}

// ReportMPEvents 用小程序指纹族上报事件。
//
// 注意 base 用 CN 计费域硬编码而非按 realm 选：global 账号也没有小程序任务，
// 行为一致，跟随原项目实现。
func (c *Client) ReportMPEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
		applyMPHeaders(req)
	})
}

// postReport 是所有指纹族共用的上报出口。
func (c *Client) postReport(ctx context.Context, cred *Credential, url string, events []map[string]any, decorate func(*http.Request)) error {
	// body 必须是数组：上游按数组解析，单对象会被静默丢弃。
	encoded, errEncode := json.Marshal(events)
	if errEncode != nil {
		return fmt.Errorf("encode report events: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build report request: %w", errReq)
	}
	decorate(req)

	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("report request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body := make([]byte, 0, 1024)
	buffer := make([]byte, 1024)
	for {
		read, errRead := resp.Body.Read(buffer)
		if read > 0 {
			body = append(body, buffer[:read]...)
			if len(body) > apiBodyLimit {
				break
			}
		}
		if errRead != nil {
			break
		}
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}
	// 上报接口对「已上报过」也返回成功，因此不检查业务码。
	return nil
}

// ReportChatActivity 上报一条对话活跃事件（CLI/billing 指纹）。
//
// 这一条上报同时点亮 growth 连登与解锁 first_buddy 任务，
// 因此是领养与连登体系的前置条件。
func (c *Client) ReportChatActivity(ctx context.Context, cred *Credential, conversationID, requestID, modelID, modelName string) error {
	if strings.TrimSpace(requestID) == "" {
		requestID = conversationID
	}
	if strings.TrimSpace(modelID) == "" {
		modelID = defaultReportModelID
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = defaultReportModelName
	}
	now := nowMs()
	event := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": conversationID, "requestId": requestID,
		"inputLength":      12,
		"requestModelId":   modelID,
		"requestModelName": modelName,
		"isPlan":           false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false,
		"maxToken":       0, "maxSteps": 0, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": "", "knowledgeName": "",
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "default", "agentType": "conversation",
		// userId 缺失会让服务端静默丢弃这条事件。
		"userId": cred.UIDValue(),
	}
	return c.ReportBillingEvents(ctx, cred, []map[string]any{event})
}

// 上报默认模型（上游对活跃上报不强校验模型，用通用值即可）。
const (
	defaultReportModelID   = "deepseek-v4-flash"
	defaultReportModelName = "DeepSeek V4 Flash"
)

// ReportWebElementClick 上报一次 web 元素点击（Library_read 等任务）。
func (c *Client) ReportWebElementClick(ctx context.Context, cred *Credential, pageURL, elementID, elementName string) error {
	now := nowMs()
	event := map[string]any{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": chromeUA,
		"machineId":    deriveID(cred.UIDValue(), "webmachine"),
		"userId":       cred.UIDValue(),
		"userNickname": cred.NicknameValue(),
		"enterpriseId": cred.EnterpriseIDValue(),
	}
	return c.ReportWebEvents(ctx, cred, pageURL, []map[string]any{event})
}

// nowMs 返回当前毫秒时间戳。
func nowMs() int64 { return time.Now().UnixMilli() }

// randomHexForExport 生成随机 hex（供包内构造事件 id 使用）。
func randomHexForExport(n int) string {
	buffer := make([]byte, n)
	if _, errRand := rand.Read(buffer); errRand != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buffer)
}
