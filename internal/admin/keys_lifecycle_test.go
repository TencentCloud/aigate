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

// 管理面密钥有效期单测：创建设置、更新设置/清除、非法值，以及续期清空过期观测。

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestKeyExpiry_CreateUpdateAndClear 覆盖 expires_at 的管理面生命周期。
func TestKeyExpiry_CreateUpdateAndClear(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	// 创建时设置有效期。
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, map[string]any{
		"name": "exp-key", "user_id": ownerID, "expires_at": future.Format(time.RFC3339),
	})
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[keyResp](t, raw)
	if created.ExpiresAt == nil {
		t.Fatal("create response should carry expires_at")
	}
	if created.ExpiredAt != nil {
		t.Fatalf("fresh key expired_at should be null, got %v", created.ExpiredAt)
	}

	// 非法格式 → 400。
	status, raw = doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, map[string]any{
		"name": "bad-exp-key", "user_id": ownerID, "expires_at": "not-a-timestamp",
	})
	assertStatus(t, status, http.StatusBadRequest, raw)

	path := "/admin/keys/" + itoa(created.ID)

	// 模拟「已过期观测」：直改库写入 expired_at。
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if _, err := store.UpdateAPIKey(context.Background(), created.ID, map[string]any{"expired_at": past}); err != nil {
		t.Fatalf("seed expired_at: %v", err)
	}

	// 续期（设置新的 expires_at）→ expired_at 清空（新的生命周期）。
	next := future.Add(24 * time.Hour).Format(time.RFC3339)
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{"expires_at": next})
	assertStatus(t, status, http.StatusOK, raw)
	renewed := mustDecode[keyResp](t, raw)
	if renewed.ExpiresAt == nil || renewed.ExpiredAt != nil {
		t.Fatalf("after renewal: expires_at=%v expired_at=%v（期望新有效期且观测清空）", renewed.ExpiresAt, renewed.ExpiredAt)
	}

	// 非法值 → 400。
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{"expires_at": "2026/10/01"})
	assertStatus(t, status, http.StatusBadRequest, raw)

	// 空串 → 400（清除必须显式 null，避免误清）。
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{"expires_at": ""})
	assertStatus(t, status, http.StatusBadRequest, raw)

	// 显式 null → 清除。
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{"expires_at": nil})
	assertStatus(t, status, http.StatusOK, raw)
	cleared := mustDecode[keyResp](t, raw)
	if cleared.ExpiresAt != nil || cleared.ExpiredAt != nil {
		t.Fatalf("after clear: expires_at=%v expired_at=%v（期望均为 null）", cleared.ExpiresAt, cleared.ExpiredAt)
	}

	// 不传 expires_at 的普通更新不影响已有有效期。
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{
		"name": "renamed", "expires_at": future.Format(time.RFC3339),
	})
	assertStatus(t, status, http.StatusOK, raw)
	renamed := mustDecode[keyResp](t, raw)
	status, raw = doRequest(t, srv, http.MethodPut, path, adminToken, map[string]any{"max_tokens": 64})
	assertStatus(t, status, http.StatusOK, raw)
	kept := mustDecode[keyResp](t, raw)
	if kept.ExpiresAt == nil || kept.Name != renamed.Name || kept.MaxTokens != 64 {
		t.Fatalf("unrelated update lost expires_at: %+v", kept)
	}
}

// TestKeyExpiry_ListAndUserResponse 覆盖列表与用户面响应中的只读字段。
func TestKeyExpiry_ListAndUserResponse(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	status, raw := doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, map[string]any{
		"name": "listed", "user_id": ownerID, "expires_at": future.Format(time.RFC3339),
	})
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[keyResp](t, raw)

	status, raw = doRequest(t, srv, http.MethodGet, "/admin/keys", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	list := mustDecode[keyListResp](t, raw)
	var found bool
	for _, k := range list.Keys {
		if k.ID == created.ID {
			found = true
			if k.ExpiresAt == nil {
				t.Fatalf("list response missing expires_at for key %d", k.ID)
			}
		}
	}
	if !found {
		t.Fatalf("created key %d not in list", created.ID)
	}
}
