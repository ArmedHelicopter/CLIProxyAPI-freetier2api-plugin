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
)

// adapter 是 core.Vendor 在 WorkBuddy 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	rt     *runtime
}

// Instances 构造 WorkBuddy 的两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	rt := newRuntime(env)
	inst := &adapter{rt: rt, region: region}
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw, region)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			if native.Realm() != region {
				return core.CredentialSeed{}, nil, fmt.Errorf("credential realm %q does not match vendor %q", native.Realm(), VendorIDFor(region))
			}
			token := native.AccessTokenValue()
			authMode := "oauth"
			if token == "" {
				token = native.DeviceTokenValue()
				authMode = "device"
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(native.Realm()),
				Label:  core.FirstNonEmpty(native.NicknameValue(), native.UIDValue(), fileName),
				FileID: core.FileIDFromFileName(fileName), UID: native.UIDValue(),
				Token: token, RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, AuthMode: authMode,
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			client := NewClient(rt, rt.options(ctx))
			accessBefore, refreshBefore := native.Snapshot()
			if errRefresh := client.RefreshToken(native); errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, Changed: refreshed,
			}, nil
		},
	})
	return inst
}

// Configure 解析 WorkBuddy 专属配置（UA、设备令牌、提示词、机器盐）。
func (v *adapter) Configure(settings core.Settings, raw map[string]string) {
	v.rt.configure(settings, raw)
}

// Match 判断一份凭证是否属于本区域。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return v.defaultRegionForParse() == v.region
}

func (v *adapter) defaultRegionForParse() Region {
	settings := v.Env.Settings()
	if settings.RealmEnabled(string(RegionCN)) {
		return RegionCN
	}
	return RegionGlobal
}

// ---- 续期 ----

// Refresh 实现 core.Refresher：用 refresh token 换新的 access token。
//
// 「刷新」与「写回」刻意分开：刷新是供应商协议（这里），写回是宿主与磁盘的
// 事务（由 internal/tasks 的保活编排统一做）。这样供应商不必认识宿主，
// 也避免了每个供应商各写一份 merge + host.auth.save + 落盘。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errRefresh := v.client(ctx).RefreshToken(native); errRefresh != nil {
		return nil, false, errRefresh
	}
	return cred, true, nil
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 探测由协议层做（按区域的路由最多三路并发并集），这里只做形态转换：
// 上游的 ModelInfo → 中立的 ModelDescriptor → 宿主契约类型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	client := v.client(ctx)
	models, errModels := client.FetchModels(v.region)
	if errModels != nil {
		return nil, errModels
	}
	v.rt.publishEffortTables(models)
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
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := v.client(ctx)
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
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	client := v.client(ctx)
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
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := v.client(ctx)
	balance, errBalance := client.FetchBalance(native)
	if errBalance != nil {
		return nil, errBalance
	}
	response := BuildQuotaResponse(native, balance)
	return &response, nil
}

// Checkin 执行一次签到。
//
// CN 调 daily-checkin 端点；global 没有该端点活动，走每日活跃奖励
// （活跃信标点亮成长热力图，Bonus Pack 次日凌晨发放），同样幂等。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	if !v.SupportsCheckin() {
		return nil, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := v.client(ctx)
	if v.region.IsGlobal() {
		result, errBonus := client.GlobalDailyBonus(ctx, native)
		if errBonus != nil {
			return nil, errBonus
		}
		if result.Already {
			return &core.CheckinResult{
				Already: true,
				Message: fmt.Sprintf("今日活跃已点亮（已连续点亮 %d 天）", result.Streak),
			}, nil
		}
		return &core.CheckinResult{
			Message: fmt.Sprintf("活跃信标已上报，今日已点亮（已连续点亮 %d 天），奖励积分次日凌晨发放", result.Streak),
		}, nil
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
	sessionID := "workbuddy_" + string(v.region) + "_" + core.RandomHex(16)
	StoreLoginSession(v.Env.Logins, sessionID, &PendingLogin{
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
	session, exists, shouldPoll := CheckLoginSession(v.Env.Logins, state)
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
	RemoveLoginSession(v.Env.Logins, state)
	logger.Info("completed workbuddy login for %s (vendor=%s)",
		core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()), v.ID())
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(v.ID(), core.FirstNonEmpty(credential.UID, core.RandomHex(16))),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商持有且仍有效。
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return OwnsLoginSession(v.Env.Logins, sessionID)
}

// MergeStorageJSON 实现 core.StorageMerger：把刷新后的令牌合并回原凭证 JSON。
//
// WorkBuddy 的凭证是嵌套的 auth/account 结构，通用合并会把顶层字段写乱，
// 因此必须由本供应商自己做。
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.Native(credential)
	if errNative != nil {
		return original, errNative
	}
	return MergeStorageJSON(original, native)
}

