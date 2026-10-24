package raccoon

// 本文件实现小浣熊的扫码登录（POST {auth_base}/login_with_qrcode_code 轮询）。
//
// 协议（xiaohuanxiong2api-panel 抓包 + 实测）：qrcode_code 由**客户端自己
// 生成**（32 位小写 hex），服务端把扫码结果挂在这个 code 上；二维码里编
// 的是公开网页 {origin}/login/mp?code=<code>&appname=…，用户微信扫码后
// 本侧轮询取票——不需要回调地址，比 office-raccoon:// 自定义协议链路更
// 适合插件部署形态。
//
// 轮询状态机：pending（没人扫）→ logging（已扫待确认）→ success（带票）/
// canceled（取消或失效）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// QrCodePath 是扫码登录的轮询端点（挂在 auth 域下）。
const QrCodePath = "/login_with_qrcode_code"

// qrAppName 是二维码承载页展示的应用名（与官方客户端一致）。
const qrAppName = "商汤小浣熊官网"

// qrLoginTTL / qrPollInterval 是登录会话有效期与轮询节流。
const (
	qrLoginTTL     = 10 * time.Minute
	qrPollInterval = 2 * time.Second
)

// PendingLogin 是一次进行中的登录会话数据。
//
// TTL 与轮询节流由 internal/auth 的会话表统一管理，这里只保留协议状态。
type PendingLogin struct {
	Code string
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

// newQRCode 生成 16 字节随机数的 hex（32 位小写），服务端据此关联扫码结果。
func newQRCode() (string, error) {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", fmt.Errorf("generate qrcode: %w", errRead)
	}
	return hex.EncodeToString(buf), nil
}

// QRContent 返回二维码里要编的 URL（微信扫这个页面完成授权）。
func QRContent(origin, code string) string {
	return strings.TrimRight(origin, "/") + "/login/mp?code=" + code + "&appname=" + qrAppName
}

// PollQrLogin 查一次扫码状态。
//
// 任何异常都降级为「没完成」：这是 2 秒一次的循环，偶发网络抖动不该中断
// 登录；把未知状态当 success 会拿到空票卡死，当 canceled 会无故刷新用户
// 正在扫的码。success 但缺 access_token 同样按未完成处理。
func PollQrLogin(ctx context.Context, authBase string, code string) (access, refresh string, err error) {
	body, errMarshal := json.Marshal(map[string]string{"qrcode_code": code})
	if errMarshal != nil {
		return "", "", fmt.Errorf("marshal qrcode poll: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+QrCodePath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", fmt.Errorf("build qrcode poll: %w", errReq)
	}
	ApplyAPIHeaders(req, "")
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		// 轮询循环里的偶发网络抖动：降级为未完成。
		return "", "", nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", nil
	}
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", nil
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return "", "", ClassifyBusiness(resp.StatusCode, *envelope.Code, envelope.Message)
	}
	var data struct {
		Status       string `json:"status"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errUnmarshal := json.Unmarshal(envelope.Data, &data); errUnmarshal != nil {
			return "", "", nil
		}
	}
	switch strings.TrimSpace(data.Status) {
	case "canceled":
		return "", "", fmt.Errorf("二维码已失效或已取消，请刷新后重新扫码")
	case "success":
		access = strings.TrimSpace(data.AccessToken)
		refresh = strings.TrimSpace(data.RefreshToken)
		if access == "" {
			// 缺票的 success：当没完成，继续轮。
			return "", "", nil
		}
		return access, refresh, nil
	default:
		// pending / logging / 未知状态：继续轮。
		return "", "", nil
	}
}
