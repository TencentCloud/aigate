//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

// Package crypto 提供密钥类字段的对称加密（AES-256-GCM）。
//
// 密文格式：`enc:v1:<base64(nonce||ciphertext)>`，前缀便于识别与一次性重加密迁移的幂等判断。
// 主密钥经 Configure 注入（进程级唯一）：空串表示禁用加密，此时 Seal 原样返回明文、
// Open 只接受明文；非空主密钥必须为 64 位 hex 或 base64 且解码后 32 字节，否则返回错误。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// CiphertextPrefix 是密文标识前缀：以此开头的字符串按密文处理，其余按明文处理。
const CiphertextPrefix = "enc:v1:"

const (
	keySize   = 32 // AES-256 主密钥长度
	nonceSize = 12 // GCM 标准 nonce 长度
)

var (
	mu   sync.RWMutex
	aead cipher.AEAD
)

// Configure 配置主密钥。
//
// masterKey 为空串表示禁用加密（后续 Seal 原样返回）；非空时必须是 64 位 hex
// 或 base64 且解码后 32 字节，否则返回错误并保持原有配置不变。
func Configure(masterKey string) error {
	key := strings.TrimSpace(masterKey)
	if key == "" {
		mu.Lock()
		aead = nil
		mu.Unlock()
		return nil
	}
	raw, err := decodeMasterKey(key)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return fmt.Errorf("init aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("init aes-gcm: %w", err)
	}
	mu.Lock()
	aead = gcm
	mu.Unlock()
	return nil
}

// Enabled 报告是否已配置主密钥：true 表示 Seal 产出密文且 Open 可解密密文。
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return aead != nil
}

// IsSealed 判断字符串是否为加密后的密文（以 enc:v1: 开头）。
func IsSealed(s string) bool {
	return strings.HasPrefix(s, CiphertextPrefix)
}

// Seal 加密明文：未配置主密钥时原样返回；否则输出 `enc:v1:` + base64(nonce||ciphertext)。
// 每次加密使用随机 nonce，同一明文两次加密的密文不同。
func Seal(plain string) (string, error) {
	mu.RLock()
	gcm := aead
	mu.RUnlock()
	if gcm == nil {
		return plain, nil
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return CiphertextPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Open 解密：非密文（无 enc:v1: 前缀）视为明文直接返回；
// 未配置主密钥时遇到密文、或密文损坏/密钥不匹配时返回错误。
func Open(stored string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}
	mu.RLock()
	gcm := aead
	mu.RUnlock()
	if gcm == nil {
		return "", errors.New("cannot decrypt sealed value: master key is not configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, CiphertextPrefix))
	if err != nil {
		return "", fmt.Errorf("decode sealed value: %w", err)
	}
	if len(raw) < nonceSize {
		return "", errors.New("sealed value is corrupted: payload shorter than nonce")
	}
	plain, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt sealed value: %w", err)
	}
	return string(plain), nil
}

// decodeMasterKey 解析主密钥：优先按 64 位 hex，其次按标准/base64url（含无填充）变体。
func decodeMasterKey(key string) ([]byte, error) {
	if len(key) == hex.EncodedLen(keySize) {
		if raw, err := hex.DecodeString(key); err == nil {
			return raw, nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		raw, err := enc.DecodeString(key)
		if err != nil {
			continue
		}
		if len(raw) != keySize {
			return nil, fmt.Errorf("invalid master key: decoded length %d, want %d bytes", len(raw), keySize)
		}
		return raw, nil
	}
	return nil, errors.New("invalid master key: must be 64 hex characters or base64-encoded 32 bytes")
}
