package core

// 本文件提供凭证 JSON 的合并骨架。
//
// 背景：六个供应商各自实现 core.StorageMerger（MergeStorageJSON），其中
// codearts / trae / minimaxcode / zcode 四份的骨架完全一样——
//
//	原文件为空或不是合法对象 → 整体替换为新凭证
//	否则 → 反序列化成 map，就地覆盖若干字段，再缩进序列化
//
// 只有「覆盖哪些字段、空值是否覆盖」各家不同。把骨架收口到这里，各家只保留
// 自己的字段清单；将来新增供应商不必再抄一遍空值/损坏分支。
//
// 不收口的两家（形态确实特殊，各自保留）：
//   - cline：键名要跟随原文件（下划线/驼峰），不能由骨架决定；
//   - workbuddy：深合并（mergeMaps）且补 type 兜底，不是「覆盖字段」。

import "encoding/json"

// MergeCredentialJSON 把 updated 的字段合并进 original，返回缩进后的 JSON。
//
// original 为空或无法反序列化成对象时，直接返回 fresh()。否则把 original
// 解成 map，交给 apply 覆盖字段，再序列化。
//
// apply 只负责写字段，不负责判空：「空值是否覆盖」是各家协议的一部分，
// 由供应商自己决定（access_token 通常无条件覆盖，refresh_token 通常仅在非空时覆盖）。
func MergeCredentialJSON(original []byte, fresh func() ([]byte, error), apply func(map[string]any)) ([]byte, error) {
	if len(original) == 0 {
		return fresh()
	}
	var target map[string]any
	if err := json.Unmarshal(original, &target); err != nil || target == nil {
		return fresh()
	}
	apply(target)
	merged, errMarshal := json.MarshalIndent(target, "", "  ")
	if errMarshal != nil {
		return fresh()
	}
	return merged, nil
}
