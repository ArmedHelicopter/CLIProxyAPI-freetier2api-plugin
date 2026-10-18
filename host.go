package main

// 本文件是根层的「宿主面」：
//   - hostBridge：把宿主能力接到 core.Host 端口；
//   - lifecycle：后台循环与在途流式转发的生命周期；
//   - management：把管理请求交给 internal/admin，并注入 Deps。
//
// 依赖方向始终是 root → internal，供应商包不感知宿主。

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

// hostBridge 实现 core.Host。
type hostBridge struct{}

// ListAuths 复用根层的凭证枚举（含「被前置插件抢走但仍按文件名归属」的兜底）。
func (hostBridge) ListAuths(ctx context.Context) ([]core.HostAuth, error) {
	return hostabi.ListAuths(ctx, "")
}

// AuthJSON 复用根层的 host.auth.get 封装。
func (hostBridge) AuthJSON(ctx context.Context, authIndex string) ([]byte, bool) {
	return hostabi.AuthJSON(ctx, "", authIndex)
}

// Settings 把根层配置映射成中立快照。
func (hostBridge) Settings() core.Settings {
	cfg := loadedConfig()
	return core.Settings{
		EnabledRealms:        append([]string(nil), cfg.EnabledRealms...),
		StateDir:             cfg.StateDir,
		PassthroughIP:        cfg.PassthroughIP,
		SanitizeFingerprints: cfg.SanitizeFingerprints,
		MachineSalt:          currentMachineSalt,
		AutoCheckin:          cfg.AutoCheckin,
		AutoCheckinAt:        cfg.AutoCheckinAt,
		AutoTasks:            cfg.AutoTasks,
		LogLevel:             cfg.LogLevel,
	}
}

// configureVendors 把全局设置与原始配置键推给每个供应商。
//
// 在 applyConfig（register / reconfigure 的共同入口）里调用一次；各供应商
// 自行解析自己的键并更新配置快照。
func configureVendors(cfg pluginConfig) {
	settings := hostBridge{}.Settings()
	raw := make(map[string]string, len(cfg.Raw)+1)
	for key, value := range cfg.Raw {
		raw[key] = value
	}
	// 页面设置覆盖 YAML 配置：用户在控制台页改过的提示词模式以页面为准。
	// 这是唯一一个「状态文件覆盖配置」的键，合并后再交给供应商解析。
	if state := snapshotState(); state.PromptMode != nil && strings.TrimSpace(*state.PromptMode) != "" {
		raw["prompt_mode"] = strings.TrimSpace(*state.PromptMode)
	}
	for _, vendor := range core.Vendors() {
		if configurable, okConfigurable := vendor.(core.Configurable); okConfigurable {
			configurable.Configure(settings, raw)
		}
	}
}

// init 注入宿主端口。
//
// 与 core.RegisterVendor 的时序无关：供应商只在使用时读取端口，
// 不会在注册期依赖配置。
func init() {
	core.ConfigureHost(hostBridge{})
}

var (
	pluginLifecycleMu sync.Mutex
	pluginRegistered  bool

	backgroundMu     sync.Mutex
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup
)

// startBackgroundWork 启动后台循环（幂等：已在运行时不重复启动）。
func startBackgroundWork() {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	if backgroundCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	backgroundCancel = cancel

	backgroundWG.Add(1)
	go func() {
		defer backgroundWG.Done()
		runTaskScheduler(ctx)
	}()
}

// stopBackgroundWork 取消后台循环并等待其退出。
func stopBackgroundWork() {
	backgroundMu.Lock()
	cancel := backgroundCancel
	backgroundCancel = nil
	backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
	backgroundWG.Wait()
}

// backgroundRunning 报告后台循环是否在运行（供管理页展示）。
func backgroundRunning() bool {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	return backgroundCancel != nil
}

var (
	streamMu       sync.Mutex
	streamCancels  = map[uint64]context.CancelFunc{}
	streamWG       sync.WaitGroup
	nextStreamKey  uint64
	pluginStreamID uint64
)

// beginPluginStream 注册一个流式转发任务，返回绑定它的 context、取消函数与结束回调。
//
// 结束回调必须被调用（通常 defer），否则 shutdown 会一直等到超时。
// 内部用 sync.Once 保证重复调用安全。
func beginPluginStream() (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	streamMu.Lock()
	nextStreamKey++
	key := nextStreamKey
	streamCancels[key] = cancel
	streamMu.Unlock()
	streamWG.Add(1)

	var once sync.Once
	finish := func() {
		once.Do(func() {
			streamMu.Lock()
			delete(streamCancels, key)
			streamMu.Unlock()
			cancel()
			streamWG.Done()
		})
	}
	return ctx, cancel, finish
}

