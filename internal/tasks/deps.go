package tasks

// 本文件定义任务引擎依赖的最小接口；由 internal/app 组合根实现。

import (
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
)

// Deps 是任务引擎需要的根层能力。
type Deps interface {
	Registry() *core.Registry
	Pool() *accounts.Pool
	Config() pluginconfig.Config
	State() state.State
	MutateState(func(*state.State))
	Now() time.Time
	RuntimeFields() []pluginapi.ConfigField
}
