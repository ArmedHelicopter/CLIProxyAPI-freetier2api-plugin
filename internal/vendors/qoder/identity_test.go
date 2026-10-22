package qoder

import (
	"strings"
	"testing"
)

// 本文件锁住设备身份的生成规则。
//
// 背景（issue #13）：国际版活动列表要求 cosy-machinetoken / cosy-machinetype
// **成对**出现，缺任一个可领取活动会被整条过滤；上游不校验这两个值是否
// 由官方组件生成，只看形态。

// TestResolveIdentityIsStablePerSeed 覆盖：同一账号每次得到同一份身份，
// 不同账号得到不同身份。
func TestResolveIdentityIsStablePerSeed(t *testing.T) {
	first := ResolveIdentity("uid-a", nil)
	second := ResolveIdentity("uid-a", nil)
	other := ResolveIdentity("uid-b", nil)
	if first != second {
		t.Errorf("同一 seed 应得到同一身份: %+v vs %+v", first, second)
	}
	if first.Token == other.Token || first.Type == other.Type || first.MachineID == other.MachineID {
		t.Error("不同 seed 应得到不同身份")
	}
	if !first.Native() {
		t.Error("派生身份应始终可用（token+type 成对）")
	}
}

// TestIdentityShapeMatchesOfficial 覆盖：形态与官方组件输出同形。
//
// token 曾只有 43 字符（SHA-512 直接截断），与官方 88 字符不同；形态不符
// 也是「活动不可见」的一个真实成因，因此这里锁住长度。
func TestIdentityShapeMatchesOfficial(t *testing.T) {
	identity := ResolveIdentity("uid-shape", nil)
	if len(identity.Token) != 88 {
		t.Errorf("token 长度 = %d, 期望 88", len(identity.Token))
	}
	if len(identity.Type) != 18 {
		t.Errorf("type 长度 = %d, 期望 18", len(identity.Type))
	}
	if len(identity.MachineID) != 32 {
		t.Errorf("machineid 长度 = %d, 期望 32", len(identity.MachineID))
	}
	if !strings.Contains(identity.MachineOS, "_") {
		t.Errorf("machineos = %q, 期望 {arch}_{platform}", identity.MachineOS)
	}
	if identity.Hostname == "" || identity.Version == "" {
		t.Errorf("hostname/version 不应为空: %+v", identity)
	}
}

// TestIdentityUsesGatewayFingerprintSeed 覆盖：签到用的身份与对话/额度链路
// 用同一个种子口径，避免同一账号在不同链路报出两套设备。
func TestIdentityUsesGatewayFingerprintSeed(t *testing.T) {
	const token = "dt-abc"
	seed := FingerprintSeed("", token)
	fromSeed := ResolveIdentity(seed, nil)
	// 重复计算一次，确保种子口径没有漂移。
	if again := ResolveIdentity(FingerprintSeed("", token), nil); again != fromSeed {
		t.Error("同一凭证的种子口径应稳定")
	}
	// 不同账号（不同凭证）必须得到不同身份——否则多账号会被报成同一台设备。
	if ResolveIdentity(FingerprintSeed("", "dt-other"), nil).Token == fromSeed.Token {
		t.Error("不同凭证不应得到相同身份")
	}
}

// TestIdentityDesktopVersionOverride 覆盖：cosy-version 可被配置覆盖。
func TestIdentityDesktopVersionOverride(t *testing.T) {
	identity := ResolveIdentity("uid", map[string]string{"qoder_desktop_version": "9.9.9"})
	if identity.Version != "9.9.9" {
		t.Errorf("version = %q, 期望 9.9.9", identity.Version)
	}
	if fallback := ResolveIdentity("uid", nil); fallback.Version != DesktopVersion {
		t.Errorf("未配置时 version = %q, 期望 %q", fallback.Version, DesktopVersion)
	}
}

// TestNativeRequiresTokenAndType 覆盖：身份可用的判据是两个头成对。
func TestNativeRequiresTokenAndType(t *testing.T) {
	cases := []struct {
		name     string
		identity MachineIdentity
		want     bool
	}{
		{"both", MachineIdentity{Token: "tok", Type: "ty"}, true},
		{"token only", MachineIdentity{Token: "tok"}, false},
		{"type only", MachineIdentity{Type: "ty"}, false},
		{"empty", MachineIdentity{}, false},
	}
	for _, testCase := range cases {
		if got := testCase.identity.Native(); got != testCase.want {
			t.Errorf("%s: Native() = %v, 期望 %v", testCase.name, got, testCase.want)
		}
	}
}
