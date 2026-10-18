package minimaxcode

// 本文件把 MiniMax Code（国内版/国际版）适配成 core.Vendor。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// adapter 是 core.Vendor 在 MiniMax Code 上的实现（每个区域一个实例）。
type adapter struct {
	core.BaseVendor
	region Region
}

// Instances 构造两个区域实例。
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
			CheckinSupport: region == RegionGlobal,
		},
		region: region,
	}
}

// SupportsLogin 报告本供应商支持登录。
func (v *adapter) SupportsLogin() bool { return true }

// Configure 解析供应商专属配置。
func (v *adapter) Configure(settings core.Settings, raw map[string]string) {
	Configure(settings, raw)
}

// ---- 凭证 ----

// Match 判断一份凭证是否属于本区域。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider, v.region)
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *adapter) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.Email != "" {
			label = native.Email
		} else if native.UserID != "" {
			label = v.Name() + "-" + native.UserID
		} else {
			label = fileName
		}
	}

	uid := native.UserID
	if uid == "" {
		uid = core.FirstNonEmpty(native.Email, fileName)
	}

	return &core.Credential{
		VendorID:     VendorIDFor(v.region),
		Region:       string(v.region),
		Label:        label,
		FileID:       strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:        native.AccessToken,
		RefreshToken: native.RefreshToken,
		ExpiresAt:    native.ExpiresAt,
		AuthMode:     native.AuthMode,
		Native:       native,
	}, nil
}

// nativeCredential 取回协议层凭证对象。
func (v *adapter) nativeCredential(cred *core.Credential) (*Credential, error) {
	return core.NativeCredential(cred, VendorIDFor(v.region), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 与重构前一致：清单按区域探测，不按凭证再过滤（保持既有行为）。
func (v *adapter) ModelsForAuth(ctx context.Context, _ *core.Credential) ([]pluginapi.ModelInfo, error) {
	return v.StaticModels(ctx)
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutput,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, native, req, BaseURLFor(v.region))
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	errStream := ChatStream(ctx, native, req, BaseURLFor(v.region), func(chunk []byte) error {
		return sink.Emit(chunk)
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
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
	return FetchQuota(ctx, native, BaseURLFor(v.region))
}

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	return Checkin(ctx, native, BaseURLFor(v.region))
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.region)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state, BaseURLFor(v.region))
}

// OwnsLoginSession 报告该会话是否由本区域创建。
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return OwnsLoginSession(sessionID, v.region)
}

// Refresh 校验并刷新凭证。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	updated, refreshed, errRefresh := RefreshCredential(ctx, native, BaseURLFor(v.region))
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessToken, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

// MergeStorageJSON 实现 core.StorageMerger。
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.nativeCredential(credential)
	if errNative != nil {
		return original, errNative
	}
	return MergeStorageJSON(original, native)
}
