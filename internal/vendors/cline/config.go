package cline

// 本文件解析 Cline 的供应商专属配置。

import (
	"strings"
	"sync"

	"freetier2api-plugin/internal/core"
)

var (
	optionsMu sync.RWMutex
	// baseURLOverride 为空表示使用内置默认基地址。
	baseURLOverride string
)

// Configure 解析 cline_base_url（保留既有键名与默认值）。
func Configure(_ core.Settings, raw map[string]string) {
	optionsMu.Lock()
	baseURLOverride = strings.TrimSpace(raw["cline_base_url"])
	optionsMu.Unlock()
}

// BaseURLOverride 返回配置的上游基地址覆盖值；空表示用内置默认。
func BaseURLOverride() string {
	optionsMu.RLock()
	defer optionsMu.RUnlock()
	return baseURLOverride
}
