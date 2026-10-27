package minimaxcode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshCredentialRejectsRevokedRefreshTokenClearly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != EpToken {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"this refresh token can no longer be used, start a new authorization"}`))
	}))
	defer server.Close()

	_ = server
	err := classifyRefreshFailure(http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "this refresh token can no longer be used"})
	if err == nil || !strings.Contains(err.Error(), "重新授权登录") {
		t.Fatalf("err = %v, want clear reauthorization error", err)
	}
}
