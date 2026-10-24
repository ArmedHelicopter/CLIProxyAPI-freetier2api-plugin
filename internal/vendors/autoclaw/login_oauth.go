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
// 卡点与解法（2026-10 修正）：
//   - 第 ② 步强制要求阿里云风控验证码（浏览器端 JS SDK，缺它得误导性的
//     631002）。插件控制台**本身就是我们自己的页面**，可以直接加载
//     o.alicdn.com 的官方 SDK（与 ZCode「领套餐」同一套控件），把
//     captchaVerifyParam 交给 LoginStart，再由这里原样转交上游。
//   - Google 的回调白名单是 RFC 8252 loopback 宽松匹配（端口任意），浏览器
//     登录完成后会停在 http://localhost:46952/auth/callback-google?code=…；
//     插件跑在远程主机/Docker 时这个地址到不了服务端，用户把地址栏整条链接
//     粘回来即可换码（第 ③ 步不需要验证码，实测）。
//   - Zai 的白名单只认官方客户端那四个登记端口
//     （18432/19654/19723/53699）。这里固定用 53699：官方客户端启动时会先占
//     第一个空闲端口（通常是 18432），挑末尾那个能避开「回调被桌面端吃掉」。
//
// 因此本实现：Zai / Google 都走完整链路（人机验证 → 授权地址 → 粘贴回调 → 换码）。

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
	ApplyOAuthHeaders(req, "")

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

// OAuthCaptchaConfig 取风控验证配置（控制台据此初始化阿里云 SDK）。
//
// 返回 enabled=false 不是错误：上游国际化构建会关掉这条登录方式，入口应当
// 如实告诉用户「这家没有网页登录」，而不是把所有人都引到一次必然失败的滑块。
func OAuthCaptchaConfig(ctx context.Context, baseURL string) (core.CaptchaConfig, error) {
	raw, errPost := postSigned(ctx, baseURL, OAuthCaptchaConfigPath, struct{}{}, "获取风控验证配置")
	if errPost != nil {
		return core.CaptchaConfig{}, core.ToPluginError(errPost)
	}
	code, msg, _ := smsEnvelope(raw)
	if code != 0 {
		return core.CaptchaConfig{}, core.NewPluginError("login_failed", describeOAuthError(code, msg), http.StatusBadGateway)
	}
	var data struct {
		Enabled bool   `json:"enabled"`
		Region  string `json:"region"`
		Prefix  string `json:"prefix"`
		SceneID string `json:"scene_id"`
	}
	if errUnmarshal := json.Unmarshal(smsData(raw), &data); errUnmarshal != nil {
		return core.CaptchaConfig{}, &core.Failure{Msg: "parse oauth captcha config: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	return core.CaptchaConfig{
		Enabled: data.Enabled,
		SceneID: strings.TrimSpace(data.SceneID),
		Region:  strings.TrimSpace(data.Region),
		Prefix:  strings.TrimSpace(data.Prefix),
	}, nil
}

// OAuthRequestURL 取 OAuth 授权地址并返回官方登录页 URL。
//
// captchaVerifyParam 是控制台页面跑完阿里云 SDK 得到的不透明字符串，网关
// 不解析、只转交。Google 在没有验证码组件的部署形态下可以传空——上游对
// google-oauth-url 的风控要求与 Zai 相同，但 Google 的回调白名单宽松，
// 部分部署实测可跳过；失败时按 631002 文案回退到「填写凭证」。
func OAuthRequestURL(ctx context.Context, baseURL string, vendor oauthVendor, navigateURI, captchaVerifyParam, deviceID string) (string, error) {
	captcha := strings.TrimSpace(captchaVerifyParam)
	if captcha == "" {
		// 本地就挡掉：不带它打上游必然得到那条误导性的 631002，
		// 不如在这里说清「验证码这一步没完成」。
		return "", core.NewPluginError("captcha_required", "缺少风控验证参数，请先完成人机验证", http.StatusBadRequest)
	}
	body := map[string]any{
		"source_id":                protocolName,
		"device_id":                deviceID,
		"navigate_uri":             navigateURI,
		"ali_captcha_verify_param": captcha,
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

// 回调端口：Zai 只能用官方客户端登记过的那四个端口（见文件头），
// Google 走 RFC 8252 loopback 匹配，端口任意。
const (
	oauthCallbackPortZai    = 53699
	oauthCallbackPortGoogle = 46952
)

// OAuthNavigateURI 拼这次登录的回调地址（同时是交给上游的 navigate_uri）。
//
// host 必须是 localhost（Zai 白名单逐字校验 host）；port<=0 时按变体取默认
// 端口。插件不监听这个端口：远程/Docker 部署下浏览器停在「打不开」的页面上，
// 用户把地址栏整条链接粘回来即可。
func OAuthNavigateURI(vendor oauthVendor, port int) string {
	if port <= 0 {
		if vendor == oauthVendorZai {
			port = oauthCallbackPortZai
		} else {
			port = oauthCallbackPortGoogle
		}
	}
	return fmt.Sprintf("http://localhost:%d%s%s", port, oauthCallbackPrefix, vendor)
}

// OAuthVariants 返回控制台要渲染的两个登录入口（顺序与官方客户端一致：Zai 在前）。
func OAuthVariants() []core.LoginVariant {
	return []core.LoginVariant{
		{ID: string(oauthVendorZai), Label: "Zai", CallbackHint: OAuthNavigateURI(oauthVendorZai, 0)},
		{ID: string(oauthVendorGoogle), Label: "Google", CallbackHint: OAuthNavigateURI(oauthVendorGoogle, 0)},
	}
}

// oauthVariantFromMetadata 解析控制台传来的变体标识。
//
// 缺省按 Google：宿主旧路径（直接点「添加认证」）不会带这个字段，而 Google
// 是唯一在任意端口都能完成回调的变体。
func oauthVariantFromMetadata(meta map[string]any) (oauthVendor, error) {
	raw := strings.ToLower(metadataString(meta, "oauth_variant"))
	switch raw {
	case "", string(oauthVendorGoogle):
		return oauthVendorGoogle, nil
	case string(oauthVendorZai):
		return oauthVendorZai, nil
	}
	return "", core.NewPluginError("invalid_request", "未知的登录方式："+raw, http.StatusBadRequest)
}

// metadataString 取 metadata 里的字符串值（缺失或非字符串返回空串）。
func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	value, okValue := meta[key].(string)
	if !okValue {
		return ""
	}
	return strings.TrimSpace(value)
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
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		// navigate_uri 的 host 必须是回环地址（Zai 白名单逐字校验 localhost；
		// 别的 host 即便 path 相同也是伪造回调）。用户从地址栏复制时，
		// 部分浏览器/代理会把 localhost 规范化成 127.0.0.1 或 [::1]，一并接受。
		return "", "", "", false
	}
	if !strings.HasPrefix(parsed.Path, oauthCallbackPrefix) {
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
