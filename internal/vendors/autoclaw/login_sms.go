package autoclaw

// 本文件实现 AutoClaw 国内版的手机号 + 短信验证码登录（core.SMSLoginProvider）。
//
// 协议依据 Agent2API providers/autoclaw/login.rs（桌面端 app.asar 逆向 + 实测）：
//
//	POST {userapi}/userapi/v1/agent-send-code  {"phone","source_id","device_id"}
//	  → code=0 且 data.result=true 才算发出；
//	POST {userapi}/userapi/v1/agent-login/     {"phone","code","platform":"web",
//	                                            "source_id","device_id"}
//	  → data {access_token, refresh_token, user_id, user_name, ...}
//
// 实测口径（别改回去）：
//   - body 的 code 必须是 JSON 数字，字符串会得到与验证码错误无关的
//     400001 参数错误；
//   - agent-login 路径结尾的斜杠是上游要求的（服务端 307 到无斜杠）；
//   - 两次调用必须用同一个 device_id（上游把验证码绑在设备上）；
//   - 国际版没有这条链路（手机验证码入口已被官方移除），在入口处拒绝。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 短信登录链路的端点（挂在 userapi 域下）。
const (
	SendCodePath = "/userapi/v1/agent-send-code"
	// LoginPath 结尾的斜杠是上游要求的。
	LoginPath = "/userapi/v1/agent-login/"
)

// smsLoginTimeout 是发码/登录请求的上限（验证码接口偶发慢，给到 20 秒）。
const smsLoginTimeout = 20 * time.Second

// 业务码 → 用户能看懂的原因（Agent2API describe_login_error 同口径）。
//
// 400001 是参数错误而不是「验证码不正确」——把两者混为一谈会把排查方向带偏
// （code 传字符串就会稳定得这个码）。
func describeSMSLoginError(code int64, upstreamMsg string) string {
	switch code {
	case 400001:
		return "请求参数有误，请检查手机号与验证码格式后重试"
	case 630202:
		return "验证码不正确，请检查后重试"
	case 630101:
		return "获取验证码过于频繁，请稍后再试"
	case 400002:
		return "请求签名校验失败（本机时钟可能有偏差），请校准系统时间后重试"
	case 410000, 400000:
		return "登录态已失效，请重新登录"
	case 631002:
		return "上游要求过风控验证，本次登录无法在此完成；请改用「填写凭证」（可从 AutoClaw 客户端登录态文件里取）"
	case 630014:
		return "风控验证未通过，请稍后重试；若持续失败请改用「填写凭证」"
	}
	if strings.TrimSpace(upstreamMsg) != "" {
		return "登录失败：" + strings.TrimSpace(upstreamMsg)
	}
	return fmt.Sprintf("登录失败（上游 code=%s）", strconv.FormatInt(code, 10))
}

// normalizePhone 归一化中国大陆手机号：容忍 +86 / 86 前缀与空格、连字符，
// 要求 1[2-9] 开头的 11 位。
func normalizePhone(raw string) (string, error) {
	var builder strings.Builder
	for _, ch := range raw {
		if ch != ' ' && ch != '-' {
			builder.WriteRune(ch)
		}
	}
	trimmed := builder.String()
	trimmed = strings.TrimPrefix(trimmed, "+86")
	if rest, okRest := strings.CutPrefix(trimmed, "86"); okRest && len(rest) == 11 {
		trimmed = rest
	}
	if len(trimmed) == 11 && strings.HasPrefix(trimmed, "1") && trimmed[1] >= '2' && trimmed[1] <= '9' {
		valid := true
		for _, ch := range trimmed {
			if ch < '0' || ch > '9' {
				valid = false
				break
			}
		}
		if valid {
			return trimmed, nil
		}
	}
	return "", core.NewPluginError("invalid_request", "请填写 11 位中国大陆手机号", http.StatusBadRequest)
}

// normalizeSMSCode 校验 6 位数字验证码并转成数字。
//
// 上游要求 body 里的 code 是 JSON 数字（传字符串得到 400001 参数错误），
// 前导零无所谓——服务端只看数值。
func normalizeSMSCode(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != 6 {
		return 0, core.NewPluginError("invalid_request", "请填写 6 位数字验证码", http.StatusBadRequest)
	}
	value, errParse := strconv.ParseInt(trimmed, 10, 64)
	if errParse != nil {
		return 0, core.NewPluginError("invalid_request", "请填写 6 位数字验证码", http.StatusBadRequest)
	}
	return value, nil
}

