package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

// 本文件实现 Cline 的登录流程（WorkOS OAuth 2.0 设备码，RFC 8628）。
//
// 三步（移植自 cline2api 的 auth.go）：
//  1. POST {workos}/user_management/authorize/device  取 device_code 与 user_code；
//  2. 用户在浏览器里完成授权，插件轮询 POST {workos}/user_management/authenticate
//     直到拿到 WorkOS 的 access/refresh token；
//  3. POST {cline}/auth/register 用 WorkOS 令牌换 Cline 自己的令牌。
//
// 为什么用设备码而不是 PKCE 回调：插件跑在宿主进程里，没有自己的
// HTTP 监听端口，回调式授权无处落地。设备码只需要用户手动打开一个链接。

const (
	// workosClientID 是 Cline 官方客户端的 WorkOS 客户端标识。
	//
	// 这不是密钥，是公开的客户端 ID（官方客户端里硬编码同一个值）。
	workosClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
	// workosDeviceAuthURL 是设备码申请端点。
	workosDeviceAuthURL = "https://api.workos.com/user_management/authorize/device"
	// workosAuthenticateURL 是设备码换令牌端点。
	workosAuthenticateURL = "https://api.workos.com/user_management/authenticate"

	// deviceGrantType 是设备码流程的授权类型（RFC 8628 固定值）。
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// minPollInterval 是轮询间隔的下限。
	//
	// 上游给的 interval 可能小到 1 秒，但那样会被 WorkOS 限流并返回
	// slow_down；5 秒是官方客户端采用的值。
	minPollInterval = 5 * time.Second
	// defaultDeviceTTL 是设备码有效期兜底（上游没给时）。
	defaultDeviceTTL = 300 * time.Second

	// loginHTTPTimeout 是登录相关单次请求的上限。
	loginHTTPTimeout = 30 * time.Second
)

// DeviceAuth 是一次设备授权的起始信息。
type DeviceAuth struct {
	// DeviceCode 是轮询用的设备码（不展示给用户）。
	DeviceCode string
	// UserCode 是用户需要在授权页输入的短码。
	UserCode string
	// VerificationURI 是授权页地址。
	VerificationURI string
	// VerificationURIComplete 是带 user_code 的完整地址（可直接打开）。
	VerificationURIComplete string
	// Interval 是上游建议的轮询间隔。
	Interval time.Duration
	// ExpiresAt 是设备码的过期时刻。
	ExpiresAt time.Time
}

// LoginStart 申请一个设备码，返回用户需要打开的授权链接。
func LoginStart(ctx context.Context) (*DeviceAuth, error) {
	form := url.Values{}
	form.Set("client_id", workosClientID)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, workosDeviceAuthURL,
		strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("build device auth request: %w", errReq)
	}
	ApplyFormHeaders(req)

	resp, errDo := httpx.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &Error{Msg: "device auth: " + errDo.Error(), Kind: KindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: "read device auth response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode device auth response: %w", errUnmarshal)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" {
		return nil, fmt.Errorf("device auth response has no device_code")
	}

	interval := time.Duration(payload.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	return &DeviceAuth{
		DeviceCode:              strings.TrimSpace(payload.DeviceCode),
		UserCode:                strings.TrimSpace(payload.UserCode),
		VerificationURI:         core.FirstNonEmpty(payload.VerificationURIComplete, payload.VerificationURI),
		VerificationURIComplete: strings.TrimSpace(payload.VerificationURIComplete),
		Interval:                interval,
		ExpiresAt:               time.Now().Add(ttl),
	}, nil
}

