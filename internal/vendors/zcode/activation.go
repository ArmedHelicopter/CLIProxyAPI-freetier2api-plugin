package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 激活事件上报：让上游把本账号当作「今日活跃的官方客户端」。
//
// 为什么需要它：billing/preview 的返回**不是**纯粹的账号权益快照，活动套餐的投放
// 以「当日活跃」为资格信号。官方桌面端每次启动发 app_launch、每个自然日发一次
// app_daily_active（packages/services/src/telemetry/telemetryCore.ts 的
// reportAppLaunch / reportAppDailyActive）。不发这两条事件的账号，preview 恒空——
// 于是插件算出的可领数为 0，控制台把按钮永久灰成「已领取」，而账号其实一个套餐
// 都没领。这与领取接口本身无关：有活动时手动领取是能成功的。
//
// 事件体严格对齐官方 sendReport 的 16 个字段；端点不在 zcode-plan 之下，也不带
// Authorization（官方该端点同样匿名上报）。

// ActivationElements 是官方在启动/日活时上报的两个元素名。
var ActivationElements = []string{"app_launch", "app_daily_active"}

// activationTimeout 是单条埋点的上限：埋点失败不得拖住额度查询。
const activationTimeout = 10 * time.Second

// activationState 是「每个账号每个自然日只上报一次」的进程内去重表。
//
// 状态属于实例而不是落盘：多 App 并存时各自独立（与 claimGate 同一约定）。
// 进程重启后多报一轮无副作用——上游本身按 device_mid + 日期去重。
type activationState struct {
	mu   sync.Mutex
	last map[string]string // uid → 本地日期（账号档案时区）
}

func newActivationState() *activationState {
	return &activationState{last: map[string]string{}}
}

// shouldReport 报告本次是否要上报，并在需要时先占位（避免并发重复上报）。
func (s *activationState) shouldReport(uid string, today string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last[uid] == today {
		return false
	}
	s.last[uid] = today
	return true
}

// forget 撤销占位：上报失败时允许下一次重试。
func (s *activationState) forget(uid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.last, uid)
	s.mu.Unlock()
}

// localDate 返回账号档案时区下的自然日（YYYY-MM-DD）。
//
// 时区取不到时退回 UTC：去重粒度粗一点没有副作用，只是可能多报一次。
func localDate(profile DeviceProfile, now time.Time) string {
	zone := strings.TrimSpace(profile.Timezone)
	if zone != "" {
		if location, errLoad := time.LoadLocation(zone); errLoad == nil {
			return now.In(location).Format("2006-01-02")
		}
	}
	return now.UTC().Format("2006-01-02")
}

// activationEventBody 构造官方 sendReport 的 16 字段事件体。
func activationEventBody(element string, cred *Credential, userID string) map[string]any {
	profile := cred.Profile
	profile.ensure()
	return map[string]any{
		"event_id":           NewUUID(),
		"client_timezone":    profile.Timezone,
		"client_language":    profile.Language,
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  profile.Screen,
		"app_version":        ClientAppVersion,
		"device_os_category": profile.OSCategory(),
		"device_os_version":  profile.OSVersion,
		"device_mid":         profile.DeviceMID,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// postActivationEvent 上报单条激活事件。
//
// 走 hostbridge.Client 与其他上游请求同一出口；不带 Authorization，只带官方
// 来源头（身份头里与本次上报相关的字段已在事件体里，头本身用于通过网关）。
func postActivationEvent(ctx context.Context, cred *Credential, body map[string]any) error {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return errMarshal
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost,
		ZCodeOrigin+PathEventReport, bytes.NewReader(encoded))
	if errNew != nil {
		return errNew
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("HTTP-Referer", ZCodeOrigin)
	req.Header.Set("X-Title", ClientTitle)

	resp, errDo := hostbridge.Client(ctx, activationTimeout).Do(req)
	if errDo != nil {
		return fmt.Errorf("激活上报失败: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("激活上报 HTTP %d: %s", resp.StatusCode, truncateText(string(raw), 120))
	}
	// 业务码非 0 视为失败；响应非 JSON 时按成功处理（网关可能回空体）。
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal == nil {
		if code := businessCode(parsed); code != -1 && code != 0 {
			return fmt.Errorf("激活上报业务码 %d: %s", code, truncateText(string(raw), 120))
		}
	}
	return nil
}

// ensureActivation 保证本账号今天已向平台报过活跃，返回失败原因（空串表示无需再报
// 或已成功）。
//
// 失败**不阻断**后续 preview：官方客户端同样不因埋点失败停业务，这里把原因交给
// 调用方决定是展示还是忽略。
func (v *adapter) ensureActivation(ctx context.Context, cred *Credential) string {
	if cred == nil {
		return ""
	}
	userID := strings.TrimSpace(cred.UserID)
	if userID == "" {
		userID = UserIDFromJWT(cred.JWTToken)
	}
	if userID == "" {
		// 无用户身份无法上报（官方事件体以 user_id 归属）；如实说明而不是静默跳过，
		// 否则用户会继续看到一个永远空的活动列表。
		return "凭证缺少 user_id，无法上报活跃事件（活动投放可能不完整；可重新登录或补充 user_id）"
	}

	today := localDate(cred.Profile, time.Now())
	if !v.activations.shouldReport(userID, today) {
		return ""
	}

	for _, element := range ActivationElements {
		if err := postActivationEvent(ctx, cred, activationEventBody(element, cred, userID)); err != nil {
			v.activations.forget(userID)
			logger.Info("zcode 激活上报失败 (%s): %v", element, err)
			return err.Error()
		}
	}
	return ""
}

// truncateText 截断过长的上游响应片段，避免把整页 HTML 塞进错误消息。
func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
