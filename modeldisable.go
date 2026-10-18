package main

// 本文件是模型禁用/启用的 ABI 适配层：请求解码、日志与响应封装。
// 禁用集合的归一、并集与匹配规则都在 internal/modelconfig。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/modelconfig"
)

// refreshDisabledModelCache 重算禁用集合（配置声明 ∪ 状态记录）。
func refreshDisabledModelCache() { modelCatalog.RefreshFromState() }

// reloadDisabledModelCache 仅供测试/初始化清空缓存。
func reloadDisabledModelCache(disabled []string) {
	_ = disabled
	modelCatalog.ResetDisabledForTest()
}

// isModelDisabled 检查某个供应商下的模型是否被禁用。
func isModelDisabled(vendorID, modelID string) bool {
	return modelCatalog.IsDisabled(vendorID, modelID)
}

// filterDisabledModelsForVendor 从某供应商的模型清单里剔除已禁用的模型。
func filterDisabledModelsForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return modelCatalog.FilterForVendor(vendorID, models)
}

// modelToggleRequest 是模型禁用/启用请求。
type modelToggleRequest struct {
	Models   []string `json:"models"`
	Disabled bool     `json:"disabled"`
}

// handleModelsToggle 批量禁用/启用模型。
func handleModelsToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body modelToggleRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}

	targets := make([]string, 0, len(body.Models))
	for _, item := range body.Models {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "请先选择要操作的模型",
		})
	}

	modelCatalog.SetDisabled(targets, body.Disabled)
	logger.Info("models %s: %d 个（%s）", toggleVerb(body.Disabled), len(targets),
		strings.Join(targets, ", "))

	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": snapshotState().DisabledModels,
		"message":         "已" + toggleVerb(body.Disabled) + " " + strconv.Itoa(len(targets)) + " 个模型（调用即时生效）",
	})
}

// normalizeModelKeyForStorage 把待禁用的模型标识归一成存储键。
func normalizeModelKeyForStorage(modelID string) string {
	return strings.Join(modelconfig.ExpandKeys(modelID), "\n")
}

// toggleVerb 返回操作动词。
func toggleVerb(disabled bool) string {
	if disabled {
		return "禁用"
	}
	return "启用"
}

// withDisabledFlag 给控制台页的模型清单打上禁用标记与别名。
func withDisabledFlag(models []consoleModel) []consoleModel {
	for index := range models {
		models[index].Disabled = isModelDisabled(models[index].VendorID, models[index].ID)
		models[index].Alias = modelAliasFor(models[index].VendorID, models[index].ID)
	}
	return models
}
