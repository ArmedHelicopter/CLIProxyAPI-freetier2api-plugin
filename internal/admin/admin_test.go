package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type stubDeps struct {
	store    *state.Store
	registry *core.Registry
	manager  *modelconfig.Manager
}

func (s stubDeps) Identity() Identity {
	return Identity{ID: "freetier2api", DisplayName: "FreeTier 2API", ProviderKey: "freetier", Version: "test"}
}
func (s stubDeps) Config() pluginconfig.Config { return pluginconfig.Default() }
func (s stubDeps) State() state.State          { return s.store.Snapshot() }
func (s stubDeps) MutateState(fn func(*state.State)) {
	s.store.Mutate(fn)
}
func (s stubDeps) Model() *modelconfig.Manager { return s.manager }
func (s stubDeps) WorkbuddyPromptMode() string { return "passthrough" }
func (s stubDeps) ResolveCredential(context.Context, string, []byte, string, map[string]string) (*core.Credential, core.Vendor, error) {
	return nil, nil, context.Canceled
}
func (s stubDeps) SaveRefreshedCredential(context.Context, string, core.Vendor, core.HostAuth, []byte, *core.Credential, string) {
}
func (s stubDeps) RefreshModels(context.Context) (int, error) { return 0, nil }
func (s stubDeps) Reconfigure() error                         { return nil }

func newStubDeps(t *testing.T) stubDeps {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	return stubDeps{store: store, registry: registry, manager: modelconfig.New(store, registry)}
}

func newService(t *testing.T) *Service {
	t.Helper()
	deps := newStubDeps(t)
	pool := accounts.NewPool(deps.registry, deps.ResolveCredential)
	return New(deps, deps.registry, pool, tasks.New(stubTaskDeps{deps: deps, pool: pool}))
}

func TestRegistrationDeclaresRoutes(t *testing.T) {
	service := newService(t)
	registration := service.Registration()
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
	service := newService(t)

	raw, err := service.Handle([]byte(`{"method":"GET","path":"/v0/management/plugins/freetier2api/status"}`))
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

	raw, err = service.Handle([]byte(`{"method":"GET","path":"/plugins/freetier2api/nope"}`))
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

type stubTaskDeps struct {
	deps stubDeps
	pool *accounts.Pool
}

func (s stubTaskDeps) Registry() *core.Registry               { return s.deps.registry }
func (s stubTaskDeps) Pool() *accounts.Pool                   { return s.pool }
func (s stubTaskDeps) Config() pluginconfig.Config            { return s.deps.Config() }
func (s stubTaskDeps) State() state.State                     { return s.deps.State() }
func (s stubTaskDeps) MutateState(fn func(*state.State))      { s.deps.MutateState(fn) }
func (s stubTaskDeps) Now() time.Time                         { return time.Now() }
func (s stubTaskDeps) RuntimeFields() []pluginapi.ConfigField { return nil }
