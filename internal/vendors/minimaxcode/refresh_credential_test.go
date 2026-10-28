package minimaxcode

import (
	"context"
	"encoding/json"
	"freetier2api-plugin/internal/hostbridge"
	"net/http"
	"strings"
	"testing"
)

func TestRefreshCredentialUsesOverrideAndRotatesTokens(t *testing.T) {
	called := false
	hostbridge.Configure(func(_, method string, _ any) (json.RawMessage, error) {
		if method != "host.http.do" {
			t.Fatalf("method = %s", method)
		}
		called = true
		body := []byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":7200}`)
		return json.Marshal(map[string]any{"status_code": 200, "headers": map[string][]string{"Content-Type": {"application/json"}}, "body": body})
	})
	defer hostbridge.Configure(nil)
	cred := &Credential{Region: RegionCN, AuthMode: "oauth", AccessToken: "old-access", RefreshToken: "old-refresh"}
	updated, changed, err := RefreshCredential(context.Background(), cred, "http://test.local")
	if err != nil || !changed || !called {
		t.Fatalf("refresh = %v %t called=%v", err, changed, called)
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
