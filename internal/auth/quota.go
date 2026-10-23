package auth

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

// QuotaTotals 是「剩余 / 总额」的折算结果。
type QuotaTotals struct {
	Remaining int64
	Total     int64
	// Unlimited 报告剩余额度无上限（上游以 -1 / unit=unlimited 表达）。
	// 无上限时 Remaining 没有意义，展示层应显示「无限」而不是数字。
	Unlimited bool
}

// Totals 折算「剩余 / 总额」。
//
// 键名按族识别（标准键、credit_*、user_quota_* / addon_quota_*）。
// **新增供应商必须复用其中一族**：这个函数只认白名单，写别的键名会静默
// 得到 0/0，页面就会一直显示 0 / 0（Trae 曾用 remaining_credit 踩过）。
func Totals(response *pluginapi.QuotaFetchResponse) QuotaTotals {
	if response == nil {
		return QuotaTotals{}
	}
	var totals QuotaTotals
	remainingFound := false
	totalFound := false
	for _, metric := range response.Summary {
		if isRemainingKey(metric.Key) && (strings.EqualFold(metric.Unit, "unlimited") || metric.Value < 0) {
			totals.Unlimited = true
			continue
		}
		switch {
		case isRemainingKey(metric.Key):
			// Zero is a valid remaining value. Track presence separately so a
			// later quota family cannot overwrite an exhausted primary bucket.
			if !remainingFound {
				totals.Remaining = int64(metric.Value)
				remainingFound = true
			}
		case isTotalKey(metric.Key):
			// A zero total is also meaningful (for example, no entitlement).
			if !totalFound {
				totals.Total = int64(metric.Value)
				totalFound = true
			}
		}
	}
	if !totalFound {
		var used int64
		for _, metric := range response.Summary {
			if isUsedKey(metric.Key) {
				used += int64(metric.Value)
			}
		}
		if used > 0 {
			totals.Total = totals.Remaining + used
		}
	}
	return totals
}

func isRemainingKey(key string) bool {
	switch key {
	case "remaining", "credit_remain", "user_quota_remaining", "addon_quota_remaining":
		return true
	}
	return false
}

func isTotalKey(key string) bool {
	switch key {
	case "total", "credit_total", "user_quota_total", "addon_quota_total":
		return true
	}
	return false
}

func isUsedKey(key string) bool {
	switch key {
	case "used", "credit_used", "user_quota_used", "addon_quota_used":
		return true
	}
	return false
}
