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

package userapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 响应结构（仅本文件使用）
// ---------------------------------------------------------------------------

type idResp struct {
	ID uint `json:"id"`
}

type keyBindingResp struct {
	AIModelID uint   `json:"ai_model_id"`
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
	Role      string `json:"role"`
	SortOrder int    `json:"sort_order"`
}

type userKeyResp struct {
	ID               uint             `json:"id"`
	Name             string           `json:"name"`
	UserID           uint             `json:"user_id"`
	GroupID          uint             `json:"group_id"`
	ProxyTokenPrefix string           `json:"proxy_token_prefix"`
	MaxTokens        int              `json:"max_tokens"`
	Enabled          bool             `json:"enabled"`
	Bindings         []keyBindingResp `json:"bindings"`
	Token            string           `json:"token"`
}

type userKeyListResp struct {
	Keys []userKeyResp `json:"keys"`
}

type okBodyResp struct {
	OK bool `json:"ok"`
}

type errBodyResp struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// seedAIModelViaAPI 经管理面创建模型并返回行 ID。
func seedAIModelViaAPI(t *testing.T, srv *httptest.Server, adminToken, modelID string) uint {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   modelID,
		"model_name": modelID,
		"url":        "https://upstream.example/v1",
		"api_key":    "sk-upstream",
		"model_type": model.ModelTypeOpenAI,
	})
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[idResp](t, body).ID
}

// seedAdminKeyForUser 经管理面为该用户建一把绑定指定模型的密钥（绕过自助口径，用于准备「已获批模型」）。
func seedAdminKeyForUser(t *testing.T, srv *httptest.Server, adminToken string, ownerID, modelID uint) userKeyResp {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, map[string]any{
		"name":     "seed-key",
		"user_id":  ownerID,
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": model.ModelRolePrimary}},
	})
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[userKeyResp](t, body)
}

// createSelfKey 以用户身份创建一把绑定指定模型的密钥。
func createSelfKey(t *testing.T, srv *httptest.Server, userToken string, modelIDs ...uint) (int, []byte) {
	t.Helper()
	bindings := make([]map[string]any, 0, len(modelIDs))
	for i, id := range modelIDs {
		role := model.ModelRoleFallback
		if i == 0 {
			role = model.ModelRolePrimary
		}
		bindings = append(bindings, map[string]any{"ai_model_id": id, "role": role, "sort_order": i})
	}
	return doRequest(t, srv, http.MethodPost, "/api/user/keys", userToken, map[string]any{
		"name":     "self-key",
		"bindings": bindings,
	})
}

// setUserKeyLimit 经管理面设置 user_key_limit。
func setUserKeyLimit(t *testing.T, srv *httptest.Server, adminToken string, limit int) {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
		"user_key_limit": limit,
	})
	assertStatus(t, status, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestUserKeys_CreateAndList 覆盖创建成功（模型取自既有密钥绑定）、响应形状、一次性明文与列表不回显明文。
func TestUserKeys_CreateAndList(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid, userToken := seedUser(t, store, "u1", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	seedAdminKeyForUser(t, srv, adminToken, uid, m1)

	status, body := doRequest(t, srv, http.MethodPost, "/api/user/keys", userToken, map[string]any{
		"name":       "my-key",
		"max_tokens": 128,
		"bindings":   []map[string]any{{"ai_model_id": m1, "role": model.ModelRolePrimary}},
	})
	assertStatus(t, status, http.StatusCreated, body)
	created := mustDecode[userKeyResp](t, body)

	if !strings.HasPrefix(created.Token, model.TokenPrefixKey) {
		t.Fatalf("created token should start with %q, got %q", model.TokenPrefixKey, created.Token)
	}
	if created.ProxyTokenPrefix == "" {
		t.Fatalf("created response missing proxy_token_prefix, response: %s", body)
	}
	if created.UserID != uid {
		t.Errorf("user_id: want %d, got %d", uid, created.UserID)
	}
	if created.Name != "my-key" || created.MaxTokens != 128 || !created.Enabled {
		t.Errorf("created response fields mismatch: %+v", created)
	}
	if len(created.Bindings) != 1 || created.Bindings[0].AIModelID != m1 ||
		created.Bindings[0].Role != model.ModelRolePrimary {
		t.Errorf("binding view mismatch: %+v", created.Bindings)
	}

	t.Run("list contains only own keys and no plaintext", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/keys", userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if bytes.Contains(body, []byte(created.Token)) {
			t.Fatalf("list echoed plaintext token, response: %s", body)
		}
		list := mustDecode[userKeyListResp](t, body)
		if len(list.Keys) != 2 {
			t.Fatalf("list: want 2 (seeded + self-service), got %d", len(list.Keys))
		}
		for i := range list.Keys {
			if list.Keys[i].UserID != uid {
				t.Errorf("list[%d] contains another owner's key: %+v", i, list.Keys[i])
			}
			if list.Keys[i].Token != "" {
				t.Errorf("list[%d] should not contain plaintext token", i)
			}
			if len(list.Keys[i].Bindings) != 1 {
				t.Errorf("list[%d] should contain binding view: %+v", i, list.Keys[i].Bindings)
			}
		}
	})

	t.Run("detail contains bindings without plaintext", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/keys/"+itoa(created.ID), userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[userKeyResp](t, body)
		if got.Token != "" {
			t.Errorf("detail should not contain plaintext token, got %q", got.Token)
		}
		if got.ID != created.ID || len(got.Bindings) != 1 || got.Bindings[0].AIModelID != m1 {
			t.Errorf("detail mismatch: %+v", got)
		}
	})
}

