package core

// 本文件定义「可选能力接口」与共享助手。
//
// 设计原则（Go 版的接口隔离）：
//   - Vendor 只保留每家都必然具备的能力；
//   - 登录/续期/额度/签到/任务/配置/存储合并等能力各自是一个小接口；
//   - 调用方用类型断言按需取用，不支持的供应商**什么都不用写**；
//   - 禁止用「空实现 + 恒 false」模拟 Java 的 default 方法——那会让
//     "不支持"与"忘了实现"无法区分。

import (
	"context"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// Meta 是供应商适配器的身份信息（组合进 adapter，替代旧的 BaseVendor）。
//
// 字段名与接口方法名不同（VendorID/ID、Prefix/ModelPrefix）：Go 不允许
// 同名字段与方法共存。
type Meta struct {
	VendorID     string
	VendorName   string
	VendorRegion string
	// Prefix 是注册模型时的 ID 前缀，空表示裸名。
	Prefix string
}

// ID 返回供应商实例标识（同时是凭证文件名前缀）。
func (m *Meta) ID() string { return m.VendorID }

// Name 返回管理端展示名。
func (m *Meta) Name() string { return m.VendorName }

// Region 返回区域标识（cn / global；无区域概念时为空）。
func (m *Meta) Region() string { return m.VendorRegion }

// ModelPrefix 返回模型注册前缀。
func (m *Meta) ModelPrefix() string { return m.Prefix }

// ModelsProvider 由「按凭证返回可用模型」与静态清单不同的供应商实现。
//
// 未实现时调用方回落到 StaticModels（绝大多数供应商两者同源）。
type ModelsProvider interface {
	ModelsForAuth(ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error)
}

// LoginProvider 由提供多步登录（设备码 / OAuth）的供应商实现。
//
// **实现它即表示支持登录**：控制台据此显示「添加账号」入口。
// 纯 API key 的供应商不实现即可，不再需要返回「不支持」的空方法。
type LoginProvider interface {
	LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error)
}

// Refresher 由需要校验/续期凭证的供应商实现。
//
// 未实现时调用方视为「已校验且无需写盘」（refreshed=false）。
type Refresher interface {
	Refresh(ctx context.Context, cred *Credential) (updated *Credential, refreshed bool, err error)
}

// QuotaProvider 由上游提供额度查询的供应商实现。
type QuotaProvider interface {
	Quota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error)
}

// CheckinProvider 由提供签到的供应商实现。
//
// SupportsCheckin 仍然保留：同一供应商的不同区域可能只有部分区域有活动
// （WorkBuddy 国内版有、国际版没有）。
type CheckinProvider interface {
	SupportsCheckin() bool
	Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error)
}

// AuthModeReporter 由「凭证本身就是敏感材料」的供应商实现。
//
// 目前只有 apikey 型供应商需要它：页面据此脱敏展示。
type AuthModeReporter interface {
	AuthMode() string
}

// StorageMerger 由「刷新后需要把新凭证合并回原文件」的供应商实现。
type StorageMerger interface {
	MergeStorageJSON(original []byte, credential *Credential) ([]byte, error)
}

// Configurable 由需要读取供应商专属配置的实例实现。
//
// 根层只解析全局键，其余原始键原样交给各供应商自己解释；
// raw 是 key → 原始字符串，实现必须容忍缺失键与 nil map。
type Configurable interface {
	Configure(settings Settings, raw map[string]string)
}

// ConsoleModel 是供应商提供的模型展示明细（控制台模型页用）。
type ConsoleModel struct {
	ID              string
	Name            string
	Description     string
	ContextLength   int64
	MaxOutputTokens int64
	Efforts         []string
	DefaultEffort   string
	SupportsImages  bool
	SupportsTools   bool
	Credits         string
}

// ModelCacheLoader 由需要预热自身模型目录的供应商实现。
//
// state.json 中的缓存按 VendorID 区分；Manager 在启动时把对应条目交给
// 该供应商，避免根层直接依赖某个具体供应商包。
type ModelCacheLoader interface {
	LoadModelCache(cached []CachedModel)
}

// ConsoleModelProvider 由需要补充展示明细（如积分倍率）的供应商实现。
//
// 未实现时控制台从已注册的模型清单回读（缺少 Credits 等扩展信息）。
type ConsoleModelProvider interface {
	ConsoleModels() []ConsoleModel
}

// As 把供应商断言为某个可选能力接口。
//
// 用法：if quota, ok := core.As[core.QuotaProvider](vendor); ok { ... }
func As[T any](vendor Vendor) (T, bool) {
	value, okCapability := any(vendor).(T)
	return value, okCapability
}

// SupportsCheckin 报告供应商是否提供签到。
func SupportsCheckin(vendor Vendor) bool {
	provider, okProvider := As[CheckinProvider](vendor)
	return okProvider && provider.SupportsCheckin()
}

