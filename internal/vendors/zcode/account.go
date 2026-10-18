package zcode

// 计费族（billing/*、usage、event/report）的公共请求层。
//
// 这些端点都在 zcode.z.ai 的 zcode-plan 之下，共用同一套身份头，且都**必须**
// 带 X-Device-Mid——缺失时上游返回 code=3001。鉴权用 Coding Plan JWT
// （API Key 通道没有套餐概念，不参与计费族）。
//
// 参考 zcode2api/app/quota.py 的 _auth_headers 与 app/claim.py 的 _billing_request。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"

	"freetier2api-plugin/cpasdk/pluginapi"
)

const (
	// billingHTTPTimeout 是计费族单次请求的上限。
	billingHTTPTimeout = 20 * time.Second
	// billingMaxBody 是计费响应的读取上限。
	billingMaxBody = 4 << 20
)

// billingHeaders 构造计费族请求头。
//
// 平台/语言/时区/设备 ID 全部取账号档案：上游按设备聚类做风控，
// 全局伪装值会让所有账号聚成同一台机器。
func billingHeaders(cred *Credential) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   UserAgent,
	}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	headers["Authorization"] = "Bearer " + cred.JWTToken
	headers["x-request-id"] = NewUUID()
	return headers
}

// billingRequest 发起一次计费族请求并解析业务码。
//
// 返回业务码与响应体（解析失败时业务码为 -1）。网络与鉴权失败统一返回
// 分类后的上游错误，调用方不必各自判断状态码。
func billingRequest(ctx context.Context, cred *Credential, method, path string, payload any) (int, map[string]any, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return 0, nil, errMarshal
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, errNew := http.NewRequestWithContext(ctx, method, BillingBase+path, bodyReader)
	if errNew != nil {
		return 0, nil, errNew
	}
	for key, value := range billingHeaders(cred) {
		req.Header.Set(key, value)
	}

	resp, errDo := httpx.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, transientErr("计费请求失败: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return 0, nil, transientErr("读取计费响应失败: " + errRead.Error())
	}

	// 非 2xx 一律交给 Classify：它内部已经处理了「403 先判验证码挑战、再判凭证失效」
	// 的顺序（见 errors.go），此处不重复那套判定。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil, Classify(resp.StatusCode, string(raw))
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return -1, nil, nil
	}
	return businessCode(parsed), parsed, nil
}

// businessCode 取上游业务码；缺失或非数字返回 -1（视为失败）。
func businessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	switch v := body["code"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if out, errParse := strconv.Atoi(strings.TrimSpace(v)); errParse == nil {
			return out
		}
	}
	return -1
}

// dataMap 取响应体的 data 对象。
func dataMap(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	data, _ := body["data"].(map[string]any)
	return data
}

// billingBlockReason 报告该凭证为何不能打计费族；可打时返回空串。
//
// 计费族只服务 Coding Plan（JWT）。纯 API Key 账号没有套餐，不该产生
// billing 流量——既拿不到数据，又会白增一条上游风控信号。
func billingBlockReason(cred *Credential) string {
	if cred == nil {
		return "凭证缺失"
	}
	if !cred.UsesPlanChannel() {
		return "该账号没有 Coding Plan 凭证（JWT），不参与套餐与额度查询"
	}
	return ""
}

// classifyBillingOutcome 把计费族的业务失败码翻成可读文案。
func classifyBillingOutcome(code int, body map[string]any) string {
	serverMsg := core.FirstNonEmpty(stringField(body, "msg"), stringField(body, "message"))
	base := map[int]string{
		1001: "套餐不存在",
		1002: "活动已结束或套餐暂不可领取",
		1003: "该套餐已经领取过",
		1004: "不符合领取条件",
		1005: "今日领取名额已用完",
		3001: "请求参数不全（缺少设备标识）",
		3007: "验证码校验失败",
		401:  "请先登录后再领取",
	}[code]
	if base == "" {
		base = "上游返回业务错误"
	}
	if serverMsg != "" {
		return base + "（" + serverMsg + "）"
	}
	return base
}

// 额度查询：billing/current（套餐）+ billing/balance（余量）。
//
// 只对 Coding Plan（JWT）账号有意义——API Key 是用户自带的按量付费 Key，
// 没有套餐窗口。参考 zcode2api/app/quota.py。

// FetchQuota 查询 ZCode 账号的套餐与余量。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, transientErr("zcode credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "ZCode Coding Plan",
			TierName: "Standard",
			TierID:   "zcode",
		},
	}

	if reason := billingBlockReason(cred); reason != "" {
		// 非 Coding Plan 账号：如实说明，不伪装成「查询成功但无数据」。
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "status", Label: "账号状态", Value: 1, Unit: reason,
		})
		return response, nil
	}

	fillSubscription(ctx, cred, response)
	metrics, errBalance := fetchBalances(ctx, cred)
	if errBalance != nil {
		return nil, errBalance
	}
	response.Summary = append(response.Summary, metrics...)
	return response, nil
}

// fillSubscription 用 billing/current 补充套餐名（失败时保留默认值）。
func fillSubscription(ctx context.Context, cred *Credential, response *pluginapi.QuotaFetchResponse) {
	code, body, errRequest := billingRequest(ctx, cred, "GET", PathBillingCurrent, nil)
	if errRequest != nil || code != codeSuccess {
		return
	}
	plans, _ := dataMap(body)["plans"].([]any)
	if len(plans) == 0 {
		return
	}
	first, ok := plans[0].(map[string]any)
	if !ok {
		return
	}
	if name := stringField(first, "name"); name != "" {
		response.Subscription.Plan = name
	}
	if planID := core.FirstNonEmpty(stringField(first, "plan_id"), stringField(first, "planId")); planID != "" {
		response.Subscription.TierName = planID
	}
}

// fetchBalances 读取各模型余量并转成指标。
func fetchBalances(ctx context.Context, cred *Credential) ([]pluginapi.QuotaMetric, error) {
	code, body, errRequest := billingRequest(ctx, cred, "GET", PathBillingBalance, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	if code != codeSuccess {
		return nil, Classify(0, classifyBillingOutcome(code, body))
	}

	balances, _ := dataMap(body)["balances"].([]any)
	if len(balances) == 0 {
		return nil, nil
	}

	metrics := make([]pluginapi.QuotaMetric, 0, len(balances))
	for _, item := range balances {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := core.FirstNonEmpty(stringField(entry, "show_name"), stringField(entry, "model"))
		if name == "" {
			continue
		}
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key:   "remaining_" + sanitizeMetricKey(name),
			Label: name + " 剩余",
			Value: floatField(entry, "remaining_units"),
			Unit:  "tokens",
		})
	}
	return metrics, nil
}

// sanitizeMetricKey 把模型名转成稳定的指标键。
func sanitizeMetricKey(name string) string {
	replacer := strings.NewReplacer(" ", "_", ".", "_", "/", "_", "-", "_")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(name)))
}

// floatField 宽松取浮点字段（JSON 数字一律 float64）。
func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}
