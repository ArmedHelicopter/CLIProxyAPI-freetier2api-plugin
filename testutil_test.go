package main

// 本文件提供「假宿主」：把宿主回调替换成内存实现，
// 使插件的完整链路（凭证解析、模型注册、出站 HTTP、任务上报、流式转发）
// 都能在没有 CPA 进程的情况下被测试。
//
// 关键设计：
//   - installFakeHost 用 t.Cleanup 做完整还原（含把重试退避压到毫秒级）；
//   - resetPluginGlobals 集中清包级全局——白盒测试最大的坑是缓存跨用例泄漏；
//   - 非流式响应刻意按宿主的「无 tag」形态返回，覆盖 rpcHostHTTPResponse 的兼容逻辑。

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/testhost"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// abiEnvelope 是插件响应信封（测试用；生产侧已统一到 core）。
type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// testCallbackID 是测试用的宿主回调 ID。
const testCallbackID = "test-callback"

// fakeHost 是 internal/testhost.Host 的薄包装：保留既有小写方法名，
// 避免大规模改写用例；新包/新用例直接使用 testhost 的导出 API。
type fakeHost struct{ *testhost.Host }

// fakeUpstreamResponse 是假宿主应答的别名（兼容既有用例）。
type fakeUpstreamResponse = testhost.UpstreamResponse

func newFakeHost() *fakeHost { return &fakeHost{testhost.New()} }

func (h *fakeHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	return h.Host.Call(callbackID, method, payload)
}

func (h *fakeHost) addAccount(authIndex, name, credentialJSON string) {
	h.AddAccount(authIndex, name, credentialJSON)
}

func (h *fakeHost) setUpstream(fn func(method, url, body string) fakeUpstreamResponse) {
	h.SetUpstream(fn)
}

func (h *fakeHost) emittedChunks() []string { return h.EmittedChunks() }

func (h *fakeHost) streamErrors() []string { return h.StreamErrors() }

func (h *fakeHost) closedStreams() []string { return h.ClosedStreams() }

// installFakeHost 把假宿主接到插件的宿主调用入口上。
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	hostabi.Configure(host.call)
	resetPluginGlobals(t)
	t.Cleanup(func() {
		hostabi.Configure(nativeHostCallScoped)
		resetPluginGlobals(t)
	})
	return host
}

// resetPluginGlobals 清掉跨用例共享的进程级状态。
//
// 白盒测试最容易踩的坑：某个用例留下的缓存/单例让后续用例看到过期数据。
// 因此把所有包级全局集中在这里清一遍。
func resetPluginGlobals(t *testing.T) {
	t.Helper()

	pluginLifecycleMu.Lock()
	pluginRegistered = false
	pluginLifecycleMu.Unlock()

	stateStore.Reset()
	reloadDisabledModelCache(nil)

	resetCredentialCache()
	// 模型目录缓存是包级的：不重置会让上一个用例的探测结果泄漏到下一个。
	workbuddy.ResetModelCacheForTest()
	// 登录会话缓存也是包级的：清空避免上一个用例的会话泄漏到下一个。
	workbuddy.ResetLoginStoreForTest()

	currentQueue.mu.Lock()
	currentQueue.running = false
	currentQueue.items = nil
	currentQueue.seq = 0
	currentQueue.mu.Unlock()

	taskLockMu.Lock()
	taskLocks = map[string]*sync.Mutex{}
	taskLockMu.Unlock()

	// 时间函数复位（个别用例会注入固定时间）。
	nowFunc = time.Now
}

// setupTestPlugin 准备一个使用临时状态目录的插件实例。
func setupTestPlugin(t *testing.T, mutate ...func(*pluginConfig)) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.LogToFile = false
	for _, apply := range mutate {
		apply(&cfg)
	}
	if errApply := applyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	if _, errState := loadState(cfg); errState != nil {
		t.Fatalf("load state: %v", errState)
	}
	return cfg
}

// buildConfigYAML 生成宿主会传给 plugin.register 的配置 YAML。
//
// 包含宿主追加的 enabled / priority 两键，验证插件对未知字段的容忍。
func buildConfigYAML(cfg pluginConfig) []byte {
	var builder strings.Builder
	builder.WriteString("enabled: true\npriority: 1\n")
	fmt.Fprintf(&builder, "state_dir: %q\n", cfg.StateDir)
	fmt.Fprintf(&builder, "log_level: %s\n", cfg.LogLevel)
	fmt.Fprintf(&builder, "enabled_realms: %s\n", strings.Join(cfg.EnabledRealms, ","))
	if len(cfg.DisabledModels) > 0 {
		fmt.Fprintf(&builder, "disabled_models: %s\n", strings.Join(cfg.DisabledModels, ","))
	}
	if mode := cfg.Raw["prompt_mode"]; mode != "" {
		fmt.Fprintf(&builder, "prompt_mode: %s\n", mode)
	}
	fmt.Fprintf(&builder, "auto_checkin_at: %q\n", cfg.AutoCheckinAt)
	return []byte(builder.String())
}

// registerRequest 构造 plugin.register 的请求体。
func registerRequest(t *testing.T, cfg pluginConfig) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if errMarshal != nil {
		t.Fatalf("marshal register request: %v", errMarshal)
	}
	return raw
}

// sampleCredentialJSON 构造一份凭证 JSON（嵌套形态）。
func sampleCredentialJSON(realm, uid string) string {
	region := workbuddy.NormalizeRegion(realm)
	return fmt.Sprintf(`{
  "auth": {"accessToken": "at-%s", "refreshToken": "rt-%s", "expiresAt": %d, "domain": "%s", "realm": "%s"},
  "account": {"uid": "%s", "enterpriseId": "ent-1", "nickname": "账号%s"},
  "device_token": "dt-%s"
}`, uid, uid, time.Now().Add(24*time.Hour).Unix(), workbuddy.DefaultDomainFor(region), region, uid, uid, uid)
}

// 小工具。

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

func marshalJSON(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

// splitChunks 把响应体切成小块，模拟宿主 32KB 分片读取的真实路径。
//
// 刻意不一次性喂完整正文：那会掩盖分片边界相关的 bug。
func splitChunks(body string) []string {
	if body == "" {
		return nil
	}
	const size = 17
	chunks := make([]string, 0, len(body)/size+1)
	for len(body) > 0 {
		take := size
		if take > len(body) {
			take = len(body)
		}
		chunks = append(chunks, body[:take])
		body = body[take:]
	}
	return chunks
}

// decodeEnvelopeResultForTest 解开插件响应信封（测试辅助）。
func decodeEnvelopeResultForTest(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if !env.OK {
		message := "unknown"
		if env.Error != nil {
			message = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("envelope is not ok: %s", message)
	}
	return env.Result
}

// callMethod 调用一个 ABI 方法并解开结果。
func callMethod(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	var request []byte
	if payload != nil {
		request = mustMarshal(payload)
	}
	raw, errHandle := handleMethod(method, request)
	if errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
	return decodeEnvelopeResultForTest(t, raw)
}

// waitFor 轮询等待条件成立（避免用固定 sleep 造成偶发失败）。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

// pluginAPI 的引用保证测试文件与生产代码用同一份契约类型。
var _ = pluginapi.Metadata{}
