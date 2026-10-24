package autoclaw

// 本文件把 AutoClaw 协议适配成 core.Vendor（骨架）。
//
// 供应商实例 = 协议 × 区域。国内版与国际版是两套独立部署：域名不同、
// key 不通用（与 WorkBuddy / Qoder 的双实例约定一致）。区域在构造期
// 固定，实例方法内不再分支。
//
// **骨架边界（Issue #24）**：AutoClaw 的上游 API 契约（域名、鉴权形态、
// 请求体细节）在仓库内没有实测依据，因此本实现只落实结构性能力：
//
//   - 凭证解析与归属（API key 型，偏严判定）；
//   - 可配置基地址（autoclawcn_base_url / autoclawglobal_base_url）；
//   - OpenAI 兼容的对话与模型清单转发；
//   - 明确的「不支持」语义：没有登录流程、没有额度接口、没有签到。
//
// 端点回填与协议细化见 docs/vendor-autoclaw-raccoon.md 的清单。

import (
	"context"
	"fmt"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// adapter 是 core.Vendor 在 AutoClaw 上的实现。
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
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(region),
				Label:  core.FirstNonEmpty(native.Label, MaskedKey(native.APIKey)),
				FileID: core.FileIDFromFileName(fileName),
				Token:  native.APIKey, AuthMode: "apikey",
			}, native, nil
		},
	})
	return inst
}

// Configure 解析 AutoClaw 专属配置（区域基地址覆盖）。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	key := v.ID() + "_base_url"
	v.baseURL = strings.TrimSpace(raw[key])
}

// ConfigFields 声明 AutoClaw 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: v.ID() + "_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "AutoClaw 上游基地址覆盖（骨架阶段必填，上游默认域名待回填）。"},
	}
}

// AuthMode 报告凭证形态（页面据此脱敏展示）。
func (v *adapter) AuthMode() string { return "apikey" }

// Match 判断一份凭证是否属于本区域实例。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	// 没有显式 vendor 字段时按区域字段 / 文件名约定归属。
	if region := RegionForCredential(raw); region != "" {
		return region == string(v.region)
	}
	return core.FileNameMatchesConvention(fileName, v.ID())
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 需要一把有效 key 才能探测（上游模型接口要认证）。借一个本实例的可用
// 凭证去打；借不到就报错——不编一份假清单，否则用户会看到调用必失败的
// 模型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	apiKey, baseURL := v.probe()
	if apiKey == "" {
		return nil, fmt.Errorf("no autoclaw credential available for model probe")
	}
	models, errModels := FetchModels(ctx, baseURL, apiKey)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 用**该凭证自己的 key** 去探测：key 失效时该账号自然拿不到模型，
// 宿主据此把它从选号里淘汰。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, v.resolveBaseURL(), native.APIKey)
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
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        "AutoClaw 上游模型",
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
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
		APIKey: native.APIKey,
		Body:   req.Payload,
		Stream: false,
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
		APIKey: native.APIKey,
		Body:   req.Payload,
		Stream: true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- 内部工具 ----

// resolveBaseURL 返回实例当前生效的基地址。
func (v *adapter) resolveBaseURL() string {
	return BaseURL(v.baseURL, v.region)
}

// probe 借一个本实例的可用凭证取 API key（供模型探测用），同时返回基地址。
func (v *adapter) probe() (string, string) {
	native, okNative := core.ProbeNativeCredential(v.Env, v.ID(), func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return "", v.resolveBaseURL()
	}
	return native.APIKey, v.resolveBaseURL()
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
)
