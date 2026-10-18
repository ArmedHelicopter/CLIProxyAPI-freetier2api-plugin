package main

// 本文件是模型别名的 ABI 适配层：请求解码/校验后交给 internal/modelconfig，
// 领域规则（唯一性、反向解析、作用域）都在那个包里。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/modelconfig"
)

// refreshModelAliasCache 从状态文件重算别名表。调用方不得持有状态锁。
func refreshModelAliasCache() { modelCatalog.RefreshFromState() }

// modelAliasFor 返回某供应商某模型设置的别名；未设置时返回空串。
func modelAliasFor(vendorID, modelID string) string {
	return modelCatalog.AliasFor(vendorID, modelID)
}

// resolveOutboundModel 把客户端请求的模型名还原成上游官方 ID。
func resolveOutboundModel(vendorID, model string) string {
	return modelCatalog.ResolveOutboundModel(vendorID, model)
}

// applyModelAliases 给某供应商的模型清单换上别名（对外 ID）。
func applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return modelCatalog.ApplyAliases(vendorID, models)
}

// modelAliasRequest 是设置/清除别名的请求。
type modelAliasRequest struct {
	Model string `json:"model"`
	Alias string `json:"alias"`
}

// handleModelAlias 设置或清除某个模型的别名。
func handleModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body modelAliasRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}

	vendorID, bare := modelconfig.VendorForAliasScope(body.Model)
	if vendorID == "" || strings.TrimSpace(bare) == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "model 必须是 <vendor>:<模型 ID> 形式",
		})
	}
	alias := strings.TrimSpace(body.Alias)
	if strings.ContainsAny(alias, " \t\r\n") {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "别名不能包含空白字符",
		})
	}

	modelCatalog.SetAlias(vendorID, bare, alias)
	return jsonResponse(http.StatusOK, map[string]any{"ok": true})
}
