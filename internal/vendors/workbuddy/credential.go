package workbuddy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
)

// 本文件实现凭证的解析与写回。
//
// 凭证有两种形态（历史原因，两种都要认）：
//   - 嵌套形：{"auth": {...}, "account": {...}, "device_token": ...}（OAuth 登录产物）
//   - 扁平形：顶层直接放 accessToken / refreshToken / uid / ...（手写或旧版）
//
// **并发约定**：所有出站请求头构造必须经取值方法（AccessTokenValue 等）加锁快照，
// 不能直读字段——任务调度器与请求转发会真并发地刷新/读取同一份凭证。

// protocolName 是本协议在文件名里的公共标识段。
//
// CN 与 GLOBAL 的凭证文件名分别是 workbuddycn-<uid>.json 与
// workbuddyglobal-<uid>.json，两者都含 "workbuddy" 段，因此用它做协议级
// 的文件名识别；具体区域由 core.ResolveVendor 的完整前缀匹配决定。
const protocolName = "workbuddy"

// Credential 是归一化后的账号凭证。
type Credential struct {
	mu sync.Mutex

	// AccessToken 是出站 Authorization 的 Bearer 值。
	AccessToken string
	// RefreshToken 用于刷新 access token。
	RefreshToken string
	// ExpiresAt 是 access token 过期时刻（Unix 秒）。
	ExpiresAt int64
	// Domain 是账号所属域名（用于推断 realm 与出站 X-Domain 头）。
	Domain string
	// realm 未导出：显式声明优先，为空时按 Domain 推断。
	realm Region
	// UID 是账号唯一标识（也是设备指纹的派生种子）。
	UID string
	// EnterpriseID 是企业租户标识（CN 的 X-Enterprise-Id）。
	EnterpriseID string
	// Nickname 是展示用昵称。
	Nickname string
	// DeviceToken 是 X-Device-Token；空表示不注入。
	DeviceToken string
	// FilePath 是凭证来源文件路径（刷新后原子写回此处）；空表示非文件来源。
	FilePath string
}

// Credential 的顶层 JSON 形态。
type credentialJSON struct {
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
	Account *struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
	DeviceToken string `json:"device_token"`

	// 扁平形态的字段。
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// RegisterPathHint 是凭证文件名的识别提示（auth.parse 用它判断文件归属）。
const RegisterPathHint = "workbuddy"

// ParseCredential 解析凭证 JSON。
//
// fallbackRegion 是账号未声明 realm 时的兜底域（来自插件配置）。
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var parsed credentialJSON
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}

	cred := &Credential{}
	if parsed.Auth != nil {
		cred.AccessToken = strings.TrimSpace(parsed.Auth.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.Auth.RefreshToken)
		cred.ExpiresAt = parsed.Auth.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Auth.Domain)
		cred.realm = normalizeRealmStrict(parsed.Auth.Realm)
	} else {
		cred.AccessToken = strings.TrimSpace(parsed.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
		cred.ExpiresAt = parsed.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Domain)
		cred.realm = normalizeRealmStrict(parsed.Realm)
	}
	if parsed.Account != nil {
		cred.UID = strings.TrimSpace(parsed.Account.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.Account.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Account.Nickname)
	} else {
		cred.UID = strings.TrimSpace(parsed.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Nickname)
	}
	cred.DeviceToken = strings.TrimSpace(parsed.DeviceToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	if cred.realm == "" {
		if inferred := realmFromDomain(cred.Domain); inferred != "" {
			cred.realm = inferred
		} else {
			cred.realm = fallbackRegion
		}
	}
	return cred, nil
}

// normalizeRealmStrict 只接受明确的 cn / global 值，其余返回空串。
func normalizeRealmStrict(raw string) Region {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(RegionCN):
		return RegionCN
	case string(RegionGlobal):
		return RegionGlobal
	}
	return ""
}

// realmFromDomain 按域名推断域。
//
// 用域名后缀而非完全匹配：国际版有多个子域（www.workbuddy.ai、api...），
// 而 cn 域全部落在 codebuddy.cn / tencent.com 之下。
func realmFromDomain(domain string) Region {
	trimmed := strings.ToLower(strings.TrimSpace(domain))
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if trimmed == "workbuddy.ai" || strings.HasSuffix(trimmed, ".workbuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

// Realm 返回凭证所属域。
func (c *Credential) Realm() Region {
	if c == nil {
		return RegionCN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.realm
}

// SetRealm 覆盖凭证域（账号级覆盖优先于插件配置兜底）。
func (c *Credential) SetRealm(region Region) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.realm = region
}

// AccessTokenValue 加锁读取 access token。
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue 加锁读取 refresh token。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// DomainValue 加锁读取域名。
func (c *Credential) DomainValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Domain
}

// UIDValue 加锁读取 UID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue 加锁读取昵称。
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// EnterpriseIDValue 加锁读取企业标识。
func (c *Credential) EnterpriseIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EnterpriseID
}

