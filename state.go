package main

// 本文件是落盘状态的根层薄适配：
//   - 类型别名与快照/修改包装（internal/state 负责读写与并发）；
//   - 机器盐的加载/生成（有写盘副作用，留在 ABI 层）；
//   - 时间助手（签到与任务按天幂等）。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// 状态类型的别名：调用方（管理接口、任务中心）继续使用包内短名。
type (
	pluginState   = state.State
	taskRecord    = state.TaskRecord
	checkinRecord = state.CheckinRecord
	cachedModel   = state.CachedModel
)

// stateStore 是插件进程内唯一的状态存储。
var stateStore = state.NewStore()

// loadState 从磁盘读取状态并刷新依赖它的内存缓存。
func loadState(cfg pluginConfig) (*pluginState, error) {
	stateStore.Configure(cfg.StateDir)
	loaded, errLoad := stateStore.Load()
	if errLoad != nil {
		return nil, errLoad
	}
	// 状态加载后重算模型缓存（禁用并集与别名表）。
	modelCatalog.ReloadFromState(*loaded)
	return loaded, nil
}

// snapshotState 返回状态副本，调用方可安全读取。
func snapshotState() pluginState { return stateStore.Snapshot() }

// mutateState 在锁内修改状态并立即落盘。
func mutateState(mutate func(*pluginState)) { stateStore.Mutate(mutate) }

// ensureMachineSalt 返回本机指纹盐：配置覆盖 > 已有盐文件 > 首次生成后落盘。
func ensureMachineSalt(cfg pluginConfig) (string, error) {
	if override := strings.TrimSpace(cfg.Raw["machine_salt"]); override != "" {
		return override, nil
	}
	path := saltFilePath(cfg)
	if raw, errRead := os.ReadFile(path); errRead == nil {
		if salt := strings.TrimSpace(string(raw)); salt != "" {
			return salt, nil
		}
	}
	buf := make([]byte, 24)
	if _, errRand := rand.Read(buf); errRand != nil {
		return "", fmt.Errorf("generate machine salt: %w", errRand)
	}
	salt := hex.EncodeToString(buf)
	if errWrite := os.WriteFile(path, []byte(salt+"\n"), 0o600); errWrite != nil {
		return "", fmt.Errorf("write machine salt: %w", errWrite)
	}
	logger.Info("machine salt generated at %s (do not change: it defines this deployment's device fingerprints)", path)
	return salt, nil
}

// todayString 返回当前本地日期（YYYY-MM-DD），用于签到与任务的按天幂等。
func todayString() string { return nowFunc().Format("2006-01-02") }

// nowFunc 允许测试注入固定时间：签到判定与「今天」的记录都依赖它。
var nowFunc = time.Now

// taskStateKey 构造任务记录的复合键（账号 + 任务码）。
func taskStateKey(uid, code string) string {
	return strings.TrimSpace(uid) + "\x1f" + strings.TrimSpace(code)
}

// nowRFC3339 返回当前时间的 RFC3339 字符串。
func nowRFC3339() string { return nowFunc().Format(time.RFC3339) }