// ModelsFor 返回某凭证可用的模型：可选实现优先，否则回落 StaticModels。
//
// cred 为空时返回空清单（宿主不会为空凭证注册模型）。
func ModelsFor(vendor Vendor, ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error) {
	if provider, okProvider := As[ModelsProvider](vendor); okProvider {
		return provider.ModelsForAuth(ctx, cred)
	}
	if cred == nil {
		return nil, nil
	}
	return vendor.StaticModels(ctx)
}

// RefreshVendor 校验/续期凭证：无 Refresher 的供应商视为已校验。
func RefreshVendor(vendor Vendor, ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if refresher, okRefresher := As[Refresher](vendor); okRefresher {
		return refresher.Refresh(ctx, cred)
	}
	return cred, false, nil
}

// EstimateTokens 按字节数估算 token（偏保守，倾向高估）。
func EstimateTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	tokens := len(payload) / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}

// QuotaUnsupported 是「上游没有额度接口」的统一错误。
func QuotaUnsupported(vendor Vendor) error {
	return NewPluginError("quota_unsupported",
		vendor.Name()+" 上游不提供额度查询接口（额度以订阅页为准）", http.StatusNotImplemented)
}

// LoginUnsupported 是「上游没有登录流程」的统一错误。
func LoginUnsupported(vendor Vendor) error {
	return NewPluginError("login_unsupported",
		vendor.Name()+" 不支持登录：请用「添加 API Key」方式添加", http.StatusNotImplemented)
}

// ConfigFieldsProvider 由需要声明 YAML 配置字段的供应商实现。
type ConfigFieldsProvider interface {
	ConfigFields() []pluginapi.ConfigField
}

// ── 套餐领取 ────────────────────────────────────────────────────────────────
//
// 有些供应商的「领取套餐」需要上游发放的活动套餐，并强制要求人机验证令牌
// （ZCode 的阿里云无痕验证）。求解器不适合塞进插件进程，因此契约只要求供应
// 商给出 **SDK 初始化参数**，由管理页在用户浏览器里渲染控件，用户通过后把
// 令牌回传给插件转发。
//
// 三个方法合成一个接口：它们描述的是同一条人工领取链路，总是一起被实现、
// 一起被调用（预览页要 SDK 参数才能渲染控件，领取要用预览结果选套餐）。
// 拆成三个小接口只会让调用点多写两次类型断言。

// CaptchaConfig 是人机验证 SDK 的初始化参数。
//
// 前端据此渲染控件，拿到令牌后原样回传 VerifyParam；Region 为空表示上游
// 未下发地域，此时不带 Verify-Region 头。
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	SceneID string `json:"scene_id"`
	Region  string `json:"region"`
	Prefix  string `json:"prefix"`
}

// PlanGrant 是套餐授予的一项额度（通常是某模型的 token 额度）。
type PlanGrant struct {
	Model  string  `json:"model"`
	Units  float64 `json:"units"`
	Period string  `json:"period,omitempty"`
}

// PlanInfo 是一个可领取的套餐。
type PlanInfo struct {
	PlanID      string      `json:"plan_id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Priority    int         `json:"priority,omitempty"`
	Grants      []PlanGrant `json:"grants,omitempty"`
	StartsAt    int64       `json:"starts_at,omitempty"`
	EndsAt      int64       `json:"ends_at,omitempty"`
}

// ClaimPlanRequest 是一次套餐领取请求。
//
// PlanID 为空表示由供应商自行挑选优先级最高的未领套餐；VerifyParam 是用户在
// 浏览器里完成人机验证后拿到的令牌。
type ClaimPlanRequest struct {
	PlanID       string `json:"plan_id"`
	VerifyParam  string `json:"verify_param"`
	VerifyRegion string `json:"verify_region,omitempty"`
}

// ClaimPlanResponse 是领取成功的结果。
type ClaimPlanResponse struct {
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name,omitempty"`
	StartsAt int64  `json:"starts_at,omitempty"`
	EndsAt   int64  `json:"ends_at,omitempty"`
}

// PlanClaimer 由投放活动套餐的供应商实现（当前只有 ZCode 的 Coding Plan）。
//
// 未实现时管理页不显示领取入口——不允许对没有套餐概念的供应商发起领取。
type PlanClaimer interface {
	// CaptchaConfig 给出人机验证控件的初始化参数。
	CaptchaConfig(ctx context.Context) (CaptchaConfig, error)
	// PreviewPlans 返回账号当前可领的套餐（已剔除已持有的）。
	// 空切片表示没有可领的，页面据此把按钮置灰成「已领取」。
	PreviewPlans(ctx context.Context, cred *Credential) ([]PlanInfo, error)
	// ClaimPlan 领取指定套餐；req.PlanID 为空时由供应商自行挑选。
	ClaimPlan(ctx context.Context, cred *Credential, req ClaimPlanRequest) (ClaimPlanResponse, error)
}

// RuntimeFieldsProvider 由需要声明管理页可改字段的供应商实现。
type RuntimeFieldsProvider interface {
	RuntimeFields() []pluginapi.ConfigField
}
