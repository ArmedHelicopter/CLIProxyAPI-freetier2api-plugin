package admin

import (
	"context"
	"sync"
	"testing"
	"time"

	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
)

// 手动批量必须按厂商串行。
//
// 上游 WAF 看的是同 IP 的突发速率：6 路并发、零间隔的批量在实测里一次点击
// 就把三个厂商同时打进 429（8 个账号挂掉 7 个）。这里断言「同厂商任意时刻
// 只有 1 个请求在飞」，跨厂商仍然并发。
func TestRunGroupedBatchSerializesPerVendor(t *testing.T) {
	vendorA := &fakeVendor{Meta: core.Meta{VendorID: "alpha"}}
	vendorB := &fakeVendor{Meta: core.Meta{VendorID: "beta"}}
	accounts := []auth.Account{
		{Vendor: vendorA, Credential: &core.Credential{FileID: "a1"}},
		{Vendor: vendorB, Credential: &core.Credential{FileID: "b1"}},
		{Vendor: vendorA, Credential: &core.Credential{FileID: "a2"}},
	}

	var mu sync.Mutex
	active := map[string]int{}
	peak := map[string]int{}
	results := runGroupedBatch(context.Background(), accounts, 4, 0, func(account auth.Account) accountActionResult {
		id := account.Vendor.ID()
		mu.Lock()
		active[id]++
		if active[id] > peak[id] {
			peak[id] = active[id]
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		active[id]--
		mu.Unlock()
		return accountActionResult{Message: account.Credential.FileIDValue()}
	})

	if peak["alpha"] != 1 {
		t.Fatalf("同厂商并发峰值 = %d, want 1", peak["alpha"])
	}
	if len(results) != len(accounts) {
		t.Fatalf("结果数 = %d, want %d", len(results), len(accounts))
	}
	// 结果必须与入参下标对齐，否则控制台会把结果串到别的账号上。
	if results[0].Message != "a1" || results[2].Message != "a2" {
		t.Fatalf("结果顺序错位：%q / %q", results[0].Message, results[2].Message)
	}
}

// 取消后不再往下打上游：剩下的账号只标「已取消」。
func TestRunGroupedBatchStopsAfterCancel(t *testing.T) {
	vendor := &fakeVendor{Meta: core.Meta{VendorID: "alpha"}}
	accounts := []auth.Account{
		{Vendor: vendor, Credential: &core.Credential{FileID: "a1"}},
		{Vendor: vendor, Credential: &core.Credential{FileID: "a2"}},
		{Vendor: vendor, Credential: &core.Credential{FileID: "a3"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	results := runGroupedBatch(ctx, accounts, 1, time.Hour, func(auth.Account) accountActionResult {
		calls++
		cancel()
		return accountActionResult{Message: "ok"}
	})

	if calls != 1 {
		t.Fatalf("取消后仍发起了 %d 次调用, want 1", calls)
	}
	if results[1].Message != "已取消" || results[2].Message != "已取消" {
		t.Fatalf("剩余账号应标记已取消：%q / %q", results[1].Message, results[2].Message)
	}
}
