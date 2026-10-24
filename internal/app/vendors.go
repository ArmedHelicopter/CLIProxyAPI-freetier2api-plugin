package app

// 本文件是唯一的供应商组合根：新增/删除供应商只改这里的工厂列表。

import (
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/autoclaw"
	"freetier2api-plugin/internal/vendors/cline"
	"freetier2api-plugin/internal/vendors/codearts"
	"freetier2api-plugin/internal/vendors/minimaxcode"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/internal/vendors/qoder"
	"freetier2api-plugin/internal/vendors/raccoon"
	"freetier2api-plugin/internal/vendors/trae"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/vendors/zcode"
)

// BuiltinFactories 返回内置供应商工厂（顺序即注册顺序）。
func BuiltinFactories() []core.VendorFactory {
	return []core.VendorFactory{
		{Instances: autoclaw.Instances},
		{Instances: func(env core.Env) []core.Vendor { return cline.Instances(env) }},
		{Instances: codearts.Instances},
		{Instances: minimaxcode.Instances},
		{Instances: func(env core.Env) []core.Vendor { return opencodezen.Instances(env) }},
		{Instances: qoder.Instances},
		{Instances: raccoon.Instances},
		{Instances: trae.Instances},
		{Instances: workbuddy.Instances},
		{Instances: zcode.Instances},
	}
}
