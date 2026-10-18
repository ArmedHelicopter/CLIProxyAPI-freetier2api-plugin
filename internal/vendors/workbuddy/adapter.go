package workbuddy

// 本文件把 WorkBuddy（CodeBuddy）协议适配成 core.Vendor。
//
// 供应商实例 = 协议 × 区域。CN 与 GLOBAL 是两套独立部署：域名、模型清单、
// reasoning 档位表、签到活动都不同，且凭证不通用（用 cn 的 token 打 global
// 域名会得到 401 TOKEN_EXPIRE）。区域在构造期固定，实例方法内不再分支。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/loginsession"
)

// adapter 是 core.Vendor 在 WorkBuddy 上的实现。
type adapter struct {
	core.BaseVendor
	region Region
}

// Instances 构造 WorkBuddy 的两个区域实例。
func Instances() []core.Vendor {
	return []core.Vendor{
		newInstance(RegionCN),
		newInstance(RegionGlobal),
	}
}

func newInstance(region Region) core.Vendor {
	return &adapter{
		BaseVendor: core.BaseVendor{
			VendorID:       VendorIDFor(region),
			VendorName:     VendorNameFor(region),
			VendorRegion:   string(region),
			CheckinSupport: !region.IsGlobal(),
		},
		region: region,
	}
}

// ---- 凭证 ----

// Match 判断一份凭证是否属于本区域。
//
// 严格要求区域一致：这是 CN 与 GLOBAL 两个实例之间唯一的区分依据，
// 缺了它两个实例会互抢同一份凭证。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	// 优先看文件内的 vendor 字段（插件自己写的，最可靠）。
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	// 其次按凭证自带的区域声明。无法判定时（扁平形手写凭证常常没有
	// realm/domain）回落到配置的兜底区域——与解析时的兜底保持一致，
	// 否则这类凭证会既不被 CN 也不被 GLOBAL 认领，等于加载不上。
	if region := RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return defaultRegionForParse() == v.region
}

// defaultRegionForParse 决定解析无区域声明的凭证时使用的兜底域。
//
// 两个域都启用时兜底 cn（历史数据以 cn 为主）；只启用一个域时用它。
func defaultRegionForParse() Region {
	settings := core.HostSettings()
	if settings.RealmEnabled(string(RegionCN)) {
		return RegionCN
	}
	return RegionGlobal
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *adapter) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}
	if native.Realm() != v.region {
		return nil, fmt.Errorf("credential realm %q does not match vendor %q", native.Realm(), v.ID())
	}
	return newCoreCredential(native, v.ID(), fileName), nil
}

// newCoreCredential 把协议层凭证包装成 core.Credential。
func newCoreCredential(native *Credential, vendorID, fileName string) *core.Credential {
	token := native.AccessTokenValue()
	authMode := "oauth"
	if token == "" {
		// 设备令牌形态的凭证没有 access/refresh token，靠 device token 出站。
		token = native.DeviceTokenValue()
		authMode = "device"
	}
	return &core.Credential{
		VendorID:     vendorID,
		Region:       string(native.Realm()),
		Label:        core.FirstNonEmpty(native.NicknameValue(), native.UIDValue(), fileName),
		UID:          native.UIDValue(),
		Token:        token,
		RefreshToken: native.RefreshTokenValue(),
		ExpiresAt:    native.ExpiresAt,
		AuthMode:     authMode,
		Native:       native,
	}
}

// nativeCredential 取回协议层凭证对象。
//
// 凭证经 core.Credential 传递时 Native 被保留，因此正常路径不重新解析。
func (v *adapter) nativeCredential(cred *core.Credential) (*Credential, error) {
	return core.NativeCredential(cred, VendorIDFor(v.region), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 探测由协议层做（按区域的路由最多三路并发并集），这里只做形态转换：
// 上游的 ModelInfo → 中立的 ModelDescriptor → 宿主契约类型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return nil, errClient
	}
	models, errModels := client.FetchModels(v.region)
	if errModels != nil {
		return nil, errModels
	}
	PublishEffortTables(v.region, models)
	return modelsToPluginAPI(models), nil
}

// describeModel 拼接模型说明（带积分倍率前缀）。
//
// 倍率是用户最关心的成本信息，放在说明最前面。
func describeModel(model ModelInfo) string {
	description := strings.TrimSpace(model.Description)
	credits := strings.TrimSpace(model.Credits)
	if credits == "" {
		return description
	}
	// 上游的 credits 形如 "x0.05"，去掉尾部 credit 字样避免 "x0.05 credits credit"。
	credits = strings.TrimSuffix(credits, "credits")
	credits = strings.TrimSuffix(credits, "credit")
	return "[" + strings.TrimSpace(credits) + " credit] " + description
}

