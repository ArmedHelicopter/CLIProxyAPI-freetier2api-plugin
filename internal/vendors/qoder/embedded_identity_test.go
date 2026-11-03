package qoder

import (
	"os"
	"runtime"
	"testing"
)

// TestExtractEmbeddedRuntimeInfo 验证内置组件能落盘成可执行文件。
func TestExtractEmbeddedRuntimeInfo(t *testing.T) {
	if embeddedAssetPath() == "" {
		t.Skip("该平台未内置 runtime-info")
	}
	path, okExtract := extractEmbeddedRuntimeInfo()
	if !okExtract {
		t.Fatalf("解包失败")
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("解包产物不可用: %v", errStat)
	}
	blob, errBlob := embeddedAssets.ReadFile(embeddedAssetPath())
	if errBlob != nil {
		t.Fatalf("读取内置资源失败: %v", errBlob)
	}
	if info.Size() != int64(len(blob)) {
		t.Fatalf("大小不符: %d != %d", info.Size(), len(blob))
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("缺少可执行位: %v", info.Mode().Perm())
	}
	// 二次调用必须命中同一份，不重复写盘。
	again, okAgain := extractEmbeddedRuntimeInfo()
	if !okAgain || again != path {
		t.Fatalf("重复解包路径不一致: %q != %q", again, path)
	}
}

// TestEmbeddedMachineIdentity 验证内置组件在本机真的能产出身份。
func TestEmbeddedMachineIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过进程调用")
	}
	if embeddedAssetPath() == "" {
		t.Skip("该平台未内置 runtime-info")
	}
	identity := embeddedMachineIdentity(RegionGlobal)
	if !identity.Native() {
		t.Fatalf("未生成机器身份")
	}
	if len(identity.Token) < 32 || identity.Type == "" {
		t.Fatalf("身份字段不完整: token_len=%d type=%q", len(identity.Token), identity.Type)
	}
}
