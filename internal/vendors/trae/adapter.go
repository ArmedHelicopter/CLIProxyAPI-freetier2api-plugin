package trae

// 本文件把 Trae 国内版、Trae 国际版与 TRAE SOLO 适配成 core.Vendor。
//
// 三个实例共享同一套协议实现，差异由 Config/profile 描述。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

type adapter struct {
	*core.NativeAdapter[*Credential]
	config   Config
	sessions *SessionStore
}

// Instances 构造 Trae 家族的全部实例。
func Instances(env core.Env) []core.Vendor {
	configs := AllConfigs()
	sessions := NewSessionStore()
	out := make([]core.Vendor, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, newInstance(env, cfg, sessions))
	}
	return out
}

func newInstance(env core.Env, cfg Config, sessions *SessionStore) core.Vendor {
	return &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
			Meta: core.Meta{VendorID: cfg.VendorID, VendorName: cfg.VendorName, VendorRegion: string(cfg.Region)},
			Env:  env,
			Extract: func(c *core.Credential) (*Credential, bool) {
				native, ok := c.Native.(*Credential)
				return native, ok
			},
			ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
				native, errParse := ParseCredentialFor(raw, cfg)
				if errParse != nil {
					return core.CredentialSeed{}, nil, errParse
				}
				native.Vendor = cfg.VendorID
				label := native.Label
				if label == "" {
					prefix := "TraeCN-"
					switch {
					case cfg.Solo:
						prefix = "TRAESOLO-"
					case cfg.Region == RegionGlobal:
						prefix = "TraeGlobal-"
					}
					if native.Nickname != "" {
						label = prefix + native.Nickname
					} else if native.UID != "" {
						label = prefix + native.UID
					} else {
						label = prefix + "Account"
					}
				}
				return core.CredentialSeed{
					VendorID: cfg.VendorID, Region: string(cfg.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.AccessToken,
					ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native, cfg)
				if errRefresh != nil {
					return core.NativeRefresh[*Credential]{}, errRefresh
				}
				return core.NativeRefresh[*Credential]{
					Native: updated, Token: updated.AccessToken, RefreshToken: updated.RefreshToken,
					ExpiresAt: updated.ExpiresAt, Changed: refreshed,
				}, nil
			},
			MergeNative: func(original []byte, native *Credential) ([]byte, error) {
				return MergeStorageJSON(original, native)
			},
		}),
		config: cfg, sessions: sessions,
	}
}

func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, v.config)
}

// ---- 模型 ----

func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, nil, v.config)
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

func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.config,
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

func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.config,
		Body:       req.Payload,
		Stream:     true,
	}, sink.Emit)
}

// ---- 额度与签到 ----

func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := FetchQuota(ctx, native, v.config)
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return response, nil
}

func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := Checkin(ctx, native, v.config)
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

func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.config, v.sessions)
}

func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state, v.sessions)
}

func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return OwnsLoginSession(v.sessions, sessionID, v.ID())
}

func (v *adapter) SupportsCheckin() bool { return SupportsCheckin(v.config) }