// LoginPoll 轮询一次设备授权状态。
//
// 返回 (nil, nil) 表示用户还没完成授权（调用方应继续轮询）；
// 返回凭证表示授权完成且已换成 Cline 令牌。
func LoginPoll(ctx context.Context, baseURL string, deviceCode string) (*Credential, error) {
	form := url.Values{}
	form.Set("grant_type", deviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", workosClientID)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, workosAuthenticateURL,
		strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("build poll request: %w", errReq)
	}
	ApplyFormHeaders(req)

	resp, errDo := httpx.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &Error{Msg: "login poll: " + errDo.Error(), Kind: KindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: "read poll response: " + errRead.Error(), Kind: KindTransient}
	}

	var payload struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode poll response: %w", errUnmarshal)
	}

	switch payload.Error {
	case "authorization_pending":
		// 用户还没在浏览器里完成授权——这是轮询期间的正常状态。
		return nil, nil
	case "slow_down":
		// 轮询太快：同样继续等，调用方按 interval 节流即可。
		return nil, nil
	case "":
		// 没有错误：继续看令牌。
	default:
		message := core.FirstNonEmpty(payload.ErrorDescription, payload.Error)
		return nil, fmt.Errorf("设备授权失败：%s", message)
	}

	if strings.TrimSpace(payload.AccessToken) == "" {
		// 既没错误也没令牌：按「还没完成」处理，继续轮询。
		return nil, nil
	}

	// 用 WorkOS 令牌换 Cline 自己的令牌。
	return registerWithCline(ctx, baseURL, payload.AccessToken, payload.RefreshToken)
}

// registerWithCline 用 WorkOS 令牌换 Cline 令牌。
func registerWithCline(ctx context.Context, baseURL, workosAccess, workosRefresh string) (*Credential, error) {
	endpoint := BaseURL(baseURL) + RegisterPath
	body, errMarshal := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode register request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build register request: %w", errReq)
	}
	ApplyJSONHeaders(req)

	resp, errDo := httpx.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &Error{Msg: "register: " + errDo.Error(), Kind: KindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: "read register response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
			UserInfo     *struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode register response: %w", errUnmarshal)
	}
	refreshToken := StripWorkOSPrefix(payload.Data.RefreshToken)
	if refreshToken == "" {
		// refresh token 是唯一的长期凭据，缺了这次登录就没有意义——
		// access token 几小时后就过期，账号会变成不可续期的死号。
		return nil, fmt.Errorf("cline registration missing refresh token")
	}
	cred := &Credential{
		AccessToken:  StripWorkOSPrefix(payload.Data.AccessToken),
		RefreshToken: refreshToken,
		ExpiresAt:    parseExpiry(payload.Data.ExpiresAt),
	}
	if payload.Data.UserInfo != nil {
		cred.Email = strings.TrimSpace(payload.Data.UserInfo.Email)
	}
	cred.Label = core.FirstNonEmpty(cred.Email, cred.AccountID)
	return cred, nil
}

// StorageJSON 构造落盘用的凭证 JSON。
//
// 落盘形态是**驼峰**（与上游一致）：登录产物自己写自己读，
// 不必迁就 cline2api 的下划线导出格式（那种格式在 ParseCredential
// 里作为兼容形态被接受）。
func (c *Credential) StorageJSON() ([]byte, error) {
	payload := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
	}
	if c.ExpiresAt > 0 {
		payload["expiresAt"] = c.ExpiresAt
	}
	if c.Email != "" {
		payload["email"] = c.Email
	}
	if c.AccountID != "" {
		payload["accountId"] = c.AccountID
	}
	if c.Label != "" {
		payload["label"] = c.Label
	}
	return json.MarshalIndent(payload, "", "  ")
}

// MergeStorageJSON 把续期后的令牌合并回原凭证 JSON。
//
// 只更新会变的两个令牌字段与过期时间，其余字段（用户手写的 label、
// accountId 等）原样保留。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if len(original) == 0 || updated == nil {
		return original, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(original, &payload); errUnmarshal != nil {
		return original, nil
	}
	// 键名按原文件已有的形态写回：下划线格式的文件不该被改成驼峰
	// （用户可能在别处按原格式读它）。
	accessKey, refreshKey, expiresKey := "accessToken", "refreshToken", "expiresAt"
	if _, okSnake := payload["access_token"]; okSnake {
		accessKey, refreshKey, expiresKey = "access_token", "refresh_token", "expires_at"
	}
	payload[accessKey] = updated.AccessToken
	payload[refreshKey] = updated.RefreshToken
	if updated.ExpiresAt > 0 {
		payload[expiresKey] = updated.ExpiresAt
	}
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return original, errMarshal
	}
	return merged, nil
}

