package autoclaw

// 本文件放包内共享的小工具。

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"freetier2api-plugin/internal/core"
)

// MaskedKey 返回脱敏后的 token（展示用）：只保留前 4 后 4 位。
func MaskedKey(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", len(trimmed)-8) + trimmed[len(trimmed)-4:]
}

// jwtExpiry 解码 JWT payload 的 exp（秒），解不出返回 0。不验签，只读 exp。
func jwtExpiry(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return 0
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// jwtUserID 解码 JWT payload 里的用户 id（依次尝试常见键），解不出返回空。
func jwtUserID(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return ""
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return core.GetString(claims, "user_id", "userId", "uid", "userID")
}
