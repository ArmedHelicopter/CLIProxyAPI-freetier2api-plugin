package qoder

// 本文件是 Qoder 的端点表面向本包的入口。
//
// 端点表的**真正的定义**在 qoderapi 子包（endpoints.go）：Qoder 的区域类型
// Region 与端点表同处一地，且被 bridge / cosy 等子包广泛使用，不适合搬动。
// 这里做一层转发，让「端点表在 endpoints.go」的约定在本包也成立——
// 四个供应商的 endpoints.go 都回答同一个问题：按区域给我端点。

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/cosy"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// Region 标识 Qoder 站点区域（别名，与 workbuddy 的 Region 语义一致）。
type Region = qoderapi.Region

const (
	// RegionCN 是国内版。
	RegionCN = qoderapi.RegionCN
	// RegionGlobal 是国际版。
	RegionGlobal = qoderapi.RegionGlobal
)

// NormalizeRegion 将字符串归一化为合法 Region 值。
func NormalizeRegion(s string) Region { return qoderapi.NormalizeRegion(s) }

// GetEndpoints 根据区域返回对应端点配置。
func GetEndpoints(region Region) qoderapi.Endpoints { return qoderapi.GetEndpoints(region) }

// baseprompt.json 是 Qoder 上游 agent_chat_generation 的请求模板（移植自 qoder2api）。
// 模板里保留了 {UUIDn}/{TIME1} 占位符：每次建 Bridge 时替换为实际值再解析成 JSON。
//
//go:embed baseprompt.json
var basePromptRaw []byte

// TemplateBase 构造一次请求模板。
//
// 与 qoder2api 的差异：这里对解析失败返回错误而不是静默忽略。模板解析失败意味着
// 所有上游请求都会失真，静默继续会让问题表现成"模型答非所问"这种难以定位的现象。
func TemplateBase() (map[string]interface{}, error) {
	template := string(basePromptRaw)
	for _, placeholder := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		template = strings.ReplaceAll(template, placeholder, cosy.NewUUID())
	}
	template = strings.ReplaceAll(template, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))

	var templateBase map[string]interface{}
	if errUnmarshal := json.Unmarshal([]byte(template), &templateBase); errUnmarshal != nil {
		return nil, fmt.Errorf("baseprompt.json 模板解析失败（内嵌资源损坏或占位符未替换）: %w", errUnmarshal)
	}
	if templateBase == nil {
		return nil, fmt.Errorf("baseprompt.json 模板为空")
	}
	return templateBase, nil
}

// BridgeSecret 返回要交给 bridge.NewBridge 的凭证串。
//
// 上游 NewBridge 内部会自己调 ParseOAuthSecret：传 dt- 明文会丢掉
// refresh_token（会话无法续期），所以有 refresh token 时必须按 qoder2api
// 导出的 JSON 形态传，与上游桌面端行为保持一致。
func BridgeSecret(cred *Credential) string {
	if cred == nil {
		return ""
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// CredentialFingerprint 生成凭证指纹，用于 Bridge 缓存键。
//
// 不直接用 token 明文做缓存键：缓存键会进日志与内存 dump。
func CredentialFingerprint(cred *Credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:16])
}
