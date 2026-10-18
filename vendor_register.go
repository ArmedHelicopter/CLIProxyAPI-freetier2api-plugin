package main

// 本文件是供应商的唯一注册点（composition root）。
//
// 依赖方向：root → internal/vendors/* → internal/core → cpasdk。
// 根层在这里完成两件事：
//   1. 逐个 core.RegisterVendor（顺序与重构前的 init 顺序一致，保证 UI 稳定）；
//   2. 注入供应商需要的根层能力（上游客户端工厂、任务动作清单、模型兜底表）。
//
// 新增供应商只需要：写 internal/vendors/<name>（含 adapter.go）+ 在下面加一行。

import (
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/cline"
	"freetier2api-plugin/internal/vendors/codearts"
	"freetier2api-plugin/internal/vendors/minimaxcode"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/internal/vendors/qoder"
	"freetier2api-plugin/internal/vendors/trae"
	"freetier2api-plugin/internal/vendors/traesolo"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/vendors/zcode"
)

// registerVendors 注册全部供应商实例。
//
// 顺序与重构前各 vendor_*.go 的 init 顺序一致：core.Vendors() 的返回顺序
// 会进入控制台列表与内容嗅探兜底，顺序变化会让 UI 抖动。
func registerVendors() {
	for _, vendor := range cline.Instances(workbuddy.DefaultContextWindowFor) {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range codearts.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range minimaxcode.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range opencodezen.Instances(workbuddy.DefaultContextWindowFor) {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range qoder.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range trae.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range traesolo.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range workbuddy.Instances() {
		core.RegisterVendor(vendor)
	}
	for _, vendor := range zcode.Instances() {
		core.RegisterVendor(vendor)
	}
}

// init 在包初始化期完成注册：宿主的 plugin.register 之前就绪，
// 因此任何时刻调用 core.ResolveVendor 都能拿到完整清单。
func init() {
	registerVendors()
}
