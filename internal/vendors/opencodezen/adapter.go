package opencodezen

// 本文件把 OpenCode ZEN 适配成 core.Vendor。
//
// 与其它供应商的关键差异：
//   - **没有登录流程**：凭证是用户自己申请的静态 API key，没有 OAuth、
//     没有设备码、没有 token 交换。因此 LoginStart / LoginPoll 返回明确的
//     「本供应商不支持登录，请用 API key 添加」；
//   - **没有区域**：只有一个部署（opencode.ai），Region() 返回空串；
//   - **没有额度接口**：上游不提供余额查询，Quota 返回「不支持」；
//   - **没有续期**：key 不会变，Refresh 只做一次可用性校验（列模型）。
//
// 适配器与协议实现同包：适配层不再放在根层，根层只负责注册。

import (
	"context"
	"fmt"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
)

// adapter 是 core.Vendor 在 OpenCode ZEN 上的实现。
type adapter struct {
	core.Meta
	// contextFallback 是模型缺上下文窗口时的兜底（由根层注入，保持原行为）。
	contextFallback func(string) int64
}

// Instances 构造 ZEN 实例（只有一个，没有区域之分）。
//
// contextFallback 为 nil 时使用 core 的通用兜底。
func Instances(contextFallback func(string) int64) []core.Vendor {
	if contextFallback == nil {
		contextFallback = core.DefaultContextWindow
	}
	return []core.Vendor{&adapter{
		Meta: core.Meta{
			VendorID:   VendorID,
			VendorName: VendorName,
		},
		contextFallback: contextFallback,
	}}
}

// AuthMode 报告本供应商的凭证形态。
//
// 页面据此决定账号列是否脱敏：API key 本身就是凭证，明文渲染进 HTML
// 等于把它写进浏览器缓存与截图里。
func (v *adapter) AuthMode() string { return "apikey" }

// Configure 解析供应商专属配置。
func (v *adapter) Configure(settings core.Settings, raw map[string]string) {
	Configure(settings, raw)
}

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
	// Label 兜底用**脱敏后**的 key：Label 会进日志、进管理接口响应、
	// 进页面 HTML，绝不能是完整凭证。
	label := native.Label
	if label == "" {
		label = MaskedKey(native.APIKey)
	}
	return &core.Credential{
		VendorID: VendorID,
		Label:    label,
		FileID:   strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:    native.APIKey,
		AuthMode: "apikey",
		Native:   native,
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

// StaticModels 返回 ZEN 的模型清单。
//
// 需要一把有效的 key 才能探测（上游模型接口要认证）。借一个本供应商的
// 可用凭证去打；借不到就返回空清单——不编一份假清单，否则用户会看到
// 调用必失败的模型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	apiKey := probeAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("no opencodezen credential available for model probe")
	}
	models, errModels := FetchModels(ctx, BaseURLOverride(), apiKey)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// ZEN 的清单是全局的（不按账号区分），因此与 StaticModels 同源；
// 但用**该凭证自己的 key** 去探测，这样 key 失效时该账号自然拿不到模型，
// 宿主据此把它从选号里淘汰。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, BaseURLOverride(), native.APIKey)
	if errModels != nil {
		// 探测失败返回空清单而不是报错：宿主会因为空清单跳过该凭证，
		// 这正是我们想要的结果（避免用坏 key 打上游）。
		logger.Debug("opencodezen: model probe failed: %v", errModels)
		return nil, nil
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
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
			contextWindow = v.contextFallback(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        "OpenCode ZEN 上游模型",
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
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, BaseURLOverride(), ChatRequest{
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
// 按声明格式完成。插件侧重复分帧会与宿主产生不一致的行为。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, BaseURLOverride(), ChatRequest{
		APIKey: native.APIKey,
		Body:   req.Payload,
		Stream: true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return httpx.ForwardRawSSE(resp.Body, sink.Emit)
}

// Refresh 校验凭证是否仍然可用。
//
// API key 不会轮换，因此这里**只校验不写回**（refreshed 恒为 false）：
// 列一次模型能验证 key 的有效性，成功即说明账号可用。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if _, errModels := FetchModels(ctx, BaseURLOverride(), native.APIKey); errModels != nil {
		return nil, false, errModels
	}
	return cred, false, nil
}

// ---- 内部工具 ----

// probeAPIKey 借一个本供应商的可用凭证取 API key（供模型探测用）。
func probeAPIKey() string {
	native, okNative := core.ProbeNativeCredential(VendorID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return ""
	}
	return native.APIKey
}
