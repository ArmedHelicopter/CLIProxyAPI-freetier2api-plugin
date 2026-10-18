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
			Message: "登录会话不存在或已过期",
		}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话数据已损坏，请重新发起登录",
		}, nil
	}

	path := SnapManagerHost + EpLoginTicket + "?ticket_id=" + url.QueryEscape(pending.TicketID) + "&secret=" + url.QueryEscape(pending.Secret)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("plugin-name", "snap_AIIDE")
	req.Header.Set("plugin-version", "5.2.0")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("poll login ticket failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户在华为云网页端完成授权...",
		}, nil
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
		return nil, fmt.Errorf("decode ticket response: %w", err)
	}

	ak := core.FirstNonEmpty(res.Credentials.AccessKeyID, res.Credential.Access)
	sk := core.FirstNonEmpty(res.Credentials.SecretAccessKey, res.Credential.Secret)
	st := core.FirstNonEmpty(res.Credentials.SecurityToken, res.Credential.SecurityToken)

	if ak == "" || sk == "" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成华为云登录...",
		}, nil
	}

	logins.Delete(sessionID)

	uid := core.FirstNonEmpty(res.UserID, res.UserName, "codearts-user")
	cred := &Credential{
		Vendor:          VendorID,
		UserID:          res.UserID,
		UserName:        res.UserName,
		DomainID:        res.DomainID,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   st,
		RefreshToken:    res.RefreshToken,
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
