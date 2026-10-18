package admin

import (
	"encoding/json"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/core"
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
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	cfg := s.deps.Config()
	accounts, failures := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:      body.AccountIDs,
		RealmEnabled: func(region string) bool { return realmEnabled(cfg, region) },
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
	jobs := make([]job, 0, len(accounts))
	for _, account := range accounts {
		jobs = append(jobs, job{entry: account.Entry, vendor: account.Vendor, credential: account.Credential})
	}
	jobResults := make([]quotaResult, len(jobs))
	semaphore := make(chan struct{}, batchConcurrency)
	done := make(chan int, len(jobs))
	for index := range jobs {
		go func(slot int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			current := jobs[slot]
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
				jobResults[slot] = result
				done <- slot
				return
			}
			quota, errQuota := quotaProvider.Quota(ctx, current.credential)
			if errQuota != nil {
				result.Message = errQuota.Error()
			} else {
				result.OK = true
				result.Summary = accountpkg.Summary(quota)
				result.Remain, result.Total = accountpkg.Totals(quota)
				result.Metrics = quota.Summary
				cacheQuotaResult(result, result.UID, result.AuthID, result.AuthIndex, result.Name, result.Label)
			}
			jobResults[slot] = result
			done <- slot
		}(index)
	}
	for range jobs {
		<-done
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": append(preResults, jobResults...)})
}
