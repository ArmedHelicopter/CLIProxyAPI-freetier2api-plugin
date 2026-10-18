package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/state"
)

// quotaResult 是单个账号的额度结果。
type quotaResult struct {
	AuthID     string                  `json:"auth_id"`
	AuthIndex  string                  `json:"auth_index,omitempty"`
	Name       string                  `json:"name,omitempty"`
	UID        string                  `json:"uid,omitempty"`
	Label      string                  `json:"label"`
	VendorID   string                  `json:"vendor_id,omitempty"`
	VendorName string                  `json:"vendor_name,omitempty"`
	Realm      string                  `json:"realm"`
	OK         bool                    `json:"ok"`
	Message    string                  `json:"message,omitempty"`
	Summary    string                  `json:"summary,omitempty"`
	Remain     int64                   `json:"remain"`
	Total      int64                   `json:"total"`
	Metrics    []pluginapi.QuotaMetric `json:"metrics,omitempty"`
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
	type job struct {
		entry      core.HostAuth
		vendor     core.Vendor
		credential *core.Credential
	}
	var preResults []quotaResult
	for _, failure := range failures {
		preResults = append(preResults, quotaResult{
			AuthID:  core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:   core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			OK:      false,
			Message: failure.Message,
		})
	}
	jobs := make([]job, 0, len(loaded))
	for _, account := range loaded {
		jobs = append(jobs, job{entry: account.Entry, vendor: account.Vendor, credential: account.Credential})
	}
	// 领取状态跟着额度一起刷新，但**批末才落盘**：Mutate 每次都会写一遍
	// state.json，逐账号写等于把一次「查询额度」放大成 N 次磁盘写。
	claims := map[string]state.ClaimRecord{}
	jobResults := accountpkg.RunBatch(jobs, batchConcurrency, func(current job) quotaResult {
		result := quotaResult{
			AuthID:     core.FirstNonEmpty(current.entry.ID, current.entry.Name),
			AuthIndex:  current.entry.AuthIndex,
			Name:       current.entry.Name,
			UID:        current.credential.FileIDValue(),
			Label:      core.FirstNonEmpty(current.credential.LabelValue(), current.entry.Label, current.entry.Name),
			VendorID:   current.vendor.ID(),
			VendorName: current.vendor.Name(),
			Realm:      current.credential.RegionValue(),
		}
		quotaProvider, okQuota := core.As[core.QuotaProvider](current.vendor)
		if !okQuota {
			result.Message = core.QuotaUnsupported(current.vendor).Error()
			return result
		}
		quota, errQuota := quotaProvider.Quota(ctx, current.credential)
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
		result.Remain, result.Total = accountpkg.Totals(quota)
		result.Metrics = quota.Summary
		s.cacheQuotaResult(result, result.UID, result.AuthID, result.AuthIndex, result.Name, result.Label)
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
