package tasks

// 本文件定义任务引擎依赖的最小接口；由 internal/app 组合根实现。

import (
	"context"
	"time"

	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// Deps 是任务引擎需要的根层能力。
type Deps interface {
	Registry() *core.Registry
	Pool() *auth.Pool
	Config() Config
	State() state.State
	MutateState(func(*state.State))
	Now() time.Time
	// SaveRefreshedCredential 把续期后的凭证写回宿主与磁盘。
	//
	// 由组合根实现（内部走 core.StorageMerger → host.auth.save → 落盘）。
	// 任务引擎依赖它而不是自己发宿主 RPC，因此本包不需要认识宿主。
	SaveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
		entry core.HostAuth, original []byte, updated *core.Credential, filePath string)
}
