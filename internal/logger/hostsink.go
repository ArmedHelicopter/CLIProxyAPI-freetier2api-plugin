// hostsink.go 把插件日志镜像进宿主的日志文件（CLIProxyAPI 的 main.log）。
//
// 为什么需要：宿主的日志页（/v8/management/observability/logs）只读
// <auth-dir>/logs/main.log，插件日志原本只走 stderr 进 journald，
// 面板里完全看不到插件内部行为。
//
// 宿主行格式（实测 gin_logger.go / usage_helpers.go 的输出）：
//
//	[2026-11-02 12:26:41] [d01155b1] [warn ] [usage_helpers.go:306] 消息
//
// 级别名右补空格到 5 字符；没有请求上下文时 ID 段写 "--------"。
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// hostRecheckInterval 是轮转检测的节流间隔。宿主会轮转 / 截断 main.log，
	// 每次写都 stat 没必要，太久又会往已 rename 的旧 inode 里写。
	hostRecheckInterval = 5 * time.Second
	// hostRetryInterval 是打开失败后的重试间隔，避免日志风暴。
	hostRetryInterval = 30 * time.Second
)

// hostSink 是宿主日志文件的追加 sink。
type hostSink struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	minLevel  Level
	enabled   bool
	lastCheck time.Time
	lastFail  time.Time
	warned    bool
}

var host = &hostSink{}

// ResolveHostLogFile 推断宿主日志文件路径，返回第一个真实存在的候选。
//
// 插件拿不到宿主的 auth-dir，只能按状态目录反推两种常见布局：
//
//	<base>/auths/logs/main.log   auth-dir 是 <base>/auths（本机部署形态）
//	<base>/logs/main.log         auth-dir 就是 <base>（默认 ~/.cli-proxy-api）
//
// 都不存在时返回空，表示宿主没把日志落到文件。
func ResolveHostLogFile(stateDir string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(stateDir), string(filepath.Separator))
	if trimmed == "" {
		return ""
	}
	base := filepath.Dir(trimmed)
	if base == "" || base == "." {
		return ""
	}
	candidates := []string{
		filepath.Join(base, "auths", "logs", "main.log"),
		filepath.Join(base, "logs", "main.log"),
	}
	if home, errHome := os.UserHomeDir(); errHome == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, ".cli-proxy-api", "logs", "main.log"))
	}
	for _, candidate := range candidates {
		if info, errStat := os.Stat(candidate); errStat == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// LevelFromName 把级别名转成 Level，未知值回落 info。
func LevelFromName(raw string) Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return LevelDebug
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// InitHostSink 开启或关闭宿主日志镜像。
//
// path 为空时禁用。文件不存在时也不启用——宿主没开
// observability.logs.logging-to-file 时，凭空造一个日志文件只会让面板读到空内容。
// 失败不致命：日志写不进去不应该影响插件功能。
func InitHostSink(enabled bool, path string, minLevel Level) {
	host.mu.Lock()
	defer host.mu.Unlock()

	if host.file != nil {
		_ = host.file.Close()
		host.file = nil
	}
	host.enabled = enabled
	host.path = strings.TrimSpace(path)
	host.minLevel = minLevel
	host.lastCheck = time.Time{}
	host.lastFail = time.Time{}
	host.warned = false
	if host.enabled {
		host.openLocked(time.Now())
	}
}

// openLocked 打开宿主日志文件。调用方必须持有 host.mu。
func (h *hostSink) openLocked(now time.Time) bool {
	h.lastCheck = now
	if h.path == "" {
		return false
	}
	if _, errStat := os.Stat(h.path); errStat != nil {
		// 宿主没把日志落到文件：不创建，保持沉默。
		return false
	}
	file, errOpen := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		h.lastFail = now
		h.noteOnce(errOpen)
		return false
	}
	h.file = file
	h.warned = false
	return true
}

// noteOnce 只在首次失败时往 stderr 提示一行，避免刷屏。
func (h *hostSink) noteOnce(err error) {
	if h.warned {
		return
	}
	h.warned = true
	fmt.Fprintf(os.Stderr, "[freetier] 宿主日志镜像不可用（%s）：%v\n", h.path, err)
}

// readyLocked 确保 sink 可用，顺带处理宿主轮转。调用方必须持有 host.mu。
func (h *hostSink) readyLocked(now time.Time) bool {
	if h.path == "" {
		return false
	}
	if h.file == nil {
		if now.Sub(h.lastFail) < hostRetryInterval {
			return false
		}
		return h.openLocked(now)
	}
	if now.Sub(h.lastCheck) < hostRecheckInterval {
		return true
	}
	h.lastCheck = now
	current, errStat := os.Stat(h.path)
	if errStat != nil {
		_ = h.file.Close()
		h.file = nil
		return false
	}
	// 宿主轮转后 main.log 换了 inode，旧 fd 写进去等于丢日志。
	if opened, errOpened := h.file.Stat(); errOpened == nil && !os.SameFile(opened, current) {
		_ = h.file.Close()
		h.file = nil
		return h.openLocked(now)
	}
	return true
}

// write 把一条日志追加到宿主日志文件。调用方已按级别过滤。
func (h *hostSink) write(lvl Level, label string, now time.Time, message string) {
	if !h.enabled || lvl < h.minLevel {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.readyLocked(now) {
		return
	}
	if _, errWrite := h.file.WriteString(formatHostLine(now, label, message)); errWrite != nil {
		_ = h.file.Close()
		h.file = nil
		h.lastFail = now
		h.noteOnce(errWrite)
	}
}

// closeHost 关闭宿主日志 sink。供 Close 调用。
func closeHost() {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.file != nil {
		_ = host.file.Close()
		host.file = nil
	}
}

// formatHostLine 按宿主 logger 的格式拼一行。
//
// 级别名右补空格到 5 字符（宿主输出 "[warn ]"、"\[info ]"），
// 请求 ID 段在没有上下文时用 "--------" 占位，来源段用插件包名。
func formatHostLine(now time.Time, label string, message string) string {
	return fmt.Sprintf("[%s] [--------] [%-5s] [freetier.go:0] [freetier] %s\n",
		now.Format("2006-01-02 15:04:05"), strings.ToLower(label), message)
}
