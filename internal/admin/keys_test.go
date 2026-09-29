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

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"aigate/internal/model"
)

// TestCreateKey_OneTimePlaintextAndListMasked 覆盖创建返回一次性明文、列表/详情不回显明文。
func TestCreateKey_OneTimePlaintextAndListMasked(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelID := createModel(t, srv, adminToken, "gpt-key", model.ModelTypeOpenAI)

	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary, "sort_order": 0},
	})
	if !strings.HasPrefix(created.Token, model.TokenPrefixKey) {
		t.Fatalf("create response token should start with %q, got %q", model.TokenPrefixKey, created.Token)
	}
	if len(created.Token) <= len(model.TokenPrefixKey) {
		t.Fatalf("create response token too short: %q", created.Token)
	}
	if created.ProxyTokenPrefix == "" {
		t.Fatal("create response missing proxy_token_prefix")
	}
	if created.UserID != ownerID {
		t.Errorf("user_id want %d, got %d", ownerID, created.UserID)
	}
	if !created.Enabled {
		t.Error("new key should be enabled by default")
	}
	if len(created.Bindings) != 1 {
		t.Fatalf("binding view want 1 entry, got %d", len(created.Bindings))
	}
	if b := created.Bindings[0]; b.AIModelID != modelID || b.ModelID != "gpt-key" || b.Provider != "mock" ||
		b.Role != model.ModelRolePrimary {
		t.Errorf("binding view content mismatch: %+v", b)
	}

	t.Run("list contains no plaintext", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/keys", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if bytes.Contains(body, []byte(created.Token)) {
			t.Fatalf("key list echoed plaintext, body: %s", body)
		}
		if !bytes.Contains(body, []byte(created.ProxyTokenPrefix)) {
			t.Errorf("key list should contain proxy_token_prefix %q, body: %s", created.ProxyTokenPrefix, body)
		}
		list := mustDecode[keyListResp](t, body)
		if len(list.Keys) != 1 {
			t.Fatalf("key list want 1 entry, got %d", len(list.Keys))
		}
		if list.Keys[0].Token != "" {
			t.Errorf("list item should not contain one-time plaintext, got %q", list.Keys[0].Token)
		}
	})

	t.Run("detail contains no plaintext but has bindings", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/keys/"+itoa(created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if bytes.Contains(body, []byte(created.Token)) {
			t.Fatalf("key detail echoed plaintext, body: %s", body)
		}
		got := mustDecode[keyResp](t, body)
		if got.Token != "" {
			t.Errorf("detail should not contain one-time plaintext, got %q", got.Token)
		}
		if len(got.Bindings) != 1 || got.Bindings[0].ModelName != "gpt-key" {
			t.Errorf("detail binding view mismatch: %+v", got.Bindings)
		}
	})

	t.Run("unknown id 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/keys/999999", adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestCreateKey_Validation 覆盖 owner 存在性与绑定校验（错误交 model 层、以 400 返回）。
func TestCreateKey_Validation(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelA := createModel(t, srv, adminToken, "gpt-a", model.ModelTypeOpenAI)
	modelB := createModel(t, srv, adminToken, "gpt-b", model.ModelTypeOpenAI)

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "owner does not exist",
			body: map[string]any{"name": "k", "user_id": 999999},
		},
		{
			name: "two primary bindings",
			body: map[string]any{
				"name":    "k",
				"user_id": ownerID,
				"bindings": []map[string]any{
					{"ai_model_id": modelA, "role": model.ModelRolePrimary},
					{"ai_model_id": modelB, "role": model.ModelRolePrimary},
				},
			},
		},
		{
			name: "binding to a missing model",
			body: map[string]any{
				"name":    "k",
				"user_id": ownerID,
				"bindings": []map[string]any{
					{"ai_model_id": 999999, "role": model.ModelRolePrimary},
				},
			},
		},
		{
			name: "invalid binding role",
			body: map[string]any{
				"name":    "k",
				"user_id": ownerID,
				"bindings": []map[string]any{
					{"ai_model_id": modelA, "role": "secondary"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, tc.body)
			assertStatus(t, status, http.StatusBadRequest, body)
			if eb := decodeError(t, body); eb.Error.Message == "" {
				t.Errorf("error response missing message, body: %s", body)
			}
		})
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/keys", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if list := mustDecode[keyListResp](t, body); len(list.Keys) != 0 {
		t.Errorf("validation failure should not persist, got %d entries", len(list.Keys))
	}
}

