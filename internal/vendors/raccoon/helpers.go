package raccoon

// 本文件放包内共享的小工具。

import "strings"

// MaskedKey 返回脱敏后的 token（展示用）：只保留前 4 后 4 位。
func MaskedKey(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", len(trimmed)-8) + trimmed[len(trimmed)-4:]
}
