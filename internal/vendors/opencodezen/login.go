package opencodezen

import (
	"strings"
)

// AuthMode 报告本供应商的凭证形态。
func (v *adapter) AuthMode() string { return "apikey" }

// MaskedKey 返回脱敏后的 key：保留前四位与后四位，中间用 *** 代替。
//
// 用于展示名兜底——**绝不**把完整 key 放进 Label，因为 Label 会进日志、
// 进管理接口响应、进页面 HTML。凭证材料只应存在于凭证文件里。
func MaskedKey(key string) string {
	trimmed := strings.TrimSpace(key)
	// 太短的 key 全遮：露出前后四位等于露出全部。
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}
