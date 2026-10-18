package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
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

func handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": false, "results": []any{},
			"error": "读取账号列表失败：" + errList.Error(),
		})
	}
	wanted := make(map[string]bool, len(body.AccountIDs))
	for _, target := range body.AccountIDs {
		wanted[strings.TrimSpace(target)] = true
	}

	type job struct {
		entry      core.HostAuth
		vendor     core.Vendor
		credential *core.Credential
	}
	var preResults []quotaResult
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		var raw []byte
		if entry.AuthIndex != "" {
			if r, ok := hostabi.AuthJSON(ctx, callbackID, entry.AuthIndex); ok {
				raw = r
			}
		}
		if len(raw) == 0 && entry.Path != "" {
			if r, err := os.ReadFile(entry.Path); err == nil && len(r) > 0 {
				raw = r
			}
		}
		if len(raw) == 0 && entry.Name != "" {
			for _, dir := range []string{"/root/.cli-proxy-api", "./auths", "."} {
				candidate := filepath.Join(dir, entry.Name)
				if r, err := os.ReadFile(candidate); err == nil && len(r) > 0 {
					raw = r
					break
				}
			}
		}
		if len(raw) == 0 {
			targetID := core.FirstNonEmpty(entry.AuthIndex, entry.ID, entry.Name)
			if r, err := hostabi.FetchAuthJSON(ctx, callbackID, targetID); err == nil && len(r) > 0 {
				raw = r
			}
		}
		if len(raw) == 0 {
			preResults = append(preResults, quotaResult{
				AuthID: core.FirstNonEmpty(entry.ID, entry.Name), Label: core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
				OK: false, Message: "凭证读取失败",
			})
			continue
		}
		credential, vendor, errResolve := deps.ResolveCredential(ctx, callbackID, raw, hostabi.AccountIdentity(entry), nil)
		if errResolve != nil {
			preResults = append(preResults, quotaResult{
				AuthID: core.FirstNonEmpty(entry.ID, entry.Name), Label: core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
				OK: false, Message: "凭证解析失败: " + errResolve.Error(),
			})
			continue
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential})
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
				result.Summary = summarizeQuotaMetrics(quota)
				result.Remain, result.Total = normalizeQuotaNumbers(quota)
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

func summarizeQuotaMetrics(quota *pluginapi.QuotaFetchResponse) string {
	if quota == nil {
		return ""
	}
	parts := make([]string, 0, len(quota.Summary)+2)
	for _, metric := range quota.Summary {
		label := strings.TrimSpace(metric.Label)
		if label == "" {
			label = metric.Key
		}
		parts = append(parts, fmt.Sprintf("%s %s", label, formatMetricValue(metric.Value)))
	}
	for _, group := range quota.Groups {
		for _, bucket := range group.Buckets {
			label := strings.TrimSpace(group.DisplayName)
			if label == "" {
				label = bucket.Window
			}
			parts = append(parts, fmt.Sprintf("%s %s", label, bucket.Description))
		}
	}
	return strings.Join(parts, " · ")
}

func formatMetricValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// normalizeQuotaNumbers 从归一的额度响应里折算「剩余 / 总额」两个数字。
//
// 各家的额度口径不同（WorkBuddy 是积分余额，Qoder 是套餐用量桶），
// 因此统一在这里按已知指标键折算，页面不必理解任何一家指标键的语义。
func normalizeQuotaNumbers(quota *pluginapi.QuotaFetchResponse) (remain, total int64) {
	if quota == nil {
		return 0, 0
	}
	for _, metric := range quota.Summary {
		switch metric.Key {
		case "credit_remain", "user_quota_remaining", "addon_quota_remaining", "remaining":
			if remain == 0 {
				remain = int64(metric.Value)
			}
		case "credit_total", "user_quota_total", "addon_quota_total", "total":
			if total == 0 {
				total = int64(metric.Value)
			}
		}
	}
	if total == 0 {
		var used int64
		for _, metric := range quota.Summary {
			switch metric.Key {
			case "credit_used", "user_quota_used", "addon_quota_used", "used":
				used += int64(metric.Value)
			}
		}
		if used > 0 {
			total = remain + used
		}
	}
	return remain, total
}
