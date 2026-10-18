package core

// 本文件保留凭证解析的属性覆盖规则。
// 归属判定与解析入口在 Registry.ParseCredential。

import "strings"

// applyAttributeOverrides 用宿主传来的属性覆盖凭证字段。
//
// realm 允许被覆盖；token 类字段不允许——属性里不该有它们。
func applyAttributeOverrides(credential *Credential, attributes map[string]string) {
	if credential == nil || len(attributes) == 0 {
		return
	}
	if realm := strings.TrimSpace(attributes["realm"]); realm != "" {
		credential.SetRegion(realm)
	}
}
