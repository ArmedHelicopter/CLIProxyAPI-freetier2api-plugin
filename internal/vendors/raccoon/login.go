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
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

// loginAuthorizePath 是官方授权页路径。
const loginAuthorizePath = "/code/authorize"

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
const pendingMessage = "等待用户在授权页完成登录…登录成功后浏览器会跳转 office-raccoon:// 开头的链接，" +
	"请把地址栏里的整条链接复制回控制台完成登录"

// CheckCallbackState 校验回调链接里的 state 与当前会话一致（逐字比对）。
func CheckCallbackState(session *PendingLogin, callbackState string) bool {
	return session != nil && strings.TrimSpace(callbackState) == session.State
}
