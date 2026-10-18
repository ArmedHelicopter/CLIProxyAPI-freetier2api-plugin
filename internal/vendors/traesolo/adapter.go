package traesolo

// 本文件把 TRAE SOLO 适配成 core.Vendor。
//
// 注意：本包的协议实现保持独立、不与 trae 包共享（产品上的三个实例
// traecn / traeglobal / traesolo 各自独立）；这里只把原先放在根层的
// 适配代码搬进来。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// adapter 是 core.Vendor 在 TRAE SOLO 上的实现。
type adapter struct {
	core.BaseVendor
}

// Instances 构造 TRAE SOLO 实例。
func Instances() []core.Vendor {
	return []core.Vendor{&adapter{BaseVendor: core.BaseVendor{
		VendorID:       VendorID,
		VendorName:     VendorName,
		VendorRegion:   "cn",
		CheckinSupport: SupportsCheckin(),
	}}}
}

// SupportsLogin 报告本供应商支持登录。
func (v *adapter) SupportsLogin() bool { return true }

// ---- 凭证 ----

// Match 判断一份凭证是否属于本供应商。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *adapter) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.Nickname != "" {
			label = "TRAESOLO-" + native.Nickname
		} else if native.UID != "" {
			label = "TRAESOLO-" + native.UID
		} else {
			label = "TRAESOLO-Account"
		}
	}

	return &core.Credential{
		VendorID:  VendorID,
		Region:    "cn",
		Label:     label,
		FileID:    strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:     native.AccessToken,
		ExpiresAt: native.ExpiresAt,
		AuthMode:  "oauth",
		Native:    native,
	}, nil
}

// nativeCredential 取回协议层凭证对象。
func (v *adapter) nativeCredential(cred *core.Credential) (*Credential, error) {
	return core.NativeCredential(cred, VendorID, func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
}

// ---- 模型 ----

// StaticModels 返回模型清单（无需凭证）。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, nil)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.nativeCredential(cred)
	models, errModels := FetchModels(ctx, native)
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
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
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
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, "", ChatRequest{
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
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return response, nil
}

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := Checkin(ctx, native)
	if errCheckin != nil {
		return nil, core.ToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Credit,
		Message: result.Message,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次授权登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state)
}

// OwnsLoginSession 报告该会话是否由本供应商创建（沿用本包既有会话表）。
func (v *adapter) OwnsLoginSession(sessionID string) bool { return OwnsLoginSession(sessionID) }

// Refresh 校验并刷新凭证。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	updated, refreshed, errRefresh := RefreshCredential(ctx, native)
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
