package autoclaw

// 本文件把 AutoClaw 协议适配成 core.Vendor。
//
// 供应商实例 = 协议 × 区域。国内版与国际版是两套独立部署（userapi 基址
// 不同），token 不通用；区域在构造期固定，实例方法内不再分支。
//
// 能力面（对齐 Agent2API 实测协议）：
//   - 凭证解析与偏严归属（accessToken/refreshToken 结构）；
//   - chat 走 region proxy 通道（X-Request-Model 声明模型）；
//   - 模型清单：global 实时拉 autoclaw-model-config，cn 用静态表；
//   - token 自动续期（POST /userapi/v1/refresh）；
//   - 积分额度查询（GET /agent-assetmgr/api/v2/wallets）；
//   - 每日签到（POST .../autoclaw-task-complete，幂等）。
//
// 登录是手机验证码 / 桌面端 OAuth，插件侧无法托管验证码交互，不实现
// LoginProvider：控制台不显示「添加账号」入口，凭证由用户粘贴 JSON 添加。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 国际版 OAuth 登录会话的有效期与轮询节流。
const (
	autoclawOAuthTTL          = 10 * time.Minute
	autoclawOAuthPollInterval = 2 * time.Second
)

// adapter 是 core.Vendor 在 AutoClaw 上的实现.
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	// baseURL 是区域基地址覆盖（Configure 填充，空表示用默认值）。
	baseURL string
}

// Instances 构造 AutoClaw 的两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	inst := &adapter{region: region}
	// 钩子捕获 inst：钩子在构造完成后才执行，Configure 写入的 baseURL 覆盖
	// 对续期同样生效（不必把覆盖值复制进 spec）。
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			if native.Realm() != region {
				return core.CredentialSeed{}, nil, fmt.Errorf("credential region %q does not match vendor %q", native.Realm(), VendorIDFor(region))
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(region),
				Label:  core.FirstNonEmpty(native.UserNameValue(), native.UIDValue(), MaskedKey(native.AccessTokenValue())),
				FileID: core.FileIDFromFileName(fileName), UID: native.UIDValue(),
				Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			accessBefore, refreshBefore := native.Snapshot()
			result, errRefresh := RefreshToken(ctx, inst.resolveBaseURL(), native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			// 上游响应无 expires_in：按 Agent2API 口径保守记 28 天。
			expiresAt := native.ExpiresAtValue()
			if result.AccessToken != accessBefore {
				expiresAt = time.Now().Add(28 * 24 * time.Hour).Unix()
			}
			native.ApplyTokens(result.AccessToken, result.RefreshToken, expiresAt)
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeCredentialJSON(original, native)
		},
	})
	return inst
}

// Configure 解析 AutoClaw 专属配置（区域基地址覆盖）。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw[v.ID()+"_base_url"])
}

// ConfigFields 声明 AutoClaw 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: v.ID() + "_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "AutoClaw 上游基地址覆盖（默认 " + DefaultBaseURLFor(v.region) + "）。"},
	}
}

// AuthMode 报告凭证形态（页面据此展示「需要重新登录」类语义）。
func (v *adapter) AuthMode() string { return "oauth" }

// Match 判断一份凭证是否属于本区域实例。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == string(v.region)
	}
	return core.FileNameMatchesConvention(fileName, v.ID())
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 探测需要一把有效凭证；借不到就返回错误（不编清单）。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	token := v.probeToken()
	if token == "" {
		return nil, fmt.Errorf("no autoclaw credential available for model probe")
	}
	models, errModels := FetchModels(ctx, v.resolveBaseURL(), token, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 用**该凭证自己的 token** 去探测：token 失效时该账号自然拿不到模型，
// 宿主据此把它从选号里淘汰。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, v.resolveBaseURL(), native.AccessTokenValue(), v.region)
	if errModels != nil {
		// 探测失败返回空清单而不是报错：宿主会因为空清单跳过该凭证。
		// 必须用 Info：宿主日志默认 info 级别，Debug 会被完全吞掉。
		logger.Info("autoclaw: model probe failed: %v", errModels)
		return nil, nil
	}
	return modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               core.FirstNonEmpty(model.Name, id),
			Description:        core.FirstNonEmpty(model.Description, "AutoClaw 上游模型"),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutputTokens,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, v.resolveBaseURL(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
		Model: req.Model,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
//
// 只投递上游的原生 SSE 数据载荷，不做 `data: ` 包装与分帧——包装由宿主
// 按声明格式完成。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.resolveBaseURL(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
		Model: req.Model,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- 额度与签到 ----

// Quota 查询账号积分余额。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	balance, errBalance := FetchBalance(ctx, v.resolveBaseURL(), native.AccessTokenValue())
	if errBalance != nil {
		return nil, core.ToPluginError(errBalance)
	}
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     string(v.region),
			TierName: VendorNameFor(v.region),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: balance.TotalBalance, Format: "number"},
		},
	}
	return &response, nil
}

// Checkin 执行一次每日签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := Checkin(ctx, v.resolveBaseURL(), native)
	if errCheckin != nil {
		return nil, core.ToPluginError(errCheckin)
	}
	message := fmt.Sprintf("签到成功：+%d 积分", result.Points)
	if result.Already {
		message = "今天已经签到过了（幂等，无新增）"
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Points,
		Message: message,
	}, nil
}