// TestUserKeys_ModelScopeRejections 覆盖模型范围收敛：无任何获批模型 → 400；引用集合外模型 → 400。
func TestUserKeys_ModelScopeRejections(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid, userToken := seedUser(t, store, "u1", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	m2 := seedAIModelViaAPI(t, srv, adminToken, "uk-m2")

	t.Run("no approved models", func(t *testing.T) {
		status, body := createSelfKey(t, srv, userToken, m1)
		assertStatus(t, status, http.StatusBadRequest, body)
		if msg := mustDecode[errBodyResp](t, body).Error.Message; !strings.Contains(msg, "no available models") {
			t.Errorf("400 message should tell to contact an admin, got %q", msg)
		}
	})

	t.Run("reference model outside allowed set", func(t *testing.T) {
		seedAdminKeyForUser(t, srv, adminToken, uid, m1) // 获批集合 = {m1}

		status, body := createSelfKey(t, srv, userToken, m2)
		assertStatus(t, status, http.StatusBadRequest, body)
		if msg := mustDecode[errBodyResp](t, body).Error.Message; !strings.Contains(msg, "model not available") {
			t.Errorf("400 message should say model unavailable, got %q", msg)
		}

		// 获批模型仍可创建；失败的那次未落库。
		status, body = createSelfKey(t, srv, userToken, m1)
		assertStatus(t, status, http.StatusCreated, body)
	})
}

// TestUserKeys_KeyLimit 覆盖 settings.user_key_limit：默认放行、超限 400、0 = 不限。
func TestUserKeys_KeyLimit(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid, userToken := seedUser(t, store, "u1", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	seedAdminKeyForUser(t, srv, adminToken, uid, m1) // 已有 1 把

	// 默认上限 5：放行（1 → 2）。
	status, body := createSelfKey(t, srv, userToken, m1)
	assertStatus(t, status, http.StatusCreated, body)

	// 上限 1：已达上限（2 >= 1）→ 400 且提示上限值。
	setUserKeyLimit(t, srv, adminToken, 1)
	status, body = createSelfKey(t, srv, userToken, m1)
	assertStatus(t, status, http.StatusBadRequest, body)
	if msg := mustDecode[errBodyResp](t, body).Error.Message; !strings.Contains(msg, "key limit reached (limit 1)") {
		t.Errorf("400 message should mention the limit, got %q", msg)
	}

	// 上限 0：不限，放行。
	setUserKeyLimit(t, srv, adminToken, 0)
	status, body = createSelfKey(t, srv, userToken, m1)
	assertStatus(t, status, http.StatusCreated, body)

	// 上限对 admin 代创建（/admin/keys）不生效。
	setUserKeyLimit(t, srv, adminToken, 1)
	seedAdminKeyForUser(t, srv, adminToken, uid, m1)
}

// TestUserKeys_CrossOwnerIsolation 覆盖行级隔离：越权访问他人密钥一律 404 且不产生修改。
func TestUserKeys_CrossOwnerIsolation(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid1, token1 := seedUser(t, store, "u1", model.RoleUser, 0, true)
	uid2, token2 := seedUser(t, store, "u2", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	k1 := seedAdminKeyForUser(t, srv, adminToken, uid1, m1)

	path := "/api/user/keys/" + itoa(k1.ID)
	requests := []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"name": "hacked"}},
		{http.MethodDelete, nil},
		{http.MethodPost, nil}, // rotate 见下（路径不同）
	}
	for _, req := range requests {
		p := path
		if req.method == http.MethodPost {
			p = path + "/rotate"
		}
		status, body := doRequest(t, srv, req.method, p, token2, req.body)
		if status != http.StatusNotFound {
			t.Errorf("%s %s cross-owner access: want 404, got %d, response: %s", req.method, p, status, body)
		}
	}

	// 即便显式传他人 user_id（user 角色被忽略）仍 404。
	status, body := doRequest(t, srv, http.MethodGet, path+"?user_id="+itoa(uid1), token2, nil)
	if status != http.StatusNotFound {
		t.Errorf("user role passing another user_id: want 404, got %d, response: %s", status, body)
	}

	// 属主本人仍可访问，且未被越权请求改动。
	status, body = doRequest(t, srv, http.MethodGet, path, token1, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[userKeyResp](t, body)
	if got.Name != "seed-key" {
		t.Errorf("key mutated by cross-owner request, name = %q", got.Name)
	}

	// 列表按属主隔离。
	for _, tc := range []struct {
		token string
		owner uint
		want  int
	}{{token1, uid1, 1}, {token2, uid2, 0}} {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/keys", tc.token, nil)
		assertStatus(t, status, http.StatusOK, body)
		list := mustDecode[userKeyListResp](t, body)
		if len(list.Keys) != tc.want {
			t.Errorf("token(owner=%d) list: want %d, got: %+v", tc.owner, tc.want, list.Keys)
		}
		for i := range list.Keys {
			if list.Keys[i].UserID != tc.owner {
				t.Errorf("token(owner=%d) list[%d] contains another owner's key: %+v", tc.owner, i, list.Keys[i])
			}
		}
	}
}

// TestUserKeys_UpdateRotateDelete 覆盖局部更新、绑定整体替换（含范围校验）、rotate 失效旧 token、删除。
func TestUserKeys_UpdateRotateDelete(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid, userToken := seedUser(t, store, "u1", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	m2 := seedAIModelViaAPI(t, srv, adminToken, "uk-m2")
	m3 := seedAIModelViaAPI(t, srv, adminToken, "uk-m3")
	seedAdminKeyForUser(t, srv, adminToken, uid, m1)
	seedAdminKeyForUser(t, srv, adminToken, uid, m2) // 获批集合 = {m1, m2}

	status, body := createSelfKey(t, srv, userToken, m1)
	assertStatus(t, status, http.StatusCreated, body)
	created := mustDecode[userKeyResp](t, body)
	path := "/api/user/keys/" + itoa(created.ID)

	t.Run("partial scalar field update", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, path, userToken, map[string]any{
			"name": "renamed", "max_tokens": 512, "enabled": false,
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[userKeyResp](t, body)
		if got.Name != "renamed" || got.MaxTokens != 512 || got.Enabled {
			t.Errorf("scalar update mismatch: %+v", got)
		}
		if len(got.Bindings) != 1 || got.Bindings[0].AIModelID != m1 {
			t.Errorf("bindings should not change when bindings omitted: %+v", got.Bindings)
		}
	})

	t.Run("bindings fully replaced", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, path, userToken, map[string]any{
			"bindings": []map[string]any{{"ai_model_id": m2, "role": model.ModelRolePrimary}},
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[userKeyResp](t, body)
		if len(got.Bindings) != 1 || got.Bindings[0].AIModelID != m2 {
			t.Fatalf("bindings after replace mismatch: %+v", got.Bindings)
		}
	})

	t.Run("out-of-scope model binding 400 and original bindings unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, path, userToken, map[string]any{
			"bindings": []map[string]any{{"ai_model_id": m3, "role": model.ModelRolePrimary}},
		})
		assertStatus(t, status, http.StatusBadRequest, body)

		status, body = doRequest(t, srv, http.MethodGet, path, userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[userKeyResp](t, body); len(got.Bindings) != 1 || got.Bindings[0].AIModelID != m2 {
			t.Errorf("rejected replace should not change bindings: %+v", got.Bindings)
		}
	})

	t.Run("rotate invalidates old token", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPost, path+"/rotate", userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		rotated := mustDecode[userKeyResp](t, body)
		if !strings.HasPrefix(rotated.Token, model.TokenPrefixKey) || rotated.Token == created.Token {
			t.Fatalf("rotate should return a new sk- plaintext, got %q", rotated.Token)
		}
		if _, err := store.GetAPIKeyByTokenHash(context.Background(), model.HashToken(created.Token)); !model.IsNotFound(
			err,
		) {
			t.Errorf("old token should be invalidated, got err=%v", err)
		}
		key, err := store.GetAPIKeyByTokenHash(context.Background(), model.HashToken(rotated.Token))
		if err != nil || key.ID != created.ID {
			t.Fatalf("new token should resolve to key %d, got (%+v, %v)", created.ID, key, err)
		}
	})

	t.Run("query after delete returns 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodDelete, path, userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if !mustDecode[okBodyResp](t, body).OK {
			t.Errorf("delete response: want ok=true, response: %s", body)
		}
		status, body = doRequest(t, srv, http.MethodGet, path, userToken, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	})

	t.Run("invalid id returns 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/keys/abc", userToken, nil)
		assertStatus(t, status, http.StatusBadRequest, body)
	})
}

