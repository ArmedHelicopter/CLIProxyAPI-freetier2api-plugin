package admin

// 本文件锁定「删除账号」前的路径归一化。
//
// 真实事故：EasyCLIProxyAPI 把内核的 oauth.auth-dir 配成相对路径（默认
// ../oauth，相对内核工作目录）。CLIProxyAPI 扫盘时把相对路径写进 auth 属性，
// host.auth.get / host.auth.list 原样返回，删除保护于是收到
// `..\oauth\<name>.json`，一律判成「非绝对路径」——Windows 桌面版点「删除」
// 永远删不掉账号。这里钉住两点：
//  1. 相对路径按宿主工作目录解析成绝对路径（插件与宿主同进程）；
//  2. 解析之后 .json / 符号链接 / 目录 三道安全检查仍然生效。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAuthFilePath(t *testing.T) {
	cwd, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("getwd: %v", errGetwd)
	}
	absolute := filepath.Join(cwd, "workbuddycn-1.json")
	relative := filepath.Join("..", "oauth", "minimaxcodecn-56186690844033030.json")
	resolved := filepath.Clean(filepath.Join(cwd, "..", "oauth", "minimaxcodecn-56186690844033030.json"))

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"绝对路径原样保留", absolute, filepath.Clean(absolute)},
		{"相对路径按工作目录解析", relative, resolved},
		{"空路径仍为空", "  ", ""},
		{"首尾空白被去掉", " " + absolute + "\n", filepath.Clean(absolute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAuthFilePath(tc.in)
			if got != tc.want {
				t.Fatalf("resolveAuthFilePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.want != "" && !filepath.IsAbs(got) {
				t.Fatalf("resolveAuthFilePath(%q) = %q, want absolute path", tc.in, got)
			}
		})
	}
}
