package tests

// 本文件覆盖「热重载后模型消失」这条故障链：
// 进程内模型缓存挂在 App 实例上，重载后随旧实例一起消失；宿主重新加载每个
// 凭证都会现场探测一轮，上游一次抖动（429/超时/5xx）就会让整家供应商从
// /v1/models 上消失。插件必须回落到 state.json 里最近一次成功的清单，
// 而不是把空清单交给宿主。

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
)

// qoderCredentialJSON 是最小可用的 Qoder 凭证（UID 由文件名提供）。
const qoderCredentialJSON = `{"type":"freetier","vendor":"qodercn","region":"cn",` +
	`"device_token":"dt-abc","refresh_token":"drt-xyz","label":"nick"}`

// qoderModelsUpstream 是 Qoder 模型清单的正常应答。
func qoderModelsUpstream(string, string, string) fakeUpstreamResponse {
	return fakeUpstreamResponse{
		Status: http.StatusOK,
		Body:   `{"assistant":[{"key":"glm-5.3","display_name":"GLM-5.3","enable":true}]}`,
	}
}

// modelIDsForAuth 调一次 model.for_auth 并返回模型 ID 集合。
func modelIDsForAuth(t *testing.T, authID string) map[string]bool {
	t.Helper()
	raw := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: authID})
	var resp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		t.Fatalf("decode model.for_auth: %v", errUnmarshal)
	}
	set := make(map[string]bool, len(resp.Models))
	for _, model := range resp.Models {
		set[model.ID] = true
	}
	return set
}

// reloadPlugin 用同一个 state_dir 重建 App 实例，模拟宿主热重载插件。
//
// 顺序与真实入口 plugin.register 一致：ApplyConfig → loadState →
// applyModelCatalog，后者把 state.json 里的模型缓存回灌进新实例。
func reloadPlugin(t *testing.T, cfg app.Config) {
	t.Helper()
	if testApp != nil {
		testApp.StopBackgroundWork()
		testApp.WaitPluginStreams(0)
	}
	instance, errNew := app.New(app.Options{Version: "test", Factories: app.BuiltinFactories()})
	if errNew != nil {
		t.Fatalf("reload app: %v", errNew)
	}
	testApp = instance
	t.Cleanup(func() {
		instance.StopBackgroundWork()
		instance.WaitPluginStreams(0)
	})
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("reload apply config: %v", errApply)
	}
	if _, errState := instance.LoadState(cfg); errState != nil {
		t.Fatalf("reload state: %v", errState)
	}
	instance.ApplyModelCatalog(cfg)
}

// TestModelsForAuthFallsBackToCachedCatalogAfterReload 验证重载后上游抖动不掉模型。
func TestModelsForAuthFallsBackToCachedCatalogAfterReload(t *testing.T) {
	host := installFakeHost(t)
	cfg := setupTestPlugin(t)
	host.addAccount("1", "qodercn-nick1456600333.json", qoderCredentialJSON)
	host.setUpstream(qoderModelsUpstream)

	before := modelIDsForAuth(t, "1")
	if !before["qodercn-glm-5.3"] {
		t.Fatalf("首轮探测应拿到上游模型: %v", before)
	}

	reloadPlugin(t, cfg)

	// 上游开始抖动：新实例的这一轮探测必然失败。
	host.setUpstream(func(string, string, string) fakeUpstreamResponse {
		return fakeUpstreamResponse{Status: http.StatusInternalServerError, Body: `{"error":"boom"}`}
	})

	after := modelIDsForAuth(t, "1")
	if !after["qodercn-glm-5.3"] {
		t.Fatalf("热重载后探测失败必须回落到 state.json 缓存，实际拿到 %v", after)
	}
}
