package raccoon

// 本文件实现小浣熊的网页授权登录（/code/authorize 授权页 + 回调链接粘贴）。
//
// 流程（与 Agent2API 的网页授权登录一致）：
//
//  1. LoginStart 生成一次性 state（64 位小写 hex），返回官方授权页 URL：
//     {origin}/code/authorize?login_source=desktop&appname=…&state=<state>
//  2. 用户在官方页面登录（微信/验证码/密码均可）；
//  3. 官方页面跳转自定义协议 office-raccoon://auth/callback?code=…&state=…
//     （浏览器打不开这个协议是正常的——地址栏里留着整条链接）；
//  4. 用户把整条回调链接粘回控制台，SubmitCallback 用 code 换票完成登录。
//
// 说明：panel 项目早期的微信扫码通道（/login/mp 承载页 +
// login_with_qrcode_code 轮询）已随上游页面下线而不可用（实测 404，
// 2026-10-24），因此本实现只保留授权页通道。
//
// state 的安全语义（Agent2API 同口径）：由插件生成、登记进共享会话表
// （带 TTL 与轮询节流）；回调里的 state 必须逐字相等才消费，一次性。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// loginAppName 是授权页上报的客户端名（与官方桌面客户端一致）。
const loginAppName = "办公小浣熊客户端"

// loginCallbackScheme / Host / Path 是官方客户端回调的固定形状。
const (
	loginCallbackScheme = "office-raccoon"
	loginCallbackHost   = "auth"
	loginCallbackPath   = "/callback"
)

// qrLoginTTL / qrPollInterval 是登录会话有效期与轮询节流。
const (
	qrLoginTTL     = 15 * time.Minute
	qrPollInterval = 2 * time.Second
)

// PendingLogin 是一次进行中的登录会话数据。
//
// TTL 与轮询节流由 internal/auth 的会话表统一管理，这里只保留协议状态。
type PendingLogin struct {
	State string
}

// StoreLoginSession 登记一次进行中的登录会话。
func StoreLoginSession(logins core.LoginSessions, sessionID string, session *PendingLogin) {
	if session == nil {
		return
	}
	logins.Register(VendorID, sessionID, qrLoginTTL, qrPollInterval, session)
}

// RemoveLoginSession 登录成功或终止后清理会话。
func RemoveLoginSession(logins core.LoginSessions, sessionID string) {
	logins.Delete(sessionID)
}

// CheckLoginSession 检查登录会话状态与节流。
// 返回 (session, exists, shouldPoll)。
func CheckLoginSession(logins core.LoginSessions, sessionID string) (*PendingLogin, bool, bool) {
	entry, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, false, false
	}
	pending, okPending := entry.Data.(*PendingLogin)
	if !okPending {
		return nil, false, false
	}
	return pending, true, pollState == core.PollReady
}

// pendingMessage 是轮询等待时的进度文案（告诉用户下一步做什么）。
const pendingMessage = "等待用户在授权页完成登录…确认授权后浏览器会跳回本机 127.0.0.1:37913/auth/callback；" +
	"插件与浏览器同机时自动完成，不同机时把浏览器地址栏里的整条链接复制回控制台即可"

// CheckCallbackState 校验回调链接里的 state 与当前会话一致（逐字比对）。
func CheckCallbackState(session *PendingLogin, callbackState string) bool {
	return session != nil && strings.TrimSpace(callbackState) == session.State
}

// ErrCodeExpired 是「授权码已失效」的上游业务码（200035）：不是登录服务
// 坏了，是用户把回调链接放太久或重复点击，文案必须提示重新发起。
const ErrCodeExpired = 200035

// ExchangeAuthorizationCode 用一次性授权码换凭证
// （POST {auth_base}/login_with_authorization_code）。
func ExchangeAuthorizationCode(ctx context.Context, authBase, code string) (access, refresh string, expiresAt int64, uid string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", 0, "", fmt.Errorf("upstream auth base URL is not configured")
	}
	body, errMarshal := json.Marshal(map[string]string{"authorization_code": code})
	if errMarshal != nil {
		return "", "", 0, "", fmt.Errorf("marshal exchange request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+LoginCodePath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", 0, "", fmt.Errorf("build exchange request: %w", errReq)
	}
	ApplyAPIHeaders(req, "")
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", 0, "", &core.Failure{Msg: "exchange: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "read exchange response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	errMsg := core.FirstNonEmpty(envelope.Message, envelope.Msg, envelope.Details)
	if envelope.Code != nil {
		if *envelope.Code == ErrCodeExpired || strings.Contains(errMsg, "authorization_code_not_found") {
			return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "授权码已失效或不存在，请重新发起登录", Kind: core.FailureKindClient}
		}
		if *envelope.Code != 0 {
			return "", "", 0, "", ClassifyBusiness(resp.StatusCode, *envelope.Code, errMsg)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UserID       string `json:"user_id"`
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errUnmarshal := json.Unmarshal(envelope.Data, &data); errUnmarshal != nil {
			return "", "", 0, "", fmt.Errorf("parse exchange data: %w", errUnmarshal)
		}
	}
	access = strings.TrimSpace(data.AccessToken)
	refresh = strings.TrimSpace(data.RefreshToken)
	if access == "" || refresh == "" {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "小浣熊未返回完整凭证（access_token/refresh_token）", Kind: core.FailureKindServer}
	}
	if ms := core.GetUnixSeconds(map[string]any{"expires_at": data.ExpiresAt}, "expires_at"); ms > 0 {
		expiresAt = ms
	}
	uid = strings.TrimSpace(data.UserID)
	return access, refresh, expiresAt, uid, nil
}

