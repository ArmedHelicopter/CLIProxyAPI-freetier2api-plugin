package main

// 本文件是 model provider 能力的 ABI 适配层：解码宿主请求、调用
// internal/modelconfig 的领域逻辑、封装响应。
//
// 供应商隔离规则（按凭证只返回同供应商模型）仍在这里：
// 它需要把凭证解析成 vendor，属于根层对宿主契约的翻译。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// modelCatalog 是模型领域逻辑的进程级实例。
var modelCatalog = modelconfig.New(stateStore)

// staticModelRPCRequest 与宿主的 model.static 请求对齐。
type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authModelRPCRequest 与宿主的 model.for_auth 请求对齐。
type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleModelStatic 返回静态模型清单（供 /v1/models 展示与客户端下拉）。
func handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := modelCatalog.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// handleModelRegister 处理宿主的 model.register 契约（ModelRegistrar）。
func handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := modelCatalog.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

// handleModelForAuth 返回某个凭证可用的模型清单。
func handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID)

	credential, vendor, errResolve := resolveVendorCredential(ctx, rpc.HostCallbackID,
		rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errResolve)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !modelCatalog.RealmEnabled(vendor.Region()) {
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}

	models, errModels := core.ModelsFor(vendor, ctx, credential)
	if errModels != nil {
		logger.Debug("model.for_auth: probe models for %s failed: %v", vendor.ID(), errModels)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	return core.OKEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   modelCatalog.ApplyAliases(vendor.ID(), modelCatalog.FilterForVendor(vendor.ID(), models)),
	})
}

// staticModels 返回静态模型清单（测试与管理页复用）。
func staticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	return modelCatalog.StaticModels(ctx)
}

// applyModelCatalog 在注册/重配置时预热模型缓存并清除「待生效」标记。
func applyModelCatalog(_ pluginConfig) {
	modelCatalog.ClearRestartPending()
	snapshot := snapshotState()
	modelCatalog.ReloadFromState(snapshot)
	workbuddy.LoadCachedModelsFromState(snapshot.Models)
}

// persistModels 把探测结果写回状态文件，供下次启动预热。
func persistModels() {
	flat := workbuddy.CachedModelsForState()
	if len(flat) == 0 {
		return
	}
	now := nowFunc().Format(time.RFC3339)
	mutateState(func(state *pluginState) {
		state.Models = flat
		state.ModelsFetchedAt = now
	})
}

// refreshModelsFromUpstream 主动刷新模型清单（管理页调用）。
func refreshModelsFromUpstream(ctx context.Context) (int, error) {
	total, errRefresh := modelCatalog.RefreshFromUpstream(ctx)
	if errRefresh != nil {
		return 0, core.ToPluginError(errRefresh)
	}
	persistModels()
	return total, nil
}

func mergeSameNameModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return modelconfig.MergeSameNameModels(models)
}

// registeredModelsForVendor 返回某供应商最近一次的官方模型清单。
func registeredModelsForVendor(vendorID string) []pluginapi.ModelInfo {
	return modelCatalog.Registered(vendorID)
}

func cacheStaticModels(vendorID string, models []pluginapi.ModelInfo) {
	modelCatalog.CacheStatic(vendorID, models)
}

func cachedStaticModels() map[string][]pluginapi.ModelInfo {
	return modelCatalog.SnapshotCached()
}

// modelSupportsImages 报告模型是否声明图片输入能力（兼容旧调用点）。
func modelSupportsImages(model pluginapi.ModelInfo) bool {
	return core.ModelSupportsImages(model)
}

func modelSupportsTools(model pluginapi.ModelInfo) bool {
	return core.ModelSupportsTools(model)
}

func extraModels(_ pluginConfig) []pluginapi.ModelInfo { return modelCatalog.ExtraModels() }

// ---- 模型别名（原 modelalias.go） ----

func modelAliasFor(vendorID, modelID string) string {
	return modelCatalog.AliasFor(vendorID, modelID)
}

func resolveOutboundModel(vendorID, model string) string {
	return modelCatalog.ResolveOutboundModel(vendorID, model)
}

func applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return modelCatalog.ApplyAliases(vendorID, models)
}

// handleModelAlias 保留旧调用点（测试直接调用）。
func handleModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return admin.ModelAlias(req)
}

// ---- 模型禁用/启用（原 modeldisable.go） ----

// refreshDisabledModelCache 重算禁用集合（配置声明 ∪ 状态记录）。
func refreshDisabledModelCache() { modelCatalog.RefreshFromState() }

// reloadDisabledModelCache 仅供测试/初始化清空缓存。
func reloadDisabledModelCache(disabled []string) {
	_ = disabled
	modelCatalog.ResetDisabledForTest()
}

func isModelDisabled(vendorID, modelID string) bool {
	return modelCatalog.IsDisabled(vendorID, modelID)
}

func filterDisabledModelsForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return modelCatalog.FilterForVendor(vendorID, models)
}

// handleModelsToggle 保留旧调用点（测试直接调用）。
func handleModelsToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return admin.ModelToggle(req)
}

// normalizeModelKeyForStorage 把待禁用的模型标识归一成存储键。
func normalizeModelKeyForStorage(modelID string) string {
	return strings.Join(modelconfig.ExpandKeys(modelID), "\n")
}
