package minimaxcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freetier2api-plugin/internal/hostbridge"
)

func TestRefreshCredentialUsesOverrideAndRotatesTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpToken || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != RefreshGrantType || r.Form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("unexpected form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 7200})
	}))
	defer server.Close()
	hostbridge.Configure(func(_, method string, payload any) (json.RawMessage, error) {
		if method != "host.http.do" {
			return nil, nil
		}
		_ = payload
		return json.RawMessage(`{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"eyJhY2Nlc3NfdG9rZW4iOiJuZXctYWNjZXNzIiwicmVmcmVzaF90b2tlbiI6Im5ldy1yZWZyZXNoIiwiZXhwaXJlc19pbiI6NzIwMH0="}`), nil
	})
	defer hostbridge.Configure(nil)
	cred := &Credential{Region: RegionCN, AuthMode: "oauth", AccessToken: "old-access", RefreshToken: "old-refresh"}
	updated, changed, err := RefreshCredential(context.Background(), cred, server.URL)
	if err != nil || !changed {
		t.Fatalf("refresh = %v %t", err, changed)
	}
	if updated.AccessToken != "new-access" || updated.RefreshToken != "new-refresh" || updated.ExpiresAt <= 0 {
		t.Fatalf("updated credential = %+v", updated)
	}
}

func TestMergeStorageJSONPreservesIdentityAndCustomFields(t *testing.T) {
	raw := []byte(`{"vendor":"minimaxcodecn","region":"cn","access_token":"old","refresh_token":"old-r","user_id":"u1","label":"My MiniMax","disabled":true,"note":"keep"}`)
	cred, err := ParseCredential(raw, RegionCN)
	if err != nil {
		t.Fatal(err)
	}
	cred.AccessToken, cred.RefreshToken = "new", "new-r"
	merged, err := MergeStorageJSON(raw, cred)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "new" || got["refresh_token"] != "new-r" || got["user_id"] != "u1" || got["label"] != "My MiniMax" || got["note"] != "keep" || got["disabled"] != true {
		t.Fatalf("merged storage lost identity/custom fields: %s", merged)
	}
}

func TestClassifyRefreshFailureRequiresReauthorization(t *testing.T) {
	err := classifyRefreshFailure(http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "this refresh token can no longer be used"})
	if err == nil || !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("error = %v", err)
	}
}
