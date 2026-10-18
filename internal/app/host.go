package app

// 本文件是 App 的宿主面：core.Host 实现、供应商配置下发、后台/流生命周期，
// 以及管理接口依赖注入。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

type hostBridge struct{ app *App }

func (h *hostBridge) ListAuths(ctx context.Context) ([]core.HostAuth, error) {
	return hostabi.ListAuths(ctx, "", h.app.registry)
}

func (h *hostBridge) AuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	return hostabi.AuthJSON(ctx, "", authIndex)
}

func (h *hostBridge) Settings() core.Settings {
	cfg := h.app.Config()
	return core.Settings{
		EnabledRealms: append([]string(nil), cfg.EnabledRealms...),
		StateDir:      cfg.StateDir,
		MachineSalt:   h.app.machineSalt,
		AutoCheckin:   cfg.AutoCheckin,
		AutoCheckinAt: cfg.AutoCheckinAt,
		AutoTasks:     cfg.AutoTasks,
		LogLevel:      cfg.LogLevel,
	}
}

// configureVendors 把全局设置与原始配置键推给每个供应商。
func (a *App) configureVendors(cfg pluginConfig) {
	settings := a.host.Settings()
	raw := make(map[string]string, len(cfg.Raw)+len(a.SnapshotState().VendorSettings))
	for key, value := range cfg.Raw {
		raw[key] = value
	}
	for key, value := range a.SnapshotState().VendorSettings {
		if strings.TrimSpace(key) != "" {
			raw[key] = value
		}
	}
	for _, vendor := range a.registry.Vendors() {
		if configurable, okConfigurable := vendor.(core.Configurable); okConfigurable {
			configurable.Configure(settings, raw)
		}
	}
}

const DefaultShutdownWaitTimeout = 10 * time.Second

// StartBackgroundWork 启动后台循环（幂等）。
func (a *App) StartBackgroundWork() {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.backgroundCancel = cancel
	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		a.runTaskScheduler(ctx)
	}()
}

// StopBackgroundWork 取消后台循环并等待退出。
func (a *App) StopBackgroundWork() {
	a.backgroundMu.Lock()
	cancel := a.backgroundCancel
	a.backgroundCancel = nil
	a.backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.backgroundWG.Wait()
}

// BackgroundRunning 报告后台循环是否运行。
func (a *App) BackgroundRunning() bool {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	return a.backgroundCancel != nil
}

// BeginPluginStream 注册一个流式转发任务。
func (a *App) BeginPluginStream() (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	a.streamMu.Lock()
	a.nextStreamKey++
	key := a.nextStreamKey
	a.streamCancels[key] = cancel
	a.streamMu.Unlock()
	a.streamWG.Add(1)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			a.streamMu.Lock()
			delete(a.streamCancels, key)
			a.streamMu.Unlock()
			cancel()
			a.streamWG.Done()
		})
	}
	return ctx, cancel, finish
}

// WaitPluginStreams 取消并等待在途流。
func (a *App) WaitPluginStreams(timeout time.Duration) {
	a.streamMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.streamCancels))
	for _, cancel := range a.streamCancels {
		cancels = append(cancels, cancel)
	}
	inflight := len(a.streamCancels)
	a.streamMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if inflight == 0 {
		a.streamWG.Wait()
		return
	}
	if timeout <= 0 {
		timeout = DefaultShutdownWaitTimeout
	}
	done := make(chan struct{})
	go func() {
		a.streamWG.Wait()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(timeout):
		a.streamMu.Lock()
		remaining := len(a.streamCancels)
		a.streamMu.Unlock()
		logger.Error("shutdown: %d stream task(s) still running after %s", remaining, time.Since(start).Round(time.Millisecond))
	}
}

// adminDeps 实现 admin.Deps。
type adminDeps struct{ app *App }

func (d adminDeps) Identity() admin.Identity {
	return admin.Identity{ID: pluginID, DisplayName: pluginDisplayName, ProviderKey: providerKey, Version: d.app.version}
}

func (d adminDeps) Config() pluginconfig.Config { return d.app.Config() }
func (d adminDeps) State() state.State          { return d.app.SnapshotState() }
func (d adminDeps) MutateState(fn func(*state.State)) {
	d.app.MutateState(fn)
}
func (d adminDeps) Model() *modelconfig.Manager       { return d.app.models }
func (d adminDeps) WorkbuddyPromptMode() string       { return d.app.promptModeFor() }
func (d adminDeps) SchedulerStatus() map[string]any   { return d.app.schedulerStatus() }
func (d adminDeps) EffectiveSettings() map[string]any { return d.app.effectiveSettings() }

func (d adminDeps) ResolveCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error) {
	return d.app.resolveVendorCredential(ctx, callbackID, storageJSON, authID, attrs)
}

func (d adminDeps) SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor, entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	d.app.saveRefreshedCredential(ctx, callbackID, vendor, entry, original, updated, filePath)
}

func (d adminDeps) RefreshModels(ctx context.Context) (int, error) {
	return d.app.refreshModelsFromUpstream(ctx)
}

func (d adminDeps) Reconfigure() error { return d.app.ApplyConfig(d.app.Config()) }

func (d adminDeps) RecordCheckin(credential *core.Credential, result *core.CheckinResult) {
	d.app.recordCheckinResult(credential, result)
}

func (d adminDeps) PokeScheduler()           { d.app.pokeScheduler() }
func (d adminDeps) RunScheduled(kind string) { d.app.runScheduledTask(kind) }

func (d adminDeps) TaskRoutes(req pluginapi.ManagementRequest, route string) (pluginapi.ManagementResponse, bool) {
	switch route {
	case "/tasks/scan":
		return d.app.handleTasksScan(req), true
	case "/tasks/run":
		return d.app.handleTasksRun(req), true
	case "/tasks/queue":
		return jsonResponse(http.StatusOK, d.app.taskQueueSnapshot()), true
	case "/tasks/auto":
		return d.app.handleTasksAuto(req), true
	case "/tasks/auto_all":
		return d.app.handleTasksAutoAll(req), true
	case "/school/status":
		ctx, cancel := managementContext(req)
		defer cancel()
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "accounts": d.app.schoolStatusForAccounts(ctx, hostCallbackID(req))}), true
	}
	return pluginapi.ManagementResponse{}, false
}

func (a *App) managementRegistration() pluginapi.ManagementRegistrationResponse {
	return a.admin.Registration()
}

func (a *App) handleManagement(request []byte) ([]byte, error) { return a.admin.Handle(request) }

func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return admin.HTMLResponse(status, body)
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	return admin.JSONResponse(status, payload)
}

func managementContext(req pluginapi.ManagementRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), managementRequestTimeout)
	return hostabi.WithCallbackID(ctx, hostCallbackID(req)), cancel
}

func hostCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

const managementRequestTimeout = 3 * time.Minute
