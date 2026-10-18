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
	wireWorkBuddy()
}

// wireWorkBuddy 注入 WorkBuddy 需要的根层能力。
//
// 任务执行与上游客户端构造都依赖宿主回调、状态目录与提示词配置，属于根层
// 职责；供应商包只消费注入进来的能力，不反向感知宿主。
func wireWorkBuddy() {
	workbuddy.SetClientFactory(newUpstreamClient)
	workbuddy.SetTaskSource(func(region workbuddy.Region) []core.Task {
		if region.IsGlobal() {
			return nil
		}
		return workbuddyTaskList()
	})
}

// init 在包初始化期完成注册：宿主的 plugin.register 之前就绪，
// 因此任何时刻调用 core.ResolveVendor 都能拿到完整清单。
func init() {
	registerVendors()
}

// workbuddyTaskList 把根层的任务动作表转成 core.Task 描述。
//
// 只暴露展示所需的元数据（标识、说明、是否耗额度）；真正的执行入口
// 仍在根层（任务中心按 Code 派发），因为任务执行需要账户遍历、并发锁与
// 状态记录，那些属于根层的编排职责而非供应商协议。
func workbuddyTaskList() []core.Task {
	out := make([]core.Task, 0, len(autoActions))
	for _, action := range autoActions {
		out = append(out, core.Task{
			Code:     action.Code,
			Desc:     action.Desc,
			UsesChat: action.UsesChat,
		})
	}
	return out
}
