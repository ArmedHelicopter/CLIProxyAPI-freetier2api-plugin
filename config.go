package main

// 本文件是配置的根层薄适配：把宿主传入的 YAML 交给 internal/pluginconfig，
// 再完成插件生命周期的副作用（日志、状态目录、机器盐、供应商配置下发）。
//
// 配置的解析与校验规则在 internal/pluginconfig；根层不再解释任何配置键。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/modelconfig"
	"freetier2api-plugin/internal/pluginconfig"
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
