package main

// 本文件实现 auth provider 能力：
//   - 凭证识别/解析/刷新与登录分发；
//   - 续期后的凭证合并写回（宿主热更新 + 磁盘兜底）。
//
// 登录协议与续期链路由各供应商实现；根层只做路由、错误分类与状态码映射。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostabi"
	"freetier2api-plugin/internal/logger"
)

const (
	// authRefreshInterval 是凭证校验成功后的下次刷新间隔。
	authRefreshInterval = 30 * time.Minute
	// authRefreshRetryAfter 是遇到抖动错误时的重试间隔。
	authRefreshRetryAfter = 5 * time.Minute
	// authRefreshHTTPTimeout 是刷新校验的请求上限。
	authRefreshHTTPTimeout = 60 * time.Second
	// credentialCacheTTL 是凭证回源查找的缓存时长。
	//
	// 凭证轮换主要由宿主驱动（它会把新 StorageJSON 交给各能力），
	// 这里的缓存主要服务管理页轮询：30 秒既够用又能及时看到换号。
	credentialCacheTTL = 30 * time.Second
)

// authParseRPCRequest 与宿主的 auth.parse 请求对齐。
type authParseRPCRequest struct {
	pluginapi.AuthParseRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authRefreshRPCRequest 与宿主的 auth.refresh 请求对齐。
type authRefreshRPCRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse 识别并解析本插件的凭证文件。
//
// 归属判定交给 core.ResolveVendor（三级：vendor 字段 → 文件名前缀 → 内容嗅探），
// 解析本身交给命中的供应商实例。这样新增供应商不需要改这里。
//
// 既不能漏认自己的文件，也不能把别的 provider 的凭证误吞——误吞会让宿主
// 用本插件的结构覆盖对方账号，且是静默的。
func handleAuthParse(request []byte) ([]byte, error) {
	var rpc authParseRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	if len(rpc.RawJSON) == 0 {
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var raw map[string]any
	if errUnmarshal := json.Unmarshal(rpc.RawJSON, &raw); errUnmarshal != nil {
		// 非 JSON 的凭证文件不归本插件处理。
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	vendor, okVendor := core.ResolveVendor(rpc.FileName, rpc.Provider, raw)
	if !okVendor {
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	credential, errCredential := vendor.Parse(rpc.RawJSON, rpc.FileName)
	if errCredential != nil {
		return nil, core.NewPluginError("credential_invalid", errCredential.Error(), http.StatusUnprocessableEntity)
	}

	logger.Info("parsed auth %s (vendor=%s)",
		core.FirstNonEmpty(credential.LabelValue(), rpc.FileName, rpc.Path), vendor.ID())

	auth, errAuth := buildAuthDataFor(vendor, credential, rpc.FileName, rpc.RawJSON)
	if errAuth != nil {
		return nil, errAuth
	}
	return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

// buildAuthDataFor 构造宿主契约的凭证记录（供应商无关）。
//
// fileName 由调用方决定：
//   - auth.parse 用宿主传来的原文件名（不重命名用户已有的文件）；
//   - auth.login.poll 用 core.FileNameFor 生成的 <vendor>-<uid>.json。
func buildAuthDataFor(vendor core.Vendor, credential *core.Credential, fileName string, storageJSON []byte) (pluginapi.AuthData, error) {
	id := strings.TrimSuffix(fileName, ".json")
	if id == "" {
		// 兜底用账号身份（文件名）；上游 UID 不是身份标识，不该出现在这里。
		id = credential.FileIDValue()
	}
	storage, errStorage := ensureVendorField(storageJSON, vendor.ID())
	if errStorage != nil {
		return pluginapi.AuthData{}, errStorage
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fileName,
		Label:       credential.LabelValue(),
		StorageJSON: storage,
		Metadata:    credentialMetadata(credential, vendor.ID()),
		Attributes:  credentialAttributes(credential, vendor.ID()),
		// 让宿主在启动后不久就驱动一次刷新校验。
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	}, nil
}

// ensureVendorField 保证凭证 JSON 里带 vendor 字段。
//
// 这是供应商归属的**最强证据**：文件名可能被用户改掉，而 vendor 字段是
// 插件自己写进去的。缺了它，用户改名后的凭证只能靠内容嗅探归属，
// 对结构相似的供应商（WorkBuddy 与 Qoder 都有 token 字段）不可靠。
func ensureVendorField(storageJSON []byte, vendorID string) ([]byte, error) {
	if len(storageJSON) == 0 {
		return storageJSON, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(storageJSON, &payload); errUnmarshal != nil {
		// 解析不了就原样返回：凭证内容由供应商负责，这里不强改。
		return storageJSON, nil
	}
	if existing, okExisting := payload[core.VendorKey].(string); okExisting &&
		strings.EqualFold(strings.TrimSpace(existing), vendorID) {
		return storageJSON, nil
	}
	payload[core.VendorKey] = vendorID
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, fmt.Errorf("encode vendor field: %w", errMarshal)
	}
	return merged, nil
}

// credentialMetadata 是回给宿主的可变元数据。
//
// refresh_token 必须在这里（见文件头注释）；token 本身绝不回传。
func credentialMetadata(credential *core.Credential, vendorID string) map[string]any {
	metadata := map[string]any{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.FileIDValue(); uid != "" {
		metadata["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		metadata["nickname"] = label
	}
	if refreshToken := credential.RefreshTokenValue(); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
		metadata["auth_mode"] = "oauth"
	}
	if expiresAt := credential.ExpiresAtValue(); expiresAt > 0 {
		metadata["expires_at"] = expiresAt
	}
	return metadata
}

// credentialAttributes 是回给宿主的不可变属性（不含任何凭证材料）。
func credentialAttributes(credential *core.Credential, vendorID string) map[string]string {
	attributes := map[string]string{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.FileIDValue(); uid != "" {
		attributes["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		attributes["nickname"] = label
	}
	if credential.RefreshTokenValue() != "" {
		attributes["auth_mode"] = "oauth"
	}
	return attributes
}

// handleAuthRefresh 校验凭证并在必要时刷新。
//
// 刷新逻辑**交给凭证所属的供应商**：各家的续期链路差异很大
// （WorkBuddy 是 X-Refresh-Token 换 token，Qoder 是 jobToken 交换），
// 共性只有「凭证失效要报错、抖动要保留、成功要写回」这三条处置策略。
//
// 错误分三类处置：
//   - 凭证确实失效（401/403）→ 返回错误，让宿主把该账号标记为坏；
//   - 上游抖动/未知错误 → 保留原凭证并给一个较短的下次刷新时间（不误伤账号）；
//   - 成功 → 若上游轮换了 token，把新凭证写回。
func handleAuthRefresh(request []byte) ([]byte, error) {
	var rpc authRefreshRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID), authRefreshHTTPTimeout)
	defer cancel()

	credential, errCredential := credentialForAuth(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errCredential != nil {
		return nil, core.NewPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return nil, core.NewPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}

	updated, refreshed, errRefresh := core.RefreshVendor(vendor, ctx, credential)
	if errRefresh != nil {
		if isCredentialRejected(errRefresh) {
			logger.Error("auth refresh rejected for %s: %v", rpc.AuthID, errRefresh)
			return nil, core.ToPluginError(errRefresh)
		}
		// 抖动或未知错误：保留原凭证，稍后重试。
		logger.Debug("auth refresh deferred for %s: %v", rpc.AuthID, errRefresh)
		auth, errData := refreshedAuthData(rpc, rpc.StorageJSON, credential)
		if errData != nil {
			return nil, errData
		}
		return core.OKEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             auth,
			NextRefreshAfter: time.Now().Add(authRefreshRetryAfter),
		})
	}

	// 上游没下发新令牌时不重写文件：否则 mtime 会无意义地变动，
	// 用户无法从文件时间判断账号是否真的续期过。
	storage := rpc.StorageJSON
	if refreshed {
		if merged, errStorage := mergeVendorStorage(rpc.StorageJSON, updated, vendor); errStorage == nil {
			storage = merged
		}
	}
	logger.Info("auth refresh succeeded for %s (vendor=%s refreshed=%t)", rpc.AuthID, vendor.ID(), refreshed)
	auth, errData := refreshedAuthData(rpc, storage, updated)
	if errData != nil {
		return nil, errData
	}
	return core.OKEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	})
}

// mergeVendorStorage 把刷新后的凭证合并回原始 StorageJSON。
//
// 合并逻辑**交给供应商自己**（经 core.StorageMerger 可选接口）：各家的 JSON
// 结构不同（WorkBuddy 是嵌套的 auth/account，Qoder 是扁平字段，Cline 是平铺的
// accessToken/refreshToken），通用合并会把顶层字段写乱。
//
// 供应商没实现该接口时原样返回：宁可不动，也不要用错误的结构覆盖用户文件。
// 合并后补 vendor 字段，保证供应商归属始终可判定。
func mergeVendorStorage(original []byte, credential *core.Credential, vendor core.Vendor) ([]byte, error) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, nil
	}
	merged, errMerge := merger.MergeStorageJSON(original, credential)
	if errMerge != nil {
		return nil, errMerge
	}
	return ensureVendorField(merged, vendor.ID())
}

// refreshedAuthData 在刷新后重建凭证记录，保留宿主侧的既有字段。
//
// 供应商取自凭证自身的 vendor 字段/区域（刷新场景下凭证已存在，
// 归属必然可判定），因此不需要调用方再传。
func refreshedAuthData(rpc authRefreshRPCRequest, storageJSON []byte, credential *core.Credential) (pluginapi.AuthData, error) {
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return pluginapi.AuthData{}, fmt.Errorf("unknown vendor %q", credential.VendorIDValue())
	}
	data, errData := buildAuthDataFor(vendor, credential, rpc.AuthID, storageJSON)
	if errData != nil {
		return pluginapi.AuthData{}, errData
	}
	// 保留宿主传入的标识字段，避免因插件重建而丢失。
	if strings.TrimSpace(rpc.AuthID) != "" {
		data.ID = rpc.AuthID
	}
	return data, nil
}