// DeviceTokenValue 加锁读取设备令牌。
func (c *Credential) DeviceTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceToken
}

// ApplyTokenRefresh 写回刷新结果。
//
// 采用「两段式快照校验」：调用方在发起网络请求前取快照，拿到响应后在本方法里
// 比对当前值是否仍是快照——若已被并发刷新过，则放弃写回，避免用旧响应覆盖新值。
func (c *Credential) ApplyTokenRefresh(accessToken, refreshToken, domain string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if trimmed := strings.TrimSpace(accessToken); trimmed != "" {
		c.AccessToken = trimmed
	}
	if trimmed := strings.TrimSpace(refreshToken); trimmed != "" {
		c.RefreshToken = trimmed
	}
	if trimmed := strings.TrimSpace(domain); trimmed != "" {
		c.Domain = trimmed
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// Snapshot 返回 access/refresh token 的当前值，供两段式刷新校验使用。
func (c *Credential) Snapshot() (accessToken, refreshToken string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince 报告凭证是否仍与快照一致。
func (c *Credential) UnchangedSince(accessToken, refreshToken string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessToken && c.RefreshToken == refreshToken
}

// StorageJSON 把凭证序列化为嵌套形态，用于写回 CPA 的凭证文件。
//
// 写成嵌套形是刻意的：CPA 的 auth 文件里还可能有用户自己维护的字段
// （disabled / prefix / proxy_url / note / weight），调用方应先做**合并更新**
// 而不是用本方法的输出整体替换（见 MergeStorageJSON）。
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	auth := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
	}
	if c.Domain != "" {
		auth["domain"] = c.Domain
	}
	if c.realm != "" {
		auth["realm"] = string(c.realm)
	}
	account := map[string]any{}
	if c.UID != "" {
		account["uid"] = c.UID
	}
	if c.EnterpriseID != "" {
		account["enterpriseId"] = c.EnterpriseID
	}
	if c.Nickname != "" {
		account["nickname"] = c.Nickname
	}
	out := map[string]any{
		// type 必须写入：宿主用它**直接判定归属**，有它就按 provider 找到
		// 本插件，不再遍历插件。缺它时宿主会把「当前询问的插件的 identifier」
		// 兜底填进 provider，归属判定过宽的前置插件会抢走本插件的凭证，
		// 导致账号在宿主侧被归到别的 provider（页面表现为账号凭空消失）。
		"type": ProviderKey,
		"auth": auth,
	}
	if len(account) > 0 {
		out["account"] = account
	}
	if c.DeviceToken != "" {
		out["device_token"] = c.DeviceToken
	}
	return json.MarshalIndent(out, "", "  ")
}

// MergeStorageJSON 在原凭证 JSON 上做合并更新，返回新的字节。
//
// 基于原文件合并而不是从头构造：CPA 的 auth 文件里可能有用户自己维护的
// disabled / prefix / proxy_url / note / weight 等字段，重建会静默丢掉它们。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	updated, errCred := cred.StorageJSON()
	if errCred != nil {
		return original, errCred
	}
	if len(original) == 0 {
		return updated, nil
	}
	var existing map[string]any
	if errUnmarshal := json.Unmarshal(original, &existing); errUnmarshal != nil {
		// 原文件不是对象（或已损坏）：退回整体替换。
		return updated, nil
	}
	var fresh map[string]any
	if errUnmarshal := json.Unmarshal(updated, &fresh); errUnmarshal != nil {
		return original, errUnmarshal
	}
	mergeMaps(existing, fresh)
	// 兜底：无论原文件有没有 type，合并结果都要带它。
	// 用户从别处复制进来的旧凭证常常缺这个字段，补上后归属就确定了。
	if _, okType := existing["type"]; !okType {
		existing["type"] = ProviderKey
	}
	return json.MarshalIndent(existing, "", "  ")
}

// mergeMaps 把 src 的键值递归合并进 dst（src 优先）。
func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		if srcMap, okSrc := value.(map[string]any); okSrc {
			if dstMap, okDst := dst[key].(map[string]any); okDst {
				mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[key] = value
	}
}

