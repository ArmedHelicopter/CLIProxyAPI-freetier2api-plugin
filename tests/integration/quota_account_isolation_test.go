package tests

// 本文件锁定额度缓存的账号隔离。
//
// 真实事故：OpenCode ZEN Free 账号在概览里显示出了 WorkBuddy 的「剩余 / 总额」。
// 根因不在 UI，而在额度缓存的键：缓存同时按 Name / Label / AuthIndex 落库，
// 这些字段跨供应商并不唯一（两个账号同昵称即可命中），于是不支持额度查询的
// 账号也会读到别家账号的结果。

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQuotaCacheIsolatedPerAccount 复现并锁定「跨账号串额度」。
//
// 两个账号故意使用同一个展示名：旧实现会把 Trae 的额度串给 OpenCode ZEN。
func TestQuotaCacheIsolatedPerAccount(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-shared.json", "{\"vendor\":\"traecn\",\"region\":\"cn\",\"access_token\":\"test-access-token\",\"uid\":\"2487573687317868\",\"label\":\"shared-account\"}")
	host.addAccount("2", "opencodezen-shared.json", "{\"api_key\":\"sk-test-opencode\",\"label\":\"shared-account\"}")
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: "{\"code\":404}"}
	})

	if resp := managementCall(t, "POST", "/quotas"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.VendorID == "traecn" && account.Quota != nil && account.Quota.OK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("trae quota should be cached, got %+v", statusAccounts(t))
	}

	for _, account := range statusAccounts(t) {
		if account.VendorID != "opencodezen" {
			continue
		}
		if account.SupportsQuota {
			t.Fatal("opencodezen must report supports_quota=false")
		}
		if account.Quota != nil {
			t.Fatalf("opencodezen must never inherit another account's quota: %+v", account.Quota)
		}
	}
}