// newDeviceID 生成设备 id（32 字节随机 → 64 位十六进制，与官方客户端形态一致）。
func newDeviceID() string {
	buf := make([]byte, 32)
	if _, errRand := rand.Read(buf); errRand != nil {
		// 随机源不可用是极罕见的系统级故障；device_id 不是安全凭证，
		// 退化为时间戳形态即可满足「本机不同设备可区分」的唯一用途。
		return fmt.Sprintf("fallback%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// postUnauthenticated 发一次带签名的匿名 userapi POST（登录时还没有 token）。
func postUnauthenticated(ctx context.Context, baseURL, path string, body any, what string) (json.RawMessage, error) {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal %s request: %w", what, errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", what, errReq)
	}
	ApplyUserAPIHeaders(req, "")

	resp, errDo := hostbridge.Client(ctx, smsLoginTimeout).Do(req)
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

// smsEnvelope 拆业务信封 {code, msg, data}。
func smsEnvelope(raw json.RawMessage) (code int64, msg string, data json.RawMessage) {
	var envelope struct {
		Code *int64          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return 0, "", nil
	}
	if envelope.Code != nil {
		code = *envelope.Code
	}
	return code, envelope.Msg, envelope.Data
}

// SendLoginCode 发送短信验证码（国内版专用）。
//
// device_id 由本函数生成并随 sessionKey 返回——上游把验证码绑在设备上，
// VerifyLoginCode 必须沿用同一个值。
func SendLoginCode(ctx context.Context, baseURL, phone string) (sessionKey string, err error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return "", errPhone
	}
	deviceID := newDeviceID()
	raw, errPost := postUnauthenticated(ctx, baseURL, SendCodePath, map[string]any{
		"phone":     phone,
		"source_id": protocolName,
		"device_id": deviceID,
	}, "发送验证码")
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, _ := smsEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeSMSLoginError(code, msg), http.StatusBadRequest)
	}
	// 源实现还要求 data.result 为真（code=0 但 result 假也算失败）。
	var data struct {
		Result bool `json:"result"`
	}
	_ = json.Unmarshal(smsData(raw), &data)
	if !data.Result {
		return "", core.NewPluginError("login_failed", "验证码发送失败，请稍后重试", http.StatusBadGateway)
	}
	return deviceID, nil
}

// smsData 从信封里取 data 节点（缺失返回字面量 null 的原始形态）。
func smsData(raw json.RawMessage) json.RawMessage {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil || len(envelope.Data) == 0 {
		return json.RawMessage("null")
	}
	return envelope.Data
}

// VerifyLoginCode 用手机号 + 验证码完成登录，返回落盘凭证。
//
// sessionKey 是 SendLoginCode 返回的 device_id；没带上时现生成一个
// （上游接受「新设备直接登录」的形态）。
func VerifyLoginCode(ctx context.Context, baseURL, sessionKey, phone, code string) (*Credential, error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return nil, errPhone
	}
	codeNum, errCode := normalizeSMSCode(code)
	if errCode != nil {
		return nil, errCode
	}
	deviceID := strings.TrimSpace(sessionKey)
	if deviceID == "" {
		deviceID = newDeviceID()
	}
	raw, errPost := postUnauthenticated(ctx, baseURL, LoginPath, map[string]any{
		"phone":     phone,
		"code":      codeNum,
		"platform":  "web",
		"source_id": protocolName,
		"device_id": deviceID,
	}, "登录")
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	envCode, msg, _ := smsEnvelope(raw)
	if envCode != 0 {
		return nil, core.NewPluginError("login_failed", describeSMSLoginError(envCode, msg), http.StatusBadRequest)
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       any    `json:"user_id"`
		UserName     string `json:"user_name"`
	}
	if errUnmarshal := json.Unmarshal(smsData(raw), &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	access := strings.TrimSpace(data.AccessToken)
	if access == "" {
		return nil, core.NewPluginError("login_failed", "登录成功但上游没有返回 access_token", http.StatusBadGateway)
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		UserName:     strings.TrimSpace(data.UserName),
		Phone:        phone,
		DeviceID:     deviceID,
		Region:       RegionCN,
		ExpiresAt:    jwtExpiry(access),
	}
	if cred.ExpiresAt == 0 {
		// 上游登录响应无 expires_in：按 28 天保守记录。
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

// uidFromAny 把上游的 user_id（数字或字符串）归一成字符串。
func uidFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	}
	return ""
}

// MaskPhone 手机号脱敏（保留前 3 后 4）。
func MaskPhone(phone string) string {
	chars := []rune(strings.TrimSpace(phone))
	if len(chars) < 7 {
		return phone
	}
	return string(chars[:3]) + "****" + string(chars[len(chars)-4:])
}
