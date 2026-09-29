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

package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Token 前缀：
//   - sk- 数据面密钥（OpenAI 生态惯例，客户端 SDK 直接使用）
//   - gk- 用户/管理员访问 token（管理面与用户面 Bearer）
const (
	TokenPrefixKey    = "sk-"
	TokenPrefixAccess = "gk-"
)

// GenerateToken 生成一个新的访问凭证：
// 返回一次性明文、入库哈希（sha256 hex）与展示用前缀（形如 sk-abcd1234…wxyz）。
func GenerateToken(prefix string) (plain, hash, displayPrefix string, err error) {
	raw := make([]byte, 24) // 24 字节 = 48 个十六进制字符
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("generate token: %w", err)
	}
	plain = prefix + hex.EncodeToString(raw)
	return plain, HashToken(plain), DisplayTokenPrefix(plain), nil
}

// HashToken 计算凭证的入库哈希。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// DisplayTokenPrefix 生成展示用前缀：首 8 位 + 省略号 + 末 4 位。
// 过短的输入（≤16 位）退化为「… + 末 4 位」：避免短 token 因掩码过窄而泄漏大部分明文
// （如 16 位 token 按旧口径会回显 12 位）。
func DisplayTokenPrefix(plain string) string {
	if len(plain) <= 16 {
		if len(plain) <= 4 {
			return "…"
		}
		return "…" + plain[len(plain)-4:]
	}
	return plain[:8] + "…" + plain[len(plain)-4:]
}