// ParseCallbackCode 从回调链接里取授权码与 state。
//
// 只认官方客户端回调的固定形态 office-raccoon://auth/callback?code=…&state=…
// （Agent2API oauth.rs 同口径）：scheme/host/path 逐项比对，不用前缀匹配——
// office-raccoon://auth/callback@evil.com 这类形态用 startsWith 会全部放行。
// 回调落在自定义协议上，任何本机程序都能构造一条丢进登录窗口，不校验 state
// 就等于把「用别人的 code 换来的凭证写进你的账号库」敞开。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", false
	}
	deepLink := strings.EqualFold(parsed.Scheme, loginCallbackScheme) &&
		strings.EqualFold(parsed.Host, loginCallbackHost) &&
		parsed.Path == loginCallbackPath
	loopback := parsed.Scheme == "http" &&
		(parsed.Hostname() == "127.0.0.1" || strings.EqualFold(parsed.Hostname(), "localhost")) &&
		parsed.Path == CallbackPath
	if !deepLink && !loopback {
		return "", "", false
	}
	query := parsed.Query()
	code = strings.TrimSpace(query.Get("code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("authorization_code"))
	}
	return code, query.Get("state"), code != ""
}

// AuthorizeURLWith 构造官方授权页 URL。
//
// 参数名必须与官方桌面客户端逐字一致：login_source=desktop & appname=<中文
// 客户端名> & state=<state>。早期实现写的是 app_name（带下划线）并多带一个
// client=desktop——官方页面认的是 appname，缺了它换票会拿到
// 「authorization code not found」（200035），表现就是「授权完登录不成功」。
func AuthorizeURLWith(origin, appName, state string) string {
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("appname", appName)
	if strings.TrimSpace(state) != "" {
		query.Set("state", state)
	}
	// 走 loopback 回跳：深链 office-raccoon:// 会被官方客户端吃掉，插件拿不到
	// code。授权页对 desktop_callback=loopback 只放行 127.0.0.1 的 /auth/callback
	// 且要求 state 是 64 位十六进制（core.RandomHex(32) 正好满足）。
	query.Set("desktop_callback", "loopback")
	query.Set("redirect", CallbackURL())
	return strings.TrimRight(origin, "/") + AuthorizePath + "?" + query.Encode()
}

// ---- loopback 回调监听 ----
//
// 授权页现在只接受两种回跳：office-raccoon:// 深链（会被官方客户端吃掉，
// 插件永远收不到 code），或 http://127.0.0.1:<port>/auth/callback。
// 因此登录时在本机起一次性监听，授权页确认后浏览器直接打过来，登录当场完成。
// 浏览器与插件不同机时回调打不到这里，地址栏仍留着完整链接，粘回控制台走
// SubmitCallback 兜底，两条路共用 completeLogin。

// CallbackPort 是 loopback 回调端口。授权页只放行 1024-65535，
// 且 redirect 必须形如 http://127.0.0.1:<port>/auth/callback。
const CallbackPort = 37913

// CallbackPath 是 loopback 回调路径（授权页硬编码校验，不能改）。
const CallbackPath = "/auth/callback"

// CallbackURL 返回本机回调地址。
func CallbackURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", CallbackPort, CallbackPath)
}

// catcher 是一次登录期间驻留的回调监听。
type catcher struct {
	server  *http.Server
	state   string
	onCode  func(code, state string) (string, error)
	once    sync.Once
	stopped chan struct{}
}

var (
	catcherMu sync.Mutex
	active    *catcher
)

// StartCallbackCatcher 为指定 state 启动回调监听。
//
// 端口被占、或已有监听在跑时返回 false，调用方据此退回「粘贴链接」流程，
// 不影响登录本身。
func StartCallbackCatcher(state string, onCode func(code, state string) (string, error)) bool {
	catcherMu.Lock()
	defer catcherMu.Unlock()
	if active != nil {
		return false
	}
	listener, errListen := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", CallbackPort))
	if errListen != nil {
		return false
	}
	current := &catcher{state: state, onCode: onCode, stopped: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, current.handle)
	current.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	active = current
	go func() {
		_ = current.server.Serve(listener)
		close(current.stopped)
	}()
	return true
}

// StopCallbackCatcher 结束当前监听（幂等）。
func StopCallbackCatcher() {
	catcherMu.Lock()
	current := active
	active = nil
	catcherMu.Unlock()
	if current == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = current.server.Shutdown(ctx)
}

func (c *catcher) handle(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" {
		writeCallbackPage(writer, http.StatusBadRequest, "回调缺少 code，请回到控制台重新发起登录。")
		return
	}
	message, errComplete := c.onCode(code, state)
	if errComplete != nil {
		writeCallbackPage(writer, http.StatusBadRequest, "登录失败："+errComplete.Error())
		return
	}
	writeCallbackPage(writer, http.StatusOK, message)
	c.once.Do(func() { go StopCallbackCatcher() })
}

// writeCallbackPage 渲染结果页。只写纯静态文本，不回显任何请求参数。
func writeCallbackPage(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<title>小浣熊登录</title><style>body{font-family:system-ui,-apple-system,'Segoe UI',sans-serif;" +
		"background:#0f1115;color:#e6e8ee;display:flex;align-items:center;justify-content:center;" +
		"height:100vh;margin:0}div{text-align:center;line-height:1.8}h1{font-size:18px;font-weight:600}" +
		"p{color:#9aa3b2;font-size:14px}</style></head><body><div><h1>" + message + "</h1>" +
		"<p>可以关闭此页面，回到控制台查看账号。</p></div></body></html>"))
}
