package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestParseCredentialBothShapes 验证凭证的两种形态都能解析。
func TestParseCredentialBothShapes(t *testing.T) {
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","domain":"www.codebuddy.cn"},
	             "account":{"uid":"u1","nickname":"昵称"},"device_token":"dt"}`
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.workbuddy.ai","device_token":"dt"}`

	for _, testCase := range []struct {
		name       string
		raw        string
		wantRealm  Region
		wantUID    string
		wantDevice string
	}{
		{"nested", nested, RegionCN, "u1", "dt"},
		// 扁平形态按 domain 推断域：workbuddy.ai 家族是国际版。
		{"flat global", flat, RegionGlobal, "u1", "dt"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(testCase.raw), RegionCN)
			if errParse != nil {
				t.Fatalf("ParseCredential: %v", errParse)
			}
			if credential.Realm() != testCase.wantRealm {
				t.Fatalf("realm = %s, want %s", credential.Realm(), testCase.wantRealm)
			}
			if credential.UIDValue() != testCase.wantUID {
				t.Fatalf("uid = %q, want %q", credential.UIDValue(), testCase.wantUID)
			}
			if credential.DeviceTokenValue() != testCase.wantDevice {
				t.Fatalf("device_token = %q", credential.DeviceTokenValue())
			}
		})
	}
}

// TestParseCredentialRejectsEmpty 验证空凭证被拒绝。
func TestParseCredentialRejectsEmpty(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{}`), RegionCN); errParse == nil {
		t.Fatal("credential without any token must be rejected")
	}
	if _, errParse := ParseCredential(nil, RegionCN); errParse == nil {
		t.Fatal("empty credential must be rejected")
	}
}

// TestMergeStorageJSONKeepsUserFields 验证写回时保留用户维护的字段。
//
// 宿主 auth 文件里可能有 disabled / prefix / proxy_url / note / weight 等
// 用户自己加的字段；重建式写回会静默丢掉它们。
func TestMergeStorageJSONKeepsUserFields(t *testing.T) {
	original := []byte(`{"auth":{"accessToken":"old"},"disabled":true,"note":"我的备注","weight":3}`)
	credential := &Credential{AccessToken: "new", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	merged, errMerge := MergeStorageJSON(original, credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &payload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if payload["disabled"] != true {
		t.Fatal("user field 'disabled' must be preserved")
	}
	if payload["note"] != "我的备注" {
		t.Fatal("user field 'note' must be preserved")
	}
	auth, okAuth := payload["auth"].(map[string]any)
	if !okAuth || auth["accessToken"] != "new" {
		t.Fatalf("token must be updated: %v", payload["auth"])
	}
}

// TestStorageJSONAlwaysCarriesType 验证写回的凭证一定带 type 字段。
//
// 宿主用 type 直接判定归属（有它就不再遍历插件）。缺这个字段时归属取决于
// 插件遍历顺序，会被判定过宽的前置插件抢走，账号在宿主侧被归到别的 provider。
func TestStorageJSONAlwaysCarriesType(t *testing.T) {
	credential := &Credential{AccessToken: "at", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	raw, errStorage := credential.StorageJSON()
	if errStorage != nil {
		t.Fatalf("StorageJSON: %v", errStorage)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["type"] != ProviderKey {
		t.Fatalf("type = %v, want %q", payload["type"], ProviderKey)
	}

	// 合并写回：原文件缺 type 时也要补上（用户从别处复制进来的旧凭证）。
	merged, errMerge := MergeStorageJSON([]byte(`{"auth":{"accessToken":"old"},"uid":"u1"}`), credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var mergedPayload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &mergedPayload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if mergedPayload["type"] != ProviderKey {
		t.Fatalf("merged type = %v, want %q", mergedPayload["type"], ProviderKey)
	}
	// 用户字段仍要保留。
	if mergedPayload["uid"] != "u1" {
		t.Fatal("merge must preserve user fields")
	}
}

// TestLooksLikeCredential 验证凭证归属判定。
//
// 判定必须偏严：宿主把所有非内建格式的文件依次喂给每个插件，
// 误吞别家的凭证会让宿主用本插件结构改写对方的账号文件（且是静默的）。
func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		// 强证据：显式声明。
		{"host provider hint", map[string]any{}, "x.json", ProviderKey, true},
		{"type field", map[string]any{"type": ProviderKey}, "x.json", "", true},
		{"type field uppercase", map[string]any{"type": "FreeTier"}, "x.json", "", true},

		// 文件名约定。CN 与 GLOBAL 是独立供应商，两者都要被本协议认出
		// （具体区域由 core.ResolveVendor 的前缀匹配决定）。
		{"filename exact", map[string]any{"foo": "bar"}, "workbuddy.json", "", true},
		{"filename cn vendor", map[string]any{"foo": "bar"}, "workbuddycn-abc.json", "", true},
		{"filename global vendor", map[string]any{"foo": "bar"}, "workbuddyglobal-abc.json", "", true},
		{"filename underscore suffix", map[string]any{"foo": "bar"}, "workbuddy_abc.json", "", true},
		{"filename contains segment", map[string]any{"foo": "bar"}, "my-workbuddy.json", "", true},

		// 本插件独有的嵌套结构。
		{"nested auth pair", map[string]any{
			"auth": map[string]any{"accessToken": "a", "refreshToken": "r"},
		}, "x.json", "", true},

		// **必须拒绝**：通用字段不足以判定归属。
		{"top-level accessToken only", map[string]any{"accessToken": "a"}, "x.json", "", false},
		{"top-level both tokens", map[string]any{"accessToken": "a", "refreshToken": "r"}, "x.json", "", false},
		{"device_token only", map[string]any{"device_token": "d"}, "x.json", "", false},
		{"nested accessToken without refresh", map[string]any{
			"auth": map[string]any{"accessToken": "a"},
		}, "x.json", "", false},

		// 明确的别家 provider。
		{"foreign type", map[string]any{"type": "qoder"}, "x.json", "qoder", false},
		{"foreign filename", map[string]any{"access_token": "x"}, "claude.json", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LooksLikeCredential(testCase.raw, testCase.fileName, testCase.provider); got != testCase.want {
				t.Fatalf("LooksLikeCredential = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestParseCredentialStillAcceptsFlatShape 验证收紧归属判定后，
// 扁平形态（手写凭证）仍能被解析——判定严不等于解析窄。
func TestParseCredentialStillAcceptsFlatShape(t *testing.T) {
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.codebuddy.cn"}`
	credential, errParse := ParseCredential([]byte(flat), RegionCN)
	if errParse != nil {
		t.Fatalf("flat credential must still parse: %v", errParse)
	}
	if credential.UIDValue() != "u1" || credential.AccessTokenValue() != "at" {
		t.Fatalf("flat credential parsed incorrectly: %+v", credential)
	}
	// 但归属判定不因它能解析就放行（除非文件名符合约定）。
	if LooksLikeCredential(map[string]any{"accessToken": "at", "refreshToken": "rt"}, "mystery.json", "") {
		t.Fatal("a parseable flat credential from an unknown file must not be claimed")
	}
}

