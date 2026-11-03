package core

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// 任务类重试：429 是软限流，过几秒就好，必须重试。
//
// 之前只有 401 会重试，一次 429 就让账号白丢当天的签到（实测手动批量 8 个
// 账号挂掉 7 个，全是 429）。
func TestWithTaskRetryRetriesSoftRate(t *testing.T) {
	attempts := 0
	err := WithTaskRetry(context.Background(), func() error {
		attempts++
		if attempts < TaskRetryAttempts {
			return &Failure{Kind: FailureKindSoftRate, Status: http.StatusTooManyRequests}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("重试后应成功，实际 %v", err)
	}
	if attempts != TaskRetryAttempts {
		t.Fatalf("尝试次数 = %d, want %d", attempts, TaskRetryAttempts)
	}
}

// 参数错误重试不会变好：必须一次就停，别把上游多打两遍。
func TestWithTaskRetryStopsOnDefinitive(t *testing.T) {
	attempts := 0
	err := WithTaskRetry(context.Background(), func() error {
		attempts++
		return &Failure{Kind: FailureKindBadParams, Status: http.StatusBadRequest}
	})
	if attempts != 1 {
		t.Fatalf("确定类错误应只尝试一次，实际 %d 次", attempts)
	}
	if err == nil {
		t.Fatal("确定类错误必须原样返回")
	}
}

// ctx 取消后不再重试，返回 ctx 错误。
func TestWithTaskRetryHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	err := WithTaskRetry(ctx, func() error {
		attempts++
		return &Failure{Kind: FailureKindSoftRate, Status: http.StatusTooManyRequests}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，实际 %v", err)
	}
	if attempts != 1 {
		t.Fatalf("取消后不该再发请求，实际 %d 次", attempts)
	}
}
