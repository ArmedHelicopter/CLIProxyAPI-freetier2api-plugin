package zcode

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestMarketingHeadersCarryCredential(t *testing.T) {
	// JWT 优先：同一凭证同时有 JWT 与 API Key 时，只能带 JWT 的 Bearer。
	h := marketingHeaders(&Credential{
		JWTToken: "jwt-token",
		APIKey:   "sk-test",
		Profile:  DeviceProfile{Platform: "win32", Arch: "x64", Language: "en-US"},
	})
	if h["Authorization"] != "Bearer jwt-token" || h["x-api-key"] != "" {
		t.Fatalf("JWT headers = %#v", h)
	}
	if h["Accept"] != "application/json" || h["x-request-id"] == "" {
		t.Fatalf("missing request headers: %#v", h)
	}
	// 语言必须来自账号档案，而不是被硬编码的 zh-CN 覆盖。
	if got := h["X-Client-Language"]; got != "en-US" {
		t.Fatalf("X-Client-Language = %q, want en-US", got)
	}

	// 纯 API Key 凭证（控制台添加的形态）必须带 x-api-key，否则上游按匿名请求处理。
	h = marketingHeaders(&Credential{APIKey: "sk-test", Profile: DeviceProfile{Platform: "win32", Arch: "x64"}})
	if h["x-api-key"] != "sk-test" || h["Authorization"] != "" {
		t.Fatalf("API key headers = %#v", h)
	}
}

func TestParseMarketingTouchNormalizesPlans(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"deliveries":[
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"low","name":"Low","priority":1,"entitlements":[{"show_name":"GLM-5.3-Flash","grant_units":100,"period":"one_time"}]}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"high","name":"High","priority":9}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"HIGH","name":"High 重复","priority":9}}}}},
		{"banner":{"background":{"args":{"button":{"text":"learn more"}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"name":"没有 plan_id"}}}}}
	]}}`)

	plans, err := parseMarketingTouch(http.StatusOK, raw)
	if err != nil {
		t.Fatalf("parseMarketingTouch: %v", err)
	}
	// 没有 plan_id 的投递与重复 plan_id（大小写不同）都要被丢弃。
	if len(plans) != 2 {
		t.Fatalf("plans = %+v, want 2", plans)
	}
	// 优先级降序，同优先级按 plan_id 升序。
	if plans[0].PlanID != "high" || plans[1].PlanID != "low" {
		t.Fatalf("order = %q, %q", plans[0].PlanID, plans[1].PlanID)
	}
	if len(plans[1].Grants) != 1 || plans[1].Grants[0].Model != "GLM-5.3-Flash" {
		t.Fatalf("grants = %+v", plans[1].Grants)
	}
}

func TestParseMarketingTouchErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantDetail string
	}{
		{
			name:       "业务码非零",
			status:     http.StatusOK,
			body:       `{"code":4001,"msg":"today not active"}`,
			wantCode:   "zcode_activity_business",
			wantDetail: "today not active",
		},
		{
			name:       "HTTP 非 2xx",
			status:     http.StatusUnauthorized,
			body:       `{"code":401,"msg":"unauthorized"}`,
			wantCode:   "zcode_activity_http",
			wantDetail: "HTTP 401",
		},
		{
			name:       "响应不是 JSON",
			status:     http.StatusOK,
			body:       `<html>login</html>`,
			wantCode:   "zcode_activity_response",
			wantDetail: "login",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMarketingTouch(test.status, []byte(test.body))
			var pluginErr *core.PluginError
			if !errors.As(err, &pluginErr) {
				t.Fatalf("err = %v, want *core.PluginError", err)
			}
			if pluginErr.Code != test.wantCode || pluginErr.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("err = %+v", pluginErr)
			}
			if !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("err = %q, want detail %q", err, test.wantDetail)
			}
		})
	}
}
