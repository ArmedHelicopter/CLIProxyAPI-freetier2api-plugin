package core

// 本文件提供凭证解析的统一入口（根层 parseVendorCredential 的中立版）。
//
// 归属判定、解析、账号身份兜底、宿主属性覆盖四步必须按固定顺序执行：
// 任何一步散落在调用方，都会让「同一账号在不同路径上算出不同身份」。

import (
	"encoding/json"
	"strings"
)

// ParseCredential 按归属解析一份凭证；解析失败返回 false。
//
// fileNameHint 只作为归属线索（宿主侧的 ID 就是去掉 .json 的文件名），
// 因此这里允许传入不带后缀的名字。
func ParseCredential(raw []byte, fileNameHint string, attributes map[string]string) (*Credential, bool) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false
	}
	fileName := strings.TrimSpace(fileNameHint)
	if fileName != "" && !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	vendor, okVendor := ResolveVendor(fileName, attributes["provider"], payload)
	if !okVendor {
		return nil, false
	}
	credential, errParse := vendor.Parse(raw, fileName)
	if errParse != nil {
		return nil, false
	}
	// 账号身份由这里兜底：供应商 Parse 可以设置它，但不设置也不会失去身份。
	// 漏填的症状是「签到后仍显示未签到」这类极难定位的错位，不能交给各家自觉。
	if credential.FileIDValue() == "" {
		credential.SetFileID(strings.TrimSuffix(fileName, ".json"))
	}
	applyAttributeOverrides(credential, attributes)
	return credential, true
}

// applyAttributeOverrides 用宿主传来的属性覆盖凭证字段。
//
// realm 允许被覆盖（账号可以自带域声明）；token 类字段不允许——
// 属性里根本不该有它们，若出现也应忽略而不是采信。
func applyAttributeOverrides(credential *Credential, attributes map[string]string) {
	if credential == nil || len(attributes) == 0 {
		return
	}
	if realm := strings.TrimSpace(attributes["realm"]); realm != "" {
		credential.SetRegion(realm)
	}
}