// isCredentialRejected 报告错误是否表示凭证真的失效。
//
// 判断依据是供应商分类后的 HTTP 状态码（401/403 凭证失效、402 额度耗尽），
// 因此对全部供应商生效，而不是只认 WorkBuddy 的错误类型。
func isCredentialRejected(err error) bool {
	var upstream core.UpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.HTTPStatus() {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return true
	}
	return false
}

// credentialCacheEntry 是凭证回源查找的缓存项。
type credentialCacheEntry struct {
	credential *core.Credential
	fetchedAt  time.Time
}

var (
	credentialCacheMu sync.Mutex
	credentialCache   = map[string]credentialCacheEntry{}
)

// credentialForAuth 取回某个账号的凭证。
//
// 优先用宿主随请求发来的 StorageJSON（最准），缺失时按 auth_index 回源。
//
// **必须支持回源**：宿主的管理端额度路由只发 AuthIndex/AuthID +
// Metadata/Attributes，**不发 StorageJSON**；而插件设计上又不把 token 放进
// metadata/attributes，因此只认 StorageJSON 会把额度查询误报成「凭证缺失」。
//
// 供应商归属由凭证自身决定（vendor 字段 → 文件名 → 内容嗅探），
// 因此这里不需要调用方指定供应商，新增供应商也不必改本函数。
func credentialForAuth(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, error) {
	if len(storageJSON) > 0 {
		if credential, okParse := core.ParseCredential(storageJSON, authID, attributes); okParse {
			return credential, nil
		}
	}
	if trimmed := strings.TrimSpace(authID); trimmed != "" {
		if cached, okCache := cachedCredential(trimmed); okCache {
			return cached, nil
		}
		raw, errFetch := hostabi.FetchAuthJSON(ctx, callbackID, trimmed)
		if errFetch == nil && len(raw) > 0 {
			if credential, okParse := core.ParseCredential(raw, trimmed, attributes); okParse {
				storeCredential(trimmed, credential)
				return credential, nil
			}
		}
	}
	return nil, fmt.Errorf("credential is not available for auth %s", authID)
}