// modelsToPluginAPI 把协议层模型元数据转成宿主契约类型。
func modelsToPluginAPI(models []ModelInfo) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model ModelInfo) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        describeModel(model),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxTokens,
			Efforts:            model.Efforts,
			DefaultEffort:      model.DefaultEffort,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      model.SupportsToolCall,
			CanDisableThinking: model.CanDisableThinking,
		}, true
	})
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 本供应商的清单按区域探测（不是按账号），因此与 StaticModels 同源。
// 宿主仍会用这份清单把「凭证 ↔ 模型」绑起来，从而淘汰跨区域凭证。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// 区域不符时返回空清单而不是报错：宿主会因此跳过该凭证，
		// 这正是我们想要的结果（避免用错域的凭证打上游）。
		return nil, nil
	}
	return v.StaticModels(ctx)
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return nil, errClient
	}
	result, errChat := client.Chat(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload, http.Header(req.Headers)),
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: result.Response,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return errClient
	}
	reader, errStream := client.ChatStream(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload, http.Header(req.Headers)),
	})
	if errStream != nil {
		return errStream
	}
	defer func() { _ = reader.Close() }()

	_, errForward := Stream(reader, StreamOptions{Emit: sink.Emit})
	// 空流不算失败：上游有时返回一条没有任何 delta 的流，
	// 把它当错误会让客户端收到无意义的错误帧。
	if errForward != nil && !IsEmptyStreamError(errForward) && ctx.Err() == nil {
		return errForward
	}
	return nil
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return nil, errClient
	}
	balance, errBalance := client.FetchBalance(native)
	if errBalance != nil {
		return nil, errBalance
	}
	response := BuildQuotaResponse(native, balance)
	return &response, nil
}

// Checkin 执行一次签到。国际版返回 nil（无此活动）。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	if !v.SupportsCheckin() {
		return nil, nil
	}
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return nil, errClient
	}
	result, errCheckin := client.DailyCheckin(native)
	if errCheckin != nil {
		return nil, errCheckin
	}
	message := "今天已经签到过了（幂等，无新增）"
	if !result.Already {
		message = fmt.Sprintf("签到成功：+%d 积分 +%d 能量", result.Credit, result.Energy)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Credit,
		Energy:  result.Energy,
		Message: message,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次设备授权登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state, authURL, errStart := RequestLoginState(ctx, v.region)
	if errStart != nil {
		return nil, errStart
	}
	sessionID := "workbuddy_" + string(v.region) + "_" + loginsession.NewID(16)
	StoreLoginSession(sessionID, &PendingLogin{
		Region: v.region,
		State:  state,
	})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(LoginSessionTTL),
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, exists, shouldPoll := CheckLoginSession(state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	if !shouldPoll {
		// 节流期间：返回 pending 等待，不打上游也不报错
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中完成授权...",
		}, nil
	}
	accessToken, refreshToken, errToken := ExchangeLoginToken(ctx, session.Region, session.State)
	if errToken != nil {
		if IsLoginPending(errToken) {
			// 用户还没在浏览器里完成授权，继续等待。
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "正在等待用户在浏览器中完成授权...",
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errToken.Error()}, nil
	}
	account, errAccount := FetchLoginAccount(ctx, session.Region, session.State, accessToken)
	if errAccount != nil {
		// token 已拿到但账号信息失败：用最小信息落盘，让用户至少能用起来
		// （账号信息会在后续刷新时补齐）。
		logger.Error("fetch account info failed after token exchange: %v", errAccount)
	}
	credential := &Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UID:          account.UID,
		Nickname:     account.Nickname,
		Domain:       account.Domain,
		EnterpriseID: account.EnterpriseID,
	}
	credential.SetRealm(session.Region)
	if credential.Domain == "" {
		credential.Domain = DefaultDomainFor(credential.Realm())
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "构建凭证失败：" + errStorage.Error(),
		}, nil
	}
	RemoveLoginSession(state)
	logger.Info("completed workbuddy login for %s (vendor=%s)",
		core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()), v.ID())
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(v.ID(), core.FirstNonEmpty(credential.UID, loginsession.NewID(16))),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商持有且仍有效。
func (v *adapter) OwnsLoginSession(sessionID string) bool { return OwnsLoginSession(sessionID) }

// SupportsLogin 报告本供应商支持登录。
func (v *adapter) SupportsLogin() bool { return true }

// Refresh 校验并刷新凭证。
//
// refreshed 报告上游是否真的下发了新令牌：false 表示仅校验通过，
// 调用方不应重写文件（否则 mtime 会无意义地变动，用户无法判断是否真的续期了）。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	client, errClient := clientFor(ctx)
	if errClient != nil {
		return nil, false, errClient
	}
	accessBefore, refreshBefore := native.Snapshot()
	if errRefresh := client.RefreshToken(native); errRefresh != nil {
		return nil, false, errRefresh
	}
	refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
	if refreshed {
		cred.ApplyTokenRefresh(native.AccessTokenValue(), native.RefreshTokenValue(), native.ExpiresAt)
	}
	return cred, refreshed, nil
}

// MergeStorageJSON 实现 core.StorageMerger：把刷新后的令牌合并回原凭证 JSON。
//
// WorkBuddy 的凭证是嵌套的 auth/account 结构，通用合并会把顶层字段写乱，
// 因此必须由本供应商自己做。
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.nativeCredential(credential)
	if errNative != nil {
		return original, errNative
	}
	return MergeStorageJSON(original, native)
}

// Tasks 返回本区域支持的任务动作。任务闭环是 CodeBuddy 国内版的活动。
func (v *adapter) Tasks() []core.Task {
	if v.region.IsGlobal() {
		return nil
	}
	return TaskActions(v.region)
}

// ---- 内部工具 ----

// clientFor 取上游客户端；未注入时返回明确错误。
func clientFor(ctx context.Context) (*Client, error) {
	client := UpstreamClient(ctx)
	if client == nil {
		return nil, core.NewPluginError("upstream_client_unavailable",
			"workbuddy upstream client is not configured", 500)
	}
	return client, nil
}
