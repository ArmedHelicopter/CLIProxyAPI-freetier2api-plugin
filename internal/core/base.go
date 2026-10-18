package core

// 本文件提供可嵌入的供应商基类：把「所有供应商都一样的默认行为」集中一处。
//
// 未覆盖的方法按 Vendor 接口的语义给出最保守的默认值：
//   - CountTokens：按字节数除以 3 估算，不调上游；
//   - Quota：报告不支持（消息按供应商定制，保持页面文案不变）；
//   - Checkin：返回 (nil, nil)，表示本供应商无签到；
//   - Tasks：空清单。

import (
	"context"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// BaseVendor 是供应商适配器的公共底座。
//
// 字段名刻意与接口方法名不同（VendorID/ID、Prefix/ModelPrefix、
// CheckinSupport/SupportsCheckin）：Go 不允许同名字段与方法共存。
type BaseVendor struct {
	VendorID     string
	VendorName   string
	VendorRegion string
	// Prefix 是注册模型时的 ID 前缀，空表示用裸名。
	Prefix string
	// CheckinSupport 报告本实例是否提供签到。
	CheckinSupport bool
	// QuotaUnsupportedMessage 是「上游无额度接口」时的可读说明。
	// 留空时使用通用文案；各供应商保留自己的文案以免页面提示变化。
	QuotaUnsupportedMessage string
}

// ID 返回供应商实例标识（同时是凭证文件名前缀）。
func (b *BaseVendor) ID() string { return b.VendorID }

// Name 返回管理端展示名。
func (b *BaseVendor) Name() string { return b.VendorName }

// Region 返回区域标识（cn / global；无区域概念时为空）。
func (b *BaseVendor) Region() string { return b.VendorRegion }

// ModelPrefix 返回模型注册前缀。
func (b *BaseVendor) ModelPrefix() string { return b.Prefix }

// CountTokens 估算 token 数（按字节数除以 3，不调上游）。
func (b *BaseVendor) CountTokens(_ context.Context, _ *Credential, req *ExecuteRequest) (int, error) {
	if req == nil || len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

// Quota 报告本供应商不提供额度查询。
func (b *BaseVendor) Quota(_ context.Context, _ *Credential) (*pluginapi.QuotaFetchResponse, error) {
	message := b.QuotaUnsupportedMessage
	if message == "" {
		message = "上游不提供额度查询接口"
	}
	return nil, NewPluginError("quota_unsupported", message, http.StatusNotImplemented)
}

// SupportsCheckin 报告是否提供签到。
func (b *BaseVendor) SupportsCheckin() bool { return b.CheckinSupport }

// Checkin 报告本供应商不支持签到。
func (b *BaseVendor) Checkin(_ context.Context, _ *Credential) (*CheckinResult, error) {
	return nil, nil
}

// Tasks 返回本实例支持的任务动作（默认没有）。
func (b *BaseVendor) Tasks() []Task { return nil }
