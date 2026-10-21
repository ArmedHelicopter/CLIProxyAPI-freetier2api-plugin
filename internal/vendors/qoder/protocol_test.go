package qoder

import (
	"testing"
)

func TestMapModel(t *testing.T) {
	// MapModel 只处理「索引未命中」的兜底：本插件自己注册的模型由
	// Bridge.resolveModel 先走 modelIndex（见 TestBridgeResolveModel）。
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "gmodel", "gmodel"},
		{"codex", "kmodel_latest", "kmodel_latest"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
		{"codex", "Performance", "performance"},   // 大小写归一
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModel(t *testing.T) {
	var index modelIndex
	index.replace(testModels())
	bridge := &Bridge{models: &index}

	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		// 对外 slug → 上游 key
		{"codex", "deepseek-v4-pro", "dmodel"},
		{"codex", "qwen3.8-max", "qmodel_38max"},
		{"codex", "glm-5.3", "gmodel"},
		// 大小写不敏感
		{"codex", "DeepSeek-V4-Pro", "dmodel"},
		// 旧上游 key 仍可直传（向后兼容）
		{"codex", "dmodel", "dmodel"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		// 索引未命中 → 内置映射兜底
		{"claude", "claude-sonnet-4-6", "gmodel"},
	}
	for _, c := range cases {
		if got := bridge.resolveModel(c.agent, c.model); got != c.expected {
			t.Errorf("resolveModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModelWithoutIndex(t *testing.T) {
	// 没有索引时（测试/独立构造）退回 MapModel，行为与变更前一致。
	bridge := &Bridge{}
	if got := bridge.resolveModel("claude", "claude-sonnet-4-6"); got != "gmodel" {
		t.Errorf("resolveModel without index = %q; want gmodel", got)
	}
}
