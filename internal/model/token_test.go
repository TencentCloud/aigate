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

package model_test

import (
	"strings"
	"testing"

	"aigate/internal/model"
)

// GenerateToken 的前缀、长度（前缀 + 48 位 hex）、哈希一致性与唯一性。
func TestGenerateToken_FormatHashAndUniqueness(t *testing.T) {
	for _, prefix := range []string{model.TokenPrefixKey, model.TokenPrefixAccess} {
		t.Run(prefix, func(t *testing.T) {
			seen := make(map[string]struct{})
			const rounds = 50
			for i := 0; i < rounds; i++ {
				plain, hash, display, err := model.GenerateToken(prefix)
				if err != nil {
					t.Fatalf("round %d: generate token: %v", i, err)
				}
				if !strings.HasPrefix(plain, prefix) {
					t.Fatalf("round %d: plain %q missing prefix %q", i, plain, prefix)
				}
				if got, want := len(plain), len(prefix)+48; got != want {
					t.Fatalf("round %d: len(plain) = %d, want %d (plain=%q)", i, got, want, plain)
				}
				if len(hash) != 64 {
					t.Fatalf("round %d: len(hash) = %d, want 64", i, len(hash))
				}
				if want := model.HashToken(plain); hash != want {
					t.Fatalf("round %d: hash = %q, want HashToken(plain) = %q", i, hash, want)
				}
				if want := plain[:8] + "…" + plain[len(plain)-4:]; display != want {
					t.Fatalf("round %d: display = %q, want %q", i, display, want)
				}
				if _, dup := seen[hash]; dup {
					t.Fatalf("round %d: duplicate token hash %q", i, hash)
				}
				seen[hash] = struct{}{}
			}
		})
	}
}

// HashToken 稳定（同输入同输出）、区分大小写，且与已知 sha256 向量一致。
func TestHashToken_StableAndCaseSensitive(t *testing.T) {
	const raw = "sk-AbC123"

	if got, again := model.HashToken(raw), model.HashToken(raw); got != again {
		t.Fatalf("HashToken is not stable: %q vs %q", got, again)
	}
	if got := model.HashToken(raw); len(got) != 64 {
		t.Fatalf("len(HashToken(%q)) = %d, want 64", raw, len(got))
	}
	if model.HashToken(raw) == model.HashToken(strings.ToLower(raw)) {
		t.Fatalf("HashToken must be case-sensitive: %q collides with lower-case form", raw)
	}
	if model.HashToken("") == model.HashToken(" ") {
		t.Fatal("HashToken must distinguish empty string from a single space")
	}

	// sha256("abc") 的已知向量。
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := model.HashToken("abc"); got != want {
		t.Fatalf("HashToken(\"abc\") = %q, want %q", got, want)
	}
}