// SaveCredentialFile 原子写回凭证文件。
func SaveCredentialFile(path string, cred *Credential) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	original, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		original = nil
	}
	merged, errMerge := MergeStorageJSON(original, cred)
	if errMerge != nil {
		return fmt.Errorf("merge credential file %s: %w", trimmed, errMerge)
	}
	tmp := trimmed + ".tmp"
	if errWrite := os.WriteFile(tmp, merged, 0o600); errWrite != nil {
		return fmt.Errorf("write credential file %s: %w", tmp, errWrite)
	}
	if errRename := os.Rename(tmp, trimmed); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace credential file %s: %w", trimmed, errRename)
	}
	return nil
}

// LooksLikeCredential 判断一份 JSON 是否属于本插件（任意区域）。
//
// **归属判定必须偏严**：宿主会把所有非内建格式的凭证文件依次喂给每个插件，
// 一旦误吞别家的凭证，宿主会用它写出本插件的凭证结构，等于污染了对方的账号
// （而且这个错误是静默的）。反过来，漏认只是加载失败，用户立刻会发现。
//
// 因此只接受**本插件特有的证据**，从强到弱：
//  1. 文件里的 type 字段或宿主传入的 provider 提示是 freetier；
//  2. 文件名符合本插件的命名约定（workbuddycn / workbuddyglobal 前缀/中缀）；
//  3. 具备本插件独有的嵌套结构（auth.accessToken + auth.refreshToken）。
//
// 刻意**不**接受顶层的 accessToken / refreshToken / device_token：
// 这些是通用字段，别的 provider 也用（例如某些插件同样写 device_token），
// 拿它们做判据必然跨插件误吞。
//
// 注意：本函数只判断「属于本协议」，**不判断区域**——区域由 core.ResolveVendor
// 的文件名前缀与 vendor 字段决定，或由具体 vendor 实例的 Match 二次确认。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 1) 显式声明（最可靠，也是推荐的写法）。
	if strings.EqualFold(strings.TrimSpace(provider), ProviderKey) {
		return true
	}
	if typeValue, okType := raw["type"].(string); okType && strings.EqualFold(strings.TrimSpace(typeValue), ProviderKey) {
		return true
	}

	// 2) 文件名约定。
	if core.FileNameMatchesConvention(fileName, VendorIDCN, VendorIDGlobal, protocolName) {
		return true
	}

	// 3) 本插件独有的嵌套结构：auth 里同时有 accessToken 与 refreshToken。
	// 单有 accessToken 不足以判定（其它 provider 也可能这样嵌套）。
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		_, hasAccess := nested["accessToken"]
		_, hasRefresh := nested["refreshToken"]
		if hasAccess && hasRefresh {
			return true
		}
	}
	return false
}

// ProviderKey 是本插件在 CPA 里占用的 provider 键。
//
// **所有供应商共用它**：宿主的 auth.identifier 只返回单个字符串、注册期调一次
// 永久缓存，且与 auth 文件的 type 字段严格相等才认领该文件。因此供应商的区分
// 落在凭证文件名前缀（workbuddycn-<uid>.json）与文件内的 vendor 字段上，
// 而不是 provider key。
const ProviderKey = core.ProviderKey

// RegionForCredential 从凭证 JSON 推断所属区域，推断不出返回空。
//
// 判定顺序：嵌套形的 auth.realm → auth.domain 后缀 → 扁平形的 realm → domain 后缀。
// 导出给根层的 vendor 适配器用：凭证归属判定需要它，而判定逻辑属于协议层。
func RegionForCredential(raw map[string]any) Region {
	if raw == nil {
		return ""
	}
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		if realm := realmField(nested); realm != "" {
			return realm
		}
	}
	return realmField(raw)
}

// realmField 从一层 JSON 里读区域：先看显式 realm 字段，再看 domain 后缀。
func realmField(level map[string]any) Region {
	if realm, okRealm := level["realm"].(string); okRealm && strings.TrimSpace(realm) != "" {
		if strict := normalizeRealmStrict(realm); strict != "" {
			return strict
		}
	}
	if domain, okDomain := level["domain"].(string); okDomain {
		if inferred := realmFromDomain(domain); inferred != "" {
			return inferred
		}
	}
	return ""
}

// 本文件实现 WorkBuddy 的每日签到。
//
// 签到是 CodeBuddy 国内版独有的活动；国际版没有该活动，调用方应在
// SupportsCheckin 为 false 时跳过（不报错、不发请求）。

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
