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
	"net/url"
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

// LoginStart 返回国际版 OAuth 的说明（上游 oauth-url 接口强制要求阿里云
// 风控验证码参数——浏览器端 JS SDK 生成，插件控制台没有该组件的位置，
// 实测不带参数得到 631002）。Agent2API 的解法是把官方 SDK 搬进自己的主
// 窗口跑；本插件的宿主形态没有等价入口，因此这里返回指引文案页，
// 用户按说明粘贴凭证或回调链接（LocalCallbackHint / SubmitCallback）。
func (oauth *oauthLoginAdapter) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	html := `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>AutoClaw 国际版登录</title></head>
<body style="font-family:-apple-system,sans-serif;max-width:640px;margin:48px auto;line-height:1.7">
<h2>AutoClaw 国际版登录</h2>
<p>国际版的 OAuth 授权链接由上游接口下发，且该接口强制要求<strong>阿里云风控验证码</strong>参数
（浏览器端 SDK 生成，无法在本控制台完成）。</p>
<p>请改用以下方式之一：</p>
<ol>
<li>在<strong>添加账号</strong>面板粘贴 <code>accessToken</code> / <code>refreshToken</code> 凭证 JSON
（可从 AutoClaw 桌面端登录态文件 <code>auth.json</code> 获取）；</li>
<li>或使用 Agent2API 等工具完成 OAuth 登录后，把拿到的凭证粘贴过来。</li>
</ol>
<p style="color:#888">凭证保存后插件会自动续期（refresh_token）。</p>
</body></html>`
	dataURL := "data:text/html;charset=utf-8," + url.PathEscape(html)
	// 宿主要求 login.start 返回非空 state（用于轮询路由与会话归属）；
	// 会话同时登记进会话表，SubmitCallback 的换票兜底通道依赖它。
	state := core.RandomHex(16)
	sessionID := oauth.ID() + "_guide_" + state
	if oauth.Env.Logins != nil {
		oauth.Env.Logins.Register(oauth.ID(), sessionID, autoclawOAuthTTL, autoclawOAuthPollInterval,
			&autoclawPendingLogin{Region: oauth.region, Vendor: oauthVendorGoogle,
				DeviceID: newDeviceID(), NavigateURI: OAuthNavigateURI(oauthVendorGoogle, 46952)})
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       dataURL,
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
