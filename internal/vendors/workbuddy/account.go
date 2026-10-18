package workbuddy

// 本文件实现 WorkBuddy 的每日签到。
//
// 签到是 CodeBuddy 国内版独有的活动；国际版没有该活动，调用方应在
// SupportsCheckin 为 false 时跳过（不报错、不发请求）。

import (
	"encoding/json"
	"fmt"
	"net/http"

	"io"
	"strings"
	"time"
)

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 表示今天已经签过（幂等成功，不是错误）。
	Already bool
	// Credit / Energy 是本次签到所得。
	Credit int64
	Energy int64
	// Streak 是连续签到天数。
	Streak int64
}

// DailyCheckin 执行每日签到。
//
// 「今天已签到」被识别为幂等成功：上游对重复签到返回业务错误，
// 直接当失败会让自动签到任务每天报错一次。
func (c *Client) DailyCheckin(cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{dailyCheckinPathV2}
	if region.IsGlobal() {
		paths = []string{dailyCheckinPath, dailyCheckinPathV2}
	}

	var lastErr error
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, map[string]any{})
		if errCall != nil {
			if IsAlreadyCheckin(errCall) {
				return &CheckinResult{Already: true}, nil
			}
			lastErr = errCall
			if upstreamErr, okErr := errCall.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errCall
		}
		var payload struct {
			Credit int64 `json:"credit"`
			Energy int64 `json:"energy"`
			Streak int64 `json:"streak"`
		}
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			// 签到成功但响应结构不认识：仍算成功（副作用已发生）。
			return &CheckinResult{}, nil
		}
		return &CheckinResult{Credit: payload.Credit, Energy: payload.Energy, Streak: payload.Streak}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("checkin endpoint is unavailable")
}

// 本文件实现 WorkBuddy 的凭证续期。
//
// 续期链路：POST <ChatBase>/v2/plugin/auth/token，用 X-Refresh-Token 头
// 带旧 refresh token 换一组新令牌。
//
// 安全红线：X-Refresh-Token 头**只允许出现在这个刷新端点**，对话请求
// 绝不能携带——把 refresh token 发给对话上游等于把长期凭据暴露给一个
// 只需要 access token 的服务。

// RefreshToken 刷新凭证的 access token。
//
// 安全红线：X-Refresh-Token 头**只允许出现在刷新端点**，对话请求绝不能携带。
//
// 并发采用两段式：锁内取快照 → 锁外发请求 → 锁内校验快照未被并发改动后再写回。
func (c *Client) RefreshToken(cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return fmt.Errorf("credential has no refresh token")
	}
	accessBefore, refreshBefore := cred.Snapshot()

	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+tokenRefreshPath, nil)
	if errReq != nil {
		return fmt.Errorf("build refresh request: %w", errReq)
	}
	applyCommonHeaders(req, c, cred)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", authRefreshSource)

	resp, errDo := c.refreshClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("refresh request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		return fmt.Errorf("read refresh response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}

	var envelope struct {
		Code int                  `json:"code"`
		Msg  string               `json:"msg"`
		Data refreshTokenResponse `json:"data"`
	}
	var token refreshTokenResponse
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal == nil && envelope.Code == 0 && strings.TrimSpace(envelope.Data.AccessToken) != "" {
		token = envelope.Data
	} else if errUnmarshal == nil && envelope.Code != 0 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	} else if errDirect := json.Unmarshal(body, &token); errDirect != nil || strings.TrimSpace(token.AccessToken) == "" {
		return &Error{Kind: KindSessionDead, Status: http.StatusUnauthorized, Msg: "refresh failed: no accessToken in response — re-login required"}
	}

	// 快照校验：并发刷新已经写过就放弃本次写回，避免用旧响应覆盖新值。
	if !cred.UnchangedSince(accessBefore, refreshBefore) {
		return nil
	}
	var expiresAt int64
	if token.ExpiresIn > 0 && token.ExpiresIn <= maxRefreshTokenExpiresIn {
		expiresAt = time.Now().Unix() + token.ExpiresIn
	}
	cred.ApplyTokenRefresh(token.AccessToken, token.RefreshToken, token.Domain, expiresAt)
	if cred.FilePath != "" {
		if errSave := SaveCredentialFile(cred.FilePath, cred); errSave != nil {
			return fmt.Errorf("save refreshed credential: %w", errSave)
		}
	}
	return nil
}

// 本文件实现 WorkBuddy 的额度查询。
//
// 额度口径是**积分余额**：上游的计费接口按资源包返回 remain/used/size，
// 这里聚合成账号级的「剩余 / 总额」并保留套餐明细。

