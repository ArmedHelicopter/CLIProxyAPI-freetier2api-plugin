package workbuddy

import (
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestConfigureAppliesPromptAndUADefaults(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("custom prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(core.Env{})
	rt.configure(core.Settings{
		EnabledRealms: []string{"cn"},
		MachineSalt:   "salt-1",
		StateDir:      dir,
	}, map[string]string{
		"workbuddy_prompt_mode":           "custom",
		"workbuddy_prompt_file":           promptPath,
		"workbuddy_client_version":        "9.9.9",
		"workbuddy_sanitize_fingerprints": "false",
	})

	cfg := *rt.cfg.Load()
	if cfg.clientVersion != "9.9.9" {
		t.Fatalf("client version = %q", cfg.clientVersion)
	}
	if cfg.promptMode != PromptModeCustom || cfg.promptText != "custom prompt" {
		t.Fatalf("prompt = %q/%q", cfg.promptMode, cfg.promptText)
	}
	if cfg.sanitizeFingerprints {
		t.Fatal("sanitize flag not applied")
	}
	if *rt.salt.Load() != "salt-1" {
		t.Fatalf("install salt = %q", *rt.salt.Load())
	}
	if len(cfg.enabledRealms) != 1 || cfg.enabledRealms[0] != "cn" {
		t.Fatalf("realms = %#v", cfg.enabledRealms)
	}

}

// TestWorkBuddyRuntimeIsolated 验证两个运行时的提示词、档位表与机器盐互不污染。
func TestWorkBuddyRuntimeIsolated(t *testing.T) {
	first := newRuntime(core.Env{})
	second := newRuntime(core.Env{})

	first.configure(core.Settings{MachineSalt: "salt-first"}, map[string]string{
		"workbuddy_prompt_mode": "custom",
	})
	second.configure(core.Settings{MachineSalt: "salt-second"}, map[string]string{
		"workbuddy_prompt_mode": "append",
	})
	first.setEffortTables(
		map[string]string{"glm-5.2": "high"},
		map[string][]string{"glm-5.2": {"low", "high"}},
	)
	second.setEffortTables(
		map[string]string{"glm-5.2": "low"},
		map[string][]string{"glm-5.2": {"low"}},
	)

	if got := *first.salt.Load(); got != "salt-first" {
		t.Fatalf("first salt = %q", got)
	}
	if got := *second.salt.Load(); got != "salt-second" {
		t.Fatalf("second salt = %q", got)
	}
	if got := first.prompt.Load().Mode; got != "custom" {
		t.Fatalf("first prompt mode = %q", got)
	}
	if got := second.prompt.Load().Mode; got != "append" {
		t.Fatalf("second prompt mode = %q", got)
	}
	defaults, supported := first.effortTables()
	if defaults["glm-5.2"] != "high" || len(supported["glm-5.2"]) != 2 {
		t.Fatalf("first effort tables = %#v / %#v", defaults, supported)
	}
	defaults, supported = second.effortTables()
	if defaults["glm-5.2"] != "low" || len(supported["glm-5.2"]) != 1 {
		t.Fatalf("second effort tables = %#v / %#v", defaults, supported)
	}
}
