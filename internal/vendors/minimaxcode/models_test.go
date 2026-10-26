package minimaxcode

import (
	"context"
	"testing"
	"time"
)

func TestFetchModelsOnlyReturnsExplicitSafeBootstrapModel(t *testing.T) {
	models, err := FetchModels(context.Background(), RegionCN)
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "minimax-agent" {
		t.Fatalf("FetchModels() = %#v, want only minimax-agent bootstrap model", models)
	}
}

func TestResolveWorkspaceModelEmptyListDoesNotFallback(t *testing.T) {
	cache := &workspaceCache{entries: map[string]workspaceCacheEntry{
		"test-empty-workspace|https://agent.minimaxi.com": {models: []workspaceModel{}, fetchedAt: time.Now()},
	}}

	cred := &Credential{AccessToken: "test-empty-workspace", UserID: "test-empty-workspace", Region: RegionCN}
	bundled, ok := findModel("minimax-m3")
	if !ok {
		t.Fatal("bundled minimax-m3 should exist")
	}
	resolved, variant, matched := resolveWorkspaceModel(context.Background(), cache, cred, "", "minimax-m3", bundled)
	if matched || resolved != "" || variant != "" {
		t.Fatalf("empty workspace must not resolve model: resolved=%q variant=%q matched=%v", resolved, variant, matched)
	}
}

func TestResolveWorkspaceModelUsesCurrentRenamedModel(t *testing.T) {
	cache := &workspaceCache{entries: map[string]workspaceCacheEntry{
		"test-renamed-workspace|https://agent.minimaxi.com": {
			models:    []workspaceModel{{ModelID: "MiniMax-M3.1-Flash", DisplayName: "M3.1 Flash"}},
			fetchedAt: time.Now(),
		},
	}}

	cred := &Credential{AccessToken: "test-renamed-workspace", UserID: "test-renamed-workspace", Region: RegionCN}
	bundled := Model{ID: "minimax-m3.1-flash", UpstreamModel: "MiniMax-M3.1-Flash-Preview"}
	resolved, variant, matched := resolveWorkspaceModel(context.Background(), cache, cred, "", "minimax-m3.1-flash", bundled)
	if !matched || resolved != "MiniMax-M3.1-Flash" || variant != "" {
		t.Fatalf("renamed workspace model = (%q, %q, %v), want current upstream ID", resolved, variant, matched)
	}
}

func TestFindModelRejectsUnknownModel(t *testing.T) {
	if _, ok := findModel("minimax-does-not-exist"); ok {
		t.Fatal("unknown model must not be reported as known")
	}
}
