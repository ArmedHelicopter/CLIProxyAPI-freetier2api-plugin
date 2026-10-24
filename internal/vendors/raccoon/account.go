package raccoon

// 本文件实现小浣熊的凭证续期、额度查询、每日签到与登录换票。
//
// 端点口径（xiaohuanxiong2api-panel 抓包实测）：
//   - 续期：POST {auth_base}/refresh body {"refresh_token":…}，响应可能
//     轮换 refresh_token（空 = 未轮换，沿用旧值）；
//   - 积分：GET {points_base}/balance（available_points 及各来源分类）；
//   - 签到：每日发放由服务端 00:00 自动入账；客户端侧的登录领取走
//     POST {origin}/api/web/desktop/v1/login/points/grant（幂等，已领回
//     granted:false）；
//   - 登录：授权码换票 POST {auth_base}/login_with_authorization_code
//     body {"authorization_code":…}；业务码 200035 = 授权码已失效。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// apiHTTPTimeout 是 JSON API 请求的上限。
const apiHTTPTimeout = 30 * time.Second

// ErrCodeExpired 是「授权码已失效」的上游业务码（200035）：不是登录服务
// 坏了，是用户把回调链接放太久或重复点击，文案必须提示重新发起。
const ErrCodeExpired = 200035

// ErrCheckinNotGranted 表示登录领取时上游返回 granted:false（今日已发放）。
var ErrCheckinNotGranted = errors.New("今日积分已发放（幂等命中）")

// RefreshToken 用 refresh token 换新票（POST {auth_base}/refresh）。
//
// 返回新的 access / refresh token；refresh 为空表示上游未轮换。
func RefreshToken(ctx context.Context, authBase string, cred *Credential) (access, refresh string, expiresAt int64, uid string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", 0, "", fmt.Errorf("upstream auth base URL is not configured")
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", 0, "", &core.Failure{Msg: "该账号没有 refresh_token，无法续期，请重新登录", Kind: core.FailureKindClient}
	}
	body, errMarshal := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if errMarshal != nil {
		return "", "", 0, "", fmt.Errorf("marshal refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+RefreshPath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", 0, "", fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", 0, "", &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", "", 0, "", &core.Failure{Status: http.StatusUnauthorized, Msg: "小浣熊刷新令牌已失效，请重新登录该账号", Kind: core.FailureKindSessionDead}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return "", "", 0, "", ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UserID       string `json:"user_id"`
		UID          string `json:"uid"`
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errUnmarshal := json.Unmarshal(envelope.Data, &data); errUnmarshal != nil {
			return "", "", 0, "", fmt.Errorf("parse refresh data: %w", errUnmarshal)
		}
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "小浣熊续期响应缺少令牌，旧凭证未被覆盖", Kind: core.FailureKindServer}
	}
	access = strings.TrimSpace(data.AccessToken)
	refresh = strings.TrimSpace(data.RefreshToken)
	if ms := core.GetUnixSeconds(map[string]any{"expires_at": data.ExpiresAt}, "expires_at"); ms > 0 {
		expiresAt = ms
	}
	uid = core.FirstNonEmpty(data.UserID, data.UID)
	return access, refresh, expiresAt, uid, nil
}

// FetchBalance 查询积分余额（GET {points_base}/balance）。
func FetchBalance(ctx context.Context, pointsBase string, cred *Credential) (available int64, err error) {
	if strings.TrimSpace(pointsBase) == "" {
		return 0, fmt.Errorf("upstream points base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, pointsBase+BalancePath, nil)
	if errReq != nil {
		return 0, fmt.Errorf("build balance request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, &core.Failure{Msg: "balance: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return 0, &core.Failure{Status: resp.StatusCode, Msg: "read balance response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, Classify(resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AvailablePoints any `json:"available_points"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return 0, Classify(resp.StatusCode, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return 0, ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
	}
	return int64(core.IntFromAny(envelope.Data.AvailablePoints)), nil
}

// ClaimDailyGrant 触发登录领取积分（POST {origin}/api/web/desktop/v1/login/points/grant）。
//
// 上游幂等：今日已发放时返回 granted:false，这里转成 ErrCheckinNotGranted。
// 每日发放本体是服务端 00:00 做的，这条只是兜住登录弹窗式的手动领取路径。
func ClaimDailyGrant(ctx context.Context, origin string, cred *Credential) (granted bool, err error) {
	if strings.TrimSpace(origin) == "" {
		return false, fmt.Errorf("upstream origin is not configured")
	}
	endpoint := strings.TrimRight(origin, "/") + DesktopBasePath + PointsGrantPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if errReq != nil {
		return false, fmt.Errorf("build grant request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return false, &core.Failure{Msg: "grant: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return false, &core.Failure{Status: resp.StatusCode, Msg: "read grant response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, Classify(resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Granted bool `json:"granted"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return false, Classify(resp.StatusCode, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return false, ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
	}
	if !envelope.Data.Granted {
		return false, ErrCheckinNotGranted
	}
	return true, nil
}

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
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	if envelope.Code != nil {
		if *envelope.Code == ErrCodeExpired {
			return "", "", 0, "", &core.Failure{Msg: "授权码已失效，请重新发起登录", Kind: core.FailureKindClient}
		}
		if *envelope.Code != 0 {
			return "", "", 0, "", ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
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

// FetchUserInfo 查询账号资料（GET {auth_base}/user_info），补 uid / 昵称。
func FetchUserInfo(ctx context.Context, authBase string, cred *Credential) (uid, nickname string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", fmt.Errorf("upstream auth base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, authBase+UserInfoPath, nil)
	if errReq != nil {
		return "", "", fmt.Errorf("build user info request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", &core.Failure{Msg: "user info: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", &core.Failure{Status: resp.StatusCode, Msg: "read user info response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", Classify(resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", Classify(resp.StatusCode, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return "", "", ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
	}
	return strings.TrimSpace(envelope.Data.ID), strings.TrimSpace(envelope.Data.Name), nil
}

// AuthorizeURL 构造网页授权页（登录入口展示给用户打开）。
func AuthorizeURL(origin, appName, state string) string {
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("appname", appName)
	query.Set("state", state)
	return strings.TrimRight(origin, "/") + AuthorizePath + "?" + query.Encode()
}

// ParseCallbackCode 从回调链接里取授权码与 state。
//
// 只认 office-raccoon://auth/callback（官方客户端回调）与
// http(s)://…/auth/callback（环回回调）两种形状；裸授权码（ac_ 前缀）也认。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	if strings.HasPrefix(raw, "ac_") && !strings.Contains(raw, "://") && !strings.Contains(raw, "?") {
		return raw, "", true
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", false
	}
	query := parsed.Query()
	switch {
	case strings.EqualFold(parsed.Scheme, "office-raccoon"):
		// 官方客户端回调：office-raccoon://auth/callback?code=…&state=…
		// host 与 path 逐项比对（前缀匹配会被 evil.com 形态绕过）。
		if !strings.EqualFold(parsed.Host, "auth") || parsed.Path != "/callback" {
			return "", "", false
		}
	case parsed.Scheme == "http" || parsed.Scheme == "https":
		if parsed.Path != "/auth/callback" {
			return "", "", false
		}
	default:
		return "", "", false
	}
	code = query.Get("code")
	if code == "" {
		code = query.Get("authorization_code")
	}
	return code, query.Get("state"), code != ""
}
