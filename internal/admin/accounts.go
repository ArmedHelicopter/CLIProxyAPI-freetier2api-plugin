package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// StatusPayload 构造账号状态概览（导出给根层测试/兼容包装）。
func (s *Service) StatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return s.buildStatusPayload(req)
}

// buildStatusPayload 构造账号状态概览（不含任何 token）。
func (s *Service) buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	cfg := s.deps.Config()
	current := s.deps.State()
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	accounts := s.listAccountSummaries(ctx, hostbridge.RequestCallbackID(req))
	modelCounts := map[string]int{}
	for _, model := range current.Models {
		modelCounts[model.Realm]++
	}
	return map[string]any{
		"ok":                    true,
		"plugin":                s.deps.Identity().ID,
		"version":               s.deps.Identity().Version,
		"enabled_realms":        cfg.EnabledRealms,
		"workbuddy_prompt_mode": s.deps.WorkbuddyPromptMode(),
		"log_level":             logger.CurrentLevel(),
		"state_dir":             cfg.StateDir,
		"accounts":              accounts,
		"account_count":         len(accounts),
		"model_counts":          modelCounts,
		"models_fetched_at":     current.ModelsFetchedAt,
		"restart_pending":       current.RestartPending,
		"settings":              s.tasks.EffectiveSettings(),
		"scheduler":             s.tasks.Status(),
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
	// SupportsQuota 是「该供应商能否查出额度」的静态能力，与 Quota（查询结果）
	// 是两件事：没有结果时它区分「还没查」与「永远查不出来」。
	SupportsQuota bool `json:"supports_quota"`
	// Claim 是该账号的套餐领取状态；SupportsClaim 为 false 时前端不渲染入口。
	Claim         *state.ClaimRecord `json:"claim,omitempty"`
	SupportsClaim bool               `json:"supports_claim"`
}

func (s *Service) getCachedQuota(keys ...string) *quotaResult {
	s.quotaMu.RLock()
	defer s.quotaMu.RUnlock()
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			if q, ok := s.quotaCache[trimmed]; ok {
				copyQ := q
				return &copyQ
			}
		}
	}
	return nil
}

func (s *Service) cacheQuotaResult(q quotaResult, keys ...string) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			s.quotaCache[trimmed] = q
		}
	}
}

func (s *Service) listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	current := s.deps.State()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{IncludeDisabled: true})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	out := make([]accountSummary, 0, len(loaded))
	for _, account := range loaded {
		entry := account.Entry
		credential := account.Credential
		vendor := account.Vendor
		summary := accountSummary{
			AuthID:          core.FirstNonEmpty(entry.ID, entry.Name),
			AuthIndex:       entry.AuthIndex,
			Label:           core.FirstNonEmpty(credential.LabelValue(), entry.Label, entry.Name, entry.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           credential.RegionValue(),
			Status:          entry.Status,
			Disabled:        entry.Disabled,
			SupportsCheckin: core.SupportsCheckin(vendor),
			SupportsQuota:   supportsQuota(vendor),
			SupportsClaim:   supportsClaim(vendor),
		}
		var modTime time.Time
		if !entry.ModTime.IsZero() {
			modTime = entry.ModTime
		} else if path := core.FirstNonEmpty(account.FilePath, entry.Path); path != "" {
			if info, errStat := os.Stat(path); errStat == nil {
				modTime = info.ModTime()
			}
		}
		if !modTime.IsZero() {
			summary.RefreshedAt = modTime.Format(time.RFC3339)
		}
		uid := credential.FileIDValue()
		if record, okRecord := current.Checkin[uid]; okRecord {
			summary.Checkin = &record
		} else if record, okLegacy := lookupLegacyCheckin(current, credential); okLegacy {
			summary.Checkin = &record
		}
		if record, okRecord := current.Claim[uid]; okRecord {
			claim := record
			summary.Claim = &claim
		}
		if q := s.getCachedQuota(uid, summary.AuthID, summary.AuthIndex, entry.Name, summary.Label); q != nil {
			summary.Quota = q
			// A successful quota read proves the credential is usable even when
			// the host retained a stale runtime error from an earlier request.
			// Do not mutate host auth state; only avoid displaying a contradictory
			// error in this plugin's account overview.
			summary.Status = displayedAccountStatus(summary.Status, q.OK)
		}
		out = append(out, summary)
	}
	return out
}

func displayedAccountStatus(status string, quotaOK bool) string {
	if strings.EqualFold(strings.TrimSpace(status), "error") && quotaOK {
		return "active"
	}
	return status
}

// supportsQuota 报告该供应商是否提供额度查询。
//
// 能力来自接口断言，而不是「查询结果里有没有这一条」——后者会让「不支持」
// 和「还没查」看起来一样，页面于是永远分不清该显示「-- / --」还是「查询中…」。
func supportsQuota(vendor core.Vendor) bool {
	_, okQuota := core.As[core.QuotaProvider](vendor)
	return okQuota
}

// supportsClaim 报告该供应商是否提供套餐领取。
func supportsClaim(vendor core.Vendor) bool {
	_, okClaimer := core.As[core.PlanClaimer](vendor)
	return okClaimer
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

func (s *Service) buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt64(req.Query, "since", 0)
	limit := int(queryInt64(req.Query, "limit", 200))
	entries, newest := logger.Snapshot(uint64(since), limit)
	return map[string]any{"ok": true, "entries": entries, "next": newest}
}

type accountBatchRequest struct {
	AccountIDs []string `json:"account_ids"`
}