// ---- 内部工具 ----

func (v *adapter) client(ctx context.Context) *Client {
	return NewClient(v.rt, v.rt.options(ctx))
}

// SupportsCheckin 报告本区域是否提供签到。
//
// CN 走 daily-checkin 端点；global 走每日活跃奖励（见 dailybonus.go），
// 两个域都有每日收入路径，因此恒为 true。
func (v *adapter) SupportsCheckin() bool { return true }

// LoadModelCache 实现 core.ModelCacheLoader：只加载本实例的缓存。
func (v *adapter) LoadModelCache(cached []core.CachedModel) {
	filtered := make([]core.CachedModel, 0, len(cached))
	for _, item := range cached {
		vendorID := strings.TrimSpace(item.VendorID)
		if vendorID == "" {
			vendorID = string(VendorIDFor(v.region))
		}
		if strings.EqualFold(vendorID, v.ID()) {
			filtered = append(filtered, item)
		}
	}
	v.rt.loadCachedModelsFromState(filtered)
}

// ConsoleModels 返回控制台模型页需要的展示明细（含积分倍率）。
func (v *adapter) ConsoleModels() []core.ConsoleModel {
	cached := v.rt.cachedModels()[NormalizeRegion(string(v.region))]
	out := make([]core.ConsoleModel, 0, len(cached))
	for _, model := range cached {
		out = append(out, core.ConsoleModel{
			ID:              model.ID,
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextWindow,
			MaxOutputTokens: model.MaxTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsToolCall,
			Credits:         model.Credits,
		})
	}
	return out
}

// ChatMetaFromBody 从请求体与请求头派生会话头族。
func ChatMetaFromBody(body []byte, headers http.Header) ChatMeta {
	conversationKey := ConversationKey(body)
	turnKey := TurnKey(body)

	// 入站透传的链路 ID 优先（调用方可能已经建好了链路）。
	traceID := ""
	if headers != nil {
		traceID = strings.TrimSpace(headers.Get("X-Trace-ID"))
	}

	conversationRequestID := ""
	switch {
	case conversationKey != "" && turnKey != "":
		conversationRequestID = TurnRequestID(conversationKey + ":" + turnKey)
	case turnKey != "":
		conversationRequestID = TurnRequestID(turnKey)
	case conversationKey != "":
		conversationRequestID = RequestIDForKey(conversationKey)
	default:
		conversationRequestID = TurnRequestID("")
	}

	return ChatMeta{
		ConversationID:        conversationKey,
		ConversationRequestID: conversationRequestID,
		TraceID:               traceID,
	}
}

// BuildQuotaResponse 把账号额度转成宿主的归一化额度结构。
//
// 结构层次：Summary 放关键数字（宿主的额度卡片直接读它），
// Groups/Buckets 放明细（展开后看）。
func BuildQuotaResponse(credential *Credential, balance *Balance) pluginapi.QuotaFetchResponse {
	region := credential.Realm()
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     string(region),
			TierName: RealmDisplayName(region),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: float64(balance.Remain), Format: "number"},
			{Key: "credit_total", Label: "总额度", Value: float64(balance.Total), Format: "number"},
		},
	}
	if balance.Total > 0 {
		used := balance.Total - balance.Remain
		if used < 0 {
			used = 0
		}
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "credit_used", Label: "已用积分", Value: float64(used), Format: "number"})
	}
	if len(balance.Packages) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(balance.Packages))
		for _, pkg := range balance.Packages {
			fraction := 0.0
			if pkg.Total > 0 {
				fraction = float64(pkg.Remain) / float64(pkg.Total)
			}
			bucket := pluginapi.QuotaBucket{
				Window:            pkg.Name,
				RemainingFraction: fraction,
				Description:       fmt.Sprintf("%d / %d", pkg.Remain, pkg.Total),
			}
			if pkg.ExpireAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ExpireAt)
			}
			buckets = append(buckets, bucket)
		}
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "资源包明细",
			Buckets:     buckets,
		}}
	}
	return response
}

// RealmDisplayName 返回域的展示名。
func RealmDisplayName(region Region) string {
	if region.IsGlobal() {
		return "国际版 (WorkBuddy AI)"
	}
	return "国内版 (CodeBuddy)"
}

// FormatUnixTime 把 Unix 秒格式化为 RFC3339（无效值返回空串）。
func FormatUnixTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
