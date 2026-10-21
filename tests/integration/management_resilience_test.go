package tests

// 本文件锁定管理页「刷新即取数」的两条韧性约束。
//
// 真实事故：Trae 国内版的「剩余 / 总额」与模型列表时有时无。根因不是上游
// 限流（直连上游 200/0.5s），而是两处生命周期绑错了对象：
//
//  1. 额度批量查询挂在管理 RPC 的 context 上。页面刷新会让宿主取消这次 RPC，
//     整轮查询半途而废，一个账号都没写进缓存，账号表于是停在「查询中…」。
//  2. 模型清单缓存只在 static 探测与手动刷新时写入。插件重载后若探测未跑完，
//     控制台就整家供应商缺模型；而前端只在「合计为 0」时才自动刷新。
//
// 对应修复：额度查询改到后台作用域（单轮 singleflight），模型列表在缓存为
// 空时按供应商补探测（singleflight + 冷却）。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// traeQuotaFixture 是 Trae 额度接口的最小响应（product_type=2 为积分包）。
const traeQuotaFixture = `{"user_entitlement_pack_list":[
 {"entitlement_base_info":{"product_type":2,"quota":{"credits_limit":4000}},"usage":{"credits_amount":1.5}}
]}`

// addTraeAccount 注册一个 Trae 国内版账号，额度接口由假宿主应答。
func addTraeAccount(t *testing.T, host *fakeHost) {
	t.Helper()
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})
}

// TestQuotaRunSurvivesManagementCancel 验证额度查询不随管理 RPC 取消而中断。
//
// 管理上下文被取消后，后台那一轮必须继续跑完并写缓存，下一次 /status 就能
// 拿到额度结果，而不是重新回到「查询中…」。
func TestQuotaRunSurvivesManagementCancel(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)

	// 被取消的 RPC：宿主取消时会直接关掉请求上下文。
	resp := managementCallWithCancel(t, "POST", "/quotas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}

	// 后台轮次仍在跑：等缓存出现后，/status 必须带额度结果。
	if !waitFor(t, 5*time.Second, func() bool {
		accounts := statusAccounts(t)
		return len(accounts) == 1 && accounts[0].Quota != nil
	}) {
		t.Fatal("quota batch must finish in the background even when the management RPC is cancelled")
	}
	accounts := statusAccounts(t)
	if !accounts[0].Quota.OK {
		t.Fatalf("trae quota should succeed, got %+v", accounts[0].Quota)
	}
}

// TestConcurrentQuotaRequestsShareOneRun 验证 singleflight：在本轮尚未结束时
// 连续刷新页面（宿主收到多个 /quotas）不会把同一批账号重复打向上游。
func TestConcurrentQuotaRequestsShareOneRun(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return fakeUpstreamResponse{Status: http.StatusOK, Body: traeQuotaFixture}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	done := make(chan struct{}, 4)
	go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	<-started // 第一轮已进入上游，后续请求应等待同一轮
	for i := 0; i < 3; i++ {
		go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	}
	time.Sleep(50 * time.Millisecond) // 让后三个请求拿到同一个进行中的轮次
	close(release)
	for i := 0; i < 4; i++ {
		<-done
	}
	if calls := host.UpstreamCalls(); calls != 1 {
		t.Fatalf("quota upstream calls = %d, want 1 (singleflight)", calls)
	}
}

// TestConsoleModelsProbeWhenCacheEmpty 验证控制台在缓存为空时补探测。
//
// 场景：插件刚重载，宿主还没跑 static 探测，但凭证已加载（active）。此时
// /models 必须主动探测一次把模型列出来，而不是整家供应商缺模型。
func TestConsoleModelsProbeWhenCacheEmpty(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	resp := managementCall(t, "GET", "/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/models = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Models []struct {
			ID       string `json:"id"`
			VendorID string `json:"vendor_id"`
		} `json:"models"`
		PendingProbes int `json:"pending_probes"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode models: %v (%s)", errUnmarshal, resp.Body)
	}
	var found bool
	for _, model := range payload.Models {
		if model.ID == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("console models must include the probed vendor models, got %+v (pending=%d)",
			payload.Models, payload.PendingProbes)
	}
}

// TestModelsForAuthFillsConsoleCache 验证 model.for_auth 顺手写入控制台缓存。
//
// 宿主加载凭证走的就是这条 RPC；写入后控制台即便不做补探测也能读到清单。
func TestModelsForAuthFillsConsoleCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"for-auth-only","name":"For Auth"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})

	models := testApp.RegisteredModels("workbuddycn")
	var found bool
	for _, model := range models {
		if model.ID == "for-auth-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.for_auth must fill the console cache, got %+v", models)
	}
}

// managementCallWithCancel 发起一次管理调用，并模拟宿主在请求发出后立即取消。
func managementCallWithCancel(t *testing.T, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: method,
		Path:   "/v0/management/plugins/freetier2api" + path,
		Headers: http.Header{
			"X-Host-Callback-Id": []string{testCallbackID},
		},
		Context: ctx,
	}))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	var env abiEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var resp pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode management response: %v (raw=%s)", errUnmarshal, env.Result)
	}
	return resp
}