func cachedCredential(authID string) (*core.Credential, bool) {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	entry, okEntry := credentialCache[authID]
	if !okEntry || time.Since(entry.fetchedAt) > credentialCacheTTL {
		return nil, false
	}
	return entry.credential, true
}

func storeCredential(authID string, credential *core.Credential) {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	credentialCache[authID] = credentialCacheEntry{credential: credential, fetchedAt: time.Now()}
}

// resetCredentialCache 清空凭证缓存（配置变更或测试使用）。
func resetCredentialCache() {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	credentialCache = map[string]credentialCacheEntry{}
}

// resolveVendorCredential 取回凭证并解析出它所属的供应商。
//
// 把「取凭证」与「定供应商」合成一步：几乎所有 ABI 处理器都需要这一对，
// 分开写会到处重复 VendorByID + 错误处理。
func resolveVendorCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, core.Vendor, error) {
	credential, errCredential := credentialForAuth(ctx, callbackID, storageJSON, authID, attributes)
	if errCredential != nil {
		return nil, nil, core.NewPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return nil, nil, core.NewPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}
	return credential, vendor, nil
}

// ---- 登录分发（原 auth_login.go） ----

const (
	// loginHTTPTimeout 是单次登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
)

// loginStartRPCRequest 与宿主的 auth.login.start 请求对齐。
type loginStartRPCRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// loginPollRPCRequest 与宿主的 auth.login.poll 请求对齐。
type loginPollRPCRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthLoginStart 开始一次登录，返回用户需要打开的授权链接。
//
// 登录协议由供应商实现，根层只做供应商路由与区域开关校验。
func handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpc loginStartRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLogin(rpc.Metadata)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	login, okLogin := core.As[core.LoginProvider](vendor)
	if !okLogin {
		return nil, core.LoginUnsupported(vendor)
	}
	response, errStart := login.LoginStart(ctx, rpc.Metadata)
	if errStart != nil {
		return nil, core.ToPluginError(errStart)
	}
	// 供应商标识由根层统一注入，不交给各供应商自己记着。
	//
	// 宿主靠 metadata.vendor 把登录会话绑定回具体供应商；缺了它宿主拿不到
	// 授权链接，页面只会看到「获取授权链接失败」。把这一步放在分发点而不是
	// 各供应商的 LoginStart 里：新增供应商时没人会记得补，且漏掉时没有任何
	// 编译期或运行期提示——本插件曾因此让 codearts / trae / traesolo 三个
	// 供应商长期无法添加账号。
	if response != nil {
		if response.Metadata == nil {
			response.Metadata = map[string]any{}
		}
		response.Metadata[core.VendorKey] = vendor.ID()
		response.Metadata["region"] = vendor.Region()
	}
	logger.Info("started login (vendor=%s)", vendor.ID())
	return core.OKEnvelope(response)
}

