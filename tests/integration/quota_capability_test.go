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

// TestQuotaUnsupportedAccountNeverStaysQuerying 覆盖不支持额度账号的展示契约。
//
// OpenCode ZEN 实现不了 QuotaProvider，因此：
//   - /status 的 supports_quota 必须是 false（页面据此显示「-- / --」）；
//   - /quotas 结果不得包含该账号，避免它出现在额度查询窗口；
//   - /status 仍然保留账号本身，供账号表展示。
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
	var quotaResponse struct {
		Results []struct {
			AuthID string `json:"auth_id"`
		} `json:"results"`
	}
	if errDecode := json.Unmarshal([]byte(resp.Body), &quotaResponse); errDecode != nil {
		t.Fatalf("decode /quotas response: %v", errDecode)
	}
	if len(quotaResponse.Results) != 0 {
		t.Fatalf("unsupported account must not enter quota results: %+v", quotaResponse.Results)
	}
	accounts = statusAccounts(t)
	if len(accounts) != 1 || accounts[0].Quota != nil {
		t.Fatalf("unsupported account should remain visible without quota result: %+v", accounts)
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
