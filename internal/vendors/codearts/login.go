package codearts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

type pendingLogin struct {
	TicketID       string
	Secret         string
	Verifier       string
	DPoPPrivateKey map[string]string
	// TicketURL 覆盖票据端点。空值表示 SnapManagerHost+EpLoginTicket；
	// 单测把它指向 httptest 服务，避免测试真的打到华为云。
	TicketURL string
}

// LoginStart 发起华为云 CodeArts 授权登录。
func LoginStart(ctx context.Context, logins core.LoginSessions) (*pluginapi.AuthLoginStartResponse, error) {
	ticketID := core.RandomHex(16)
	secret := core.RandomHex(16)
	verifier, challenge, errPKCE := core.NewPKCEPair()
	if errPKCE != nil {
		return nil, fmt.Errorf("generate pkce: %w", errPKCE)
	}
	dpopJWK, err := generateDPoPJWK()
	if err != nil {
		return nil, fmt.Errorf("generate dpop jwk: %w", err)
	}

	redirectURI := "http://127.0.0.1:7866/oauth/callback"
	v := url.Values{}
	v.Set("client_id", ClientID)
	v.Set("response_type", "code")
	v.Set("ticket_id", ticketID)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("redirect_uri", redirectURI)

	authURL := PortalHost + "/authorize?" + v.Encode()
	sessionID := "codearts_" + core.RandomHex(8)

	logins.Register(VendorID, sessionID, 15*time.Minute, 0, &pendingLogin{
		TicketID:       ticketID,
		Secret:         secret,
		Verifier:       verifier,
		DPoPPrivateKey: dpopJWK,
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询华为云 ticket 登录结果。
func LoginPoll(ctx context.Context, logins core.LoginSessions, sessionID string) (*pluginapi.AuthLoginPollResponse, error) {
	session, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录并在浏览器授权后立即粘贴回调链接",
		}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话数据已损坏，请重新发起登录",
		}, nil
	}
	return pollLoginTicket(ctx, logins, sessionID, pending)
}

// pollLoginTicket 用会话里当前的 ticket/secret 换一次凭证。
//
// ticket/secret 会被 SubmitCallback 换成上游签发的那一对，因此普通轮询与
// 粘贴回调共用这一段——区别只在「ticket 从哪来」。
func pollLoginTicket(ctx context.Context, logins core.LoginSessions, sessionID string, pending *pendingLogin) (*pluginapi.AuthLoginPollResponse, error) {
	ticketURL := strings.TrimSpace(pending.TicketURL)
	if ticketURL == "" {
		ticketURL = SnapManagerHost + EpLoginTicket
	}
	creds, ready, errFetch := fetchTicketCredentials(ctx, ticketURL, pending.TicketID, pending.Secret)
	if errFetch != nil {
		return nil, errFetch
	}
	if !ready {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成华为云登录…",
		}, nil
	}

	logins.Delete(sessionID)

	uid := core.FirstNonEmpty(creds.UserID, creds.UserName, "codearts-user")
	cred := &Credential{
		Vendor:          VendorID,
		UserID:          creds.UserID,
		UserName:        creds.UserName,
		DomainID:        creds.DomainID,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SecurityToken:   creds.SecurityToken,
		RefreshToken:    creds.RefreshToken,
		CodeVerifier:    pending.Verifier,
		ClientID:        ClientID,
		DPoPPrivateKey:  pending.DPoPPrivateKey,
		Label:           "CodeArts-" + uid,
	}

	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, errStorage
	}

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "华为云账号登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freetier",
			FileName:    "codearts-" + uid + ".json",
			Label:       cred.Label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// ticketCredentials 是华为云登录票据换取到的临时凭证。
type ticketCredentials struct {
	UserName        string
	UserID          string
	DomainID        string
	RefreshToken    string
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	Expiration      string
}

