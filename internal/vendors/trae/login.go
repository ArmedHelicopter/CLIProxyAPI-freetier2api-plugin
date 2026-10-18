package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 本文件实现 Trae 家族授权登录链接生成与轮询状态检查。

// BuildLoginURL 构造某个 profile 的授权登录地址。
func BuildLoginURL(cfg Config, machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect", "0")

	traceID := MachineTraceID(machineID, deviceID)

	return cfg.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traceID) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(cfg.IDEVersion) +
		"&x_app_type=stable"
}

// LoginStart 启动登录流程。
func LoginStart(ctx context.Context, cfg Config, store *SessionStore) (*pluginapi.AuthLoginStartResponse, error) {
	EnsureCallbackServer(store)

	machineID := core.RandomHex(16)
	deviceID := core.RandomHex(16)
	traceID := MachineTraceID(machineID, deviceID)
	callbackURL := "http://127.0.0.1:18080/authorize"

	loginURL := BuildLoginURL(cfg, machineID, deviceID, callbackURL)
	sessionID := cfg.VendorID + "_" + core.RandomHex(8)

	RegisterPending(store, &PendingSession{
		SessionID: sessionID,
		VendorID:  cfg.VendorID,
		Region:    cfg.Region,
		IsSolo:    cfg.Solo,
		MachineID: machineID,
		DeviceID:  deviceID,
		TraceID:   traceID,
		State:     "pending",
		CreatedAt: time.Now(),
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       loginURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询状态。
func LoginPoll(ctx context.Context, state string, store *SessionStore) (*pluginapi.AuthLoginPollResponse, error) {
	session, ok := GetPending(store, state)
	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}

	switch session.State {
	case "success":
		RemovePending(store, state)
		if session.AuthData != nil {
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusSuccess,
				Message: "登录成功",
				Auth:    *session.AuthData,
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功",
		}, nil
	case "failed":
		RemovePending(store, state)
		errMsg := session.ErrMsg
		if errMsg == "" {
			errMsg = "授权登录失败"
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMsg,
		}, nil
	default:
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
}

// PendingSession 记录一次进行中的 Trae 或 Trae Solo 登录。
type PendingSession struct {
	SessionID string
	VendorID  string // trae-cn, trae-global, trae-solo
	Region    Region
	IsSolo    bool
	MachineID string
	DeviceID  string
	TraceID   string
	State     string // "pending", "success", "failed"
	AuthData  *pluginapi.AuthData
	ErrMsg    string
	CreatedAt time.Time
}

// 回调服务是进程级单例（固定端口），但会话表按 App 实例注册。
//
// 多个 App 同时登录时，回调请求需要轮流在各自实例的会话表里匹配，
// 不能只记住最后一个实例，否则先发起的登录会永远等不到回调。
var (
	callbackServerMu sync.Mutex
	callbackServerOn bool
	callbackStoresMu sync.RWMutex
	callbackStores   = map[*SessionStore]struct{}{}
)

// SessionStore 是单个 App 实例的 Trae 登录会话表。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*PendingSession
}

// NewSessionStore 构造会话表。
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*PendingSession{}}
}

func (s *SessionStore) register(p *PendingSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.sessions[p.SessionID] = p
}

func (s *SessionStore) get(sessionID string) (*PendingSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return nil, false
	}
	return session, true
}

func (s *SessionStore) remove(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *SessionStore) owns(sessionID, vendorID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	switch strings.ToLower(strings.TrimSpace(vendorID)) {
	case "traecn", "trae-cn":
		if strings.HasPrefix(norm, "traecn_") || strings.HasPrefix(norm, "trae_cn_") {
			return true
		}
	case "traeglobal", "trae-global":
		if strings.HasPrefix(norm, "traeglobal_") || strings.HasPrefix(norm, "trae_global_") {
			return true
		}
	case "traesolo", "trae-solo":
		if strings.HasPrefix(norm, "traesolo_") || strings.HasPrefix(norm, "trae_solo_") {
			return true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return false
	}
	return strings.EqualFold(session.VendorID, vendorID)
}

// matchExact 只按 trace/machine 精确匹配，找不到时返回 nil。
func (s *SessionStore) matchExact(info *CallbackInfo) *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		if session.State != "pending" {
			continue
		}
		if (info.TraceID != "" && session.TraceID == info.TraceID) ||
			(info.MachineID != "" && session.MachineID == info.MachineID) {
			return session
		}
	}
	return nil
}

