package autoclaw

// 本文件实现 AutoClaw 凭证的刷新写回合并。
//
// 复用 core.MergeCredentialJSON 骨架：只覆盖 token 相关字段，
// 用户手工维护的 note / disabled 等字段原样保留。

import "freetier2api-plugin/internal/core"

// MergeCredentialJSON 把刷新后的令牌合并回原凭证 JSON。
func MergeCredentialJSON(original []byte, cred *Credential) ([]byte, error) {
	return core.MergeCredentialJSON(original, cred.StorageJSON, func(target map[string]any) {
		target["accessToken"] = cred.AccessTokenValue()
		target["refreshToken"] = cred.RefreshTokenValue()
		target["expiresAt"] = cred.ExpiresAtValue()
	})
}