// fetchTicketCredentials 用 ticket/secret 调登录票据接口换 AK/SK。
//
// ready=false 表示上游还没准备好（票据未签发、HTTP 4xx/5xx、或响应里还没给
// AK/SK），调用方应当继续轮询；只有传输层与解码错误才返回 error。
func fetchTicketCredentials(ctx context.Context, ticketURL, ticketID, secret string) (*ticketCredentials, bool, error) {
	path := ticketURL + "?ticket_id=" + url.QueryEscape(ticketID) + "&secret=" + url.QueryEscape(secret)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("plugin-name", "snap_AIIDE")
	req.Header.Set("plugin-version", "5.2.0")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("poll login ticket failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, nil
	}

	var res struct {
		UserName     string `json:"user_name"`
		UserID       string `json:"user_id"`
		DomainID     string `json:"domain_id"`
		RefreshToken string `json:"refresh_token"`
		Credential   struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"security_token"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, false, fmt.Errorf("decode ticket response: %w", err)
	}

	ak := core.FirstNonEmpty(res.Credentials.AccessKeyID, res.Credential.Access)
	sk := core.FirstNonEmpty(res.Credentials.SecretAccessKey, res.Credential.Secret)
	if ak == "" || sk == "" {
		return nil, false, nil
	}

	return &ticketCredentials{
		UserName:        res.UserName,
		UserID:          res.UserID,
		DomainID:        res.DomainID,
		RefreshToken:    res.RefreshToken,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   core.FirstNonEmpty(res.Credentials.SecurityToken, res.Credential.SecurityToken),
		Expiration:      core.FirstNonEmpty(res.Credentials.Expiration, res.Credential.ExpiresAt),
	}, true, nil
}

// CallbackInfo 是华为云 localhost 回调里带回来的信息。
type CallbackInfo struct {
	// TicketID 与 Secret 是上游签发的登录票据凭据。
	TicketID string
	Secret   string
	// Redirect 是回调指向的华为云登录页（票据还没换到凭证时，用户要再去点它）。
	Redirect string
}

// ParseCallback 解析用户粘贴的 localhost 回调链接。
//
// 实际回调形如：
//
//	http://127.0.0.1/oauth/callback?secret=<票据密钥>&
//		redirect=<encoded codearts.huaweicloud.com/portal/login?...&ticket_id=<票据 ID>...>
//
// 关键点：这里的 ticket_id/secret 是**华为云签发**的，不是 LoginStart 送进
// /authorize 的那一对。所以必须从回调里取——继续拿自造值轮询，永远查不到凭证。
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
	info := &CallbackInfo{Secret: q.Get("secret"), Redirect: q.Get("redirect")}
	if info.Redirect != "" {
		if portal, errPortal := url.Parse(info.Redirect); errPortal == nil {
			pq := portal.Query()
			info.TicketID = pq.Get("ticket_id")
			// 内层 secret 是门户登录页真正使用的那一个，缺失时回落到外层。
			if secret := pq.Get("secret"); secret != "" {
				info.Secret = secret
			}
		}
	}
	// 容错：用户可能粘的是解码后的链接（参数直接摊在回调上）。
	if info.TicketID == "" {
		info.TicketID = q.Get("ticket_id")
	}

	if info.TicketID == "" || info.Secret == "" {
		return nil, fmt.Errorf("callback missing ticket_id or secret")
	}
	return info, nil
}

// SubmitCallback 接受用户粘贴的 localhost 回调链接。
//
// 浏览器跳回 127.0.0.1 时，插件与浏览器不在同一台机器（远程主机 / Docker /
// WSL）就收不到这条回调，登录会永久停在「等待授权中」。用户把地址栏里的
// 整条链接粘回来，这里取出上游签发的 ticket/secret 覆盖自造值，随即换一次凭证。
func SubmitCallback(ctx context.Context, logins core.LoginSessions, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	if sessionID == "" {
		return nil, core.NewPluginError("invalid_request", "缺少登录会话 state，请重新发起登录", http.StatusBadRequest)
	}
	session, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found",
			"登录会话不存在或已过期，请重新发起登录并在浏览器授权后立即粘贴回调链接", http.StatusNotFound)
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "登录会话数据已损坏，请重新发起登录", http.StatusBadRequest)
	}

	// 解析失败时不回显原始链接：里面带着票据密钥。
	info, errParse := ParseCallback(rawURL)
	if errParse != nil {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是浏览器地址栏里的完整链接（含 http:// 与后面的参数）",
			http.StatusBadRequest)
	}

	// 只有上游签发的这对值能换到凭证，覆盖掉 LoginStart 的自造值。
	pending.TicketID = info.TicketID
	pending.Secret = info.Secret

	response, errPoll := pollLoginTicket(ctx, logins, sessionID, pending)
	if errPoll != nil {
		return nil, core.ToPluginError(errPoll)
	}

	result := &core.CallbackSubmitResult{
		Status:  response.Status,
		Message: response.Message,
	}
	if response.Status == pluginapi.AuthLoginStatusSuccess {
		result.Auth = response.Auth
		return result, nil
	}
	// 票据还没换到凭证：把门户登录页给用户，让他在能联网的浏览器里点开完成授权。
	result.RedirectURL = info.Redirect
	return result, nil
}

