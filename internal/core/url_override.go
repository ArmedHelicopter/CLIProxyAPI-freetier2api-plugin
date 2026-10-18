package core

import (
	"strings"
	"sync"
)

// URLOverride 是线程安全的 base URL 覆盖值（空表示使用内置默认）。
type URLOverride struct {
	mu    sync.RWMutex
	value string
}

// Set 更新覆盖值（自动去空白）。
func (o *URLOverride) Set(value string) {
	o.mu.Lock()
	o.value = strings.TrimSpace(value)
	o.mu.Unlock()
}

// Raw 返回原始覆盖值（空表示未配置）。
func (o *URLOverride) Raw() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.value
}

// Get 返回覆盖值；为空时返回 fallback。
func (o *URLOverride) Get(fallback string) string {
	if value := o.Raw(); value != "" {
		return value
	}
	return fallback
}