func (s *Service) handleAccountDeleteRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids 不能为空"})
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	entries, errList := hostbridge.ListAuths(ctx, callbackID, s.registry)
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
		if errDelete := s.deleteAuthFile(ctx, callbackID, entry); errDelete != nil {
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

func (s *Service) deleteAuthFile(ctx context.Context, callbackID string, entry core.HostAuth) error {
	raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("读取凭证失败，无法确认归属")
	}
	if _, okParse := s.registry.ParseCredential(raw, hostbridge.AccountIdentity(entry), nil); !okParse {
		return fmt.Errorf("该凭证不属于本插件，拒绝删除")
	}
	targetPath := resolveAuthFilePath(core.FirstNonEmpty(filePath, entry.Path))
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

// resolveAuthFilePath 把宿主给出的凭证路径归一化成绝对路径。
//
// 宿主不一定给绝对路径：EasyCLIProxyAPI 把内核的 oauth.auth-dir 配成相对路径
// （默认 ../oauth，相对内核工作目录），CLIProxyAPI 扫盘时按 auth-dir 原样
// WalkDir 并把相对路径写进 auth 属性，host.auth.get / host.auth.list 又原样
// 返回。于是删除保护收到的是 `..\oauth\<name>.json` 这种相对路径，一律判成
// 「非绝对路径」——用户点「删除」什么都删不掉，账号永远留在页面上。
//
// 插件与宿主运行在同一个进程里、工作目录一致，所以用 filepath.Abs 解析出来的
// 就是宿主实际读写的那个文件。这也是宿主管理接口 DELETE /v0/management/auth-files
// 自己的做法：先取 auth 的 path 属性，非绝对时再 filepath.Abs。归一化只做路径
// 换算，.json / 符号链接 / 目录 三道安全检查仍由 ensureAuthFilePathSafe 负责。
func resolveAuthFilePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	if abs, errAbs := filepath.Abs(trimmed); errAbs == nil {
		return abs
	}
	return trimmed
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

func (s *Service) handleAccountAddRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountAddRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
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
	vendor, okVendor := s.registry.ByID(vendorID)
	if !okVendor {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("未知供应商 %q", vendorID)})
	}
	payload := map[string]any{
		"type":         s.deps.Identity().ProviderKey,
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
	callbackID := hostbridge.RequestCallbackID(req)

	if errSave := saveAuthFile(callbackID, fileName, rawJSON); errSave != nil {
		return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errSave.Error()})
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

func (s *Service) handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if errDecode := decodeJSONBody(req.Body, &body); errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	results := s.runForAccounts(ctx, hostbridge.RequestCallbackID(req), body.AccountIDs,
		func(vendor core.Vendor, _ *core.Credential) bool { return core.SupportsCheckin(vendor) },
		func(ctx context.Context, vendor core.Vendor, credential *core.Credential) (accountActionOutcome, error) {
			checkin, okCheckin := core.As[core.CheckinProvider](vendor)
			if !okCheckin {
				return accountActionOutcome{}, fmt.Errorf("本供应商不提供签到")
			}
			result, errCheckin := checkin.Checkin(ctx, credential)
			if errCheckin != nil {
				return accountActionOutcome{}, errCheckin
			}
			if result == nil {
				return accountActionOutcome{}, fmt.Errorf("签到未返回结果")
			}
			s.tasks.RecordCheckin(credential, result)
			// NotRecorded 表示这次签到**没有真正完成**（上游窗口未开启、
			// 领取被拒、未确认发放）。如实标成「跳过」而不是成功，控制台
			// 就不会显示「签到完成」而用户实际没拿到积分。
			return accountActionOutcome{Message: result.Message, Skipped: result.NotRecorded}, nil
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
	// Skipped 报告本次动作「没有真正完成」。
	//
	// 它与 OK 区分开：签到类动作可能请求成功但**什么都没领到**（上游窗口
	// 未开启、领取被风控拒绝、未能确认发放）。把这类结果算作成功会让
	// 控制台显示「签到完成」，用户以为领到了而实际没到账。
	Skipped bool `json:"skipped,omitempty"`
}

// accountActionOutcome 是一次账号动作的结果。
type accountActionOutcome struct {
	// Message 是可读说明。
	Message string
	// Skipped 报告动作「请求成功但没有真正完成」。
	Skipped bool
}

func (s *Service) runForAccounts(
	ctx context.Context,
	callbackID string,
	targets []string,
	filter func(core.Vendor, *core.Credential) bool,
	action func(context.Context, core.Vendor, *core.Credential) (accountActionOutcome, error),
) []accountActionResult {
	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: cfg.EnabledRealms,
		Filter: func(account auth.Account) bool {
			return filter == nil || filter(account.Vendor, account.Credential)
		},
	})
	results := make([]accountActionResult, 0, len(loaded)+len(failures))
	for _, failure := range failures {
		results = append(results, accountActionResult{
			AuthID: core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:  core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			OK:     false, Message: failure.Message,
		})
	}
	jobResults := auth.RunBatch(loaded, batchConcurrency, func(current auth.Account) accountActionResult {
		result := accountActionResult{
			AuthID:   core.FirstNonEmpty(current.Entry.ID, current.Entry.Name),
			Label:    core.FirstNonEmpty(current.Credential.LabelValue(), current.Entry.Label),
			VendorID: current.Vendor.ID(),
			Realm:    current.Credential.RegionValue(),
		}
		outcome, errAction := action(ctx, current.Vendor, current.Credential)
		if errAction != nil {
			result.OK = false
			result.Message = errAction.Error()
		} else {
			result.OK = true
			result.Skipped = outcome.Skipped
			result.Message = outcome.Message
		}
		return result
	})
	return append(results, jobResults...)
}
