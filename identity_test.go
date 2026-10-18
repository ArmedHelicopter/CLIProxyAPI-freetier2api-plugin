package main

// 本文件覆盖账号身份（FileID）在各路径上的一致性。

import (
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// 模拟宿主对同一条账号记录给出的三种标识（真实取值形态）。
func TestAccountIdentityIsStableAcrossPaths(t *testing.T) {
	entries := []core.HostAuth{
		// 有文件名的常规账号（Qoder / ZCode / WorkBuddy 都是这种）。
		{ID: "qodercn-nick1456600333", Name: "qodercn-nick1456600333.json", AuthIndex: "019fa299-3d61-7192-b903-0145508e658c"},
		{ID: "zcode-ac90---6IY9", Name: "zcode-ac90---6IY9.json", AuthIndex: "a1b2c3d4"},
		{ID: "workbuddycn-79fdc1fc", Name: "workbuddycn-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", AuthIndex: "deadbeef"},
	}
	for _, entry := range entries {
		got := accountIdentity(entry)
		// 身份取 Name（磁盘文件名），它才是凭证解析时用的同一个标识；
		// ID 可能被截断（宿主对长文件名有截断行为），不能当身份。
		want := strings.TrimSuffix(entry.Name, ".json")
		if got != want {
			t.Fatalf("accountIdentity(%+v) = %q, want %q", entry, got, want)
		}
		// 关键：身份与 AuthIndex 无关——早期签到走 AuthIndex、展示走 Name，
		// 于是同一账号算出两个身份。这条断言锁死该回归。
		if got == entry.AuthIndex {
			t.Fatalf("账号身份不得等于 AuthIndex（会导致签到/展示错位）: %q", got)
		}
	}
}

// 运行时账号没有 Name（宿主内建 provider），必须回落到 ID。
func TestAccountIdentityFallsBackToID(t *testing.T) {
	if got := accountIdentity(core.HostAuth{ID: "runtime-1", AuthIndex: "idx"}); got != "runtime-1" {
		t.Fatalf("got %q, want runtime-1", got)
	}
	// 三者皆空时返回空串，由调用方决定处置（而不是凭空造一个身份）。
	if got := accountIdentity(core.HostAuth{}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	// .json 后缀必须剥掉，否则与凭证解析时的文件名不一致。
	if got := accountIdentity(core.HostAuth{Name: "cline-me@x.com.json"}); got != "cline-me@x.com" {
		t.Fatalf("got %q, want cline-me@x.com", got)
	}
}

// 端到端：同一条账号记录，签到路径与展示路径必须解析出同一个账号身份，
// 并且在 parseVendorCredential 的 FileID 兜底之后仍然一致。
func TestCheckinAndDisplayResolveSameIdentity(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Qoder 凭证（UID 取自文件名）——这正是出问题的形态。
	const qoderCredential = `{"type":"freetier","vendor":"qodercn","region":"cn",
		"device_token":"dt-abc","refresh_token":"drt-xyz","label":"nick"}`
	entry := core.HostAuth{
		ID:        "qodercn-nick1456600333",
		Name:      "qodercn-nick1456600333.json",
		AuthIndex: "019fa299-3d61-7192-b903-0145508e658c",
	}

	identity := accountIdentity(entry)
	credential, okParse := core.ParseCredential([]byte(qoderCredential), identity, nil)
	if !okParse {
		t.Fatal("qoder credential not parsed")
	}

	// 核心断言：凭证解析出的 FileID 与两条路径传入的文件名一致。
	if got := credential.FileIDValue(); got != identity {
		t.Fatalf("FileID = %q, want %q", got, identity)
	}
	// 且不得再是 AuthIndex —— 那是导致签到/展示错位的根源。
	if credential.FileIDValue() == entry.AuthIndex {
		t.Fatal("FileID 不得等于 AuthIndex")
	}
	if credential.FileIDValue() == "" {
		t.Fatal("FileID 不得为空（签到记录会因此永远查不到）")
	}
}

// FileID 兜底：供应商 Parse 没设置 FileID 时，根层必须补上。
func TestParseVendorCredentialFillsFileID(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// WorkBuddy 的 UID 是上游 UUID；FileID 仍必须是文件名。
	const wbCredential = `{"type":"freetier","vendor":"workbuddycn",
		"auth":{"accessToken":"at","refreshToken":"rt","realm":"cn"},
		"account":{"uid":"79fdc1fc-de43-40da-b6f0-fe05d9e4367b"}}`
	credential, okParse := core.ParseCredential([]byte(wbCredential), "workbuddycn-79fdc1fc.json", nil)
	if !okParse {
		t.Fatal("workbuddy credential not parsed")
	}

	if got := credential.FileIDValue(); got != "workbuddycn-79fdc1fc" {
		t.Fatalf("FileID = %q, want workbuddycn-79fdc1fc", got)
	}
	// 上游 UID 必须原样保留：设备指纹与 X-Uid 头都靠它，改了会被上游当作新设备。
	if got := credential.UIDValue(); got != "79fdc1fc-de43-40da-b6f0-fe05d9e4367b" {
		t.Fatalf("UID = %q（上游身份不得被 FileID 覆盖）", got)
	}
}

// 上游标识不参与任何身份判定。
func TestUpstreamUIDIsNotUsedForIdentity(t *testing.T) {
	credential := &core.Credential{UID: "upstream-uuid", FileID: "file-id"}
	if credential.FileIDValue() == credential.UIDValue() {
		t.Fatal("两个字段必须是独立语义")
	}
}
