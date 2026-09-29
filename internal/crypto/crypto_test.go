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

package crypto_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"aigate/internal/crypto"
)

// 约定的测试主密钥：hexKey 为 64 位 hex；base64Key / rawBase64Key 为同一 32 字节的两条 base64 变体。
var (
	hexKey       = strings.Repeat("0123456789abcdef", 4)
	keyBytes     = []byte("0123456789abcdef0123456789abcdef") // 32 字节
	base64Key    = base64.StdEncoding.EncodeToString(keyBytes)
	rawBase64Key = base64.RawStdEncoding.EncodeToString(keyBytes)
)

// configureKey 配置主密钥并在测试结束时恢复为禁用态（避免包级状态串扰其它用例）。
func configureKey(t *testing.T, key string) {
	t.Helper()
	if err := crypto.Configure(key); err != nil {
		t.Fatalf("Configure(%q): %v", key, err)
	}
	t.Cleanup(func() { _ = crypto.Configure("") })
}

func TestConfigure_AcceptsHexAndBase64(t *testing.T) {
	for _, key := range []string{hexKey, base64Key, rawBase64Key, "  " + hexKey + "  "} {
		if err := crypto.Configure(key); err != nil {
			t.Fatalf("Configure(%q) should succeed, got %v", key, err)
		}
		if !crypto.Enabled() {
			t.Fatalf("Enabled = false after Configure(%q), want true", key)
		}
		_ = crypto.Configure("")
	}
}

func TestConfigure_EmptyDisables(t *testing.T) {
	configureKey(t, hexKey)
	if err := crypto.Configure(""); err != nil {
		t.Fatalf("Configure(\"\"): %v", err)
	}
	if crypto.Enabled() {
		t.Fatal("Enabled = true after Configure(\"\"), want false")
	}
	// 空白串同样视为未配置。
	if err := crypto.Configure("   "); err != nil {
		t.Fatalf("Configure(\"   \"): %v", err)
	}
	if crypto.Enabled() {
		t.Fatal("Enabled = true after Configure(whitespace), want false")
	}
}

func TestConfigure_RejectsInvalidKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "too-short", key: "abc"},
		{name: "not-hex-nor-base64", key: "not-a-valid-master-key!!"},
		{name: "hex-wrong-length", key: strings.Repeat("ab", 16)},
		{name: "base64-wrong-length", key: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))},
		{name: "base64-48-bytes", key: strings.Repeat("z", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := crypto.Configure(tt.key); err == nil {
				t.Fatalf("Configure(%q) should fail, got success", tt.key)
			}
			if crypto.Enabled() {
				t.Fatal("invalid master key must not enable encryption")
			}
		})
	}
}

