package app

// 本文件是 model provider 能力的 ABI 适配层。

import (
	"context"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (a *App) handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		logger.Debug("按授权查模型：%s 无可用凭证：%v", rpc.AuthID, errResolve)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !a.models.RealmEnabled(vendor.Region()) {
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	models, errModels := core.ModelsFor(vendor, ctx, credential)
	if errModels != nil {
		// 探测失败 ≠ 该凭证没有模型。进程内模型缓存挂在 App 实例上，热重载后
		// 随旧实例一起消失，宿主重新加载每个凭证都会现场探测一轮；上游一次抖动
		// （429、超时、瞬时 5xx）就会让整家供应商从 /v1/models 上消失，只能手动
		// 重存设置才恢复。这里回落到 state.json 里最近一次成功的清单，把「瞬时
		// 失败」与「凭证真的不可用」分开——后者由供应商返回空清单表达，不走这里。
		// 计费通道类供应商（core.ModelFallbackVeto）例外：那份清单即权限判据，
		// 探测失败必须如实报空。
		if cached := a.models.Registered(vendor.ID()); len(cached) > 0 && !core.VetoesModelFallback(vendor) {
			logger.Info("按授权查模型：%s 探测失败，回落到缓存 %d 个模型：%v",
				vendor.ID(), len(cached), errModels)
			models, errModels = cached, nil
		}
	}
	if errModels != nil {
		logger.Debug("按授权查模型：%s 探测模型失败：%v", vendor.ID(), errModels)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	// 顺手写入控制台回读缓存（官方 ID，未过滤/未换别名）。
	//
	// 宿主加载每个凭证文件都会走这条 RPC，因此插件启动后不再依赖 static 探测
	// 的时序：控制台列表不会因为探测还没跑完而缺整家供应商。
	// 空清单不写缓存：供应商在探测失败时按契约返回空结果（让宿主淘汰坏凭证），
	// 若照样覆盖，一次瞬时失败就会把整家供应商从控制台抹掉。
	if !a.models.CacheProbedModels(vendor.ID(), models) {
		logger.Info("按授权查模型：%s 未返回 %s 的模型（宿主将丢弃该凭证）",
			vendor.ID(), rpc.AuthID)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   a.models.ApplyAliases(vendor.ID(), a.models.FilterForVendor(vendor.ID(), models)),
	})
}

func (a *App) applyModelCatalog(_ pluginConfig) {
	a.models.ClearRestartPending()
	snapshot := a.SnapshotState()
	a.models.ReloadFromState(snapshot)
	a.models.LoadCachedState(snapshot.Models)
}

func (a *App) refreshModelsFromUpstream(ctx context.Context) (int, error) {
	total, errRefresh := a.models.RefreshFromUpstream(ctx)
	if errRefresh != nil {
		return 0, core.ToPluginError(errRefresh)
	}
	return total, nil
}

func (a *App) resolveOutboundModel(vendorID, model string) string {
	return a.models.ResolveOutboundModel(vendorID, model)
}

func (a *App) applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return a.models.ApplyAliases(vendorID, models)
}

func (a *App) isModelDisabled(vendorID, modelID string) bool {
	return a.models.IsDisabled(vendorID, modelID)
}
