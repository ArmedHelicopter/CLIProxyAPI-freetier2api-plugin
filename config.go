package main

// 本文件负责全局配置与落盘状态：
//   - 配置解析/校验在 internal/pluginconfig，根层只做生命周期副作用；
//   - state.json 的读写/快照在 internal/state，根层保留类型别名与机器盐生成。
//
// 机器盐一旦生成必须保持不变：变更会让全部账号的派生机器码漂移。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// pluginConfig 是插件有效配置（全局键的强类型视图 + 供应商原始键）。
type pluginConfig = pluginconfig.Config

// defaultPluginConfig 返回内置默认配置。
//
// 供应商专属默认值（UA、提示词、设备令牌）由各供应商的 Configure 自己补，
// 不再出现在全局配置结构里。
func defaultPluginConfig() pluginConfig {
	return pluginconfig.Default()
}

// defaultStateDir 是 internal/pluginconfig 的薄包装（测试可直接调用）。
func defaultStateDir() string { return pluginconfig.DefaultStateDir() }

// 全局配置：原子指针 + 两个访问器（读多写少，且 reconfigure 与请求并发）。
var currentConfig atomic.Pointer[pluginConfig]

// loadedConfig 返回当前生效的配置副本。
func loadedConfig() pluginConfig {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func storeConfig(cfg pluginConfig) { currentConfig.Store(&cfg) }

// decodeConfig 解析宿主传入的配置 YAML。
func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg, errDecode := pluginconfig.Decode(raw, defaultPluginConfig())
	if errDecode != nil {
		return cfg, errDecode
	}
	// 区域别名（cn:glm-5.2）在这里展开成每家供应商的规范键，
	// 保证 loadedConfig().DisabledModels 可直接参与集合运算。
	cfg.DisabledModels = modelconfig.ExpandDisabledList(cfg.DisabledModels)
	return cfg, nil
}

// realmEnabled 判断某个区域是否被配置启用。
//
// **空区域一律视为启用**：区域只对 WorkBuddy 这类「同一协议的不同部署」
// 有意义；单一部署的供应商 Region() 返回空串，不该被 enabled_realms 卡掉。
func realmEnabled(cfg pluginConfig, realm string) bool {
	if strings.TrimSpace(realm) == "" {
		return true
	}
	for _, item := range cfg.EnabledRealms {
		if item == realm {
			return true
		}
	}
	return false
}

// promptModeFor 返回当前生效的提示词模式（供管理端展示）。
func promptModeFor() string { return workbuddy.PromptMode() }

// currentMachineSalt 是本部署的指纹盐，由 applyConfig 计算后随 Settings 下发。
var currentMachineSalt string

// applyConfig 应用配置：初始化日志与状态目录、设置指纹盐、下发供应商配置。
//
// register 与 reconfigure 共用这一个入口，所有副作用都收敛在这里。
func applyConfig(cfg pluginConfig) error {
	if errDir := ensureStateDir(cfg); errDir != nil {
		return errDir
	}
	logger.SetLevel(cfg.LogLevel)
	if errLog := logger.InitFile(cfg.LogToFile, filepath.Join(cfg.StateDir, "logs")); errLog != nil {
		logger.Error("init log file sink failed: %v", errLog)
	}
	stateStore.Configure(cfg.StateDir)
	salt, errSalt := ensureMachineSalt(cfg)
	if errSalt != nil {
		return errSalt
	}
	currentMachineSalt = salt
	storeConfig(cfg)
	// 模型层需要全局设置快照：启用域、额外模型、配置声明的禁用项。
	modelCatalog.Configure(modelconfig.Settings{
		EnabledRealms:    cfg.EnabledRealms,
		ExtraModels:      cfg.ExtraModels,
		DisabledModels:   cfg.DisabledModels,
		ContextWindowFor: workbuddy.DefaultContextWindowFor,
	})
	modelCatalog.RefreshFromState()
	// 供应商专属配置：根层只提供原始键，由各包自己解释。
	configureVendors(cfg)
	return nil
}

// ensureStateDir 创建状态目录。
func ensureStateDir(cfg pluginConfig) error {
	dir := strings.TrimSpace(cfg.StateDir)
	if dir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state dir %s: %w", dir, errMkdir)
	}
	return nil
}

// stateFilePath 返回状态文件路径。
func stateFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, pluginconfig.StateFileName)
}

// saltFilePath 返回机器盐文件路径。
func saltFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, pluginconfig.SaltFileName)
}

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
