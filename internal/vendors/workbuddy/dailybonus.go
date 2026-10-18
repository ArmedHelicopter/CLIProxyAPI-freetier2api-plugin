package workbuddy

// 本文件实现国际版（global）账号的「每日活跃奖励」，即国际版的签到。
//
// 背景（结论来自 codebuddy-intl-cpa 项目的实测，2026-09-28 → 09-29，
// 7 个账号交叉验证）：国际版没有国内版「加油站」式的每日签到——
// 签到活动状态接口对国际版永远 active:false，daily-checkin 端点也不会入账。
// 国际版的每日收入路径是**活跃信标**：官方客户端每次对话都会向
// POST /v2/report 发一条 chat_request_send 事件。当天成长热力图
// （GET /activity/growth/heatmap）score > 0 即视为「点亮」，次日凌晨
// （中国时区）自动发放 30 积分的 Bonus Pack（平台奖励积分，30 天有效）；
// 连续点亮还能解锁连登档位（14d +50、28d +150）。
//
// 设计要点（与参考实现保持一致）：
//   - 幂等：以热力图最后一格作为 API 自己的「今天」，点亮则不再上报，
//     因此不依赖本地时区做自然日判定；
//   - 信标不重试：事件按天幂等，但盲目重发会把当天计数翻倍（score 2 → 4）；
//     单次失败交给下一个排程点兜底；
//   - 每天多个排程点：已点亮的点只花一次热力图读，几乎零成本。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// GlobalDailyBonusHours 是国际版每日活跃奖励的默认排程点（本地时间）。
// 4 个点足够兜住「宿主刚启动网络未就绪」这类瞬时失败；运行幂等，
// 多跑只多一次热力图读。
var GlobalDailyBonusHours = []int{8, 12, 16, 20}

// growthHeatmapCell 是成长热力图上的一格。
type growthHeatmapCell struct {
	Date  string `json:"date"`
	Score int64  `json:"score"`
}

// GrowthHeatmapToday 读取成长热力图，返回「今天」的一格与全部已点亮日期
// （热力图顺序，最早在前）。
//
// 响应有两种形态：cells 数组，或日期为键的 map（与 HeatmapYesterdayMissed
// 的解析保持一致）。空热力图按错误处理，避免把「读不到」误判成「未点亮」。
func (c *Client) GrowthHeatmapToday(ctx context.Context, cred *Credential) (growthHeatmapCell, []string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthHeatmapPath, nil)
	if errCall != nil {
		return growthHeatmapCell{}, nil, errCall
	}
	return parseGrowthHeatmap(data)
}

// parseGrowthHeatmap 解析热力图响应（纯函数，便于对两种响应形态做单测）。
func parseGrowthHeatmap(data json.RawMessage) (growthHeatmapCell, []string, error) {
	var wrapper struct {
		Cells []growthHeatmapCell `json:"cells"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil && len(wrapper.Cells) > 0 {
		lit := make([]string, 0, len(wrapper.Cells))
		for _, cell := range wrapper.Cells {
			if cell.Score > 0 {
				lit = append(lit, cell.Date)
			}
		}
		return wrapper.Cells[len(wrapper.Cells)-1], lit, nil
	}
	var byDate map[string]struct {
		Score int64 `json:"score"`
	}
	if errUnmarshal := json.Unmarshal(data, &byDate); errUnmarshal != nil || len(byDate) == 0 {
		return growthHeatmapCell{}, nil, fmt.Errorf("heatmap empty or unrecognised")
	}
	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	lit := make([]string, 0, len(dates))
	for _, date := range dates {
		if byDate[date].Score > 0 {
			lit = append(lit, date)
		}
	}
	last := dates[len(dates)-1]
	return growthHeatmapCell{Date: last, Score: byDate[last].Score}, lit, nil
}

// DailyBonusBeaconEvent 构造一条每日活跃信标（chat_request_send）。
//
// 字段形态镜像官方客户端的对话上报：conversationId/requestId 用一次性
// 随机值，让每天看起来都是新会话而非重放；userId 必须带，缺失时上游
// 返回 200 但静默丢弃事件。单测锁字段名——写错一个就是静默不计分。
func DailyBonusBeaconEvent(credential *Credential) map[string]any {
	now := nowMs()
	conversationID := "wb-daily-" + randomHexForExport(8)
	return map[string]any{
		"eventCode":            "chat_request_send",
		"timestamp":            now,
		"mode":                 "craft",
		"conversationId":       conversationID,
		"requestId":            conversationID,
		"inputLength":          12,
		"requestModelId":       "deepseek-v4-flash",
		"requestModelName":     "DeepSeek V4 Flash",
		"mentionContexts":      []any{},
		"knowledgeId":          []any{},
		"knowledgeName":        []any{},
		"presentAt":            now,
		"rootRequestId":        conversationID,
		"parentConversationId": conversationID,
		"agentName":            "default",
		"agentType":            "conversation",
		"userId":               credential.UIDValue(),
	}
}

// SendDailyBonusBeacon 上报一条每日活跃信标（计费域 /v2/report）。
//
// 刻意走单次上报出口（postReport 无重试）：理由见文件头设计要点。
func (c *Client) SendDailyBonusBeacon(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is required")
	}
	return c.ReportBillingEvents(ctx, cred, []map[string]any{DailyBonusBeaconEvent(cred)})
}

// GlobalDailyBonus 执行一次国际版每日活跃奖励（幂等）。
//
// 返回的 CheckinResult 语义：
//   - Already=true：今天已点亮（本轮或早前轮次点亮的都算），无需再上报；
//   - Already=false：本轮新上报了信标。积分在次日凌晨发放，因此 Credit/Energy
//     此刻拿不到，恒为 0；Streak 携带热力图上的点亮天数供展示。
//
// 仅接受 global 域凭证：CN 的成长域在 copilot.tencent.com，收入路径是
// DailyCheckin，两者互不通用。
func (c *Client) GlobalDailyBonus(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !region.IsGlobal() {
		return nil, fmt.Errorf("daily bonus is only available for global-realm accounts")
	}
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	today, lit, errToday := c.GrowthHeatmapToday(ctx, cred)
	if errToday != nil {
		return nil, fmt.Errorf("heatmap: %w", errToday)
	}
	if today.Score > 0 {
		return &CheckinResult{Already: true, Streak: int64(len(lit))}, nil
	}
	if errSend := c.SendDailyBonusBeacon(ctx, cred); errSend != nil {
		return nil, fmt.Errorf("report: %w", errSend)
	}
	// 热力图聚合滞后数秒，立即回读可能仍是 0；尽力而为，仅用于日志展示，
	// 当天状态以本条上报为准（下个排程点会看到点亮）。
	if _, litAfter, errRe := c.GrowthHeatmapToday(ctx, cred); errRe == nil {
		lit = litAfter
	}
	return &CheckinResult{Already: false, Streak: int64(len(lit))}, nil
}