// 非法主密钥不得破坏已生效的配置。
func TestConfigure_InvalidKeepsPreviousKey(t *testing.T) {
	configureKey(t, hexKey)
	sealed, err := crypto.Seal("sk-keep-me")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := crypto.Configure("bad-key"); err == nil {
		t.Fatal("Configure(bad-key) should fail")
	}
	if !crypto.Enabled() {
		t.Fatal("Enabled = false after invalid master key, want true")
	}
	got, err := crypto.Open(sealed)
	if err != nil || got != "sk-keep-me" {
		t.Fatalf("Open after failed Configure = (%q, %v), want plaintext", got, err)
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	plaintexts := []string{"sk-upstream-secret", "", "中文密钥-值", strings.Repeat("x", 4096)}
	for _, key := range []string{"", hexKey, base64Key, rawBase64Key} {
		if err := crypto.Configure(key); err != nil {
			t.Fatalf("Configure(%q): %v", key, err)
		}
		for _, plain := range plaintexts {
			sealed, err := crypto.Seal(plain)
			if err != nil {
				t.Fatalf("key=%q Seal(%q): %v", key, plain, err)
			}
			if key == "" && sealed != plain {
				t.Fatalf("Seal(%q) with encryption disabled = %q, want passthrough", plain, sealed)
			}
			got, err := crypto.Open(sealed)
			if err != nil {
				t.Fatalf("key=%q Open(%q): %v", key, sealed, err)
			}
			if got != plain {
				t.Fatalf("key=%q round trip = %q, want %q", key, got, plain)
			}
		}
	}
	_ = crypto.Configure("")
}

func TestSeal_EmitsPrefixAndRandomNonce(t *testing.T) {
	configureKey(t, hexKey)
	first, err := crypto.Seal("sk-same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := crypto.Seal("sk-same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(first, crypto.CiphertextPrefix) {
		t.Fatalf("ciphertext missing prefix: %q", first)
	}
	if first == second {
		t.Fatal("same plaintext encrypted twice gives equal results, want random nonce")
	}
	if strings.Contains(first, "sk-same") {
		t.Fatalf("plaintext found in ciphertext: %q", first)
	}
}

func TestIsSealed(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "sealed", in: crypto.CiphertextPrefix + "AAAA", want: true},
		{name: "plain", in: "sk-plaintext", want: false},
		{name: "empty", in: "", want: false},
		{name: "prefix-without-payload", in: crypto.CiphertextPrefix, want: true},
		{name: "other-version", in: "enc:v2:AAAA", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := crypto.IsSealed(tt.in); got != tt.want {
				t.Fatalf("IsSealed(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestOpen_PlaintextPassthrough(t *testing.T) {
	_ = crypto.Configure("")
	if got, err := crypto.Open("sk-plain"); err != nil || got != "sk-plain" {
		t.Fatalf("Open(plaintext) with encryption disabled = (%q, %v)", got, err)
	}
	configureKey(t, hexKey)
	if got, err := crypto.Open("sk-plain"); err != nil || got != "sk-plain" {
		t.Fatalf("Open(plaintext) with encryption enabled = (%q, %v)", got, err)
	}
}

func TestOpen_SealedWithoutMasterKeyFails(t *testing.T) {
	configureKey(t, hexKey)
	sealed, err := crypto.Seal("sk-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := crypto.Configure(""); err != nil {
		t.Fatalf("Configure(\"\"): %v", err)
	}
	if _, err := crypto.Open(sealed); err == nil {
		t.Fatal("Open(ciphertext) with encryption disabled should fail")
	}
}

func TestOpen_CorruptedCiphertextFails(t *testing.T) {
	configureKey(t, hexKey)
	sealed, err := crypto.Seal("sk-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	payload := strings.TrimPrefix(sealed, crypto.CiphertextPrefix)
	// 首字符替换为另一个合法 base64 字符，保证与原文不同（命中 GCM 认证失败而非解码失败）。
	flipped := "A"
	if payload[0] == 'A' {
		flipped = "B"
	}

	tests := []struct {
		name string
		in   string
	}{
		{name: "not-base64", in: crypto.CiphertextPrefix + "!!!not-base64!!!"},
		{name: "empty-payload", in: crypto.CiphertextPrefix},
		{name: "truncated-nonce", in: crypto.CiphertextPrefix + payload[:8]},
		{name: "tampered", in: crypto.CiphertextPrefix + flipped + payload[1:]},
		{name: "truncated-ciphertext", in: crypto.CiphertextPrefix + payload[:len(payload)-6]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := crypto.Open(tt.in); err == nil {
				t.Fatalf("Open(%q) should fail, got success", tt.in)
			}
		})
	}
}

func TestOpen_WrongMasterKeyFails(t *testing.T) {
	configureKey(t, hexKey)
	sealed, err := crypto.Seal("sk-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	otherKey := strings.Repeat("fedcba9876543210", 4)
	if err := crypto.Configure(otherKey); err != nil {
		t.Fatalf("Configure(other): %v", err)
	}
	if _, err := crypto.Open(sealed); err == nil {
		t.Fatal("Open after master key change should fail")
	}
}
