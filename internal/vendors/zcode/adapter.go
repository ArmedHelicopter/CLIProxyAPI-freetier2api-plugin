package zcode

// 本文件把 ZCode 适配成 core.Vendor。
//
// 凭证形态是 oauth：账号由控制台发起 OAuth 登录产生（一次拿到 Coding Plan
// JWT 与兑换出的 API Key）。之所以声明为 oauth 而不是 apikey，是因为页面按
// auth_mode 决定「添加账号」的动作——apikey 会直接弹输入框、不发起登录。
//
// 通道选择：对话只走 API Key 通道（api.z.ai，免人机验证码）。Plan 通道
// （Bearer JWT）被上游强制要求 X-Aliyun-Captcha-Verify-Param，而本插件不内置
// 浏览器验证码求解。JWT 仍保留，供额度查询与套餐领取使用。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// adapter 是 core.Vendor 在 ZCode 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
}

// Instances 构造 ZCode 实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{&adapter{NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName, VendorRegion: "global"},
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
			label := native.Label
			if label == "" {
				label = core.FirstNonEmpty(native.Email, "ZCode-"+MaskedKey(native.OutboundToken()))
			}
			return core.CredentialSeed{
				VendorID: VendorID, Region: "global", Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: core.FirstNonEmpty(native.UserID, native.Email),
				Token: native.APIKey, AuthMode: native.AuthMode,
			}, native, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})}}
}

// AuthMode 返回凭证形态：oauth。
func (v *adapter) AuthMode() string { return "oauth" }

// Configure 解析供应商专属配置。
func (v *adapter) Configure(settings core.Settings, raw map[string]string) { Configure(settings, raw) }

// Match 判断一份凭证是否属于本供应商。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- 模型 ----

// StaticModels 返回模型清单。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxTokens),
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
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
	payload, errChat := Chat(ctx, BaseURLOverride(), ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, BaseURLOverride(), ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return FetchQuota(ctx, native)
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state)
}

// OwnsLoginSession 报告该会话是否由本供应商创建。
func (v *adapter) OwnsLoginSession(sessionID string) bool { return OwnsLoginSession(sessionID) }

// Refresh 校验凭证。
//
// ZCode 没有 refresh_token 流程（JWT 与 API Key 都不支持续期），因此这里只做
// 本地校验并返回 refreshed=false——不触发无意义的写盘。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errValidate := ValidateCredential(ctx, native); errValidate != nil {
		return nil, false, errValidate
	}
	return cred, false, nil
}

// ValidateCredential 校验凭证形态与上游可用性。
//
// 只做本地形态校验，不产生上游流量：宿主会在每次刷新时调用本方法，
// 让它去打上游等于把额度查询变成了周期性的风控信号。
func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return transientErr("zcode credential is nil")
	}
	if strings.TrimSpace(cred.APIKey) == "" && strings.TrimSpace(cred.JWTToken) == "" {
		return transientErr("zcode credential missing both api_key and jwt_token")
	}
	return nil
}
