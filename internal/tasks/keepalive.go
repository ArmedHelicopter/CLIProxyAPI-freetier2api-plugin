package tasks

// 本文件是「凭证续期 + 写回」的唯一编排。
//
// 此前这件事有两份实现：管理端点的批量保活，以及 workbuddy 在
// RunScheduled(keepalive) 里自己发宿主 RPC + 写磁盘。后者让供应商包依赖了
// internal/hostbridge，破坏了「vendor 只依赖 core/hostbridge/logger」的边界，
// 而且两份实现的「合并 → host.auth.save → 落盘」逻辑会各自漂移。
//
// 现在供应商只负责一件事：实现 core.Refresher 刷新 token；写回由本文件统一
// 完成，供应商因此不必认识宿主。

import (
	"context"

	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// KeepaliveResult 是一个账号的续期结果（管理端点 JSON 契约的一部分）。
type KeepaliveResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// RefreshAccount 刷新单个账号的凭证，凭证发生变更时写回宿主与磁盘。
//
// 写回走 deps.SaveRefreshedCredential（由组合根实现），因此本包不需要知道
// 宿主 RPC 的存在。callbackID 为空表示后台任务（无请求作用域）。
// 返回 refreshed 表示凭证确实变了。
func (s *Service) RefreshAccount(ctx context.Context, callbackID string, account *accountContext) (refreshed bool, err error) {
	updated, changed, errRefresh := core.RefreshVendor(account.vendor, ctx, account.credential)
	if errRefresh != nil {
		return false, errRefresh
	}
	if !changed {
		return false, nil
	}
	s.deps.SaveRefreshedCredential(ctx, callbackID, account.vendor,
		account.entry, account.rawJSON, updated, account.filePath)
	return true, nil
}

// Keepalive 对全部（或指定的）账号执行 token 续期，供管理端点调用。
//
// 逐账号的失败不影响其它账号：结果里逐条给出原因，由调用方汇总。
func (s *Service) Keepalive(ctx context.Context, callbackID string, targets []string, concurrency int) []KeepaliveResult {
	loaded, _ := s.deps.Pool().Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	jobs := make([]*accountContext, 0, len(loaded))
	for _, item := range loaded {
		jobs = append(jobs, newAccountContext(item))
	}
	return auth.RunBatch(jobs, concurrency, func(job *accountContext) KeepaliveResult {
		return s.keepaliveOne(ctx, callbackID, job)
	})
}

// keepaliveOne 是单账号的续期与结果归一。
func (s *Service) keepaliveOne(ctx context.Context, callbackID string, account *accountContext) KeepaliveResult {
	label := account.label()
	result := KeepaliveResult{
		AuthID:   core.FirstNonEmpty(account.entry.ID, account.entry.Name),
		Label:    label,
		VendorID: account.vendor.ID(),
		Realm:    account.credential.RegionValue(),
	}
	refreshed, errRefresh := s.RefreshAccount(ctx, callbackID, account)
	if errRefresh != nil {
		result.Message = errRefresh.Error()
		logger.Error("keepalive %s: %v", label, errRefresh)
		return result
	}
	result.OK = true
	if refreshed {
		result.Message = "Token 续期成功"
	} else {
		result.Message = "凭证有效（未变更）"
	}
	logger.Info("keepalive %s: refreshed=%t", label, refreshed)
	return result
}
