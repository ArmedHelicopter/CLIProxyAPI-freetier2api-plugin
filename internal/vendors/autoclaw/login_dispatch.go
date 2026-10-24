// 本文件实现 AutoClaw 两个区域的登录能力分派。
//
// 供应商实例 = 协议 × 区域，登录方式随地区不同（core 的接口隔离纪律：
// 只实现自己真实具备的能力，控制台据此渲染不同的添加表单）：
//   - smsLoginAdapter（国内版）：core.SMSLoginProvider（手机号 + 验证码）；
//   - oauthLoginAdapter（国际版）：core.LoginProvider + core.CallbackSubmitter
//     （Google OAuth 网页登录 + 回调链接粘贴）。
// 共享的 adapter 自身不实现任何登录接口——把 SMS 方法挂在共享类型上会让
// 国际版在控制台也出现手机号表单（虽然运行时会被拒绝，但 UI 撒谎了）。

package autoclaw

import (
	"context"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// SendLoginCode 发送短信验证码（国内版专用，core.SMSLoginProvider）。
func (sms *smsLoginAdapter) SendLoginCode(ctx context.Context, phone string) (string, error) {
	if sms.region.IsGlobal() {
		return "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	return SendLoginCode(ctx, sms.resolveBaseURL(), phone)
}

// VerifyLoginCode 用验证码完成登录（国内版专用）。
func (sms *smsLoginAdapter) VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) ([]byte, string, error) {
	if sms.region.IsGlobal() {
		return nil, "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	cred, errLogin := VerifyLoginCode(ctx, sms.resolveBaseURL(), sessionKey, phone, code)
	if errLogin != nil {
		return nil, "", core.ToPluginError(errLogin)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, "", core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	label := core.FirstNonEmpty(cred.UserNameValue(), MaskPhone(cred.Phone), cred.UIDValue())
	return storageJSON, label, nil
}

// autoclawPendingLogin 是一次进行中的国际版 OAuth 登录会话。
//
// LoginStart 把 deviceID 与变体放进会话表；SubmitCallback 取出后用回调里
// 的 code/state 换票（state 用上游带回的值，官方客户端就是原样传回）。
type autoclawPendingLogin struct {
	Region      Region
	Vendor      oauthVendor
	DeviceID    string
	NavigateURI string
}

// LoginStart 发起一次国际版 OAuth 登录（Google）。
//
// 返回的 URL 是官方登录页；登录完成后浏览器停在本机回调地址上，用户把
// 地址栏整条链接粘回控制台（SubmitCallback）完成换票。国内版没有 OAuth
// （上游开关 enabled:false），返回说明性错误。
func (oauth *oauthLoginAdapter) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	if !oauth.region.IsGlobal() {
		return nil, core.NewPluginError("login_unsupported",
			"AutoClaw 国内版使用「手机号 + 验证码」登录，请在添加账号面板选择对应入口", http.StatusBadRequest)
	}
	if oauth.Env.Logins == nil {
		return nil, core.NewPluginError("login_unavailable", "登录会话表未初始化（宿主未就绪）", http.StatusInternalServerError)
	}
	deviceID := newDeviceID()
	// Zai 的白名单只认官方客户端的四个登记端口且第②步要过风控验证码
	// （插件侧无验证码组件），先只开放 Google。
	vendor := oauthVendorGoogle
	navigateURI := OAuthNavigateURI(vendor, 46952)
	state := core.RandomHex(16)
	sessionID := oauth.ID() + "_oauth_" + state
	oauth.Env.Logins.Register(oauth.ID(), sessionID, autoclawOAuthTTL, autoclawOAuthPollInterval,
		&autoclawPendingLogin{Region: oauth.region, Vendor: vendor, DeviceID: deviceID, NavigateURI: navigateURI})
	oauthURL, errURL := OAuthRequestURL(ctx, oauth.resolveBaseURL(), vendor, navigateURI, "", deviceID)
	if errURL != nil {
		oauth.Env.Logins.Delete(sessionID)
		return nil, errURL
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       oauthURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(autoclawOAuthTTL),
		Metadata: map[string]any{
			core.VendorKey: oauth.ID(),
			"region":       string(oauth.region),
		},
	}, nil
}

// LoginPoll 轮询一次国际版 OAuth 登录状态（等待用户粘贴回调链接）。
func (oauth *oauthLoginAdapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	entry, pollState := oauth.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	_ = entry
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusPending,
		Message: "等待用户在授权页完成 Google 登录…登录完成后浏览器会停在 localhost 回调页，" +
			"请把地址栏里的整条链接复制回控制台提交",
	}, nil
}

// LocalCallbackHint 返回控制台要展示的本机回调地址（Google 宽松匹配任意端口）。
func (oauth *oauthLoginAdapter) LocalCallbackHint() string {
	return "http://localhost:46952/auth/callback-google"
}

// SubmitCallback 解析用户粘贴的回调链接并完成国际版 OAuth 换票。
func (oauth *oauthLoginAdapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	entry, pollState := oauth.Env.Logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found",
			"登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "登录会话数据已损坏，请重新发起登录", http.StatusBadRequest)
	}
	vendor, code, upstreamState, okCode := ParseOAuthCallback(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是登录完成后浏览器停在的整条链接（含 code=…）",
			http.StatusBadRequest)
	}
	if vendor != pending.Vendor {
		return nil, core.NewPluginError("callback_mismatch",
			"回调链接与本次登录的登录方式不一致，请重新发起登录", http.StatusBadRequest)
	}
	cred, errExchange := OAuthExchangeCode(ctx, oauth.resolveBaseURL(), vendor, code, upstreamState, pending.NavigateURI, pending.DeviceID)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	oauth.Env.Logins.Delete(sessionID)
	label := core.FirstNonEmpty(cred.UserNameValue(), cred.UIDValue())
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "AutoClaw 登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(oauth.ID(), cred.UIDValue()),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}
