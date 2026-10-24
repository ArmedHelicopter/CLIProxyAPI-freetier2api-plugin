package hostbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestListHostAuthsClaimsHijackedCredentials 验证被前置插件抢走的凭证仍被列出。
//
// 宿主遍历插件解析凭证时，一旦某个插件报错或认领就中止循环，且会把
// 「当前询问的插件的 identifier」兜底填进 provider。归属判定过宽的前置插件
// 会把本插件的文件抢走（provider 变成对方），只按 provider 过滤会让这些
// 账号从页面凭空消失。文件名仍是本插件约定，因此按文件名兜底认领。
func TestMergeHostAuthEntryPrefersActiveFileStatus(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		source     string
		wantStatus string
	}{
		{name: "stale error is cleared", target: "error", source: "active", wantStatus: "active"},
		{name: "empty runtime status uses file status", target: "", source: "active", wantStatus: "active"},
		{name: "non-error runtime status remains authoritative", target: "active", source: "disabled", wantStatus: "active"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := core.HostAuth{Status: test.target}
			mergeHostAuthEntry(&target, core.HostAuth{Status: test.source})
			if target.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", target.Status, test.wantStatus)
			}
		})
	}
}

func TestListHostAuthsClaimsHijackedCredentials(t *testing.T) {
	registry := core.NewRegistry()
	if !fileNameBelongsToPlugin("workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", registry) {
		t.Fatal("workbuddy-<uuid>.json must be recognized by file name")
	}
	if !fileNameBelongsToPlugin("my-workbuddy.json", registry) {
		t.Fatal("file name containing the workbuddy segment must be recognized")
	}
	// 别家的文件名不能被认领，否则会把别人的账号当成自己的。
	for _, foreign := range []string{"qoder-login-x.json", "claude-1.json", "codex-a@b.com.json", "auth.json"} {
		if fileNameBelongsToPlugin(foreign, registry) {
			t.Fatalf("foreign file %q must not be claimed", foreign)
		}
	}

	// provider 被抢走（qoder）但文件名是本插件的：仍要列出来。
	entries := filterPluginAuths([]core.HostAuth{
		{Name: "workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", Provider: "qoder", AuthIndex: "1"},
		{Name: "workbuddy-110693a0-d32d-4d81-8a0a-7a3445069f96.json", Provider: "workbuddy", AuthIndex: "2"},
		{Name: "qoder-login-f34c07f7.json", Provider: "qoder", AuthIndex: "3"},
	}, registry)
	if len(entries) != 2 {
		t.Fatalf("want 2 workbuddy credentials (one hijacked), got %d: %+v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Provider == "qoder" && strings.Contains(entry.Name, "qoder-login") {
			t.Fatalf("foreign credential leaked in: %+v", entry)
		}
	}
}

// TestResolveAuthPathNormalizesHostPath 锁定宿主路径只在边界换算一次。
//
// 宿主不一定给绝对路径：EasyCLIProxyAPI 把 oauth.auth-dir 配成 ../oauth，
// 扫盘得到的相对路径原样经 host.auth.list / host.auth.get 返回。所有路径消费方
// （删除、stat、读文件兜底、续期写回）都拿这里的结果，不能再各自换算。
func TestResolveAuthPathNormalizesHostPath(t *testing.T) {
	cwd, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("getwd: %v", errGetwd)
	}
	absolute := filepath.Join(cwd, "workbuddy-1.json")
	relative := filepath.Join("..", "oauth", "traecn-1.json")
	resolved := filepath.Clean(filepath.Join(cwd, "..", "oauth", "traecn-1.json"))

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
			got := ResolveAuthPath(tc.in)
			if got != tc.want {
				t.Fatalf("ResolveAuthPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
