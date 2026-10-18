package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// StatusPayload 构造账号状态概览（导出给根层测试/兼容包装）。
func StatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return buildStatusPayload(req)
}

// buildStatusPayload 构造账号状态概览（不含任何 token）。
func buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	cfg := deps.Config()
	current := deps.State()
	ctx, cancel := managementContext(req)
	defer cancel()

	accounts := listAccountSummaries(ctx, hostCallbackID(req))
	modelCounts := map[string]int{}
	for _, model := range current.Models {
		modelCounts[model.Realm]++
	}
	return map[string]any{
		"ok":                true,
		"plugin":            deps.Identity().ID,
		"version":           deps.Identity().Version,
		"enabled_realms":    cfg.EnabledRealms,
		"prompt_mode":       deps.PromptMode(),
		"log_level":         logger.CurrentLevel(),
		"state_dir":         cfg.StateDir,
		"accounts":          accounts,
		"account_count":     len(accounts),
		"model_counts":      modelCounts,
		"models_fetched_at": current.ModelsFetchedAt,
		"restart_pending":   current.RestartPending,
		"settings":          deps.EffectiveSettings(),
		"scheduler":         deps.SchedulerStatus(),
	}
}

// accountSummary 是账号概览条目（不含凭证）。
type accountSummary struct {
	AuthID          string               `json:"auth_id"`
	AuthIndex       string               `json:"auth_index"`
	Label           string               `json:"label"`
	VendorID        string               `json:"vendor_id"`
	VendorName      string               `json:"vendor_name"`
	Realm           string               `json:"realm"`
	Status          string               `json:"status"`
	Disabled        bool                 `json:"disabled"`
	RefreshedAt     string               `json:"refreshed_at,omitempty"`
	Checkin         *state.CheckinRecord `json:"checkin,omitempty"`
	Quota           *quotaResult         `json:"quota,omitempty"`
	SupportsCheckin bool                 `json:"supports_checkin"`
}

var (
	quotaCacheMu sync.RWMutex
	quotaCache   = map[string]quotaResult{}
)

func getCachedQuota(keys ...string) *quotaResult {
	quotaCacheMu.RLock()
	defer quotaCacheMu.RUnlock()
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			if q, ok := quotaCache[trimmed]; ok {
				copyQ := q
				return &copyQ
			}
		}
	}
	return nil
}

func cacheQuotaResult(q quotaResult, keys ...string) {
	quotaCacheMu.Lock()
	defer quotaCacheMu.Unlock()
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			quotaCache[trimmed] = q
		}
	}
}

func listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		logger.Debug("list host auths failed: %v", errList)
		return nil
	}
	current := deps.State()
	out := make([]accountSummary, 0, len(entries))
	seenIndex := make(map[string]int)

	for _, entry := range entries {
		summary := accountSummary{
			AuthID:    core.FirstNonEmpty(entry.ID, entry.Name),
			AuthIndex: entry.AuthIndex,
			Label:     core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
			Status:    entry.Status,
			Disabled:  entry.Disabled,
		}
		var modTime time.Time
		if !entry.ModTime.IsZero() {
			modTime = entry.ModTime
		} else if entry.Path != "" {
			if fi, err := os.Stat(entry.Path); err == nil {
				modTime = fi.ModTime()
			}
		}
		var accountUID string
		if raw, filePath, okRaw := hostabi.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex); okRaw {
			if modTime.IsZero() && filePath != "" {
				if fi, err := os.Stat(filePath); err == nil {
					modTime = fi.ModTime()
				}
			}
			if credential, okParse := core.ParseCredential(raw, hostabi.AccountIdentity(entry), nil); okParse {
				summary.VendorID = credential.VendorIDValue()
				summary.Realm = credential.RegionValue()
				if vendor, okVendor := core.VendorByID(summary.VendorID); okVendor {
					summary.VendorName = vendor.Name()
					summary.SupportsCheckin = core.SupportsCheckin(vendor)
				}
				if label := credential.LabelValue(); label != "" {
					summary.Label = label
				}
				if uid := credential.FileIDValue(); uid != "" {
					accountUID = uid
					if record, okRecord := current.Checkin[uid]; okRecord {
						summary.Checkin = &record
					} else if record, okLegacy := lookupLegacyCheckin(current, credential); okLegacy {
						summary.Checkin = &record
					}
				}
			}
		}
		if modTime.IsZero() && entry.Name != "" {
			for _, dir := range []string{"/root/.cli-proxy-api", "."} {
				candidate := filepath.Join(dir, entry.Name)
				if fi, err := os.Stat(candidate); err == nil {
					modTime = fi.ModTime()
					break
				}
			}
		}
		if !modTime.IsZero() {
			summary.RefreshedAt = modTime.Format(time.RFC3339)
		}
		if q := getCachedQuota(accountUID, summary.AuthID, summary.AuthIndex, entry.Name, summary.Label); q != nil {
			summary.Quota = q
		}
		dedupKey := accountUID
		if dedupKey == "" {
			dedupKey = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(summary.AuthID), ".json"))
		}
		if idx, exists := seenIndex[dedupKey]; exists && dedupKey != "" {
			if out[idx].RefreshedAt == "" && summary.RefreshedAt != "" {
				out[idx].RefreshedAt = summary.RefreshedAt
			}
			if out[idx].Label == "" || out[idx].Label == out[idx].AuthID {
				if summary.Label != "" {
					out[idx].Label = summary.Label
				}
			}
			if out[idx].Checkin == nil && summary.Checkin != nil {
				out[idx].Checkin = summary.Checkin
			}
			continue
		}
		if dedupKey != "" {
			seenIndex[dedupKey] = len(out)
		}
		out = append(out, summary)
	}
	return out
}

