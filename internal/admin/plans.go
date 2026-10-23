package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	authpkg "freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件是套餐领取的两个管理端点：打开弹窗（实时预览）与提交领取。
//
// **按钮状态不在这里算**：它搭额度响应回来——额度查询本来就打 billing/current，
// 顺手看一眼 preview 就能回答「有没有可领的」，由 handleQuotasRequest 统一落进
// state（见 quota.go）。控制台每次进页面都会刷一次额度，于是状态自动保持新鲜，
// 不需要额外的轮询端点，也不需要任何缓存与失效逻辑。
//
// 供应商侧由 core.PlanClaimer 承接——本层不 import 任何供应商包（分层由
// tests/architecture_test.go 守住）。

// planPreviewRequest 是打开领取弹窗的请求体。
type planPreviewRequest struct {
	AccountID string `json:"account_id"`
}

// planClaimRequest 是提交领取的请求体。
type planClaimRequest struct {
	AccountID    string `json:"account_id"`
	PlanID       string `json:"plan_id"`
	VerifyParam  string `json:"verify_param"`
	VerifyRegion string `json:"verify_region,omitempty"`
}

// planTarget 是一个已定位的领取目标：账号的套餐能力与对应凭证。
type planTarget struct {
	Claimer    core.PlanClaimer
	Credential *core.Credential
	AuthID     string
}

// resolvePlanTarget 解析请求里的账号并取出其套餐领取能力。
//
// 预览与领取共用这段定位逻辑：两者都是「按账号 ID 找凭证 → 断言能力」，
// 只有随后的动作不同。失败原因以字符串返回（面向页面提示），空串表示成功。
func (s *Service) resolvePlanTarget(ctx context.Context, callbackID, accountID string) (planTarget, string) {
	identifier := strings.TrimSpace(accountID)
	if identifier == "" {
		return planTarget{}, "缺少 account_id"
	}
	loaded, failures := s.pool.Load(ctx, callbackID, authpkg.Options{
		Targets:       []string{identifier},
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	if len(loaded) == 0 {
		if len(failures) > 0 {
			return planTarget{}, failures[0].Message
		}
		return planTarget{}, "账号不存在或已被删除"
	}
	account := loaded[0]
	claimer, okClaimer := core.As[core.PlanClaimer](account.Vendor)
	if !okClaimer {
		return planTarget{}, "该供应商不提供套餐领取"
	}
	return planTarget{
		Claimer:    claimer,
		Credential: account.Credential,
		AuthID:     core.FirstNonEmpty(account.Entry.ID, account.Entry.Name),
	}, ""
}

// handlePlansPreview 返回账号**实时**可领套餐与人机验证控件参数。
//
// 不读任何缓存：这是用户点开弹窗那一刻的真实状态，复用缓存会让刚领完的用户
// 仍看到旧列表。
func (s *Service) handlePlansPreview(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body planPreviewRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	target, reason := s.resolvePlanTarget(ctx, hostbridge.RequestCallbackID(req), body.AccountID)
	if reason != "" {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": reason})
	}

	plans, errPreview := target.Claimer.PreviewPlans(ctx, target.Credential)
	if errPreview != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": errPreview.Error()})
	}
	if plans == nil {
		plans = []core.PlanInfo{}
	}

	payload := map[string]any{"ok": true, "plans": plans}
	if len(plans) > 0 {
		// 只有真能领才返回控件参数：没得领时不必让页面加载验证码脚本。
		if config, errConfig := target.Claimer.CaptchaConfig(ctx); errConfig == nil {
			payload["captcha"] = config
		}
	}
	return JSONResponse(http.StatusOK, payload)
}

// handlePlansClaim 提交人机验证令牌并领取套餐。
func (s *Service) handlePlansClaim(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body planClaimRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	target, reason := s.resolvePlanTarget(ctx, hostbridge.RequestCallbackID(req), body.AccountID)
	if reason != "" {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": reason})
	}

	result, errClaim := target.Claimer.ClaimPlan(ctx, target.Credential, core.ClaimPlanRequest{
		PlanID:       strings.TrimSpace(body.PlanID),
		VerifyParam:  strings.TrimSpace(body.VerifyParam),
		VerifyRegion: strings.TrimSpace(body.VerifyRegion),
	})
	if errClaim != nil {
		// 业务失败（已领过/名额用完/验证码失败）都带着可读原因，用200 回给页面弹提示。
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "message": errClaim.Error()})
	}

	// 新额度与「已领取」状态都走同一条额度管线，直接复用它而不是另写一份查询+落盘。
	s.refreshAccountQuota(req, target.AuthID)

	return JSONResponse(http.StatusOK, map[string]any{
		"ok":        true,
		"message":   "领取成功：" + result.PlanName,
		"plan_id":   result.PlanID,
		"plan_name": result.PlanName,
		"starts_at": result.StartsAt,
		"ends_at":   result.EndsAt,
	})
}

// refreshAccountQuota 借用额度管线刷新单个账号。
//
// 领取成功后需要做两件事：刷新余量、把按钮置灰成「已领取」。这两件事本来就在
// 额度管线里（余额来自 billing/balance，领取状态来自 billing/preview），复用它
// 就够了——单独再写一份查询只会把同一套逻辑抄第三遍。
func (s *Service) refreshAccountQuota(req pluginapi.ManagementRequest, authID string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	body, errMarshal := json.Marshal(accountBatchRequest{AccountIDs: []string{authID}})
	if errMarshal != nil {
		return
	}
	s.handleQuotasRequest(pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    req.Path,
		Headers: req.Headers,
		Body:    body,
	})
}
