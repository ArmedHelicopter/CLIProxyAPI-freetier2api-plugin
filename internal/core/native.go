package core

// NativeAdapter[T] 统一供应商适配器里重复的凭证生命周期：
// Parse / Native / Refresh / MergeStorageJSON。
//
// 供应商仍保留各自不同的 Match、模型清单、对话与额度实现；
// 这里只抽象“同一份代码在不同供应商里重复”的部分。

import (
	"context"
)

// CredentialSeed 是供应商解析出的归一化凭证字段。
type CredentialSeed struct {
	VendorID     string
	Region       string
	Label        string
	FileID       string
	UID          string
	Token        string
	RefreshToken string
	ExpiresAt    int64
	AuthMode     string
}

// NativeRefresh 是一次续期的结果。
type NativeRefresh[T any] struct {
	Native       T
	Token        string
	RefreshToken string
	ExpiresAt    int64
	Changed      bool
}

// NativeSpec 是 NativeAdapter 的钩子集合。
type NativeSpec[T any] struct {
	Meta    Meta
	Env     Env
	Extract func(*Credential) (T, bool)

	// ParseSeed 解析原生凭证并返回归一化字段。
	ParseSeed func(raw []byte, fileName string) (CredentialSeed, T, error)
	// RefreshNative 续期；nil 表示该供应商不需要续期。
	RefreshNative func(ctx context.Context, native T) (NativeRefresh[T], error)
	// MergeNative 把新令牌合并回原 JSON；nil 表示不写回。
	MergeNative func(original []byte, native T) ([]byte, error)
}

// NativeAdapter 是 core.Vendor 的凭证生命周期基类。
type NativeAdapter[T any] struct {
	Meta
	Env  Env
	spec NativeSpec[T]
}

// NewNativeAdapter 构造一个适配器基类。
func NewNativeAdapter[T any](spec NativeSpec[T]) *NativeAdapter[T] {
	return &NativeAdapter[T]{Meta: spec.Meta, Env: spec.Env, spec: spec}
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (a *NativeAdapter[T]) Parse(raw []byte, fileName string) (*Credential, error) {
	seed, native, errParse := a.spec.ParseSeed(raw, fileName)
	if errParse != nil {
		return nil, errParse
	}
	if seed.VendorID == "" {
		seed.VendorID = a.ID()
	}
	if seed.Region == "" {
		seed.Region = a.Region()
	}
	if seed.FileID == "" {
		seed.FileID = FileIDFromFileName(fileName)
	}
	return &Credential{
		VendorID:     seed.VendorID,
		Region:       seed.Region,
		Label:        seed.Label,
		FileID:       seed.FileID,
		UID:          seed.UID,
		Token:        seed.Token,
		RefreshToken: seed.RefreshToken,
		ExpiresAt:    seed.ExpiresAt,
		AuthMode:     seed.AuthMode,
		Native:       native,
	}, nil
}

// Native 取回供应商原生凭证对象。
func (a *NativeAdapter[T]) Native(cred *Credential) (T, error) {
	var zero T
	if a.spec.Extract == nil {
		return zero, NewPluginError("credential_missing", "vendor adapter has no native extractor", 401)
	}
	if cred == nil {
		return zero, NewPluginError("credential_missing", "credential is nil", 401)
	}
	native, okNative := a.spec.Extract(cred)
	if !okNative || any(native) == nil {
		return zero, NewPluginError("credential_missing", "credential was not parsed by the "+a.ID()+" vendor", 401)
	}
	return native, nil
}

// Refresh 校验/续期凭证；未提供钩子时视为无需刷新。
func (a *NativeAdapter[T]) Refresh(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if a.spec.RefreshNative == nil {
		return cred, false, nil
	}
	native, errNative := a.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	result, errRefresh := a.spec.RefreshNative(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if result.Changed {
		cred.ApplyTokenRefresh(result.Token, result.RefreshToken, result.ExpiresAt)
		cred.Native = result.Native
	}
	return cred, result.Changed, nil
}

// MergeStorageJSON 把续期后的令牌合并回原凭证 JSON。
func (a *NativeAdapter[T]) MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	if a.spec.MergeNative == nil {
		return original, nil
	}
	native, errNative := a.Native(cred)
	if errNative != nil {
		return original, errNative
	}
	return a.spec.MergeNative(original, native)
}