// TestUpdateKey_PartialAndBindings 覆盖局部更新与绑定整体替换；非法绑定不产生半更新。
func TestUpdateKey_PartialAndBindings(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelA := createModel(t, srv, adminToken, "gpt-a", model.ModelTypeOpenAI)
	modelB := createModel(t, srv, adminToken, "gpt-b", model.ModelTypeOpenAI)

	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelA, "role": model.ModelRolePrimary, "sort_order": 0},
	})

	t.Run("scalar-only update keeps bindings unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), adminToken, map[string]any{
			"name":       "renamed-key",
			"max_tokens": 2048,
			"enabled":    false,
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[keyResp](t, body)
		if got.Name != "renamed-key" || got.MaxTokens != 2048 || got.Enabled {
			t.Errorf("scalar field update mismatch: %+v", got)
		}
		if got.UserID != ownerID {
			t.Errorf("user_id should not change, want %d, got %d", ownerID, got.UserID)
		}
		if len(got.Bindings) != 1 || got.Bindings[0].AIModelID != modelA {
			t.Errorf("bindings should not change when not provided, got %+v", got.Bindings)
		}
	})

	t.Run("bindings replaced as a whole", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), adminToken, map[string]any{
			"bindings": []map[string]any{
				{"ai_model_id": modelA, "role": model.ModelRolePrimary, "sort_order": 0},
				{"ai_model_id": modelB, "role": model.ModelRoleFallback, "sort_order": 1},
			},
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[keyResp](t, body)
		if len(got.Bindings) != 2 {
			t.Fatalf("bindings want 2 entries, got %+v", got.Bindings)
		}
		if got.Bindings[1].AIModelID != modelB || got.Bindings[1].Role != model.ModelRoleFallback {
			t.Errorf("fallback binding mismatch: %+v", got.Bindings[1])
		}
	})

	t.Run("invalid bindings 400 and original bindings unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), adminToken, map[string]any{
			"bindings": []map[string]any{
				{"ai_model_id": modelA, "role": model.ModelRolePrimary},
				{"ai_model_id": modelB, "role": model.ModelRolePrimary},
			},
		})
		assertStatus(t, status, http.StatusBadRequest, body)

		status, body = doRequest(t, srv, http.MethodGet, "/admin/keys/"+itoa(created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[keyResp](t, body); len(got.Bindings) != 2 {
			t.Errorf("bindings after invalid replacement should stay 2 entries, got %+v", got.Bindings)
		}
	})

	t.Run("unknown id 404", func(t *testing.T) {
		status, body := doRequest(
			t,
			srv,
			http.MethodPut,
			"/admin/keys/999999",
			adminToken,
			map[string]any{"enabled": true},
		)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestRotateKey_InvalidatesOldToken 覆盖 rotate 返回新明文、前缀更新、旧 token 立即失效。
func TestRotateKey_InvalidatesOldToken(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelID := createModel(t, srv, adminToken, "gpt-rotate", model.ModelTypeOpenAI)

	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary},
	})

	status, body := doRequest(t, srv, http.MethodPost, "/admin/keys/"+itoa(created.ID)+"/rotate", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	rotated := mustDecode[keyResp](t, body)

	if !strings.HasPrefix(rotated.Token, model.TokenPrefixKey) {
		t.Fatalf("rotated token should start with %q, got %q", model.TokenPrefixKey, rotated.Token)
	}
	if rotated.Token == created.Token {
		t.Fatal("plaintext after rotate should differ from old plaintext")
	}
	if rotated.ProxyTokenPrefix == created.ProxyTokenPrefix {
		t.Errorf("proxy_token_prefix should change after rotate, still %q", rotated.ProxyTokenPrefix)
	}

	// 旧 token 立即失效，新 token 可解析到同一密钥。
	if _, err := store.GetAPIKeyByTokenHash(context.Background(), model.HashToken(created.Token)); !model.IsNotFound(
		err,
	) {
		t.Errorf("old token should be invalidated (want not found), got err=%v", err)
	}
	key, err := store.GetAPIKeyByTokenHash(context.Background(), model.HashToken(rotated.Token))
	if err != nil {
		t.Fatalf("new token should resolve, got err=%v", err)
	}
	if key.ID != created.ID {
		t.Errorf("new token should resolve to key %d, got %d", created.ID, key.ID)
	}
}

// TestDeleteKey 覆盖删除返回 ok、随后查询 404、重复删除 404。
func TestDeleteKey(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelID := createModel(t, srv, adminToken, "gpt-del", model.ModelTypeOpenAI)
	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary},
	})

	status, body := doRequest(t, srv, http.MethodDelete, "/admin/keys/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if !mustDecode[okResp](t, body).OK {
		t.Errorf("delete response want ok=true, body: %s", body)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/keys/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)

	status, body = doRequest(t, srv, http.MethodDelete, "/admin/keys/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}
