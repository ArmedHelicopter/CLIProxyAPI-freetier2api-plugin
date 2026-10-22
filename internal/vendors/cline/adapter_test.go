package cline

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"cline_base_url": "http://cline.test"})
	if v.baseURL != "http://cline.test" {
		t.Fatalf("baseURL = %q", v.baseURL)
	}
}

// TestModelCreditsFromCostTier 锁定控制台的「费用倍率」列取计费档。
//
// Cline 上游不提供数值倍率，最精确的成本信息就是 free / pass 两档；
// 未知档必须不出现条目（控制台显示「—」），不能伪装成免费。
func TestModelCreditsFromCostTier(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.modelsToPluginAPI([]Model{
		{ID: "cline-free/glm-5.3", Name: "GLM", Cost: CostFree},
		{ID: "cline-pass/glm-5.3", Name: "GLM", Cost: CostPass},
		{ID: "cline-unknown/x", Name: "X"},
	})

	credits := v.ModelCredits()
	if got := credits["cline-free/glm-5.3"]; got != "免费" {
		t.Errorf("free tier = %q; want 免费", got)
	}
	if got := credits["cline-pass/glm-5.3"]; got != "订阅" {
		t.Errorf("pass tier = %q; want 订阅", got)
	}
	if _, ok := credits["cline-unknown/x"]; ok {
		t.Error("unknown cost tier must not be reported as a credits value")
	}
}