// 本文件实现 Cline 的凭证续期。
//
// 续期链路（移植自 cline2api 的 refreshClineToken）：
//   POST {base}/auth/refresh
//   {"refreshToken":"<rt>","grantType":"refresh_token"}
//
// 注意两个细节，都不是笔误：
//   - 字段是**驼峰** refreshToken，不是标准 OAuth 的 refresh_token；
//   - 授权类型字段叫 **grantType**，不是 grant_type。
//     上游自己定义了这套形状，照标准 OAuth 发会被拒。

// refreshHTTPTimeout 是续期请求的上限。
const refreshHTTPTimeout = 30 * time.Second

// RefreshCredential 用 refresh token 换一组新的令牌。
//
// 返回 updated=false 表示上游没下发新令牌（理论上不该发生，但上游偶尔
// 会回同样的值）；调用方据此决定是否写盘。
func RefreshCredential(ctx context.Context, baseURL string, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	refreshToken := StripWorkOSPrefix(cred.RefreshToken)
	if refreshToken == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "凭证没有 refresh token，无法续期")
	}

	endpoint := BaseURL(baseURL) + RefreshPath
	body, errMarshal := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if errMarshal != nil {
		return nil, false, fmt.Errorf("encode refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, false, fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyJSONHeaders(req)

	resp, errDo := httpx.Client(ctx, refreshHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, false, &Error{Msg: "refresh: " + errDo.Error(), Kind: KindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, false, &Error{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false, fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}
	newAccess := StripWorkOSPrefix(payload.Data.AccessToken)
	if newAccess == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "续期响应里没有 accessToken")
	}
	nextRefresh := StripWorkOSPrefix(payload.Data.RefreshToken)
	if nextRefresh == "" {
		// 上游没轮换 refresh token：保留旧的（否则下次续期会失败）。
		nextRefresh = refreshToken
	}
	if newAccess == cred.AccessToken && nextRefresh == cred.RefreshToken {
		// 上游回了同样的值：算「有效」但不算「已刷新」，避免无意义写盘。
		return cred, false, nil
	}

	updated := &Credential{
		AccessToken:  newAccess,
		RefreshToken: nextRefresh,
		ExpiresAt:    parseExpiry(payload.Data.ExpiresAt),
		Email:        cred.Email,
		Label:        cred.Label,
		AccountID:    cred.AccountID,
	}
	return updated, true, nil
}

// NewCredentialError 构造一个凭证失效错误。
func NewCredentialError(code, message string) *Error {
	return &Error{Code: code, Msg: message, Kind: KindCredential, Status: http.StatusUnauthorized}
}

// parseExpiry 解析过期时刻，兼容上游的多种形态。
//
// 上游的 expiresAt 是 any：可能是 Unix 毫秒（float64 / int64）、
// 也可能是 RFC3339 字符串。解析失败返回 0——调用方会当成「未知」，
// 立即续期一次，代价只是一次多余的请求。
func parseExpiry(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		return millisToSeconds(int64(value))
	case int64:
		return millisToSeconds(value)
	case int:
		return millisToSeconds(int64(value))
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse != nil {
			return 0
		}
		return millisToSeconds(parsed)
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
				return parsed.Unix()
			}
		}
		return 0
	}
	return 0
}

// millisToSeconds 把 Unix 毫秒转成 Unix 秒。
//
// 上游给的是毫秒。值小于 1e12 时说明它其实是秒（上游部分接口混用），
// 这时原样返回——硬按毫秒除会让过期时间落到 1970 年，账号被判定为
// 永久过期而反复续期。
func millisToSeconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 1_000_000_000_000 {
		return value
	}
	return value / 1000
}

// NeedsRefresh 报告凭证是否该续期了（提前 60 秒）。
func NeedsRefresh(cred *Credential, now time.Time) bool {
	if cred == nil {
		return true
	}
	if cred.AccessToken == "" {
		return true
	}
	if cred.ExpiresAt == 0 {
		// 过期时间未知：不主动续期，交给上游 401 触发。
		return false
	}
	return now.Add(60*time.Second).Unix() >= cred.ExpiresAt
}
