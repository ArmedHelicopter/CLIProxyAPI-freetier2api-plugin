package main

// 本文件只保留 c-shared 必需的入口、版本注入与 ABI 转接。
// 领域逻辑位于 internal/app；C ABI 在 cgo.go。

import "freetier2api-plugin/internal/app"

// pluginVersion 由 CI 通过 -ldflags -X main.pluginVersion=<tag> 注入。
var pluginVersion = app.DefaultVersion

var pluginApp = app.MustNew(app.Options{
	Version:   pluginVersion,
	Factories: app.BuiltinFactories(),
})

// handleMethod 保留给 cgo.go 与历史测试调用。
func handleMethod(method string, request []byte) ([]byte, error) {
	return pluginApp.HandleMethod(method, request)
}
