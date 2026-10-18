package console

// Package console 提供内嵌的控制台页面。
//
// 页面是纯静态 HTML（不含账号、额度或凭证），通过管理接口按需拉取数据；
// 渲染动态数据统一走 textContent，避免把上游内容当成 HTML 注入。
//
// 页面从 Go 源码里抽出来单独存放：1,400 行的 HTML/JS 字符串放在 .go 文件里
// 既无法被编辑器高亮/格式化，也会让代码文件失去可读性。

import (
	_ "embed"
	"strings"
)

//go:embed console.html
var page string

// Page 返回控制台页面（注入管理接口的 base path）。
func Page(basePath string) string {
	return strings.NewReplacer("__BASE__", basePath).Replace(page)
}
