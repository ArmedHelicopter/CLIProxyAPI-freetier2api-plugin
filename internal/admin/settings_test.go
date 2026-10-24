package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// TestNormalizePromptMode 锁住全局提示词过滤模式的取值集合。
func TestNormalizePromptMode(t *testing.T) {
	valid := map[string]string{
		"":            "",
		"passthrough": PromptModePassThrough,
		" CUSTOM ":    PromptModeCustom,
		"append":      PromptModeAppend,
	}
	for raw, want := range valid {
		got, err := normalizePromptMode(&raw)
		if err != nil || got != want {
			t.Errorf("normalizePromptMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}

	bad := "overwrite"
	_, errBad := normalizePromptMode(&bad)
	pluginErr, okErr := errBad.(*core.PluginError)
	if !okErr || pluginErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("unsupported mode should be a 400 plugin error, got %T %v", errBad, errBad)
	}

	if got, err := normalizePromptMode(nil); err != nil || got != "" {
		t.Fatalf("absent field = %q, %v", got, err)
	}
}

// TestSettingsViewReportsEffectivePromptMode 验证 /status 回的是生效值（页面覆盖优先）。
func TestSettingsViewReportsEffectivePromptMode(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	if got := promptModeOf(t, service); got != PromptModePassThrough {
		t.Fatalf("默认模式 = %q, want passthrough", got)
	}

	deps.store.Mutate(func(current *state.State) { current.PromptMode = PromptModeAppend })
	if got := promptModeOf(t, service); got != PromptModeAppend {
		t.Fatalf("页面覆盖未生效: %q", got)
	}
}

func promptModeOf(t *testing.T, service *Service) string {
	t.Helper()
	raw, errMarshal := json.Marshal(service.settingsView())
	if errMarshal != nil {
		t.Fatalf("marshal settings: %v", errMarshal)
	}
	var settings struct {
		PromptMode string `json:"prompt_mode"`
	}
	if errUnmarshal := json.Unmarshal(raw, &settings); errUnmarshal != nil {
		t.Fatalf("unmarshal settings: %v", errUnmarshal)
	}
	if !strings.Contains(string(raw), `"prompt_mode"`) {
		t.Fatalf("settings 缺少 prompt_mode: %s", raw)
	}
	return settings.PromptMode
}
