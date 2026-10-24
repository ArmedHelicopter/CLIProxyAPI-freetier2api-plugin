package zcode

import (
	"context"
	"net/http"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
)

// TestGateSerializesPerCredential 验证同一凭据的闸门串行：持有者未释放时
// 后来者拿不到资格，释放后可再次获得；不同凭据互不阻塞。
func TestGateSerializesPerCredential(t *testing.T) {
	gate := gateFor("user-a")

	if errAcquire := gate.acquire(context.Background()); errAcquire != nil {
		t.Fatalf("first acquire: %v", errAcquire)
	}

	// 持有中：短超时内第二次获取必须失败（排队中，而不是立即拿锁）。
	ctxQuick, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if errSecond := gate.acquire(ctxQuick); errSecond == nil {
		gate.release()
		t.Fatal("second acquire must block while the gate is held")
	}

	// 释放后立即可再取。
	gate.release()
	if errThird := gate.acquire(context.Background()); errThird != nil {
		t.Fatalf("acquire after release: %v", errThird)
	}
	gate.release()

	// 不同凭据的闸门互不影响。
	if errOther := gateFor("user-b").acquire(context.Background()); errOther != nil {
		t.Fatalf("other credential acquire blocked: %v", errOther)
	}
	gateFor("user-b").release()
}

// TestAcquireGateRespectsCancelledContext 验证宿主取消请求时排队者立即退出。
func TestAcquireGateRespectsCancelledContext(t *testing.T) {
	gate := gateFor("user-cancel")
	if errAcquire := gate.acquire(context.Background()); errAcquire != nil {
		t.Fatalf("first acquire: %v", errAcquire)
	}
	defer gate.release()

	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errAcquire := acquireGateForCredential(ctxCancelled, &Credential{UserID: "user-cancel"}); errAcquire == nil {
		t.Fatal("cancelled context must not acquire the gate")
	}
}

// TestClassifyConcurrencyLimitTextAt500 验证 500 包装的并发超限文案归 SoftRate，
// 而不是落进 5xx 上游故障分支。
func TestClassifyConcurrencyLimitTextAt500(t *testing.T) {
	body := `{"message":"model concurrency limit exceeded","type":"server_error","code":"internal_server_error"}`
	failure := Classify(http.StatusInternalServerError, body)
	if failure.Kind != core.FailureKindSoftRate {
		t.Fatalf("kind = %v, want SoftRate", failure.Kind)
	}
}
