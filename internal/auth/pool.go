// Package auth 统一账号枚举、凭证解析、去重、批量执行与登录会话登记。
//
// admin / tasks / quota / keepalive 都曾各自实现一遍“列 auth → 取 JSON →
// 解析凭证 → 过滤 → 去重”，且去重键不一致。本包把这条链收口一次。
//
// 登录会话（SessionRegistry）也放在这里：它与凭证枚举共享“按 App 实例隔离”
// 的生命周期，供应商只经core.Env.Logins 使用，不需要认识本包。
package auth

import (
	"context"
	"os"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// Resolver 由根层注入：把原始凭证解析成 (Credential, Vendor)。
type Resolver func(ctx context.Context, callbackID string, storageJSON []byte, authID string, attrs map[string]string) (*core.Credential, core.Vendor, error)

// Pool 是账号枚举与批量解析的入口。
type Pool struct {
	Registry *core.Registry
	Resolve  Resolver
}

// NewPool 构造账号池。
func NewPool(registry *core.Registry, resolve Resolver) *Pool {
	return &Pool{Registry: registry, Resolve: resolve}
}

// Account 是一个已解析的可用账号。
type Account struct {
	Entry      core.HostAuth
	Credential *core.Credential
	Vendor     core.Vendor
	RawJSON    []byte
	FilePath   string
}

// Failure 是一条无法解析的账号记录。
type Failure struct {
	Entry   core.HostAuth
	Message string
}

// Options 控制账号筛选。
//
// EnabledRealms 直接给域列表而不是判定闭包：realm 过滤的语义只有一处实现
// （core.RealmEnabled），调用方不必各自包一层闭包——那正是此前四处重复的来源。
// 留空表示不限制，与 core.RealmEnabled(nil, _) == true 一致。
type Options struct {
	Targets         []string
	Filter          func(Account) bool
	EnabledRealms   []string
	IncludeDisabled bool
}

// Load 返回可用账号与失败条目；按 credential FileID 去重。
func (p *Pool) Load(ctx context.Context, callbackID string, opts Options) ([]Account, []Failure) {
	entries, errList := hostbridge.ListAuths(ctx, callbackID, p.Registry)
	if errList != nil {
		return nil, []Failure{{Message: "读取账号列表失败：" + errList.Error()}}
	}
	wanted := map[string]bool{}
	for _, target := range opts.Targets {
		if trimmed := strings.TrimSpace(target); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	accounts := make([]Account, 0, len(entries))
	failures := make([]Failure, 0)
	seen := map[string]bool{}
	for _, entry := range entries {
		if !opts.IncludeDisabled && !entry.Usable() {
			continue
		}
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
		if !okRaw && strings.TrimSpace(entry.Path) != "" {
			if data, errRead := os.ReadFile(entry.Path); errRead == nil && len(data) > 0 {
				raw, filePath, okRaw = data, entry.Path, true
			}
		}
		if !okRaw || len(raw) == 0 {
			failures = append(failures, Failure{Entry: entry, Message: "凭证读取失败"})
			continue
		}
		credential, vendor, errResolve := p.Resolve(ctx, callbackID, raw, hostbridge.AccountIdentity(entry), nil)
		if errResolve != nil {
			failures = append(failures, Failure{Entry: entry, Message: "凭证解析失败: " + errResolve.Error()})
			continue
		}
		if !core.RealmEnabled(opts.EnabledRealms, vendor.Region()) {
			continue
		}
		uid := credential.FileIDValue()
		if uid != "" {
			if seen[uid] {
				continue
			}
			seen[uid] = true
		}
		account := Account{Entry: entry, Credential: credential, Vendor: vendor, RawJSON: raw, FilePath: filePath}
		if opts.Filter != nil && !opts.Filter(account) {
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, failures
}
