package admin

import (
	"fmt"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
)

type keepaliveResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

func (s *Service) handleKeepaliveRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostabi.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostabi.RequestCallbackID(req)

	cfg := s.deps.Config()
	loaded, _ := s.pool.Load(ctx, callbackID, accounts.Options{
		RealmEnabled: func(region string) bool { return core.RealmEnabled(cfg.EnabledRealms, region) },
	})
	type job struct {
		entry      core.HostAuth
		vendor     core.Vendor
		credential *core.Credential
		rawJSON    []byte
		filePath   string
	}
	jobs := make([]job, 0, len(loaded))
	for _, account := range loaded {
		if account.FilePath != "" {
			account.Credential.SetFilePath(account.FilePath)
		}
		jobs = append(jobs, job{
			entry: account.Entry, vendor: account.Vendor, credential: account.Credential,
			rawJSON: account.RawJSON, filePath: account.FilePath,
		})
	}
	if len(jobs) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": true, "message": "没有需要续期的可用账号", "succeeded": 0, "failed": 0, "results": []any{},
		})
	}

	results := accounts.RunBatch(jobs, batchConcurrency, func(j job) keepaliveResult {
		label := core.FirstNonEmpty(j.credential.LabelValue(), j.entry.Label, j.entry.Name)
		result := keepaliveResult{
			AuthID: core.FirstNonEmpty(j.entry.ID, j.entry.Name), Label: label,
			VendorID: j.vendor.ID(), Realm: j.credential.RegionValue(),
		}
		updated, refreshed, errRefresh := core.RefreshVendor(j.vendor, ctx, j.credential)
		if errRefresh != nil {
			result.Message = errRefresh.Error()
			logger.Error("keepalive %s: %v", label, errRefresh)
			return result
		}
		result.OK = true
		if refreshed {
			result.Message = "Token 续期成功"
			s.deps.SaveRefreshedCredential(ctx, callbackID, j.vendor, j.entry, j.rawJSON, updated, j.filePath)
		} else {
			result.Message = "凭证有效（未变更）"
		}
		logger.Info("keepalive %s: refreshed=%t", label, refreshed)
		return result
	})
	succeeded, failed := 0, 0
	firstErrMsg := ""
	for _, result := range results {
		if result.OK {
			succeeded++
			continue
		}
		failed++
		if firstErrMsg == "" {
			firstErrMsg = result.Label + ": " + result.Message
		}
	}
	message := ""
	switch {
	case failed > 0 && succeeded == 0:
		message = fmt.Sprintf("全部账号续期失败（%s）", firstErrMsg)
	case failed > 0:
		message = fmt.Sprintf("续期完成：%d 个成功，%d 个失败（%s）", succeeded, failed, firstErrMsg)
	default:
		message = fmt.Sprintf("全账号 Token 续期成功（共 %d 个账号已更新）", succeeded)
	}
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": failed == 0, "total": len(jobs), "succeeded": succeeded, "failed": failed,
		"message": message, "results": results,
	})
}
