package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
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

func handleKeepaliveRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "读取账号列表失败：" + errList.Error()})
	}
	type job struct {
		entry      core.HostAuth
		vendor     core.Vendor
		credential *core.Credential
		rawJSON    []byte
		filePath   string
	}
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, filePath, okRaw := hostabi.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, vendor, errResolve := deps.ResolveCredential(ctx, callbackID, raw, hostabi.AccountIdentity(entry), nil)
		if errResolve != nil {
			continue
		}
		if filePath != "" {
			credential.SetFilePath(filePath)
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential, rawJSON: raw, filePath: filePath})
	}
	if len(jobs) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": true, "message": "没有需要续期的可用账号", "succeeded": 0, "failed": 0, "results": []any{},
		})
	}

	results := make([]keepaliveResult, len(jobs))
	succeeded, failed := 0, 0
	firstErrMsg := ""
	for i, j := range jobs {
		label := core.FirstNonEmpty(j.credential.LabelValue(), j.entry.Label, j.entry.Name)
		result := keepaliveResult{
			AuthID: core.FirstNonEmpty(j.entry.ID, j.entry.Name), Label: label,
			VendorID: j.vendor.ID(), Realm: j.credential.RegionValue(),
		}
		updated, refreshed, errRefresh := core.RefreshVendor(j.vendor, ctx, j.credential)
		if errRefresh != nil {
			failed++
			result.Message = errRefresh.Error()
			if firstErrMsg == "" {
				firstErrMsg = fmt.Sprintf("%s: %s", label, errRefresh.Error())
			}
			logger.Error("keepalive %s: %v", label, errRefresh)
		} else {
			succeeded++
			result.OK = true
			if refreshed {
				result.Message = "Token 续期成功"
				deps.SaveRefreshedCredential(ctx, callbackID, j.vendor, j.entry, j.rawJSON, updated, j.filePath)
			} else {
				result.Message = "凭证有效（未变更）"
			}
			logger.Info("keepalive %s: refreshed=%t", label, refreshed)
		}
		results[i] = result
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

var _ = json.Marshal
var _ = strings.TrimSpace
