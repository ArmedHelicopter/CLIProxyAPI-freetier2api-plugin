package core

// 本文件提供供应商共用的随机串与 PKCE 助手。
//
// 这些逻辑原本在 codearts/qoder/zcode/opencodezen/minimaxcode/workbuddy
// 各有一份（签名还不一致）；统一到这里，随机源不可用时统一走确定性兜底。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var randomFallbackCounter int64

// RandomHex 生成 n 字节随机十六进制串（2n 个字符）。
//
// 随机源不可用时退回基于时间与计数的确定性串：这些 id 只用于会话/请求标识的
// 唯一性，退化的熵比让整条链路失败更可接受。
func RandomHex(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&randomFallbackCounter, 1))
	}
	return hex.EncodeToString(buf)
}

// NewPKCEPair 生成 PKCE 的 verifier 与 S256 challenge。
func NewPKCEPair() (verifier, challenge string, err error) {
	buffer := make([]byte, 32)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
