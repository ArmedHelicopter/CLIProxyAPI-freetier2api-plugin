package tests

// 本文件覆盖 auth 能力：登录分发与跨供应商凭证归属判定。

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
)

// 登录会话绑定的回归测试。
//
// 守卫的问题：宿主靠 AuthLoginStartResponse.Metadata 里的 vendor 字段把登录
// 会话绑定回具体供应商；缺了它宿主拿不到授权链接，页面只显示「获取授权链接
// 失败」。这条约束曾经只靠各供应商自己记得设置，导致 codearts / trae /
// traesolo 三个供应商长期无法添加账号——而插件侧 LoginStart 明明成功返回了
// URL，日志上只有一句 "started login"，排查方向被完全带偏。
//
// 现在标识由根层（auth_login.go 的分发点）统一注入，本用例即该契约的守卫。

// TestLoginStartInjectsVendorMetadata 验证根层为供应商补齐登录会话绑定。
//
// 用**真实的 codearts** 而不是桩：它的 LoginStart 纯本地构造（无网络调用），
// 且其包内实现本身不设置 Metadata——正是回归要覆盖的场景。
func TestLoginStartInjectsVendorMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	vendor, okVendor := testApp.Registry().ByID("codearts")
	if !okVendor {
		t.Fatal("codearts vendor not registered")
	}

	result := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendor.ID()},
	})

	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode login response: %v (raw=%s)", errUnmarshal, result)
	}
	if response.URL == "" {
		t.Fatal("授权链接为空")
	}
	// 核心断言：供应商未设置 Metadata，根层也必须补上 vendor 绑定。
	if got := response.Metadata[core.VendorKey]; got != vendor.ID() {
		t.Fatalf("metadata.vendor = %v, want %q（宿主靠它绑定登录会话）", got, vendor.ID())
	}
	if got := response.Metadata["region"]; got != vendor.Region() {
		t.Fatalf("metadata.region = %v, want %q", got, vendor.Region())
	}
}

// 跨供应商归属判定的回归测试。
//
// 这是本插件最关键的守卫：宿主把所有非内建格式的凭证依次喂给每个插件，
// 归属判定过宽会让宿主用本插件的结构覆盖对方账号（且是静默的）。
// 新增供应商时，这些用例保证「各家的凭证只被自家认领」。

func TestAuthParseVendorOwnershipDiscrimination(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// 判据说明：这些用例**刻意不给 provider**（空串），模拟宿主还没把文件
	// 归属给任何插件时的判定。给 provider 会让 workbuddy 的
	// `provider == "freetier"` 兜底把一切都收下，测试就测不出判别力了。
	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		// OpenCodeZEN：opencodezen 文件名 + api_key。
		{"opencodezen file name", "opencodezen-key1.json", "", `{"api_key":"sk-abc"}`, true},
		{"opencodezen zen_key field", "my-account.json", "", `{"zen_key":"sk-abc"}`, true},
		// Cline：cline 文件名 / workos 前缀 / 驼峰 refreshToken。
		{"cline file name", "cline-me@x.com.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		{"cline workos prefix", "acc.json", "", `{"access_token":"workos:at"}`, true},
		// 既有供应商：仍按各自约定认领。
		{"workbuddy nested", "workbuddycn-u1.json", "", `{"account":{"uid":"u1"},"auth":{"accessToken":"at","realm":"cn"}}`, true},
		{"qoder pt token", "qodercn-u1.json", "", `{"token":"pt-abc"}`, true},
		// 新增供应商：ZCode, TraeSOLO, Trae国内版/国际版, CodeArts, MiniMax Code
		{"zcode filename", "zcode-key1.json", "", `{"api_key":"abc.123"}`, true},
		{"traesolo filename", "traesolo-user1.json", "", `{"accessToken":"at_123","refreshToken":"rt_456"}`, true},
		{"traecn filename", "traecn-user1.json", "", `{"accessToken":"at_123"}`, true},
		{"traeglobal filename", "traeglobal-user1.json", "", `{"accessToken":"at_123","region":"global"}`, true},
		{"codearts filename", "codearts-user1.json", "", `{"access_key_id":"ak","secret_access_key":"sk"}`, true},
		{"minimaxcodecn filename", "minimaxcodecn-user1.json", "", `{"token":"at_123"}`, true},
		{"minimaxcodeglobal filename", "minimaxcodeglobal-user1.json", "", `{"token":"at_123","region":"global"}`, true},

		// 跨供应商误吞防护：通用结构必须落在**正确**的供应商头上。
		{"qoder pt token is qoder", "acc.json", "", `{"access_token":"pt-abc"}`, true},
		{"cline camel is cline", "acc.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		// 真正谁都不该认的：只含通用字段、没有专属特征。
		{"flat device token is nothing", "acc.json", "", `{"device_token":"dt-abc","account":{"uid":"u1"}}`, false},
		{"generic snake pair is nothing", "acc.json", "", `{"access_token":"at","refresh_token":"rt"}`, false},
		{"generic token is nothing", "acc.json", "", `{"token":"sk-abc"}`, false},
		{"generic access_token is nothing", "acc.json", "", `{"access_token":"at"}`, false},
		{"workbuddy provider hint overrides", "other.json", app.ProviderKey, `{"accessToken":"at"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: tc.provider,
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != tc.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, tc.wantHandle)
			}
		})
	}
}
