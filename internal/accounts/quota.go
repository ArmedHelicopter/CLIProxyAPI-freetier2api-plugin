package accounts

import (
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// Summary 生成一行额度摘要。
func Summary(response *pluginapi.QuotaFetchResponse) string {
	if response == nil {
		return ""
	}
	parts := make([]string, 0, len(response.Summary)+2)
	for _, metric := range response.Summary {
		label := strings.TrimSpace(metric.Label)
		if label == "" {
			label = metric.Key
		}
		parts = append(parts, label+" "+FormatValue(metric.Value))
	}
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			label := strings.TrimSpace(group.DisplayName)
			if label == "" {
				label = bucket.Window
			}
			parts = append(parts, label+" "+bucket.Description)
		}
	}
	return strings.Join(parts, " · ")
}

// FormatValue 格式化额度数值。
func FormatValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// Totals 折算「剩余 / 总额」。
//
// 优先使用标准键 remaining/total/used；同时兼容各供应商已有的键名。
func Totals(response *pluginapi.QuotaFetchResponse) (remaining, total int64) {
	if response == nil {
		return 0, 0
	}
	for _, metric := range response.Summary {
		switch metric.Key {
		case "remaining", "credit_remain", "user_quota_remaining", "addon_quota_remaining":
			if remaining == 0 {
				remaining = int64(metric.Value)
			}
		case "total", "credit_total", "user_quota_total", "addon_quota_total":
			if total == 0 {
				total = int64(metric.Value)
			}
		}
	}
	if total == 0 {
		var used int64
		for _, metric := range response.Summary {
			switch metric.Key {
			case "used", "credit_used", "user_quota_used", "addon_quota_used":
				used += int64(metric.Value)
			}
		}
		if used > 0 {
			total = remaining + used
		}
	}
	return remaining, total
}
