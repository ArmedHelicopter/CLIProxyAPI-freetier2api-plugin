package tests

import (
	"testing"

	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// TestAppsHaveIsolatedRegistryAndState 验证 App 实例之间不共享注册表与状态。
func TestAppsHaveIsolatedRegistryAndState(t *testing.T) {
	first, errFirst := app.New(app.Options{Version: "a"})
	if errFirst != nil {
		t.Fatalf("new first: %v", errFirst)
	}
	second, errSecond := app.New(app.Options{Version: "b", Factories: app.BuiltinFactories()})
	if errSecond != nil {
		t.Fatalf("new second: %v", errSecond)
	}
	if len(first.Registry().Vendors()) != 0 {
		t.Fatalf("first app registry should be empty")
	}
	if len(second.Registry().Vendors()) == 0 {
		t.Fatalf("second app registry should contain builtins")
	}

	cfgFirst := app.DefaultConfig()
	cfgFirst.StateDir = t.TempDir()
	if errApply := first.ApplyConfig(cfgFirst); errApply != nil {
		t.Fatalf("apply first: %v", errApply)
	}
	cfgSecond := app.DefaultConfig()
	cfgSecond.StateDir = t.TempDir()
	if errApply := second.ApplyConfig(cfgSecond); errApply != nil {
		t.Fatalf("apply second: %v", errApply)
	}
	first.MutateState(func(state *state.State) {
		state.VendorSettings = map[string]string{"workbuddy_prompt_mode": "custom"}
	})
	if got := second.SnapshotState().VendorSettings["workbuddy_prompt_mode"]; got != "" {
		t.Fatalf("second app saw first app state: %q", got)
	}
}

// 保持 import core 被使用（防止未来删除能力断言测试时误删）。
var _ = core.ProviderKey