func (v *adapter) SupportsCheckin() bool { return true }

// ---- 内部工具 ----

// resolveBaseURL 返回实例当前生效的基地址。
func (v *adapter) resolveBaseURL() string {
	return BaseURL(v.baseURL, v.region)
}

// probeToken 借一个本实例的可用凭证取 access token（供模型探测用）。
func (v *adapter) probeToken() string {
	native, okNative := core.ProbeNativeCredential(v.Env, v.ID(), func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return ""
	}
	return native.AccessTokenValue()
}

// ---- 登录（国内短信 / 国际 OAuth）----

// SendLoginCode 发送短信验证码（国内版专用，core.SMSLoginProvider）。
func (v *adapter) SendLoginCode(ctx context.Context, phone string) (string, error) {
	if v.region.IsGlobal() {
		return "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	return SendLoginCode(ctx, v.resolveBaseURL(), phone)
}

// VerifyLoginCode 用验证码完成登录（国内版专用）。
func (v *adapter) VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) ([]byte, string, error) {
	if v.region.IsGlobal() {
		return nil, "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	cred, errLogin := VerifyLoginCode(ctx, v.resolveBaseURL(), sessionKey, phone, code)
	if errLogin != nil {
		return nil, "", core.ToPluginError(errLogin)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, "", core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	label := core.FirstNonEmpty(cred.UserNameValue(), MaskPhone(cred.Phone), cred.UIDValue())
	return storageJSON, label, nil
}

// autoclawPendingLogin 是一次进行中的国际版 OAuth 登录会话。
//
// LoginStart 把 deviceID 与变体放进会话表；SubmitCallback 取出后用回调里
// 的 code/state 换票（state 用上游带回的值，官方客户端就是原样传回）。
type autoclawPendingLogin struct {
	Region      Region
	Vendor      oauthVendor
	DeviceID    string
	NavigateURI string
}

// LoginStart 发起一次国际版 OAuth 登录（Google）。
//
// 返回的 URL 是官方登录页；登录完成后浏览器停在本机回调地址上，用户把
// 地址栏整条链接粘回控制台（SubmitCallback）完成换票。国内版没有 OAuth
// （上游开关 enabled:false），返回说明性错误。
func (v *adapter) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	if !v.region.IsGlobal() {
		return nil, core.NewPluginError("login_unsupported",
			"AutoClaw 国内版使用「手机号 + 验证码」登录，请在添加账号面板选择对应入口", http.StatusBadRequest)
	}
	if v.Env.Logins == nil {
		return nil, core.NewPluginError("login_unavailable", "登录会话表未初始化（宿主未就绪）", http.StatusInternalServerError)
	}
	deviceID := newDeviceID()
	// Zai 的白名单只认官方客户端的四个登记端口且第②步要过风控验证码
	// （插件侧无验证码组件），先只开放 Google。
	vendor := oauthVendorGoogle
	navigateURI := OAuthNavigateURI(vendor, 46952)
	state := core.RandomHex(16)
	sessionID := v.ID() + "_oauth_" + state
	v.Env.Logins.Register(v.ID(), sessionID, autoclawOAuthTTL, autoclawOAuthPollInterval,
		&autoclawPendingLogin{Region: v.region, Vendor: vendor, DeviceID: deviceID, NavigateURI: navigateURI})
	oauthURL, errURL := OAuthRequestURL(ctx, v.resolveBaseURL(), vendor, navigateURI, "", deviceID)
	if errURL != nil {
		v.Env.Logins.Delete(sessionID)
		return nil, errURL
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       oauthURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(autoclawOAuthTTL),
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll 轮询一次国际版 OAuth 登录状态（等待用户粘贴回调链接）。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	entry, pollState := v.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	_ = entry
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusPending,
		Message: "等待用户在授权页完成 Google 登录…登录完成后浏览器会停在 localhost 回调页，" +
			"请把地址栏里的整条链接复制回控制台提交",
	}, nil
}

// LocalCallbackHint 返回控制台要展示的本机回调地址（Google 宽松匹配任意端口）。
func (v *adapter) LocalCallbackHint() string {
	return "http://localhost:46952/auth/callback-google"
}

// SubmitCallback 解析用户粘贴的回调链接并完成国际版 OAuth 换票。
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	entry, pollState := v.Env.Logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found",
			"登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "登录会话数据已损坏，请重新发起登录", http.StatusBadRequest)
	}
	vendor, code, upstreamState, okCode := ParseOAuthCallback(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是登录完成后浏览器停在的整条链接（含 code=…）",
			http.StatusBadRequest)
	}
	if vendor != pending.Vendor {
		return nil, core.NewPluginError("callback_mismatch",
			"回调链接与本次登录的登录方式不一致，请重新发起登录", http.StatusBadRequest)
	}
	cred, errExchange := OAuthExchangeCode(ctx, v.resolveBaseURL(), vendor, code, upstreamState, pending.NavigateURI, pending.DeviceID)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	v.Env.Logins.Delete(sessionID)
	label := core.FirstNonEmpty(cred.UserNameValue(), cred.UIDValue())
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "AutoClaw 登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(v.ID(), cred.UIDValue()),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CallbackSubmitter    = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.SMSLoginProvider     = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
