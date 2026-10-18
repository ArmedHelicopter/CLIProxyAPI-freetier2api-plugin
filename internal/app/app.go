package app

// Package app 是插件的组合根与 ABI 适配层。
//
// 一个 App 实例持有配置、状态、模型、供应商注册表、登录会话与后台任务；
// package main 只持有 C ABI 必需的默认实例。测试可以构造多个 App 并验证隔离。

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"freetier2api-plugin/internal/accounts"
	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/loginsession"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

// DefaultVersion 是未注入发行版号时的版本。
const DefaultVersion = "0.1.0"

// Options 是 App 的构造参数。
type Options struct {
	Version   string
	Factories []core.VendorFactory
	// Host 仅用于测试/嵌入；为空时使用内部 hostBridge。
	Host core.Host
	// Now 允许测试注入时钟；为空时使用 time.Now。
	Now func() time.Time
}

// App 是插件的一个完整运行实例。
type App struct {
	version string
	now     func() time.Time

	host     core.Host
	registry *core.Registry
	logins   *loginsession.Registry

	cfg    atomic.Pointer[pluginconfig.Config]
	store  *state.Store
	models *modelconfig.Manager

	machineSalt string

	lifecycleMu sync.Mutex
	registered  bool

	credentialMu    sync.Mutex
	credentialCache map[string]credentialCacheEntry

	backgroundMu     sync.Mutex
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup

	streamMu       sync.Mutex
	streamCancels  map[uint64]context.CancelFunc
	streamWG       sync.WaitGroup
	nextStreamKey  uint64
	pluginStreamID uint64

	pool        *accounts.Pool
	taskCatalog *tasks.Catalog
	admin       *admin.Service

	schedulerMu      sync.Mutex
	schedulerPoke    chan string
	schedulerRunning bool

	taskLockMu sync.Mutex
	taskLocks  map[string]*sync.Mutex
	taskQueue  *queueState
}

// New 构造一个 App。
func New(opts Options) (*App, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	version := opts.Version
	if version == "" {
		version = DefaultVersion
	}
	a := &App{
		version:         version,
		now:             now,
		registry:        core.NewRegistry(),
		logins:          loginsession.NewRegistry(),
		store:           state.NewStore(),
		credentialCache: map[string]credentialCacheEntry{},
		streamCancels:   map[uint64]context.CancelFunc{},
		schedulerPoke:   make(chan string, 4),
		taskLocks:       map[string]*sync.Mutex{},
		taskQueue:       &queueState{},
	}
	if opts.Host != nil {
		a.host = opts.Host
	} else {
		a.host = &hostBridge{app: a}
	}
	env := core.Env{Host: a.host, Registry: a.registry, Logins: a.logins}
	for _, factory := range opts.Factories {
		if factory.Instances == nil {
			continue
		}
		for _, vendor := range factory.Instances(env) {
			if errRegister := a.registry.Register(vendor); errRegister != nil {
				return nil, errRegister
			}
		}
	}
	a.models = modelconfig.New(a.store, a.registry)
	a.pool = accounts.NewPool(a.registry, a.resolveVendorCredential)
	a.taskCatalog = tasks.NewCatalog(a.registry)
	a.admin = admin.New(adminDeps{app: a}, a.registry, a.pool, a.taskCatalog)
	defaults := defaultPluginConfig()
	a.storeConfig(defaults)
	a.models.Configure(modelconfig.Settings{
		EnabledRealms:    defaults.EnabledRealms,
		ExtraModels:      defaults.ExtraModels,
		DisabledModels:   defaults.DisabledModels,
		ContextWindowFor: core.DefaultContextWindow,
	})
	return a, nil
}

// MustNew 构造 App 并在失败时 panic（组合根启动期错误）。
func MustNew(opts Options) *App {
	a, err := New(opts)
	if err != nil {
		panic(err)
	}
	return a
}

// Version 返回对外版本号。
func (a *App) Version() string { return a.version }

// Registry 返回供应商注册表。
func (a *App) Registry() *core.Registry { return a.registry }

// Model 返回模型管理器。
func (a *App) Model() *modelconfig.Manager { return a.models }
