package minimaxcode

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRequestWithCredentialRefreshRetriesOnce(t *testing.T) {
	calls, refreshes := 0, 0
	first := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("bad token"))}
	resp, err := requestWithCredentialRefreshUsing(first, func() (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("body"))}, nil
	}, func() error { refreshes++; return nil }, errors.New("original auth error"))
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response=%v err=%v", resp, err)
	}
	if calls != 1 || refreshes != 1 {
		t.Fatalf("calls=%d refreshes=%d", calls, refreshes)
	}
}

func TestRequestWithCredentialRefreshReturnsOriginalOnRefreshFailure(t *testing.T) {
	original := errors.New("original auth error")
	first := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("bad token"))}
	calls := 0
	resp, err := requestWithCredentialRefreshUsing(first, func() (*http.Response, error) { calls++; return nil, nil }, func() error { return errors.New("refresh failed") }, original)
	if resp != nil || !errors.Is(err, original) || calls != 0 {
		t.Fatalf("response=%v err=%v calls=%d", resp, err, calls)
	}
}

func TestRequestWithCredentialRefreshReturnsOriginalOnRetryFailure(t *testing.T) {
	original := errors.New("original auth error")
	first := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("bad token"))}
	calls := 0
	resp, err := requestWithCredentialRefreshUsing(first, func() (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("bad token"))}, nil
	}, func() error { return nil }, original)
	if resp != nil || !errors.Is(err, original) || calls != 1 {
		t.Fatalf("response=%v err=%v calls=%d", resp, err, calls)
	}
}
