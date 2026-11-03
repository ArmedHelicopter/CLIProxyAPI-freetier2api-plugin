package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetHostSink 关闭单例，避免用例之间互相污染。
func resetHostSink(t *testing.T) {
	t.Helper()
	InitHostSink(false, "", LevelInfo)
	t.Cleanup(func() { InitHostSink(false, "", LevelInfo) })
}

// seedLog 造一个已存在的宿主日志文件并返回路径。
func seedLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.log")
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("准备日志文件失败：%v", errWrite)
	}
	return path
}

// TestFormatHostLineMatchesHostLayout 断言输出与宿主 logger 的格式逐字一致。
//
// 宿主实际输出形如：
//
//	[2026-11-02 12:26:41] [d01155b1] [warn ] [usage_helpers.go:306] 消息
//
// 级别名右补空格到 5 字符，请求 ID 无上下文时用 "--------"。
func TestFormatHostLineMatchesHostLayout(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 26, 41, 0, time.Local)

	got := formatHostLine(now, "INFO", "已解析授权")
	want := "[2026-11-02 12:26:41] [--------] [info ] [freetier.go:0] [freetier] 已解析授权\n"
	if got != want {
		t.Fatalf("info 行格式不符\n got=%q\nwant=%q", got, want)
	}

	if line := formatHostLine(now, "ERROR", "x"); !strings.Contains(line, "[error]") {
		t.Fatalf("error 是 5 字符，不该补空格，实际 %q", line)
	}
	if line := formatHostLine(now, "DEBUG", "x"); !strings.Contains(line, "[debug]") {
		t.Fatalf("debug 是 5 字符，不该补空格，实际 %q", line)
	}
}

// TestHostSinkWritesOnlyAtOrAboveMinLevel 断言级别过滤生效。
func TestHostSinkWritesOnlyAtOrAboveMinLevel(t *testing.T) {
	resetHostSink(t)
	path := seedLog(t, "seed\n")
	InitHostSink(true, path, LevelInfo)

	now := time.Now()
	host.write(LevelDebug, "DEBUG", now, "调试不该出现")
	host.write(LevelInfo, "INFO", now, "信息应出现")
	host.write(LevelError, "ERROR", now, "错误应出现")

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("读取日志失败：%v", errRead)
	}
	text := string(data)
	if strings.Contains(text, "调试不该出现") {
		t.Fatal("低于 minLevel 的日志不该写入")
	}
	if !strings.Contains(text, "信息应出现") || !strings.Contains(text, "错误应出现") {
		t.Fatalf("达标的日志没写进去：%q", text)
	}
}

// TestHostSinkSkipsMissingFile 断言宿主没落文件时不去创建。
//
// 造一个空文件会让面板日志页读到插件自己的空日志，比看不到更误导。
func TestHostSinkSkipsMissingFile(t *testing.T) {
	resetHostSink(t)
	path := filepath.Join(t.TempDir(), "absent.log")
	InitHostSink(true, path, LevelInfo)

	host.write(LevelInfo, "INFO", time.Now(), "不该落盘")

	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("文件不存在时不该创建，err=%v", errStat)
	}
}

// TestHostSinkReopensAfterRotation 断言宿主轮转后写到新文件而不是旧 inode。
func TestHostSinkReopensAfterRotation(t *testing.T) {
	resetHostSink(t)
	path := seedLog(t, "old\n")
	InitHostSink(true, path, LevelInfo)

	host.write(LevelInfo, "INFO", time.Now(), "轮转前")

	if errRename := os.Rename(path, path+".1"); errRename != nil {
		t.Fatalf("模拟轮转失败：%v", errRename)
	}
	if errWrite := os.WriteFile(path, []byte("new\n"), 0o600); errWrite != nil {
		t.Fatalf("重建日志失败：%v", errWrite)
	}
	// 绕过 5 秒节流，强制下一次写做 inode 检查。
	host.mu.Lock()
	host.lastCheck = time.Time{}
	host.mu.Unlock()

	host.write(LevelInfo, "INFO", time.Now(), "轮转后")

	current, errCurrent := os.ReadFile(path)
	if errCurrent != nil {
		t.Fatalf("读取新日志失败：%v", errCurrent)
	}
	if !strings.Contains(string(current), "轮转后") {
		t.Fatalf("轮转后应写入新文件，实际 %q", current)
	}
	rotated, errRotated := os.ReadFile(path + ".1")
	if errRotated != nil {
		t.Fatalf("读取轮转文件失败：%v", errRotated)
	}
	if strings.Contains(string(rotated), "轮转后") {
		t.Fatal("不该继续写已 rename 的旧 inode")
	}
}

// TestResolveHostLogFilePrefersAuthsLayout 断言优先命中 auths 子目录布局。
func TestResolveHostLogFilePrefersAuthsLayout(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, ".freetier-plugin")
	if errMk := os.MkdirAll(stateDir, 0o700); errMk != nil {
		t.Fatalf("建状态目录失败：%v", errMk)
	}
	logDir := filepath.Join(base, "auths", "logs")
	if errMk := os.MkdirAll(logDir, 0o700); errMk != nil {
		t.Fatalf("建日志目录失败：%v", errMk)
	}
	want := filepath.Join(logDir, "main.log")
	if errWrite := os.WriteFile(want, []byte("x"), 0o600); errWrite != nil {
		t.Fatalf("写日志文件失败：%v", errWrite)
	}

	if got := ResolveHostLogFile(stateDir); got != want {
		t.Fatalf("推导路径不符\n got=%q\nwant=%q", got, want)
	}
}

// TestResolveHostLogFileEmptyWhenMissing 断言两种布局都不存在时返回空。
func TestResolveHostLogFileEmptyWhenMissing(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, ".freetier-plugin")
	if errMk := os.MkdirAll(stateDir, 0o700); errMk != nil {
		t.Fatalf("建状态目录失败：%v", errMk)
	}
	// HOME 指向临时目录，排除本机真实 ~/.cli-proxy-api 的干扰。
	t.Setenv("HOME", base)
	if got := ResolveHostLogFile(stateDir); got != "" {
		t.Fatalf("都不存在时应返回空，实际 %q", got)
	}
}

// TestInitHostSinkDisabled 断言关闭后不写任何东西。
func TestInitHostSinkDisabled(t *testing.T) {
	resetHostSink(t)
	path := seedLog(t, "seed\n")
	InitHostSink(false, path, LevelInfo)

	host.write(LevelInfo, "INFO", time.Now(), "关闭后不该写")

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("读取日志失败：%v", errRead)
	}
	if strings.Contains(string(data), "关闭后不该写") {
		t.Fatal("sink 关闭时不该写入")
	}
}

// TestLogMirrorsIntoHostFile 走公开的 Info 入口，确认整条链路接通。
func TestLogMirrorsIntoHostFile(t *testing.T) {
	resetHostSink(t)
	path := seedLog(t, "seed\n")
	InitHostSink(true, path, LevelInfo)
	SetLevel("info")
	t.Cleanup(func() { SetLevel("info") })

	Info("端到端镜像 %s", "ok")

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("读取日志失败：%v", errRead)
	}
	text := string(data)
	if !strings.Contains(text, "端到端镜像 ok") {
		t.Fatalf("Info 没镜像进宿主日志：%q", text)
	}
	if !strings.Contains(text, "[freetier.go:0] [freetier]") {
		t.Fatalf("镜像行缺少来源段：%q", text)
	}
}