// latestPending 返回最近创建的待处理会话（回调里没带可匹配标识时的兜底）。
func (s *SessionStore) latestPending() *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *PendingSession
	for _, session := range s.sessions {
		if session.State == "pending" && time.Since(session.CreatedAt) <= 15*time.Minute {
			if latest == nil || session.CreatedAt.After(latest.CreatedAt) {
				latest = session
			}
		}
	}
	return latest
}

func (s *SessionStore) gcLocked() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.Sub(session.CreatedAt) > 15*time.Minute {
			delete(s.sessions, id)
		}
	}
}

// registerCallbackStore 把某个 App 实例的会话表加入回调匹配集合。
func registerCallbackStore(store *SessionStore) {
	if store == nil {
		return
	}
	callbackStoresMu.Lock()
	callbackStores[store] = struct{}{}
	callbackStoresMu.Unlock()
}

func callbackStoresSnapshot() []*SessionStore {
	callbackStoresMu.RLock()
	defer callbackStoresMu.RUnlock()
	stores := make([]*SessionStore, 0, len(callbackStores))
	for store := range callbackStores {
		stores = append(stores, store)
	}
	return stores
}

// matchCallbackSession 在全部已注册实例里先精确匹配，再退回归属最新的待处理会话。
func matchCallbackSession(info *CallbackInfo) *PendingSession {
	stores := callbackStoresSnapshot()
	for _, store := range stores {
		if matched := store.matchExact(info); matched != nil {
			return matched
		}
	}
	var latest *PendingSession
	for _, store := range stores {
		candidate := store.latestPending()
		if candidate != nil && (latest == nil || candidate.CreatedAt.After(latest.CreatedAt)) {
			latest = candidate
		}
	}
	return latest
}

// EnsureCallbackServer 保证本机 18080 回调监听服务已启动。
func EnsureCallbackServer(store *SessionStore) {
	registerCallbackStore(store)

	callbackServerMu.Lock()
	defer callbackServerMu.Unlock()
	if callbackServerOn {
		return
	}

	listener, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		logger.Error("trae: 监听 127.0.0.1:18080 失败（可能有其他实例在运行）: %v", err)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", handleAuthorizeCallback)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		logger.Info("trae: 本地授权回调监听已启动: http://127.0.0.1:18080/authorize")
		if errServe := srv.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logger.Error("trae: 回调服务退出: %v", errServe)
		}
	}()
	callbackServerOn = true
}

// RegisterPending 注册一个进行中的登录会话。
func RegisterPending(store *SessionStore, s *PendingSession) { store.register(s) }

// GetPending 取出登录会话。
func GetPending(store *SessionStore, sessionID string) (*PendingSession, bool) {
	return store.get(sessionID)
}

// RemovePending 删除登录会话。
func RemovePending(store *SessionStore, sessionID string) { store.remove(sessionID) }

// OwnsSession 检查会话是否归属于该供应商。
func OwnsSession(store *SessionStore, sessionID, vendorID string) bool {
	return store.owns(sessionID, vendorID)
}

// MachineTraceID 从 machineID+deviceID 派生 16 位 trace ID。
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= 16 {
		return h[len(h)-16:]
	}
	return strings.Repeat("0", 16-len(h)) + h
}

// CallbackInfo 回调解析结果。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
	TraceID      string
	MachineID    string
	DeviceID     string
}

// ParseCallback 解析 /authorize 接收到的链接。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		TraceID:      q.Get("loginTraceID"),
		MachineID:    q.Get("machine_id"),
		DeviceID:     q.Get("device_id"),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = core.GetString(userInfo, "UserID")
	info.Nickname = core.GetString(userInfo, "ScreenName")
	info.EnterpriseID = core.GetString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := core.GetString(userJwt, "Token")
	jwtRefresh := core.GetString(userJwt, "RefreshToken")

	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		info.AccessToken = jwtToken
		if exp := getInt64Param(userJwt, "TokenExpireAt"); exp > 0 {
			if exp > 1e12 {
				exp /= 1000
			}
			info.ExpiresAt = exp
		}
	}

	if info.RefreshToken == "" && info.AccessToken == "" {
		return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
	}

	return info, nil
}