// TestUserKeys_AdminTargetsUser 覆盖 admin 用 ?user_id= 代管：可创建/列出/读取指定用户密钥，
// 不指定 user_id 时按自己名下处理（他人 id → 404）。
func TestUserKeys_AdminTargetsUser(t *testing.T) {
	srv, store := newTestServer(t)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	uid, _ := seedUser(t, store, "u1", model.RoleUser, 0, true)
	m1 := seedAIModelViaAPI(t, srv, adminToken, "uk-m1")
	seedAdminKeyForUser(t, srv, adminToken, uid, m1)

	// admin 指定 user_id 创建。
	status, body := doRequest(t, srv, http.MethodPost, "/api/user/keys?user_id="+itoa(uid), adminToken, map[string]any{
		"name":     "admin-made",
		"bindings": []map[string]any{{"ai_model_id": m1, "role": model.ModelRolePrimary}},
	})
	assertStatus(t, status, http.StatusCreated, body)
	created := mustDecode[userKeyResp](t, body)
	if created.UserID != uid {
		t.Errorf("admin-created user_id: want %d, got %d", uid, created.UserID)
	}

	// admin 指定 user_id 列出与读取。
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys?user_id="+itoa(uid), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if list := mustDecode[userKeyListResp](t, body); len(list.Keys) != 2 {
		t.Errorf("admin list: want 2, got %+v", list.Keys)
	}
	status, body = doRequest(
		t,
		srv,
		http.MethodGet,
		"/api/user/keys/"+itoa(created.ID)+"?user_id="+itoa(uid),
		adminToken,
		nil,
	)
	assertStatus(t, status, http.StatusOK, body)

	// admin 不指定 user_id：目标为自己，访问他人密钥 id → 404。
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys/"+itoa(created.ID), adminToken, nil)
	if status != http.StatusNotFound {
		t.Errorf("admin without user_id accessing another's key: want 404, got %d, response: %s", status, body)
	}

	// 非法 user_id → 400。
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys?user_id=abc", adminToken, nil)
	assertStatus(t, status, http.StatusBadRequest, body)

	// 不存在的 user_id → 404。
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys?user_id=999999", adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}

// TestUserKeys_RequiresAuth 覆盖用户面鉴权：无 token 一律 401。
func TestUserKeys_RequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	requests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/user/keys"},
		{http.MethodGet, "/api/user/keys"},
		{http.MethodGet, "/api/user/keys/1"},
		{http.MethodPut, "/api/user/keys/1"},
		{http.MethodDelete, "/api/user/keys/1"},
		{http.MethodPost, "/api/user/keys/1/rotate"},
	}
	for _, req := range requests {
		status, body := doRequest(t, srv, req.method, req.path, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d, response: %s", req.method, req.path, status, body)
		}
	}
}
