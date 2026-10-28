package zcode

import (
	"testing"
)

func TestHasImageModality(t *testing.T) {
	if !hasImageModality([]string{"text", "IMAGE"}) {
		t.Fatal("expected image modality to be detected case-insensitively")
	}
	if hasImageModality([]string{"text", "audio"}) {
		t.Fatal("audio-only modality must not be treated as image support")
	}
}

func TestModelsToPluginAPIPreservesImageCapability(t *testing.T) {
	models := modelsToPluginAPI([]Model{{ID: "GLM-5.3-Flash", SupportsImages: true}})
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	if len(models[0].SupportedInputModalities) != 2 || models[0].SupportedInputModalities[1] != "image" {
		t.Fatalf("modalities = %#v, want [text image]", models[0].SupportedInputModalities)
	}
}

func TestParseConfigModels(t *testing.T) {
	body := []byte(`{"code":0,"data":{"builtinModels":[
		{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
		 "reasoning":{"levels":{"low":{},"high":{}}}},
		{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,
		 "capabilities":{"input":["text"]}}
	]}}`)
	models := parseConfigModels(body)
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].ID != "GLM-5.3" || !models[0].IsReasoning {
		t.Fatalf("unexpected first model: %+v", models[0])
	}
	if !models[1].SupportsImages {
		t.Fatalf("GLM-5.3-Flash 应支持图片: %+v", models[1])
	}
	if models[0].ContextWindow != 1000000 || models[0].MaxTokens != 128000 {
		t.Fatalf("元数据未解析: %+v", models[0])
	}
}
