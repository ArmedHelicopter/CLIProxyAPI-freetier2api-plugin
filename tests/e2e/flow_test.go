package e2e

// 本文件是完整 ABI 端到端冒烟：注册 → 凭证解析 → 模型 → 执行。
// 具体供应商行为由 tests/ 集成测试覆盖，这里只验证 App 入口链路可达。

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/tests/testhost"
)

func TestPluginABIFlow(t *testing.T) {
	host := testhost.New()
	hostbridge.Configure(host.Call)
	t.Cleanup(func() { hostbridge.Configure(nil) })

	instance, errNew := app.New(app.Options{Version: "test", Factories: app.BuiltinFactories()})
	if errNew != nil {
		t.Fatalf("new app: %v", errNew)
	}
	t.Cleanup(func() { instance.StopBackgroundWork(); instance.WaitPluginStreams(0) })

	cfg := app.DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	raw, errHandle := instance.HandleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"state_dir: `+cfg.StateDir+`\nlog_level: error\n"}`))
	if errHandle != nil {
		t.Fatalf("register: %v", errHandle)
	}
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("register envelope: %s %v", raw, err)
	}
}