// handleAuthLoginPoll 轮询一次登录状态。
//
// 会话状态由供应商自己持有（各自的 state 结构不同），根层不参与。
func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpc loginPollRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	sessionID := strings.TrimSpace(rpc.State)
	if sessionID == "" {
		return nil, core.NewPluginError("invalid_request", "login state is required", http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLoginSession(sessionID)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(hostabi.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	login, okLogin := core.As[core.LoginProvider](vendor)
	if !okLogin {
		return nil, core.LoginUnsupported(vendor)
	}
	response, errPoll := login.LoginPoll(ctx, sessionID)
	if errPoll != nil {
		return nil, core.ToPluginError(errPoll)
	}
	return core.OKEnvelope(response)
}

// vendorForLogin 从登录请求的 Metadata 解析目标供应商。
//
// 控制台页把用户选的供应商标识放在 Metadata 的 vendor 字段里；
// 缺失时回落到配置里启用的第一个供应商（兼容旧版页面只传 realm 的情况）。
func vendorForLogin(metadata map[string]any) (core.Vendor, error) {
	if metadata != nil {
		if rawVendor, okVendor := metadata[core.VendorKey].(string); okVendor {
			vendor, okLookup := core.VendorByID(rawVendor)
			if !okLookup {
				return nil, core.NewPluginError("invalid_request",
					fmt.Sprintf("unknown vendor %q", rawVendor), http.StatusBadRequest)
			}
			if !vendorEnabled(vendor) {
				return nil, core.NewPluginError("vendor_disabled",
					fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
			}
			return vendor, nil
		}
		// 旧版页面只传 realm：按区域找第一个匹配的供应商。
		if rawRegion, okRegion := metadata["region"].(string); okRegion && strings.TrimSpace(rawRegion) != "" {
			if vendor, okFind := firstVendorForRegion(rawRegion); okFind {
				return vendor, nil
			}
		}
	}
	if vendor, okDefault := firstEnabledVendor(); okDefault {
		return vendor, nil
	}
	return nil, core.NewPluginError("vendor_disabled", "no vendor is enabled by plugin configuration", http.StatusBadRequest)
}

// vendorForLoginSession 按登录会话找供应商。
//
// 会话归属由持有会话表的供应商自证（loginSessionOwner 接口）。重构前这里
// 还有一段「按 session id 前缀猜供应商」的兜底（trae_solo_ / workbuddy_ /
// copilot_），那是会话登记不统一时的补丁；现在每个持有会话的供应商都实现了
// 归属查询，猜前缀只会把会话错误路由给别家，因此删除。
func vendorForLoginSession(sessionID string) (core.Vendor, error) {
	for _, vendor := range core.Vendors() {
		if !vendorEnabled(vendor) {
			continue
		}
		if owner, okOwner := core.As[core.LoginSessionOwner](vendor); okOwner && owner.OwnsLoginSession(sessionID) {
			return vendor, nil
		}
	}
	return nil, core.NewPluginError("session_not_found", "登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
}

// vendorEnabled 报告供应商的区域是否被配置启用。
func vendorEnabled(vendor core.Vendor) bool {
	return realmEnabled(loadedConfig(), vendor.Region())
}

// firstEnabledVendor 返回配置里第一个启用的供应商。
func firstEnabledVendor() (core.Vendor, bool) {
	for _, vendor := range core.Vendors() {
		if vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// firstVendorForRegion 返回某个区域第一个启用的供应商。
func firstVendorForRegion(region string) (core.Vendor, bool) {
	normalized := strings.ToLower(strings.TrimSpace(region))
	for _, vendor := range core.Vendors() {
		if vendor.Region() == normalized && vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// newHexID 生成 n 字节的随机十六进制串（2n 个字符）。
//
// 随机源不可用时退回基于时间与计数的确定性串：登录会话 id 只影响
// 会话查找的唯一性，退化的熵在这种场景下可接受——比返回空串让整个
// 登录链路崩掉好。
func newHexID(n int) string {
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&hexIDCounter, 1))
	}
	return hex.EncodeToString(buf)
}

var hexIDCounter int64

// ---- 凭证写回（原 credentials.go） ----

// recordCheckinResult 把一次签到结果写进状态（供页面展示）。
//
// 接收中立的 core.CheckinResult：各家的签到结果字段不同，状态里只记
// 两家都有的部分。
func recordCheckinResult(credential *core.Credential, result *core.CheckinResult) {
	if credential == nil || result == nil {
		return
	}
	uid := credential.FileIDValue()
	if uid == "" {
		return
	}
	today := todayString()
	mutateState(func(state *pluginState) {
		record := state.Checkin[uid]
		record.LastDate = today
		if result.Credit > 0 {
			record.Credit = result.Credit
		}
		if result.Energy > 0 {
			record.Energy = result.Energy
		}
		state.Checkin[uid] = record
	})
}

// saveRefreshedCredential 把续期后的凭证写回宿主与磁盘。
//
// 写两处是刻意的双保险：
//   - host.auth.save 让宿主热更新内存注册表；
//   - 直接写文件兜住 host.auth.save 不可用的场景。
func saveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	payload, okPayload := refreshedStorageJSON(vendor, original, updated)
	if okPayload {
		if _, errSave := hostabi.Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
			Name: entry.Name,
			JSON: payload,
		}); errSave != nil {
			logger.Debug("keepalive %s: host.auth.save failed: %v", entry.Name, errSave)
		}
	}
	targetPath := filePath
	if targetPath == "" {
		targetPath = entry.Path
	}
	if targetPath == "" {
		return
	}
	if errWrite := os.WriteFile(targetPath, payload, 0o600); errWrite != nil {
		logger.Debug("keepalive %s: write credential file failed: %v", entry.Name, errWrite)
	}
}

// refreshedStorageJSON 用供应商自己的合并逻辑更新凭证 JSON。
func refreshedStorageJSON(vendor core.Vendor, original []byte, updated *core.Credential) ([]byte, bool) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, false
	}
	merged, errMerge := merger.MergeStorageJSON(original, updated)
	if errMerge != nil {
		return original, false
	}
	return merged, true
}
