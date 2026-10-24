package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 营销触达（marketing/touch）：ZCode 的投放通道。
//
// 与计费族（billing/*）是两条独立链路：营销通道只读、不参与领取，且对 API Key
// 账号同样开放（billing 只服务 JWT 的 Coding Plan）。投放以「当日活跃」为资格
// 信号，事件上报由调用方在探测前完成（见 activation.go）。

const pathMarketingTouch = "/api/v1/marketing/touch"

// marketingHeaders 复用上游身份与鉴权头；本接口是只读 GET，只额外声明 Accept。
func marketingHeaders(cred *Credential) map[string]string {
	headers := upstreamAuthHeaders(cred)
	headers["Accept"] = "application/json"
	return headers
}

// queryMarketingTouch 拉取账号当前可见的活动投放。
func queryMarketingTouch(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	u, errParse := url.Parse(ZCodeOrigin + pathMarketingTouch)
	if errParse != nil {
		return nil, errParse
	}
	// seq=0 取首屏。上游没有公开游标语义，这里不做猜测性翻页：多翻一轮会把
	// 同一批投放重复拉一遍，反而放大请求量。
	u.RawQuery = "seq=0"

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if errNew != nil {
		return nil, errNew
	}
	for key, value := range marketingHeaders(cred) {
		req.Header.Set(key, value)
	}

	resp, errDo := hostbridge.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	return parseMarketingTouch(resp.StatusCode, raw)
}

// parseMarketingTouch 解析营销触达响应。
//
// 只认 data.deliveries[].banner.background.args.zcode_plan：活动套餐塞在 banner
// 的渲染参数里，其余投递（纯文案/图片）没有 plan_id，会被 normalizePlans 丢掉。
//
// 不按 starts_at/ends_at 二次过滤：上游已按当前时间筛选投放，本机时钟漂移只会
// 误伤。这里返回的是「上游当前投放」，不是「本账号此刻可领」。
func parseMarketingTouch(status int, raw []byte) ([]core.PlanInfo, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, core.NewPluginError("zcode_activity_response",
			"营销活动响应不是合法 JSON: "+truncateText(string(raw), 120), http.StatusBadGateway)
	}
	if status < 200 || status >= 300 {
		return nil, core.NewPluginError("zcode_activity_http",
			fmt.Sprintf("营销活动查询 HTTP %d: %s", status, truncateText(string(raw), 120)), http.StatusBadGateway)
	}
	// 2xx 且没有业务码时按成功处理：网关可能剥掉信封，与激活上报同一约定。
	if code := businessCode(payload); code != 0 && code != -1 {
		message := core.FirstNonEmpty(core.RawStringField(payload, "msg"), "上游未给出原因")
		return nil, core.NewPluginError("zcode_activity_business",
			fmt.Sprintf("营销活动查询业务码 %d: %s", code, message), http.StatusBadGateway)
	}

	deliveries := objectRows(dataMap(payload)["deliveries"])
	plans := make([]map[string]any, 0, len(deliveries))
	for _, delivery := range deliveries {
		if plan := nestedMap(delivery, "banner", "background", "args", "zcode_plan"); plan != nil {
			plans = append(plans, plan)
		}
	}
	return normalizePlans(plans), nil
}

// nestedMap 沿字段路径逐层取对象，任一层缺失或不是对象时返回 nil。
func nestedMap(raw map[string]any, path ...string) map[string]any {
	for _, key := range path {
		if raw, _ = raw[key].(map[string]any); raw == nil {
			return nil
		}
	}
	return raw
}
