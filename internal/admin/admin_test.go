package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

type stubDeps struct {
	store *state.Store
}

func (s stubDeps) Identity() Identity {
	return Identity{ID: "freetier2api", DisplayName: "FreeTier 2API", ProviderKey: "freetier", Version: "test"}
}
func (s stubDeps) Config() pluginconfig.Config { return pluginconfig.Default() }
func (s stubDeps) State() state.State          { return s.store.Snapshot() }
func (s stubDeps) MutateState(fn func(*state.State)) {
	s.store.Mutate(fn)
}
func (s stubDeps) Model() *modelconfig.Manager       { return modelconfig.New(s.store) }
func (s stubDeps) PromptMode() string                { return "passthrough" }
func (s stubDeps) SchedulerStatus() map[string]any   { return map[string]any{"running": false} }
func (s stubDeps) EffectiveSettings() map[string]any { return map[string]any{} }
func (s stubDeps) ResolveCredential(context.Context, string, []byte, string, map[string]string) (*core.Credential, core.Vendor, error) {
	return nil, nil, context.Canceled
}
func (s stubDeps) SaveRefreshedCredential(context.Context, string, core.Vendor, core.HostAuth, []byte, *core.Credential, string) {
}
func (s stubDeps) RefreshModels(context.Context) (int, error)          { return 0, nil }
func (s stubDeps) RecordCheckin(*core.Credential, *core.CheckinResult) {}
func (s stubDeps) PokeScheduler()                                      {}
func (s stubDeps) RunScheduled(string)                                 {}
func (s stubDeps) TaskRoutes(pluginapi.ManagementRequest, string) (pluginapi.ManagementResponse, bool) {
	return pluginapi.ManagementResponse{}, false
}

func newStubDeps(t *testing.T) stubDeps {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	return stubDeps{store: store}
}

func TestRegistrationDeclaresRoutes(t *testing.T) {
	Configure(newStubDeps(t))
	registration := Registration()
	found := false
	for _, route := range registration.Routes {
		if route.Path == "/plugins/freetier2api/status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("status route missing: %#v", registration.Routes)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/console" {
		t.Fatalf("resources = %#v", registration.Resources)
	}
}

func TestHandleStatusAndUnknownRoute(t *testing.T) {
	Configure(newStubDeps(t))

	raw, err := Handle([]byte(`{"method":"GET","path":"/v0/management/plugins/freetier2api/status"}`))
	if err != nil {
		t.Fatalf("handle status: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !strings.Contains(string(raw), `"StatusCode":200`) && !strings.Contains(string(raw), `"status_code":200`) {
		t.Fatalf("unexpected envelope: %s", raw)
	}

	raw, err = Handle([]byte(`{"method":"GET","path":"/plugins/freetier2api/nope"}`))
	if err != nil {
		t.Fatalf("handle unknown: %v", err)
	}
	var notFound struct {
		Result struct {
			StatusCode int
		}
	}
	if err := json.Unmarshal(raw, &notFound); err != nil {
		t.Fatalf("decode not-found: %v", err)
	}
	if notFound.Result.StatusCode != 404 {
		t.Fatalf("unknown route should 404: %s", raw)
	}
}
