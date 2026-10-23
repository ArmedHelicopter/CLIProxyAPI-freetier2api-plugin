package tests

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/vendors/zcode"
	"freetier2api-plugin/tests/testhost"
)

func TestZCodeActivityPreviewIsReadOnlyAndSupportsAPIKey(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "zcode-api.json", `{"vendor":"zcode","api_key":"sk-test","user_id":"u-test","profile":{"platform":"win32","arch":"x64","device_mid":"dev-test"}}`)
	var claimCalls int
	host.SetUpstream(func(method, rawURL, body string) testhost.UpstreamResponse {
		if method == http.MethodPost && rawURL == "https://zcode.z.ai/api/v1/event/report" {
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0}`}
		}
		if method == http.MethodGet && rawURL == "https://zcode.z.ai/api/v1/marketing/touch?seq=0" {
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0,"data":{"deliveries":[{"campaign_id":"c1","banner":{"background":{"args":{"zcode_plan":{"plan_id":"dev-day-1024","name":"ZCode 1024 Dev Day","priority":9}}}}}]}}`}
		}
		if rawURL == "https://zcode.z.ai/api/v1/zcode-plan/billing/claim" {
			claimCalls++
		}
		return testhost.UpstreamResponse{Status: 404, Body: `{"code":404}`}
	})

	raw, err := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: "POST", Path: "/v0/management/plugins/freetier2api/activities/preview",
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}}, Body: []byte(`{"account_id":"zcode-api"}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env abiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("%v raw=%s env=%+v", err, raw, env)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if !json.Valid(resp.Body) || string(resp.Body) == "" {
		t.Fatalf("invalid body: %s", resp.Body)
	}
	if claimCalls != 0 {
		t.Fatalf("activity preview invoked claim %d times", claimCalls)
	}
	_ = zcode.ClientAppVersion
}
