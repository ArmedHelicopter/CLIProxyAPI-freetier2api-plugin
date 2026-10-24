package autoclaw

// 本文件实现 AutoClaw 国际版的 OAuth 网页登录（Zai / Google）与国内版
// core.SMSLoginProvider 的接线。
//
// 国际版协议（Agent2API providers/autoclaw/oauth.rs，桌面端逆向 + 实测）：
//
//	① POST {intl}/userapi/overseasv1/oauth-captcha-config {}
//	     → data.enabled 是「这个构建要不要走 OAuth」的开关（国内 false）；
//	② POST {intl}/userapi/overseasv1/{zai|google}-oauth-url
//	     {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	     → data.oauth_url；
//	③ 浏览器登录 → 302 到 {navigate_uri}?code=…&state=…
//	   POST {intl}/userapi/overseasv1/{zai|google}-oauth-login
//	     {source_id, device_id, code, state, navigate_uri} → token 对。
//
// 卡点与取舍：
//   - 第 ② 步强制要求阿里云风控验证码（浏览器端 JS SDK，缺它得误导性的
//     631002）。插件控制台可以在页面上跑官方 SDK（Agent2API 就是把官方
//     SDK 原样搬到自己的窗口里跑的），但控制台的登录框契约是「打开 URL +
//     轮询/粘贴回调」，没有渲染验证码组件的位置。
//   - Google 的回调白名单是 RFC 8252 loopback 宽松匹配（端口任意），浏览器
//     登录完成后会停在 http://localhost:<任意>/auth/callback-google?code=…，
//     用户把地址栏整条链接粘回来即可换码（第 ③ 步不需要验证码，实测）。
//   - Zai 的白名单只认官方客户端那四个登记端口（18432/19654/19723/53699），
//     且第 ② 步的验证码这关插件侧过不去 —— Zai 暂不支持，入口处明确说明。
//
// 因此本实现：Google = 完整链路（无需验证码组件）；Zai = 入口拒绝并说明。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 国际版 OAuth 端点（挂在 userapi 域下）。
const (
	OAuthCaptchaConfigPath = "/userapi/overseasv1/oauth-captcha-config"
	OAuthURLPathZai        = "/userapi/overseasv1/zai-oauth-url"
	OAuthURLPathGoogle     = "/userapi/overseasv1/google-oauth-url"
	OAuthLoginPathZai      = "/userapi/overseasv1/zai-oauth-login"
	OAuthLoginPathGoogle   = "/userapi/overseasv1/google-oauth-login"
)

// oauthCallbackPrefix 是回调路径前缀（与官方客户端逐字同款）。
const oauthCallbackPrefix = "/auth/callback-"

// oauthVendor 标识国际版 OAuth 的两个变体。
type oauthVendor string

const (
	oauthVendorZai    oauthVendor = "zai"
	oauthVendorGoogle oauthVendor = "google"
)

// urlPath / loginPath 返回该变体的两个端点。
func (v oauthVendor) urlPath() string {
	if v == oauthVendorZai {
		return OAuthURLPathZai
	}
	return OAuthURLPathGoogle
}

func (v oauthVendor) loginPath() string {
	if v == oauthVendorZai {
		return OAuthLoginPathZai
	}
	return OAuthLoginPathGoogle
}

// label 返回界面上那个按钮的名字。
func (v oauthVendor) label() string {
	if v == oauthVendorZai {
		return "Zai"
	}
	return "Google"
}

// describeOAuthError 业务码 → 用户能看懂的原因（Agent2API 同口径：
// 631002 的原文「当前版本已停止服务」对网关用户是错的——它其实是缺风控验证）。
func describeOAuthError(code int64, upstreamMsg string) string {
	switch code {
	case 631002:
		return "本次登录缺少风控验证（验证码未通过或已过期），请重新点击登录再试一次"
	case 630014:
		return "风控验证未通过，请重新点击登录再试一次"
	case 631001:
		return "授权码无效或已过期，请重新点击登录"
	case 400002:
		return "请求签名校验失败（本机时钟可能有偏差），请校准系统时间后重试"
	}
	if strings.TrimSpace(upstreamMsg) != "" {
		return "登录失败：" + strings.TrimSpace(upstreamMsg)
	}
	return fmt.Sprintf("登录失败（上游 code=%s）", strconv.FormatInt(code, 10))
}

// postSigned 发一次带签名的匿名 userapi POST（OAuth 链路自己的错误码表）。
func postSigned(ctx context.Context, baseURL, path string, body any, what string) (json.RawMessage, error) {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal %s request: %w", what, errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", what, errReq)
	}
	ApplyUserAPIHeaders(req, "")

	resp, errDo := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: what + ": " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read " + what + " response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	return json.RawMessage(raw), nil
}