// TestDeriveIDStabilityAndIsolation 验证设备指纹的稳定性与隔离性。
func TestDeriveIDStabilityAndIsolation(t *testing.T) {
	rt := newRuntime(core.Env{})
	setSalt := func(s string) { value := s; rt.salt.Store(&value) }
	setSalt("salt-a")
	first := rt.deriveID("uid-1", "machine")
	second := rt.deriveID("uid-1", "machine")
	if first != second {
		t.Fatal("deriveID must be stable for the same input")
	}
	if len(first) != 36 {
		t.Fatalf("deriveID length = %d, want 36 (official device id shape)", len(first))
	}
	if rt.deriveID("uid-2", "machine") == first {
		t.Fatal("different accounts must get different fingerprints")
	}
	// 换机器盐 = 换设备：指纹必须整体漂移。
	setSalt("salt-b")
	if rt.deriveID("uid-1", "machine") == first {
		t.Fatal("changing the install salt must change fingerprints")
	}
}

// TestBuildCacheKeyIsolatesAccounts 验证缓存键的账号隔离。
//
// uid 前缀是硬隔离因子：跨账号复用同一 cache key 会让上游命中
// 错账号的前缀缓存，泄露对方对话内容。
func TestBuildCacheKeyIsolatesAccounts(t *testing.T) {
	keyA := buildCacheKey("account-aaaa1111", "conv-1")
	keyB := buildCacheKey("account-bbbb2222", "conv-1")
	if keyA == keyB {
		t.Fatal("different accounts must produce different cache keys")
	}
	if !strings.HasPrefix(keyA, "wb2a-account-") {
		t.Fatalf("cache key should carry the uid prefix: %q", keyA)
	}
	if buildCacheKey("account-aaaa1111", "conv-1") != keyA {
		t.Fatal("cache key must be deterministic")
	}
}

// TestParseBalanceMatchesUpstreamShape 验证余额解析对齐上游的真实契约。
//
// 上游该接口用 PascalCase 字段、双层信封（Response.Data.Accounts），
// 且 Cycle 期套餐优先。早期实现按 camelCase 解析，结果恒为 0。
func TestParseBalanceMatchesUpstreamShape(t *testing.T) {
	raw := `{"Response":{"Data":{"Accounts":[
	  {"PackageName":"基础套餐","CycleCapacitySize":1000,"CycleCapacityRemain":400,"CycleCapacityUsed":600},
	  {"PackageName":"加油包","CapacitySize":500,"CapacityRemain":500}
	]}}}`
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	balance := parseBalance(payload)

	// Cycle 套餐：1000 总量、400 剩余；加油包：500/500。合计 1500 / 900。
	if balance.Total != 1500 {
		t.Fatalf("total = %d, want 1500", balance.Total)
	}
	if balance.Remain != 900 {
		t.Fatalf("remain = %d, want 900", balance.Remain)
	}
	if len(balance.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(balance.Packages))
	}
	if balance.Packages[0].Name != "基础套餐" || balance.Packages[0].Remain != 400 {
		t.Fatalf("first package = %+v", balance.Packages[0])
	}
}

// TestPackageRemainUsedCycleWins 验证 Cycle 期套餐的取数规则。
func TestPackageRemainUsedCycleWins(t *testing.T) {
	// CycleUsed 大于 size-remain 时，应以 CycleUsed 为准反推 remain。
	remain, used, size := packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 1000, CycleCapacityRemain: 900, CycleCapacityUsed: 300,
	})
	if size != 1000 || used != 300 || remain != 700 {
		t.Fatalf("cycle rule: remain=%d used=%d size=%d, want 700/300/1000", remain, used, size)
	}
	// 无 Cycle 期时回落 Capacity 三字段。
	remain, used, size = packageRemainUsed(billingResourceAccount{
		CapacitySize: 500, CapacityRemain: 200,
	})
	if size != 500 || used != 300 || remain != 200 {
		t.Fatalf("capacity fallback: remain=%d used=%d size=%d, want 200/300/500", remain, used, size)
	}
	// remain 超出 size 时钳到 size（上游偶发脏数据）。
	remain, _, size = packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 100, CycleCapacityRemain: 999,
	})
	if remain != 100 || size != 100 {
		t.Fatalf("clamp: remain=%d size=%d, want 100/100", remain, size)
	}
}