// waitPluginStreams 取消所有在途流式转发，并在限定时间内等待它们退出。
//
// 这里的等待是**有界**的：取消之后这些 goroutine 只做清理（关闭上游流与宿主流），
// 即使它们阻塞在不可取消的宿主调用上，随后的宿主调用排空也会兜住。
func waitPluginStreams(timeout time.Duration) {
	streamMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(streamCancels))
	for _, cancel := range streamCancels {
		cancels = append(cancels, cancel)
	}
	inflight := len(streamCancels)
	streamMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if inflight == 0 {
		streamWG.Wait()
		return
	}
	if timeout <= 0 {
		timeout = shutdownWaitTimeout
	}
	done := make(chan struct{})
	go func() {
		streamWG.Wait()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		streamMu.Lock()
		remaining := len(streamCancels)
		streamMu.Unlock()
		logger.Error("shutdown: %d stream task(s) still running after %s",
			remaining, time.Since(start).Round(time.Millisecond))
	}
}

// nextPluginStreamID 生成进程内唯一的流标识（仅用于日志与诊断）。
func nextPluginStreamID() uint64 {
	streamMu.Lock()
	defer streamMu.Unlock()
	pluginStreamID++
	return pluginStreamID
}

// adminDeps 实现 admin.Deps，把根层的进程级能力注入管理接口。
type adminDeps struct{}

func (adminDeps) Identity() admin.Identity {
	return admin.Identity{
		ID:          pluginID,
		DisplayName: pluginDisplayName,
		ProviderKey: providerKey,
		Version:     effectivePluginVersion(),
	}
}

func (adminDeps) Config() pluginconfig.Config { return loadedConfig() }

func (adminDeps) State() state.State { return snapshotState() }

func (adminDeps) MutateState(fn func(*state.State)) {
	mutateState(fn)
}

func (adminDeps) Model() *modelconfig.Manager { return modelCatalog }

func (adminDeps) PromptMode() string { return promptModeFor() }

func (adminDeps) SchedulerStatus() map[string]any { return schedulerStatus() }

func (adminDeps) EffectiveSettings() map[string]any { return effectiveSettings() }

func (adminDeps) ResolveCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error) {
	return resolveVendorCredential(ctx, callbackID, storageJSON, authID, attrs)
}

func (adminDeps) SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor, entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	saveRefreshedCredential(ctx, callbackID, vendor, entry, original, updated, filePath)
}

func (adminDeps) RefreshModels(ctx context.Context) (int, error) {
	return refreshModelsFromUpstream(ctx)
}

func (adminDeps) RecordCheckin(credential *core.Credential, result *core.CheckinResult) {
	recordCheckinResult(credential, result)
}

func (adminDeps) PokeScheduler() { pokeScheduler() }

func (adminDeps) RunScheduled(kind string) { runScheduledTask(kind) }

// TaskRoutes 把任务相关路由分发给根层实现（共享队列与账号锁）。
func (adminDeps) TaskRoutes(req pluginapi.ManagementRequest, route string) (pluginapi.ManagementResponse, bool) {
	switch route {
	case "/tasks/scan":
		return handleTasksScan(req), true
	case "/tasks/run":
		return handleTasksRun(req), true
	case "/tasks/queue":
		return jsonResponse(http.StatusOK, taskQueueSnapshot()), true
	case "/tasks/auto":
		return handleTasksAuto(req), true
	case "/tasks/auto_all":
		return handleTasksAutoAll(req), true
	case "/school/status":
		ctx, cancel := managementContext(req)
		defer cancel()
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "accounts": schoolStatusForAccounts(ctx, hostCallbackID(req)),
		}), true
	}
	return pluginapi.ManagementResponse{}, false
}

// managementRegistration 返回本插件声明的管理路由与资源页。
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return admin.Registration()
}

// handleManagement 是管理接口的总分发。
func handleManagement(request []byte) ([]byte, error) {
	return admin.Handle(request)
}

// 响应/上下文助手：任务中心仍在根层使用，保持原签名。

func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return admin.HTMLResponse(status, body)
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	return admin.JSONResponse(status, payload)
}

func errorResponse(err error) pluginapi.ManagementResponse {
	return admin.ErrorResponse(err)
}

// buildStatusPayload 保留给测试与兼容调用。
func buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return admin.StatusPayload(req)
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

// managementRequestTimeout 与管理接口的上游调用上限一致。
const managementRequestTimeout = 3 * time.Minute

func init() {
	admin.Configure(adminDeps{})
}
