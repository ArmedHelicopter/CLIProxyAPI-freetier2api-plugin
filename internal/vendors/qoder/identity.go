package qoder

// 本文件生成活动平台要求的设备身份。
//
// 上游对国际版活动列表按设备身份过滤：请求头里没有成对的
// cosy-machinetoken / cosy-machinetype 时，HTTP 依然返回 200，但
// 「每日领取 100 Credits」这类 CLAIM_BENEFIT 活动会被整条过滤，只剩
// VIEW_DETAILS —— 看起来就像账号没有签到资格（issue #13）。
//
// 实测（2026-10-22，同一个国际号逐头隔离）：
//   - 两个头都不带 → 可领活动不可见；
//   - 只带 machinetoken（缺 type）→ 依旧不可见；
//   - token 与 type **成对**出现 → CLAIM_BENEFIT 可见，且 token 取任意
//     长度（≥20）的值都可见。
//
// 也就是说上游只要求「两个头成对存在且形态像客户端值」，并不校验它们
// 是否由官方二进制生成；签名链路（chat.go）本来就用同一套派生值。因此
// 身份完全在插件内按账号种子稳定派生，不读用户的任何文件、不执行外部
// 组件、不联网。

import (
	"os"
	"runtime"
	"strings"

	"freetier2api-plugin/internal/core"
)

// MachineIdentity 是一次签到请求要带的设备身份。
type MachineIdentity struct {
	// MachineID 是 cosy-machineid（上游不校验内容）。
	MachineID string
	// Token / Type 是成对出现的机器身份。
	Token string
	Type  string
	// MachineOS 形如 x86_64_win32。
	MachineOS string
	// Hostname 是主机名（规整为可放进请求头的 ASCII）。
	Hostname string
	// Version 是 cosy-version（桌面端版本）。
	Version string
}

// Native 报告身份是否可用（token 与 type 必须成对）。
func (m MachineIdentity) Native() bool { return m.Token != "" && m.Type != "" }

// DesktopVersion 是活动平台认的桌面端版本（cosy-version 头）。
//
// 上游对它的取值不敏感（实测填任意值活动列表一致），保留是为了与官方
// 桌面端请求同形，可通过 qoder_desktop_version 覆盖。
const DesktopVersion = "0.4.8"

// ResolveIdentity 按账号种子生成一份稳定的设备身份。
//
// seed 用账号指纹种子（见 FingerprintSeed）：同一账号每次得到同一份身份，
// 换账号则不同——与 chat.go 的 cosy 会话身份口径一致。
func ResolveIdentity(seed string, raw map[string]string) MachineIdentity {
	identity := MachineIdentity{
		MachineID: DeriveMachineID(seed),
		Token:     DeriveMachineToken(seed),
		Type:      DeriveMachineType(seed),
		MachineOS: machineOSHeader(),
		Hostname:  headerSafeHostname(),
		Version:   core.FirstNonEmpty(strings.TrimSpace(raw["qoder_desktop_version"]), DesktopVersion),
	}
	if identity.Hostname == "" {
		identity.Hostname = "DESKTOP-QODER"
	}
	return identity
}

// machineOSHeader 返回官方口径的 {arch}_{platform}。
func machineOSHeader() string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	platform := "linux"
	switch {
	case strings.HasPrefix(runtime.GOOS, "win"):
		platform = "win32"
	case runtime.GOOS == "darwin":
		platform = "darwin"
	}
	return arch + "_" + platform
}

// headerSafeHostname 把主机名规整成请求头可用的 ASCII；取不到时返回空。
func headerSafeHostname() string {
	name, errHostname := os.Hostname()
	if errHostname != nil {
		return ""
	}
	var builder strings.Builder
	for _, char := range name {
		if char > 0x20 && char < 0x7f {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}