// lookupLegacyCheckin 读取「账号身份迁移前的 UID 键」下的签到记录（只读回退）。
func lookupLegacyCheckin(current state.State, credential *core.Credential) (state.CheckinRecord, bool) {
	legacyUID := credential.UIDValue()
	if legacyUID == "" || legacyUID == credential.FileIDValue() {
		return state.CheckinRecord{}, false
	}
	record, okRecord := current.Checkin[legacyUID]
	return record, okRecord
}

func buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt64(req.Query, "since", 0)
	limit := int(queryInt64(req.Query, "limit", 200))
	entries, newest := logger.Snapshot(uint64(since), limit)
	return map[string]any{"ok": true, "entries": entries, "next": newest}
}

type accountBatchRequest struct {
	AccountIDs []string `json:"account_ids"`
}

func handleAccountDeleteRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids 不能为空"})
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "读取账号列表失败：" + errList.Error()})
	}
	wanted := make(map[string]bool, len(body.AccountIDs))
	for _, target := range body.AccountIDs {
		wanted[strings.TrimSpace(target)] = true
	}
	results := make([]accountActionResult, 0, len(body.AccountIDs))
	deleted := 0
	for _, entry := range entries {
		if !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		result := accountActionResult{
			AuthID: core.FirstNonEmpty(entry.ID, entry.Name),
			Label:  core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
		}
		if errDelete := deleteAuthFile(ctx, callbackID, entry); errDelete != nil {
			result.OK = false
			result.Message = errDelete.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		result.Message = "已删除"
		deleted++
		results = append(results, result)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "results": results})
}

func deleteAuthFile(ctx context.Context, callbackID string, entry core.HostAuth) error {
	raw, filePath, okRaw := hostabi.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("读取凭证失败，无法确认归属")
	}
	if _, okParse := core.ParseCredential(raw, hostabi.AccountIdentity(entry), nil); !okParse {
		return fmt.Errorf("该凭证不属于本插件，拒绝删除")
	}
	targetPath := strings.TrimSpace(filePath)
	if targetPath == "" {
		targetPath = strings.TrimSpace(entry.Path)
	}
	if targetPath == "" {
		return fmt.Errorf("该账号没有对应的凭证文件（运行时凭证），无法删除")
	}
	if errSafe := ensureAuthFilePathSafe(targetPath); errSafe != nil {
		return errSafe
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		if os.IsNotExist(errRemove) {
			return nil
		}
		return fmt.Errorf("删除文件失败：%w", errRemove)
	}
	logger.Info("deleted auth file %s (vendor=%s)", filepath.Base(targetPath), entry.Name)
	return nil
}

func ensureAuthFilePathSafe(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("拒绝删除非绝对路径：%s", path)
	}
	if !strings.HasSuffix(strings.ToLower(path), ".json") {
		return fmt.Errorf("拒绝删除非凭证文件：%s", filepath.Base(path))
	}
	info, errStat := os.Lstat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return nil
		}
		return fmt.Errorf("检查文件失败：%w", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝删除符号链接：%s", filepath.Base(path))
	}
	if info.IsDir() {
		return fmt.Errorf("拒绝删除目录：%s", filepath.Base(path))
	}
	return nil
}

type accountAddRequest struct {
	Vendor string `json:"vendor"`
	APIKey string `json:"api_key"`
	Label  string `json:"label"`
}

func handleAccountAddRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountAddRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	vendorID := strings.TrimSpace(body.Vendor)
	apiKey := strings.TrimSpace(body.APIKey)
	label := strings.TrimSpace(body.Label)
	if vendorID == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "vendor 不能为空"})
	}
	if apiKey == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "api_key 不能为空"})
	}
	vendor, okVendor := core.VendorByID(vendorID)
	if !okVendor {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("未知供应商 %q", vendorID)})
	}
	payload := map[string]any{
		"type":         deps.Identity().ProviderKey,
		core.VendorKey: vendor.ID(),
		"api_key":      apiKey,
	}
	if label != "" {
		payload["label"] = label
	}
	rawJSON, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
	}
	seed := label
	if seed == "" {
		seed = maskedAPIKey(apiKey)
	}
	if seed == "" {
		seed = core.RandomHex(8)
	}
	fileName := core.FileNameFor(vendor.ID(), seed)
	callbackID := hostCallbackID(req)

	saved := false
	if _, errSave := hostabi.Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: fileName, JSON: rawJSON,
	}); errSave == nil {
		saved = true
	} else {
		logger.Debug("host.auth.save failed for %s: %v", fileName, errSave)
	}
	for _, dir := range []string{"/root/.cli-proxy-api", "."} {
		dst := filepath.Join(dir, fileName)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			if errWrite := os.WriteFile(dst, rawJSON, 0o600); errWrite == nil {
				saved = true
				break
			}
		}
	}
	if !saved {
		return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存凭证文件失败"})
	}
	logger.Info("added auth file %s for vendor %s", fileName, vendor.ID())
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "auth_id": fileName, "message": "账号保存成功"})
}

// maskedAPIKey 生成 API Key 的脱敏种子（Cline/OpenCode ZEN 等 key 型凭证）。
func maskedAPIKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}

func handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	results := runForAccounts(ctx, hostCallbackID(req), body.AccountIDs,
		func(vendor core.Vendor, _ *core.Credential) bool { return core.SupportsCheckin(vendor) },
		func(ctx context.Context, vendor core.Vendor, credential *core.Credential) (string, error) {
			checkin, okCheckin := core.As[core.CheckinProvider](vendor)
			if !okCheckin {
				return "", fmt.Errorf("本供应商不提供签到")
			}
			result, errCheckin := checkin.Checkin(ctx, credential)
			if errCheckin != nil {
				return "", errCheckin
			}
			if result == nil {
				return "", fmt.Errorf("签到未返回结果")
			}
			deps.RecordCheckin(credential, result)
			return result.Message, nil
		})
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

type accountActionResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

func runForAccounts(
	ctx context.Context,
	callbackID string,
	targets []string,
	filter func(core.Vendor, *core.Credential) bool,
	action func(context.Context, core.Vendor, *core.Credential) (string, error),
) []accountActionResult {
	entries, errList := hostabi.ListAuths(ctx, callbackID)
	if errList != nil {
		return []accountActionResult{{OK: false, Message: "读取账号列表失败：" + errList.Error()}}
	}
	wanted := make(map[string]bool, len(targets))
	for _, target := range targets {
		wanted[strings.TrimSpace(target)] = true
	}
	cfg := deps.Config()
	type job struct {
		entry      core.HostAuth
		vendor     core.Vendor
		credential *core.Credential
	}
	jobs := make([]job, 0, len(entries))
	seenUID := make(map[string]bool)
	for _, entry := range entries {
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, okRaw := hostabi.AuthJSON(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, vendor, errResolve := deps.ResolveCredential(ctx, callbackID, raw, hostabi.AccountIdentity(entry), nil)
		if errResolve != nil || !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		if filter != nil && !filter(vendor, credential) {
			continue
		}
		uid := credential.FileIDValue()
		if uid == "" {
			uid = core.FirstNonEmpty(entry.ID, entry.Name)
		}
		if uid != "" {
			if seenUID[uid] {
				continue
			}
			seenUID[uid] = true
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential})
	}
	results := make([]accountActionResult, len(jobs))
	concurrency := batchConcurrency
	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	semaphore := make(chan struct{}, concurrency)
	done := make(chan int, len(jobs))
	for index := range jobs {
		go func(slot int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			current := jobs[slot]
			result := accountActionResult{
				AuthID:   core.FirstNonEmpty(current.entry.ID, current.entry.Name),
				Label:    core.FirstNonEmpty(current.credential.LabelValue(), current.entry.Label),
				VendorID: current.vendor.ID(),
				Realm:    current.credential.RegionValue(),
			}
			message, errAction := action(ctx, current.vendor, current.credential)
			if errAction != nil {
				result.OK = false
				result.Message = errAction.Error()
			} else {
				result.OK = true
				result.Message = message
			}
			results[slot] = result
			done <- slot
		}(index)
	}
	for range jobs {
		<-done
	}
	return results
}
