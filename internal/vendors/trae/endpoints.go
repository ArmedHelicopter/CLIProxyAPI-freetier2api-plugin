package trae

import (
	"freetier2api-plugin/internal/upstream"

	"encoding/json"
	"fmt"
	"freetier2api-plugin/internal/core"
	"net/http"
	"strings"
)

// 本文件定义 Trae 家族（国内版/国际版/TRAE SOLO）的端点与 profile。
//
// 三个实例共享同一套协议；差异集中在 VendorID、域名、UA、function 与
// 默认模型，统一由 Config 描述，避免复制整份协议实现。

type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "traecn"
	VendorNameCN     = "Trae 国内版"
	VendorIDGlobal   = "traeglobal"
	VendorNameGlobal = "Trae 国际版"
	VendorIDSolo     = "traesolo"
	VendorNameSolo   = "TRAE SOLO"

	// 国内版 (CN)
	HostChatCN    = "https://trae-api-cn.mchost.guru"
	HostAuthCN    = "https://api.trae.cn"
	HostConsoleCN = "https://www.trae.cn"
	ClientIDCN    = "en1oxy7wnw8j9n"
	IDEVersionCN  = "3.3.67"

	// 国际版 (Global/SG)
	HostChatGlobal    = "https://coresg-normal.trae.ai"
	HostAuthGlobal    = "https://growsg-normal.trae.ai"
	HostConsoleGlobal = "https://www.trae.ai"
	ClientIDGlobal    = "ono9krqynydwx5"
	IDEVersionGlobal  = "3.5.51"

	// TRAE SOLO
	HostChatSolo       = HostChatCN
	HostAuthSolo       = HostAuthCN
	HostOAuthSolo      = "https://api.trae.com.cn"
	HostConsoleSolo    = HostConsoleCN
	ClientIDSolo       = ClientIDCN
	IDEVersionSolo     = "0.1.52"
	IDEVersionCodeSolo = "20260811"
	FunctionSolo       = "solo_work_lite"
	DeviceBrandSolo    = "83DG"
	OSVersionSolo      = "Windows 11 Pro"

	// 公共端点路径
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
)

// Config 是一个 Trae 实例的完整 profile。
type Config struct {
	VendorID   string
	VendorName string
	Region     Region

	ChatHost    string
	AuthHost    string
	RefreshHost string
	CheckinHost string
	QuotaHost   string
	ConsoleHost string

	ClientID       string
	IDEVersion     string
	Function       string
	DefaultModel   string
	AppID          string
	DeviceBrand    string
	OSVersion      string
	IDEVersionCode string

	Solo bool
}

// ConfigFor 返回国内/国际版默认配置（保留给协议层测试使用）。
func ConfigFor(r Region) Config {
	if r == RegionGlobal {
		return Config{
			VendorID:     VendorIDGlobal,
			VendorName:   VendorNameGlobal,
			Region:       RegionGlobal,
			ChatHost:     HostChatGlobal,
			AuthHost:     HostAuthGlobal,
			RefreshHost:  HostAuthGlobal,
			ConsoleHost:  HostConsoleGlobal,
			ClientID:     ClientIDGlobal,
			IDEVersion:   IDEVersionGlobal,
			Function:     "chat_v3",
			DefaultModel: "gpt-5",
			AppID:        AppID,
			DeviceBrand:  "PC",
		}
	}
	return Config{
		VendorID:     VendorIDCN,
		VendorName:   VendorNameCN,
		Region:       RegionCN,
		ChatHost:     HostChatCN,
		AuthHost:     HostAuthCN,
		RefreshHost:  HostAuthCN,
		CheckinHost:  HostAuthCN,
		QuotaHost:    HostAuthCN,
		ConsoleHost:  HostConsoleCN,
		ClientID:     ClientIDCN,
		IDEVersion:   IDEVersionCN,
		Function:     "chat_v3",
		DefaultModel: "glm-5.2",
		AppID:        AppID,
		DeviceBrand:  "83DG",
	}
}

// ConfigForVendor 返回某个供应商实例的 profile。
func ConfigForVendor(vendorID string) (Config, bool) {
	switch vendorID {
	case VendorIDCN:
		return ConfigFor(RegionCN), true
	case VendorIDGlobal:
		return ConfigFor(RegionGlobal), true
	case VendorIDSolo:
		return SoloConfig(), true
	default:
		return Config{}, false
	}
}

// SoloConfig 返回 TRAE SOLO profile。
func SoloConfig() Config {
	return Config{
		VendorID:       VendorIDSolo,
		VendorName:     VendorNameSolo,
		Region:         RegionCN,
		ChatHost:       HostChatSolo,
		AuthHost:       HostAuthSolo,
		RefreshHost:    HostOAuthSolo,
		CheckinHost:    HostAuthSolo,
		QuotaHost:      HostAuthSolo,
		ConsoleHost:    HostConsoleSolo,
		ClientID:       ClientIDSolo,
		IDEVersion:     IDEVersionSolo,
		Function:       FunctionSolo,
		DefaultModel:   "glm-5.2",
		AppID:          AppID,
		DeviceBrand:    DeviceBrandSolo,
		OSVersion:      OSVersionSolo,
		IDEVersionCode: IDEVersionCodeSolo,
		Solo:           true,
	}
}

// AllConfigs 返回三个实例的 profile，顺序与历史注册顺序一致。
func AllConfigs() []Config {
	return []Config{ConfigFor(RegionCN), ConfigFor(RegionGlobal), SoloConfig()}
}

func (c Config) clientUA() string { return "Trae/" + c.IDEVersion }

func (c Config) supportsCheckin() bool { return c.Solo || c.Region == RegionCN }

func configForCredential(cred *Credential) Config {
	if cred == nil {
		return ConfigFor(RegionCN)
	}
	if cfg, okConfig := ConfigForVendor(cred.Vendor); okConfig {
		return cfg
	}
	return ConfigFor(cred.Region)
}

// 错误分类：解析 Trae 家族上游错误码。

type ErrorKind = upstream.Kind

const (
	KindCredential = upstream.KindCredential
	KindRateLimit  = upstream.KindRateLimit
	KindTransient  = upstream.KindTransient
	KindClient     = upstream.KindClient
)

type Error = upstream.Error

func Classify(status int, body string) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.Message, payload.Msg, body)
	code := ""
	if payload.Code != 0 {
		code = intString(payload.Code)
	}

	switch {
	case status == http.StatusUnauthorized || payload.Code == 4001 || payload.Code == 4008:
		return &Error{Kind: KindCredential, Status: status, Code: code, Msg: msg}
	case payload.Code == 1005 || strings.Contains(strings.ToLower(msg), "credit") || strings.Contains(msg, "积分不足"):
		return &Error{Kind: KindCredential, Status: status, Code: "1005", Msg: "账号积分不足"}
	case status == http.StatusTooManyRequests || payload.Code == 429 || payload.Code == 9074:
		return &Error{Kind: KindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &Error{Kind: KindClient, Status: status, Code: code, Msg: msg}
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