func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getInt64Param(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func handleAuthorizeCallback(w http.ResponseWriter, r *http.Request) {
	info, errParse := ParseCallback(r.URL.String())
	if errParse != nil {
		renderCallbackHTML(w, http.StatusBadRequest, "授权回调解析失败", errParse.Error())
		return
	}

	matched := matchCallbackSession(info)

	if matched == nil {
		renderCallbackHTML(w, http.StatusNotFound, "未找到对应的登录会话", "登录会话可能已过期或已被处理，请返回控制台重新发起登录。")
		return
	}

	// 执行 Token 换取
	authHost := HostOAuthSolo
	clientID := "trae_login"
	if !matched.IsSolo {
		cfg, okConfig := ConfigForVendor(matched.VendorID)
		if !okConfig {
			cfg = ConfigFor(matched.Region)
		}
		authHost = cfg.AuthHost
		clientID = cfg.ClientID
	}

	accessToken := info.AccessToken
	refreshToken := info.RefreshToken
	expiresAt := info.ExpiresAt

	if refreshToken != "" {
		newAccess, newRefresh, exp, errEx := exchangeTraeToken(r.Context(), authHost, clientID, refreshToken)
		if errEx != nil {
			logger.Error("trae: callback exchange token warning: %v, falling back to original", errEx)
		} else {
			accessToken = newAccess
			if newRefresh != "" {
				refreshToken = newRefresh
			}
			expiresAt = exp
		}
	}

	uid := info.UID
	if uid == "" {
		uid = "user_" + matched.MachineID[:min(8, len(matched.MachineID))]
	}
	nickname := info.Nickname
	if nickname == "" {
		nickname = uid
	}

	label := "Trae-" + nickname
	if matched.IsSolo {
		label = "TraeSolo-" + nickname
	} else if matched.Region == RegionGlobal {
		label = "TraeGlobal-" + nickname
	}

	storageMap := map[string]any{
		"type":          "freetier",
		"vendor":        matched.VendorID,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expires_at":    expiresAt,
		"uid":           uid,
		"nickname":      nickname,
		"enterprise_id": info.EnterpriseID,
		"machine_id":    matched.MachineID,
		"device_id":     matched.DeviceID,
		"label":         label,
	}
	if !matched.IsSolo {
		storageMap["region"] = string(matched.Region)
	}

	rawStorage, errJSON := json.MarshalIndent(storageMap, "", "  ")
	if errJSON != nil {
		matched.State = "failed"
		matched.ErrMsg = "生成凭证失败: " + errJSON.Error()
		renderCallbackHTML(w, http.StatusInternalServerError, "生成凭证失败", errJSON.Error())
		return
	}

	fileName := matched.VendorID + "-" + uid + ".json"
	matched.AuthData = &pluginapi.AuthData{
		Provider:    "freetier",
		ID:          matched.VendorID + "-" + uid,
		FileName:    fileName,
		Label:       label,
		StorageJSON: rawStorage,
	}
	matched.State = "success"

	logger.Info("trae: 授权成功，账号=%s (%s), vendor=%s", uid, nickname, matched.VendorID)
	renderCallbackHTML(w, http.StatusOK, "授权成功", "账号已成功授权！现在可以关闭此窗口返回管理面板。")
}

func exchangeTraeToken(ctx context.Context, authHost, clientID, refreshToken string) (string, string, int64, error) {
	payload := map[string]string{
		"refreshToken": refreshToken,
		"client_id":    clientID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authHost+"/api/v1/auth/exchange", bytes.NewReader(bodyBytes))
	if errReq != nil {
		return "", "", 0, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/1.0.0")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", 0, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", 0, fmt.Errorf("upstream exchange error %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken         string `json:"accessToken"`
			RefreshToken        string `json:"refreshToken"`
			TokenExpireAt       int64  `json:"tokenExpireAt"`
			TokenExpireDuration int64  `json:"tokenExpireDuration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", 0, err
	}
	if res.Code != 0 || res.Data.AccessToken == "" {
		return "", "", 0, fmt.Errorf("exchange error code=%d msg=%s", res.Code, res.Msg)
	}

	exp := res.Data.TokenExpireAt
	if exp > 1e12 {
		exp /= 1000
	}
	if exp <= 0 && res.Data.TokenExpireDuration > 0 {
		exp = time.Now().Unix() + res.Data.TokenExpireDuration
	}

	return res.Data.AccessToken, res.Data.RefreshToken, exp, nil
}

func renderCallbackHTML(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	color := "#3fb950"
	if code >= 400 {
		color = "#f85149"
	}
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #0d1117; color: #c9d1d9; }
    .card { background: #161b22; border: 1px solid #30363d; padding: 36px 40px; border-radius: 12px; box-shadow: 0 8px 24px rgba(0,0,0,0.5); text-align: center; max-width: 440px; }
    h2 { color: %s; margin: 0 0 16px 0; font-size: 24px; }
    p { margin: 8px 0; font-size: 15px; color: #8b949e; line-height: 1.5; }
    .msg { color: #f0f6fc; font-weight: 500; font-size: 16px; margin-bottom: 16px; }
    .tip { color: #6e7681; font-size: 13px; margin-top: 24px; }
  </style>
</head>
<body>
  <div class="card">
    <h2>%s</h2>
    <div class="msg">%s</div>
    <p class="tip">您可以关闭此页面返回管理面板。</p>
  </div>
  <script>
    if (%d === 200) {
      setTimeout(function() { window.close(); }, 3000);
    }
  </script>
</body>
</html>`, title, color, title, message, code)
	_, _ = w.Write([]byte(html))
}

// Token 续期：调用 ExchangeToken 轮换 accessToken 与 refreshToken。

func RefreshCredential(ctx context.Context, cred *Credential, cfg Config) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	payload := map[string]string{
		"refreshToken": cred.RefreshToken,
		"client_id":    cfg.ClientID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RefreshHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cfg.clientUA())

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("trae refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken         string `json:"accessToken"`
			RefreshToken        string `json:"refreshToken"`
			TokenExpireAt       int64  `json:"tokenExpireAt"`
			TokenExpireDuration int64  `json:"tokenExpireDuration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("decode exchange token response: %w", err)
	}

	if result.Code != 0 || result.Data.AccessToken == "" {
		return nil, false, fmt.Errorf("exchange token failed: code=%d msg=%s", result.Code, result.Msg)
	}

	expiresAt := result.Data.TokenExpireAt
	if expiresAt > 1e12 {
		expiresAt /= 1000
	}
	if expiresAt <= 0 && result.Data.TokenExpireDuration > 0 {
		expiresAt = time.Now().Unix() + result.Data.TokenExpireDuration
	}

	updated := *cred
	updated.AccessToken = result.Data.AccessToken
	if result.Data.RefreshToken != "" {
		updated.RefreshToken = result.Data.RefreshToken
	}
	updated.ExpiresAt = expiresAt

	return &updated, true, nil
}

func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	return core.MergeCredentialJSON(original, updated.StorageJSON, func(m map[string]any) {
		m["access_token"] = updated.AccessToken
		if updated.RefreshToken != "" {
			m["refresh_token"] = updated.RefreshToken
		}
		if updated.ExpiresAt > 0 {
			m["expires_at"] = updated.ExpiresAt
		}
		// TRAE SOLO 的凭证可能是嵌套 {auth:{...}} 结构，两处都要写。
		if authObj, ok := m["auth"].(map[string]any); ok {
			authObj["accessToken"] = updated.AccessToken
			if updated.RefreshToken != "" {
				authObj["refreshToken"] = updated.RefreshToken
			}
			if updated.ExpiresAt > 0 {
				authObj["expiresAt"] = updated.ExpiresAt
			}
		}
	})
}