func generateDPoPJWK() (map[string]string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pad := func(b *big.Int, sz int) []byte {
		buf := make([]byte, sz)
		raw := b.Bytes()
		copy(buf[sz-len(raw):], raw)
		return buf
	}
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(pad(key.PublicKey.X, 32)),
		"y":   base64.RawURLEncoding.EncodeToString(pad(key.PublicKey.Y, 32)),
		"d":   base64.RawURLEncoding.EncodeToString(pad(key.D, 32)),
	}, nil
}

// Token 续期：使用 refresh_token 与 DPoP 签名换取新 STS 凭证。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/client.go 与 dpop.go

// RefreshCredential 执行 Token 续期。
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	tokenURL := SnapManagerHost + EpOAuthTokens
	proof, errProof := buildDPoPProof(cred.DPoPPrivateKey, tokenURL)
	if errProof != nil {
		return nil, false, fmt.Errorf("build dpop proof: %w", errProof)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", core.FirstNonEmpty(cred.ClientID, ClientID))
	form.Set("refresh_token", cred.RefreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var res struct {
		RefreshToken string `json:"refresh_token"`
		Credentials  struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, false, fmt.Errorf("decode refresh response: %w", err)
	}

	if res.Credentials.AccessKeyID == "" || res.Credentials.SecretAccessKey == "" {
		return nil, false, fmt.Errorf("refresh token response missing ak/sk")
	}

	updated := *cred
	updated.AccessKeyID = res.Credentials.AccessKeyID
	updated.SecretAccessKey = res.Credentials.SecretAccessKey
	updated.SecurityToken = res.Credentials.SecurityToken
	if res.RefreshToken != "" {
		updated.RefreshToken = res.RefreshToken
	}
	updated.Expiration = res.Credentials.Expiration

	return &updated, true, nil
}

// MergeStorageJSON 合并续期凭证。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	return core.MergeCredentialJSON(original, updated.StorageJSON, func(m map[string]any) {
		m["access_key_id"] = updated.AccessKeyID
		m["secret_access_key"] = updated.SecretAccessKey
		m["security_token"] = updated.SecurityToken
		if updated.RefreshToken != "" {
			m["refresh_token"] = updated.RefreshToken
		}
		if updated.Expiration != "" {
			m["expiration"] = updated.Expiration
		}
	})
}

func buildDPoPProof(jwk map[string]string, htu string) (string, error) {
	if jwk == nil || jwk["d"] == "" || jwk["x"] == "" || jwk["y"] == "" {
		return "", fmt.Errorf("missing dpop private key material")
	}

	xRaw, errX := base64.RawURLEncoding.DecodeString(jwk["x"])
	yRaw, errY := base64.RawURLEncoding.DecodeString(jwk["y"])
	dRaw, errD := base64.RawURLEncoding.DecodeString(jwk["d"])
	if errX != nil || errY != nil || errD != nil {
		return "", fmt.Errorf("decode dpop jwk base64 failed")
	}

	curve := elliptic.P256()
	privKey := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xRaw),
			Y:     new(big.Int).SetBytes(yRaw),
		},
		D: new(big.Int).SetBytes(dRaw),
	}

	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   jwk["x"],
			"y":   jwk["y"],
		},
	}
	headerJSON, _ := json.Marshal(header)

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	jti := hex.EncodeToString(b)

	payload := map[string]any{
		"htm": "POST",
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": jti,
	}
	payloadJSON, _ := json.Marshal(payload)

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))

	r, s, errSign := ecdsa.Sign(rand.Reader, privKey, digest[:])
	if errSign != nil {
		return "", errSign
	}

	// 归一化为低 S
	n := curve.Params().N
	halfN := new(big.Int).Rsh(n, 1)
	if s.Cmp(halfN) > 0 {
		s.Sub(n, s)
	}

	pad := func(val *big.Int) []byte {
		buf := make([]byte, 32)
		raw := val.Bytes()
		copy(buf[32-len(raw):], raw)
		return buf
	}

	sigBytes := append(pad(r), pad(s)...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sigBytes), nil
}