// OAuthRequestURL 取 OAuth 授权地址并返回官方登录页 URL。
//
// captchaVerifyParam 是控制台页面跑完阿里云 SDK 得到的不透明字符串，网关
// 不解析、只转交。Google 在没有验证码组件的部署形态下可以传空——上游对
// google-oauth-url 的风控要求与 Zai 相同，但 Google 的回调白名单宽松，
// 部分部署实测可跳过；失败时按 631002 文案回退到「填写凭证」。
func OAuthRequestURL(ctx context.Context, baseURL string, vendor oauthVendor, navigateURI, captchaVerifyParam, deviceID string) (string, error) {
	body := map[string]any{
		"source_id":    protocolName,
		"device_id":    deviceID,
		"navigate_uri": navigateURI,
	}
	if strings.TrimSpace(captchaVerifyParam) != "" {
		body["ali_captcha_verify_param"] = strings.TrimSpace(captchaVerifyParam)
	}
	raw, errPost := postSigned(ctx, baseURL, vendor.urlPath(), body, "获取 "+vendor.label()+" 授权地址")
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, _ := smsEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeOAuthError(code, msg), http.StatusBadRequest)
	}
	var data struct {
		OAuthURL string `json:"oauth_url"`
	}
	if errUnmarshal := json.Unmarshal(smsData(raw), &data); errUnmarshal != nil {
		return "", &core.Failure{Msg: "parse oauth url response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	if strings.TrimSpace(data.OAuthURL) == "" {
		return "", core.NewPluginError("login_failed", "上游没有返回授权地址，请重试", http.StatusBadGateway)
	}
	return strings.TrimSpace(data.OAuthURL), nil
}

// OAuthExchangeCode 用回调里的一次性授权码换凭证。
//
// state 是**上游**回调带回来的那个值（不是插件生成的）——官方客户端就是
// 原样传回；它为空时传空串（上游接受这种形态）。
func OAuthExchangeCode(ctx context.Context, baseURL string, vendor oauthVendor, code, state, navigateURI, deviceID string) (*Credential, error) {
	body := map[string]any{
		"source_id":    protocolName,
		"device_id":    deviceID,
		"code":         strings.TrimSpace(code),
		"state":        strings.TrimSpace(state),
		"navigate_uri": navigateURI,
	}
	raw, errPost := postSigned(ctx, baseURL, vendor.loginPath(), body, vendor.label()+" 登录换取凭证")
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	status, msg, _ := smsEnvelope(raw)
	if status != 0 {
		return nil, core.NewPluginError("login_failed", describeOAuthError(status, msg), http.StatusBadRequest)
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       any    `json:"user_id"`
		UserName     string `json:"user_name"`
	}
	if errUnmarshal := json.Unmarshal(smsData(raw), &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse oauth login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	access := strings.TrimSpace(data.AccessToken)
	if access == "" {
		return nil, core.NewPluginError("login_failed", "登录成功但上游没有返回 access_token", http.StatusBadGateway)
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		UserName:     strings.TrimSpace(data.UserName),
		DeviceID:     deviceID,
		Region:       RegionGlobal,
		ExpiresAt:    jwtExpiry(access),
	}
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = time.Now().Add(28 * 24 * time.Hour).Unix()
	}
	if cred.UID == "" {
		cred.UID = uidFromAny(data.UserID)
	}
	if cred.UID == "" {
		cred.UID = jwtUserID(access)
	}
	return cred, nil
}

// OAuthNavigateURI 拼这次登录的回调地址（同时是交给上游的 navigate_uri）。
//
// host 必须是 localhost（Zai 白名单逐字校验 host）；Google 是 RFC 8252
// loopback 宽松匹配，端口任意——这里用 46952（未占用的高位端口），
// 浏览器登录完成后停在「打不开」的页面上，用户把地址栏整条链接粘回来。
func OAuthNavigateURI(vendor oauthVendor, port int) string {
	if port <= 0 {
		port = 46952
	}
	return fmt.Sprintf("http://localhost:%d%s%s", port, oauthCallbackPrefix, vendor)
}

// ParseOAuthCallback 从回调链接里提取 code / state / 变体。
//
// 接受两条形态：/auth/callback-zai|google（本插件拼的 navigate_uri）与
// 官方客户端的 query 形态。code 一次性、state 原样带回（换码要用上游的）。
func ParseOAuthCallback(raw string) (vendor oauthVendor, code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", "", false
	}
	if parsed.Hostname() != "localhost" || !strings.HasPrefix(parsed.Path, oauthCallbackPrefix) {
		// navigate_uri 的 host 必须是 localhost（Zai 白名单逐字校验 host；
		// 别的 host 即便 path 相同也是伪造回调）。
		return "", "", "", false
	}
	switch strings.TrimPrefix(parsed.Path, oauthCallbackPrefix) {
	case string(oauthVendorZai):
		vendor = oauthVendorZai
	case string(oauthVendorGoogle):
		vendor = oauthVendorGoogle
	default:
		return "", "", "", false
	}
	query := parsed.Query()
	code = strings.TrimSpace(query.Get("code"))
	state = strings.TrimSpace(query.Get("state"))
	return vendor, code, state, code != ""
}
