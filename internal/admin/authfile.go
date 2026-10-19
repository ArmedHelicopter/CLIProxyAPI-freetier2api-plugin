package admin

// 本文件提供凭证落盘的唯一入口。
//
// 「添加 API Key 账号」与「粘贴 localhost 回调完成登录」两条路径都要把一份凭证
// 交给宿主，只有走这里才不会各自长出一套「宿主写 + 兜底写盘」的副本。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// authFileDirs 是宿主不可用时的兜底落盘目录：容器里是 /root/.cli-proxy-api，
// 本地直跑时是进程工作目录。
var authFileDirs = []string{"/root/.cli-proxy-api", "."}

// saveAuthFile 落盘一份凭证。
//
// 首选 host.auth.save：它写 auths 目录并热更新宿主内存里的注册表。只有这条
// 回调不可用时才直接写盘（与凭证续期写盘兜底同一个理由）。两条都失败才算失败——
// 调用方必须能把「没保存成功」如实告诉用户，不能出现页面显示成功而磁盘上什么都没有。
func saveAuthFile(callbackID, fileName string, raw []byte) error {
	name := strings.TrimSpace(fileName)
	if name == "" || len(raw) == 0 {
		return fmt.Errorf("凭证缺少文件名或内容")
	}

	_, errSave := hostbridge.Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name, JSON: raw,
	})
	if errSave == nil {
		logger.Info("saved auth file %s via host.auth.save", name)
		return nil
	}
	logger.Debug("host.auth.save failed for %s: %v", name, errSave)

	for _, dir := range authFileDirs {
		if fi, errStat := os.Stat(dir); errStat != nil || !fi.IsDir() {
			continue
		}
		if errWrite := os.WriteFile(filepath.Join(dir, name), raw, 0o600); errWrite == nil {
			logger.Info("saved auth file %s on disk", name)
			return nil
		}
	}
	return fmt.Errorf("host.auth.save 不可用且落盘失败（%s）", name)
}
