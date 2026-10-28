package autoclaw

// 本文件锁定国际版模型目录（autoclaw-model-config）的响应形态解析。
//
// 上游实测返回的是**顶层 models 数组**（没有 code/msg/data 信封）：
//
//	{"models":[{"id":"zai_auto",...,"input":["image","text"]}, ...]}
//
// 旧实现只认 data.models：JSON 解析成功、模型却是 0 条，于是宿主把该凭证
// 当作「没有可用模型」淘汰，控制台模型列表整片为空，而日志里只有一行
// "probe returned 0 model(s)"。这里同时锁住两种形态与「空清单要报错」。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelConfigTopLevelFixture 是上游实测响应的结构（数值按真实值）。
const modelConfigTopLevelFixture = `{"models":[
 {"id":"zai_auto","name":"Auto","reasoning":true,"input":["image","text"],
  "contextWindow":1048576,"maxTokens":131072,
  "tooltip":"Automatically picks the best model for your task."},
 {"id":"zai_auto-fast","name":"Auto Fast","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":131072},
 {"id":"zaicoding_glm-5.3","name":"GLM-5.3","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":307200,"tooltip":"GLM-5.3 coding"}]}`

// modelConfigEnvelopeFixture 是旧信封形态。
const modelConfigEnvelopeFixture = `{"code":0,"msg":"SUCCESS","data":{"models":[
 {"id":"legacy-model","name":"Legacy","contextWindow":200000,"maxTokens":8192}]}}`

// serveModels 起一个只服务 ModelsConfigPath 的桩，并断言带了鉴权头。
func serveModels(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, ModelsConfigPath)
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "tok-abc") {
			t.Errorf("Authorization = %q, want token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestFetchModelsParsesTopLevelModels(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigTopLevelFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3（顶层 models 未被识别）", len(models))
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	auto, okAuto := byID["zai_auto"]
	if !okAuto {
		t.Fatalf("missing zai_auto, got %v", byID)
	}
	if auto.Name != "Auto" || auto.ContextWindow != 1048576 || auto.MaxOutputTokens != 131072 || !auto.IsReasoning {
		t.Fatalf("zai_auto metadata = %+v", auto)
	}
	if !auto.SupportsImages || !auto.SupportsTools {
		t.Fatalf("zai_auto capabilities = %+v, want images+tools", auto)
	}
	if glm := byID["zaicoding_glm-5.3"]; glm.SupportsImages {
		t.Fatalf("zaicoding_glm-5.3 (input=text) must not claim image support")
	}
}

func TestFetchModelsStillParsesEnvelope(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigEnvelopeFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 1 || models[0].ID != "legacy-model" {
		t.Fatalf("models = %+v, want legacy-model", models)
	}
}

func TestFetchModelsErrorsOnUnknownShape(t *testing.T) {
	useLoopbackHost(t)
	// 解析成功但没有任何模型：必须报错，而不是静默返回空清单。
	baseURL := serveModels(t, `{"code":0,"msg":"SUCCESS","data":{}}`)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch == nil {
		t.Fatalf("expected error for empty model list, got %+v", models)
	}
	if !strings.Contains(errFetch.Error(), "0 model") {
		t.Fatalf("error = %v, want it to mention 0 model", errFetch)
	}
}
