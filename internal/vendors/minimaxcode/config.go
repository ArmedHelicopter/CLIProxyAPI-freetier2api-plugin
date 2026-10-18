package minimaxcode

// 本文件解析 MiniMax Code 的供应商专属配置。

import (
	"strings"
	"sync"

	"freetier2api-plugin/internal/core"
)

var (
	optionsMu sync.RWMutex
	// baseURLCN / baseURLGlobal 为空表示使用内置默认基地址。
	baseURLCN     string
	baseURLGlobal string
)

// Configure 解析两个区域的基地址覆盖，兼容新旧键名：
//   - minimax_code_cn_base_url / minimax_cn_base_url
//   - minimax_code_global_base_url / minimax_global_base_url
func Configure(_ core.Settings, raw map[string]string) {
	optionsMu.Lock()
	baseURLCN = core.FirstNonEmpty(raw["minimax_code_cn_base_url"], raw["minimax_cn_base_url"])
	baseURLGlobal = core.FirstNonEmpty(raw["minimax_code_global_base_url"], raw["minimax_global_base_url"])
	optionsMu.Unlock()
}

// BaseURLFor 返回某区域的基地址覆盖值；空表示用内置默认。
func BaseURLFor(region Region) string {
	optionsMu.RLock()
	defer optionsMu.RUnlock()
	if region == RegionGlobal {
		return strings.TrimSpace(baseURLGlobal)
	}
	return strings.TrimSpace(baseURLCN)
}
