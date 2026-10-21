package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/state"
)

// quotaJob 是额度查询里的一个账号任务。
type quotaJob struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
}

// quotaResult 是单个账号的额度结果。
type quotaResult struct {
	AuthID     string `json:"auth_id"`
	AuthIndex  string `json:"auth_index,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	Label      string `json:"label"`
	VendorID   string `json:"vendor_id,omitempty"`
	VendorName string `json:"vendor_name,omitempty"`
	Realm      string `json:"realm"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Remain     int64  `json:"remain"`
	Total      int64  `json:"total"`
	// Unlimited 报告剩余额度无上限，展示层据此显示「无限」。
	Unlimited bool                    `json:"unlimited,omitempty"`
	Metrics   []pluginapi.QuotaMetric `json:"metrics,omitempty"`
}

func (s *Service) handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       body.AccountIDs,
		EnabledRealms: cfg.EnabledRealms,
	})
	var preResults []quotaResult
	for _, failure := range failures {
		preResults = append(preResults, quotaResult{
			AuthID:  core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:   core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			OK:      false,
			Message: failure.Message,
		})
	}
	jobs := make([]quotaJob, 0, len(loaded))
	for _, account := range loaded {
		jobs = append(jobs, quotaJob{entry: account.Entry, vendor: account.Vendor, credential: account.Credential})
	}
	// 领取状态跟着额度一起刷新，但**批末才落盘**：Mutate 每次都会写一遍
	// state.json，逐账号写等于把一次「查询额度」放大成 N 次磁盘写。
	claims := map[string]state.ClaimRecord{}
	jobResults := accountpkg.RunBatch(jobs, batchConcurrency, func(current quotaJob) (result quotaResult) {
		result = quotaResult{
			AuthID:     core.FirstNonEmpty(current.entry.ID, current.entry.Name),
			AuthIndex:  current.entry.AuthIndex,
			Name:       current.entry.Name,
			UID:        current.credential.FileIDValue(),
			Label:      core.FirstNonEmpty(current.credential.LabelValue(), current.entry.Label, current.entry.Name),
			VendorID:   current.vendor.ID(),
			VendorName: current.vendor.Name(),
			Realm:      current.credential.RegionValue(),
		}
		// **成功与失败都要缓存**：缓存是账号表唯一的数据源。只缓存成功结果会让
		// 失败账号在下次 /status 时退回「没有结果」，页面于是永远停在「查询中…」——
		// 而「上游没有额度接口」这类结论早已确定，不存在还会变。
		defer func() {
			s.cacheQuotaResult(result, result.UID, result.AuthID, result.AuthIndex, result.Name, result.Label)
		}()
		quotaProvider, okQuota := core.As[core.QuotaProvider](current.vendor)
		if !okQuota {
			result.Message = core.QuotaUnsupported(current.vendor).Error()
			return result
		}
		quota, errQuota := quotaProvider.Quota(ctx, current.credential)
		if errQuota != nil && isAuthExpired(errQuota) {
			// 凭证过期类错误：先保活刷新一次再重试。MiniMax 的 access token 只有
			// 1 小时有效期，不刷新的话额度列会一直挂着 401，用户只能自己想到去点
			// 「一键保活」——那条路还要求他先认出这是凭证问题。
			if refreshed := s.refreshedCredential(ctx, callbackID, current); refreshed != nil {
				quota, errQuota = quotaProvider.Quota(ctx, refreshed)
			}
		}
		if errQuota != nil {
			result.Message = errQuota.Error()
			return result
		}
		if quota.Claim != nil {
			claims[result.UID] = state.ClaimRecord{
				ClaimStatus: *quota.Claim,
				CheckedAt:   time.Now().Format(time.RFC3339),
			}
		}
		result.OK = true
		result.Summary = accountpkg.Summary(quota)
		totals := accountpkg.Totals(quota)
		result.Remain, result.Total, result.Unlimited = totals.Remaining, totals.Total, totals.Unlimited
		result.Metrics = quota.Summary
		return result
	})
	if len(claims) > 0 {
		s.deps.MutateState(func(current *state.State) {
			for uid, record := range claims {
				current.Claim[uid] = record
			}
		})
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": append(preResults, jobResults...)})
}

// isAuthExpired 判断错误是否属于「凭证已过期/失效」类（401）。
//
// 只认 401：403/404 往往意味着账号没这个权限或上游改版，刷新不会变好，
// 盲目刷新只会白白多打一次上游。
func isAuthExpired(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindCredential || failure.HTTPStatus() == http.StatusUnauthorized
	}
	var pluginErr *core.PluginError
	if errors.As(err, &pluginErr) {
		return pluginErr.HTTPStatus == http.StatusUnauthorized
	}
	return false
}

// refreshedCredential 对单个账号跑一次保活，并返回刷新后的凭证。
//
// 复用保活那条既有通道（它负责刷新 + 写盘 + 热更新宿主），额度查询因此不需要
// 自己再造一套刷新逻辑。保活失败或账号消失时返回 nil，由调用方按原错误上报。
func (s *Service) refreshedCredential(ctx context.Context, callbackID string, target quotaJob) *core.Credential {
	authID := core.FirstNonEmpty(target.entry.ID, target.entry.Name)
	if authID == "" {
		return nil
	}
	s.tasks.Keepalive(ctx, callbackID, []string{authID}, 1)

	loaded, _ := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       []string{authID},
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	for _, account := range loaded {
		if account.Vendor.ID() == target.vendor.ID() {
			return account.Credential
		}
	}
	return nil
}
