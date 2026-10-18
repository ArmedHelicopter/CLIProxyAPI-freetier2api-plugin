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
	Configure(core.Settings{
		EnabledRealms:        []string{"cn"},
		SanitizeFingerprints: false,
		MachineSalt:          "salt-1",
		StateDir:             dir,
	}, map[string]string{
		"prompt_mode":    "custom",
		"prompt_file":    promptPath,
		"client_version": "9.9.9",
	})

	runtimeMu.RLock()
	cfg := runtimeCfg
	runtimeMu.RUnlock()
	if cfg.clientVersion != "9.9.9" {
		t.Fatalf("client version = %q", cfg.clientVersion)
	}
	if cfg.promptMode != PromptModeCustom || cfg.promptText != "custom prompt" {
		t.Fatalf("prompt = %q/%q", cfg.promptMode, cfg.promptText)
	}
	if cfg.sanitizeFingerprints {
		t.Fatal("sanitize flag not applied")
	}
	if currentInstallSalt() != "salt-1" {
		t.Fatalf("install salt = %q", currentInstallSalt())
	}
	if len(cfg.enabledRealms) != 1 || cfg.enabledRealms[0] != "cn" {
		t.Fatalf("realms = %#v", cfg.enabledRealms)
	}

	// 还原全局状态，避免影响同包其它测试。
	Configure(core.Settings{}, nil)
}
