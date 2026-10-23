package tests

// 本文件锁定账号概览「剩余 / 总额」列的状态语义。
//
// 真实事故：OpenCode ZEN 上游没有额度接口，它的账号在概览里永久停在
// 「查询中…」。根因不是查询失败，而是页面把「没有结果」一律当成「正在查询」，
// 于是「不支持额度」与「还没查」这两种状态在数据上完全无法区分。
//
// 修复把它拆成两件事：
//  1. 后端给出静态能力 supports_quota（接口断言）；
//  2. 额度缓存**成功与失败都写**，让「不支持」也能作为结果被记住。
//
// 两个断言都必须通过，否则页面会重新退化成「一直查询中」。

import (
	"encoding/json"
	"net/http"
	"testing"
)

// managementCall 与 managementCallWithBody 见 testutil_test.go。

// statusAccounts 拉一次 /status 并取出账号列表。
func statusAccounts(t *testing.T) []struct {
	AuthID        string `json:"auth_id"`
	VendorID      string `json:"vendor_id"`
	SupportsQuota bool   `json:"supports_quota"`
	Quota         *struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"quota"`
} {
	t.Helper()
	resp := managementCall(t, "GET", "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Accounts []struct {
			AuthID        string `json:"auth_id"`
			VendorID      string `json:"vendor_id"`
			SupportsQuota bool   `json:"supports_quota"`
			Quota         *struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			} `json:"quota"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status: %v (%s)", errUnmarshal, resp.Body)
	}
	return payload.Accounts
}

// TestQuotaUnsupportedAccountNeverStaysQuerying 覆盖事故本身。
//
// OpenCode ZEN 实现不了 QuotaProvider，因此：
//   - /status 的 supports_quota 必须是 false（页面据此显示「-- / --」）；
//   - 查过一次额度后，结果（ok=false）必须被缓存下来——否则下次 /status
//     又回到「没有结果」，页面就重新退化成「查询中…」。
func TestQuotaUnsupportedAccountNeverStaysQuerying(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)

	accounts := statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(accounts), accounts)
	}
	account := accounts[0]
	if account.VendorID != "opencodezen" {
		t.Fatalf("vendor = %q, want opencodezen", account.VendorID)
	}
	if account.SupportsQuota {
		t.Fatal("opencodezen has no quota API: supports_quota must be false, " +
			"otherwise the console shows「查询中…」forever")
	}
	if account.Quota != nil {
		t.Fatalf("quota must be absent before the first query: %+v", account.Quota)
	}

	// 查一次额度：不支持也要产生一条「已确定」的结果。
	resp := managementCall(t, "POST", "/quotas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}

	accounts = statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account after quota query, got %d", len(accounts))
	}
	if accounts[0].Quota == nil {
		t.Fatal("the unsupported-quota result must be cached: without it the console " +
			"falls back to「查询中…」on every status refresh")
	}
	if accounts[0].Quota.OK {
		t.Fatalf("unsupported quota must report ok=false, got %+v", accounts[0].Quota)
	}
	if accounts[0].Quota.Message == "" {
		t.Fatal("unsupported quota must carry an explanatory message")
	}
}

// TestClineAlsoReportsNoQuota 确认修复不是 OpenCode 专用补丁。
//
// Cline 与 OpenCode ZEN 同样没有额度接口。两者都必须在 /status 里报
// supports_quota=false，页面才会立即显示「-- / --」而不是「查询中…」。
func TestClineAlsoReportsNoQuota(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "cline-test.json", `{"type":"cline","refreshToken":"workos:rt-test"}`)

	accounts := statusAccounts(t)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(accounts), accounts)
	}
	if accounts[0].VendorID != "cline" {
		t.Fatalf("vendor = %q, want cline", accounts[0].VendorID)
	}
	if accounts[0].SupportsQuota {
		t.Fatal("cline has no quota API: supports_quota must be false")
	}
	if accounts[0].Quota != nil {
		t.Fatalf("quota must be absent before any query: %+v", accounts[0].Quota)
	}
}
