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
// 修复与宿主管理接口 DELETE /v0/management/auth-files 的解析规则保持一致：
// 非绝对路径先按宿主工作目录（插件与宿主同进程）解析成绝对路径，再走
// .json / 符号链接 / 目录 三道安全检查。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// traeCredentialJSON 是一份可被 Trae 供应商解析的凭证（删除前要校验归属）。
const traeCredentialJSON = `{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`

// managementCallWithBody 调一次带 JSON body 的插件管理接口。
func managementCallWithBody(t *testing.T, method, path string, body any) pluginapi.ManagementResponse {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method:  method,
		Path:    "/v0/management/plugins/freetier2api" + path,
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}},
	}
	if body != nil {
		request.Body = mustMarshal(body)
	}
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(request))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	return decodeManagementResponse(t, raw)
}

// decodeManagementResponse 解开 management.handle 的信封，取出响应体。
func decodeManagementResponse(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env abiEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode management response: %v (raw=%s)", errUnmarshal, env.Result)
	}
	return response
}

// newRelativeAuthFile 在测试工作目录里落一份凭证文件，返回（相对文件名, 绝对路径）。
//
// 刻意放在工作目录而不是 t.TempDir()：后者在 Windows 上通常位于另一块盘，
// filepath.Rel 跨盘无法构造出宿主实际返回的 `..\oauth\<name>.json` 形态。
func newRelativeAuthFile(t *testing.T, pattern, content string) (string, string) {
	t.Helper()
	file, errTemp := os.CreateTemp(".", pattern)
	if errTemp != nil {
		t.Fatalf("create temp auth file: %v", errTemp)
	}
	absolute := file.Name()
	t.Cleanup(func() { _ = os.Remove(absolute) })
	if _, errWrite := file.WriteString(content); errWrite != nil {
		_ = file.Close()
		t.Fatalf("write temp auth file: %v", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("close temp auth file: %v", errClose)
	}
	return filepath.Base(absolute), absolute
}

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

// TestAccountDeleteResolvesHostRelativePath 复现事故：宿主返回相对路径时删除必须成功。
func TestAccountDeleteResolvesHostRelativePath(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name, absolute := newRelativeAuthFile(t, "delete-relative-*.json", traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", name) // 宿主原样返回相对路径

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

	name, absolute := newRelativeAuthFile(t, "delete-guard-*.txt", traeCredentialJSON)
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", name)

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
