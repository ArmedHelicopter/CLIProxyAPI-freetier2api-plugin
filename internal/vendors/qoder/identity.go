package qoder

// 本文件解析活动平台要求的设备身份。
//
// 上游对 `cosy-machinetoken` + `cosy-machinetype` 做**精确校验**（2026-10-23
// 逐头隔离实测，见 identity_test.go 锁住的判据）：
//
//	改动 token 任意一个字符 / 截断一位 / 增补一位 → 可领取活动全部消失；
//	把 A 设备的 token 配上 B 设备的 type → 同样消失；
//	只有官方组件 runtime-info 签发的**原始配对**才会让活动出现。
//
// 这两个值由官方桌面端的 UMID 组件签发（88 字符 base64url，内含签名段），
// 插件无法伪造。此前的做法是按账号种子派生机器头——那不是"补上身份"，而是
// 主动把自己标记成非官方客户端：
//
//	国内版：不发机器头即可正常领取；一旦发了派生 token，可领活动被过滤。
//	国际版：不发机器头看不到活动；派生值同样看不到；容器里的官方组件
//	        （vmInfo.isVm=true）也看不到——活动只下发给登录过官方客户端的
//	        那台真机。
//
// 因此本文件只做一件事：把**用户自己提供的**真实身份读出来。identity 读取
// 顺序是「凭证字段 → 全局配置」，都没有就一个机器头都不发——发假的比不发
// 更糟。插件绝不自行生成、也不去读用户的 Qoder 安装目录。

import (
	"strings"

	"freetier2api-plugin/internal/core"
)

// MachineIdentity 是一次签到请求要带的设备身份。
type MachineIdentity struct {
	// Token / Type 是成对出现的机器身份，由官方组件签发，必须原样使用。
	Token string
	Type  string
	// Code 是 cosy-machinecode，可选（实测不影响活动可见性）。
	Code string
	// Version 是 cosy-version（桌面端版本）。
	Version string
}

// Native 报告身份是否可用（token 与 type 必须成对，缺一不可）。
func (m MachineIdentity) Native() bool { return m.Token != "" && m.Type != "" }

// DesktopVersion 是活动平台认的桌面端版本（cosy-version 头）。
const DesktopVersion = "0.4.8"

// ResolveIdentity 组装本次签到要用的设备身份。
//
// cred 提供账号自带的身份字段（凭证文件里手写的 machine_token / machine_type），
// raw 是插件全局配置（qoder_machine_token / qoder_machine_type / …）。
// 两者都没有时返回零值——调用方据此**不发**任何机器头。
func ResolveIdentity(cred *Credential, raw map[string]string) MachineIdentity {
	identity := MachineIdentity{
		Version: core.FirstNonEmpty(strings.TrimSpace(raw["qoder_desktop_version"]), DesktopVersion),
	}
	if cred != nil {
		identity.Token = strings.TrimSpace(cred.MachineToken)
		identity.Type = strings.TrimSpace(cred.MachineType)
		identity.Code = strings.TrimSpace(cred.MachineCode)
	}
	identity.Token = core.FirstNonEmpty(identity.Token, strings.TrimSpace(raw["qoder_machine_token"]))
	identity.Type = core.FirstNonEmpty(identity.Type, strings.TrimSpace(raw["qoder_machine_type"]))
	identity.Code = core.FirstNonEmpty(identity.Code, strings.TrimSpace(raw["qoder_machine_code"]))
	if !identity.Native() {
		// 只有一个头等于没有：上游要求成对，半套身份既解锁不了活动，还会
		// 让请求形态偏离官方客户端。清空以免调用方误以为「有身份」。
		return MachineIdentity{Version: identity.Version}
	}
	return identity
}
