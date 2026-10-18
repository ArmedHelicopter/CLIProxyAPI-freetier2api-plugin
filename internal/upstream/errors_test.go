package upstream

import (
	"net/http"
	"testing"
)

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        *Error
		wantStatus int
		wantRetry  bool
	}{
		{"credential", &Error{Kind: KindCredential}, http.StatusUnauthorized, false},
		{"rate", &Error{Kind: KindRateLimit}, http.StatusTooManyRequests, true},
		{"quota", &Error{Kind: KindHardCredit}, http.StatusPaymentRequired, true},
		{"bad params", &Error{Kind: KindBadParams}, http.StatusBadRequest, false},
		{"status passthrough", &Error{Kind: KindClient, Status: http.StatusConflict}, http.StatusConflict, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.HTTPStatus(); got != tc.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tc.wantStatus)
			}
			if got := tc.err.Retryable(); got != tc.wantRetry {
				t.Fatalf("Retryable() = %v, want %v", got, tc.wantRetry)
			}
		})
	}
}
