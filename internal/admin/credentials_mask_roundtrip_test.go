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

package admin_test

// 覆盖「掩码回写防护」：GET 返回掩码后原样 PUT 回写，不得把真实密钥覆盖成掩码串。

import (
	"context"
	"net/http"
	"testing"

	"aigate/internal/model"
)

// TestCredentials_MaskedValueRoundTripDoesNotOverwrite 覆盖典型客户端行为：
// GET（拿到掩码）→ 原样 PUT（只改 name）→ 真实密钥必须保持不变；
// 显式给出新明文（非掩码）时正常替换。
func TestCredentials_MaskedValueRoundTripDoesNotOverwrite(t *testing.T) {
	srv, store, _, adminToken := credServer(t)

	created := createCredential(t, srv, adminToken, "round-trip", []map[string]any{
		headerSecret("Authorization", "sk-real-secret-123456"),
	}, nil)
	path := "/admin/credentials/" + itoa(created.ID)

	// 1) GET 拿到掩码，把它原样带回 PUT（只改 name）。
	status, raw := doRequest(t, srv, http.MethodGet, path, adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got := mustDecode[credentialResp](t, raw)
	if len(got.Secrets) != 1 {
		t.Fatalf("secrets = %+v", got.Secrets)
	}
	masked := got.Secrets[0].Value
	if masked == "sk-real-secret-123456" {
		t.Fatalf("GET must not echo plaintext")
	}

	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{
		"name": "round-trip-renamed",
		"secrets": []map[string]any{
			headerSecret("Authorization", masked),
		},
	})
	assertStatus(t, status, http.StatusOK, raw)

	// 直读存储层：真实密钥保持不变（未被掩码串覆盖）。
	c, err := store.GetCredentialByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	secrets, err := c.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Value != "sk-real-secret-123456" {
		t.Fatalf("masked round-trip overwrote the secret: %+v", secrets)
	}
	if c.Name != "round-trip-renamed" {
		t.Fatalf("name update lost: %q", c.Name)
	}

	// 2) 显式给出新明文时正常替换。
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{
		"secrets": []map[string]any{
			headerSecret("Authorization", "sk-brand-new-secret"),
		},
	})
	assertStatus(t, status, http.StatusOK, raw)
	c, err = store.GetCredentialByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID (after replace): %v", err)
	}
	secrets, err = c.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets (after replace): %v", err)
	}
	if len(secrets) != 1 || secrets[0].Value != "sk-brand-new-secret" {
		t.Fatalf("explicit replacement failed: %+v", secrets)
	}

	// 3) 掩码匹配是多字节安全的路径也要成立（沿用 model.MaskSecret 的口径）。
	if mask := model.MaskSecret("sk-brand-new-secret"); mask == "sk-brand-new-secret" {
		t.Fatal("sanity: MaskSecret should mask")
	}
}