// Balance 是账号的资源余额。
type Balance struct {
	// Remain 是剩余积分。
	Remain int64
	// Total 是本期总额度。
	Total int64
	// ExpiringSoon 是即将过期的积分（窗口由调用方定义）。
	ExpiringSoon int64
	// Packages 是套餐明细。
	Packages []BalancePackage
}

// BalancePackage 是单项资源包的余额。
type BalancePackage struct {
	// Name 是资源包名称。
	Name string
	// Remain 是剩余量。
	Remain int64
	// Total 是总量。
	Total int64
	// ExpireAt 是过期时刻（Unix 秒，0 表示不过期）。
	ExpireAt int64
}

// billingResourceAccount 是余额响应里的单个套餐。
//
// 字段是**上游原样的 PascalCase**（上游该接口不使用 camelCase）。
type billingResourceAccount struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，空 = 无到期
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// billingResourceResponse 是余额接口的响应结构。
//
// 双层信封：Response.Data.Accounts。请求体也必须带分页与产品码，
// 否则上游返回空列表（这一点早期实现漏了，导致余额恒为 0）。
type billingResourceResponse struct {
	Response struct {
		Data struct {
			Accounts []billingResourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// packageEndLayout 是套餐到期时间的墙钟格式（UTC+8）。
const packageEndLayout = "2006-01-02 15:04:05"

// packageRemainUsed 聚合单个套餐的 remain/used/size。
//
// 取数规则（照抄上游的单一事实来源，勿改）：
//   - Cycle 期套餐优先（CycleCapacitySize > 0）：用 Cycle 三字段，
//     remain 先钳到 [0, size]；used 取 CycleUsed 与 size-remain 的较大者，
//     且当 used 更大时反推 remain = size - used；
//   - 否则回退 Capacity 三字段，used 为 0 但 size > remain 时按差值补 used。
func packageRemainUsed(account billingResourceAccount) (remain, used, size int64) {
	if account.CycleCapacitySize > 0 {
		remain = account.CycleCapacityRemain
		size = account.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if account.CycleCapacityUsed > used {
			used = account.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = account.CapacityRemain
	used = account.CapacityUsed
	size = account.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// FetchBalance 查询账号余额。
//
// global 域先试无 /v2 的路径，404 时回落带 /v2 的路径；CN 只有带 /v2 的。
// 这个差异来自上游两个部署的历史，不是笔误。
func (c *Client) FetchBalance(cred *Credential) (*Balance, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{billingMeterPathV2}
	if region.IsGlobal() {
		paths = []string{billingMeterPath, billingMeterPathV2}
	}

	// 请求体是上游要求的固定形状：缺 ProductCode 或分页会拿到空列表。
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}

	var lastErr error
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, body)
		if errCall != nil {
			lastErr = errCall
			// 只在 404 时换路径：其他错误换路径也救不回来。
			if upstreamErr, okErr := errCall.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errCall
		}
		var payload billingResourceResponse
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			lastErr = fmt.Errorf("decode balance: %w", errUnmarshal)
			continue
		}
		return parseBalance(payload), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("balance endpoint is unavailable")
}

// parseBalance 归一化余额响应。
//
// remain/total 是各套餐的**求和**（用户关心的是账号总量，不是单个套餐）。
func parseBalance(payload billingResourceResponse) *Balance {
	balance := &Balance{}
	for _, account := range payload.Response.Data.Accounts {
		remain, _, size := packageRemainUsed(account)
		balance.Remain += remain
		balance.Total += size
		balance.Packages = append(balance.Packages, BalancePackage{
			Name:     strings.TrimSpace(account.PackageName),
			Remain:   remain,
			Total:    size,
			ExpireAt: parsePackageExpiry(account.CycleEndTime),
		})
	}
	return balance
}

// parsePackageExpiry 解析套餐到期时刻（UTC+8 墙钟）；解析失败返回 0。
func parsePackageExpiry(raw string) int64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	parsed, errParse := time.ParseInLocation(packageEndLayout, trimmed, cstZone)
	if errParse != nil {
		return 0
	}
	return parsed.Unix()
}

// billingJSON 发起一次计费域请求并解开信封。
func (c *Client) billingJSON(cred *Credential, url, method string, body any) (json.RawMessage, error) {
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(c.opts.Context, method, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	return c.doJSON(req)
}

// FetchQuota 汇总账号的额度信息（供宿主的管理端展示）。
func (c *Client) FetchQuota(cred *Credential) (*Balance, error) {
	return c.FetchBalance(cred)
}
