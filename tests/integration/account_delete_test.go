package tests

// 本文件锁定「删除账号」对宿主相对路径的兼容性。
//
// 真实事故：EasyCLIProxyAPI 把内核的 oauth.auth-dir 配成相对路径（默认
// ../oauth，相对 cpa-core 工作目录）。CLIProxyAPI 扫盘时用
// filepath.Join(auth-dir, 文件名) 合成凭证路径且不做 filepath.Abs，于是
// host.auth.get / host.auth.list 返回 `..\oauth\<name>.json`。插件原来的删除
// 保护直接以 filepath.IsAbs 拒绝，Windows 桌面版点「删除」只会得到
// 「拒绝删除非绝对路径」，账号永远删不掉。
//
// 路径归一化现在统一在 hostbridge 边界完成（hostbridge.ResolveAuthPath），
// 这里用「工作目录 / oauth 平级」的真实布局复现宿主的相对 auth-dir。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// traeCredentialJSON 是一份可被 Trae 供应商解析的凭证（删除前要校验归属）。
const traeCredentialJSON = `{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`

// accountDeleteResult 是 POST /accounts/delete 的响应形状。
type accountDeleteResult struct {
	OK      bool `json:"ok"`
	Deleted int  `json:"deleted"`
	Results []struct {
		AuthID  string `json:"auth_id"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"results"`
}

// relativeAuthFile 复现宿主的相对 auth-dir 布局：
//
//	<root>/core    ← 进程工作目录（宿主 cpa-core 的位置）
//	<root>/oauth   ← auth-dir（配置为 ../oauth）
//
// 返回宿主实际会给出的相对路径（`../oauth/<name>`）与绝对路径。
func relativeAuthFile(t *testing.T, name, content string) (relative, absolute string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "core")
	oauth := filepath.Join(root, "oauth")
	for _, dir := range []string{work, oauth} {
		if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
			t.Fatalf("mkdir %s: %v", dir, errMkdir)
		}
	}
	absolute = filepath.Join(oauth, name)
	if errWrite := os.WriteFile(absolute, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	chdirForTest(t, work)
	return filepath.Join("..", "oauth", name), absolute
}

// chdirForTest 切换工作目录并在用例结束还原：路径归一化依赖进程工作目录。
func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	previous, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("getwd: %v", errGetwd)
	}
	if errChdir := os.Chdir(dir); errChdir != nil {
		t.Fatalf("chdir %s: %v", dir, errChdir)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

// TestAccountDeleteWithAbsoluteHostPath 覆盖标准 CLIProxyAPI 部署：auth-dir 默认
// `~/.cli-proxy-api` 展开后是绝对路径，删除必须照常工作（#14 之前这条本来就是通的，
// 但归一化改动不能破坏它）。
func TestAccountDeleteWithAbsoluteHostPath(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-absolute-1.json"
	dir := t.TempDir()
	absolute := filepath.Join(dir, name)
	if errWrite := os.WriteFile(absolute, []byte(traeCredentialJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", absolute)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Deleted != 1 || len(payload.Results) != 1 || !payload.Results[0].OK {
		t.Fatalf("绝对路径删除失败: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); !os.IsNotExist(errStat) {
		t.Fatalf("凭证文件应已删除，实际 stat 结果: %v", errStat)
	}
}

// TestAccountDeleteResolvesHostRelativePath 复现事故：宿主返回相对路径时删除必须成功。
func TestAccountDeleteResolvesHostRelativePath(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-relative-1.json"
	relative, absolute := relativeAuthFile(t, name, traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", relative)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Deleted != 1 || len(payload.Results) != 1 || !payload.Results[0].OK {
		t.Fatalf("相对路径删除失败: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); !os.IsNotExist(errStat) {
		t.Fatalf("凭证文件应已删除，实际 stat 结果: %v", errStat)
	}
}

// TestAccountDeleteKeepsRelativePathGuards 确认解析相对路径之后安全检查仍然生效：
// 非 .json 文件不许删，用户文件不会因为宿主路径形态变化而被误删。
func TestAccountDeleteKeepsRelativePathGuards(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "delete-guard-1.txt"
	relative, absolute := relativeAuthFile(t, name, traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", relative)

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/delete = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if payload.Deleted != 0 || len(payload.Results) != 1 || payload.Results[0].OK {
		t.Fatalf("非 .json 文件不应被删除: %s", response.Body)
	}
	if _, errStat := os.Stat(absolute); errStat != nil {
		t.Fatalf("被拒绝删除的文件不应消失: %v", errStat)
	}
}

// TestAccountDeleteReportsMissingFile 锁定「文件不存在 ≠ 已删除」。
// 宿主路径若因工作目录基准不同而指向别处，静默成功会让页面显示「已删除」
// 而账号其实还在。
func TestAccountDeleteReportsMissingFile(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	root := t.TempDir()
	work := filepath.Join(root, "core")
	if errMkdir := os.MkdirAll(work, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	chdirForTest(t, work)
	name := "delete-missing-1.json"
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", filepath.Join("..", "oauth", name))

	response := managementCallWithBody(t, "POST", "/accounts/delete", map[string]any{
		"account_ids": []string{name},
	})
	var payload accountDeleteResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode delete response: %v (%s)", errUnmarshal, response.Body)
	}
	if payload.Deleted != 0 || len(payload.Results) != 1 || payload.Results[0].OK {
		t.Fatalf("文件不存在时不应报告删除成功: %s", response.Body)
	}
}
