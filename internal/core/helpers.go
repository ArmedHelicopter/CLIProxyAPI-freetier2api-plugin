package core

// 本文件提供各供应商共用的解析与转换助手。
//
// 这些函数原本在多个供应商包里各有一份（签名还漂移过），统一收敛到这里：
// 同一种语义只允许有一份实现，供应商包只负责"哪些键、什么特征"的差异。

import (
	"encoding/json"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// FirstNonEmpty 返回第一个去空白后非空的字符串（返回去空白后的值）。
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// GetString 依次尝试多个键，返回第一个非空字符串（去空白）。
func GetString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if text, okText := value.(string); okText {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// GetMap 依次尝试多个键，返回第一个 map 值（缺失或类型不符返回 nil）。
func GetMap(m map[string]any, keys ...string) map[string]any {
	if m == nil {
		return nil
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if sub, okSub := value.(map[string]any); okSub {
			return sub
		}
	}
	return nil
}

// GetInt64 依次尝试多个键，返回第一个可转换的整数。
//
// 不做任何单位归一化：需要把毫秒转成秒的供应商用 GetUnixSeconds。
func GetInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		switch number := value.(type) {
		case float64:
			return int64(number)
		case int64:
			return number
		case int:
			return int64(number)
		case json.Number:
			if parsed, errParse := number.Int64(); errParse == nil {
				return parsed
			}
		}
	}
	return 0
}

// millisecondThreshold 是「这个时间戳看起来是毫秒」的判定阈值。
//
// 1e12 秒约等于公元 33658 年，正常秒级时间戳不可能超过它；反之毫秒级
// 时间戳（13 位）必然超过。上游各家混用两种单位，统一在这里归一化。
const millisecondThreshold = 1e12

// GetUnixSeconds 与 GetInt64 相同，但把明显是毫秒的时间戳归一化成秒。
func GetUnixSeconds(m map[string]any, keys ...string) int64 {
	value := GetInt64(m, keys...)
	if value > millisecondThreshold {
		return value / 1000
	}
	return value
}

// FileNameMatchesConvention 判断文件名是否符合某个供应商的命名约定。
//
// 约定：<name>.json / <name>-<id>.json / <name>_<id>.json / <name>.<id>.json，
// 或名字里含 -<name> / _<name> 段（如 my-cline.json）。
func FileNameMatchesConvention(fileName string, names ...string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		name = strings.ToLower(name)
		if base == name {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, name+separator) ||
				strings.Contains(base, separator+name) {
				return true
			}
		}
	}
	return false
}

// VendorFieldVerdict 按凭证里的 vendor 字段判定归属。
//
// 返回 (decided, claimed)：
//   - decided=false 表示 vendor 字段没有给出结论（缺失，或只是家族别名，
//     例如 minimaxcode 的 "minimax"、trae 的 "trae"），调用方继续看其它证据；
//   - decided=true 且 claimed=true 表示明确属于本供应商；
//   - decided=true 且 claimed=false 表示明确属于别家，**必须拒绝**。
//
// 这一段是"归属必须偏严"的第一道闸：显式声明了别家就绝不认领。
func VendorFieldVerdict(raw map[string]any, vendorID string, aliases ...string) (decided, claimed bool) {
	declared := strings.ToLower(GetString(raw, "vendor"))
	if declared == "" {
		return false, false
	}
	if declared == strings.ToLower(strings.TrimSpace(vendorID)) {
		return true, true
	}
	for _, alias := range aliases {
		if declared == strings.ToLower(strings.TrimSpace(alias)) {
			return false, false
		}
	}
	return true, false
}

// NativeExtractor 从归一化凭证里取回供应商原生对象。
type NativeExtractor[T any] func(*Credential) (*T, bool)

// NativeCredential 取回某凭证的供应商原生对象。
//
// extract 由各供应商提供，断言写在包内（Go 不允许对类型参数做断言）。
// 统一在这里处理 nil 与「不是本供应商解析的凭证」两种错误，避免每个
// 供应商各写一份错误文案。
func NativeCredential[T any](cred *Credential, vendorID string, extract NativeExtractor[T]) (*T, error) {
	if cred == nil {
		return nil, NewPluginError("credential_missing", "credential is nil", 401)
	}
	native, okNative := extract(cred)
	if !okNative || native == nil {
		return nil, NewPluginError("credential_missing",
			"credential was not parsed by the "+vendorID+" vendor", 401)
	}
	return native, nil
}

// ConvertModels 把供应商原生模型清单批量转成宿主契约类型。
//
// vendorID 传空串：本插件的模型以裸名注册（见 SplitModelID 的说明）。
// convert 返回 false 表示跳过该条目（通常是缺 ID）。
func ConvertModels[T any](models []T, convert func(T) (ModelDescriptor, bool)) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		descriptor, okModel := convert(model)
		if !okModel {
			continue
		}
		out = append(out, ModelInfoToPluginAPI("", descriptor))
	}
	return out
}

// DefaultMaxOutputTokens 是上游没给输出上限时的兜底值。
//
// 取 16384：这是各家上游对主流模型的常见默认值，也是 qoder2api 与
// workbuddy2api 各自的兜底清单里一致采用的值。
const DefaultMaxOutputTokens int64 = 16384

// ContentTypeJSON 是插件返回给宿主的 JSON 内容类型。
const ContentTypeJSON = "application/json; charset=utf-8"

// DefaultContextWindowTokens 是模型元数据缺失时的兜底上下文窗口。
//
// 与 workbuddy 兜底表一致（1,000,000）：取小了会让客户端误以为超出上下文，
// 取大了只会让上游按自身限制拒绝——后者可恢复。
const DefaultContextWindowTokens int64 = 1000000

// DefaultContextWindow 返回兜底上下文窗口（忽略模型名）。
func DefaultContextWindow(_ string) int64 { return DefaultContextWindowTokens }
