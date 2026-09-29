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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aigate/internal/model"
)

// TestCreateUser_RolesAndToken 覆盖创建返回一次性明文（gk- 前缀）、角色默认与显式取值、非法角色 400。
func TestCreateUser_RolesAndToken(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	t.Run("default role is user and token is one-time", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
			"username": "alice",
		})
		assertStatus(t, status, http.StatusCreated, body)
		got := mustDecode[userResp](t, body)
		if got.ID == 0 {
			t.Fatalf("create response missing id, body: %s", body)
		}
		if got.Role != model.RoleUser {
			t.Errorf("default role want %q, got %q", model.RoleUser, got.Role)
		}
		if !got.Enabled {
			t.Error("enabled default want true")
		}
		if !strings.HasPrefix(got.Token, model.TokenPrefixAccess) {
			t.Errorf("token should start with %q, got %q", model.TokenPrefixAccess, got.Token)
		}
		if got.APITokenPrefix == "" {
			t.Error("create response missing api_token_prefix")
		}
	})

	t.Run("explicit admin creation", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
			"username": "carol",
			"role":     model.RoleAdmin,
		})
		assertStatus(t, status, http.StatusCreated, body)
		if got := mustDecode[userResp](t, body); got.Role != model.RoleAdmin {
			t.Errorf("role want %q, got %q", model.RoleAdmin, got.Role)
		}
	})

	t.Run("explicitly disabled", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
			"username": "dave",
			"role":     model.RoleUser,
			"enabled":  false,
		})
		assertStatus(t, status, http.StatusCreated, body)
		if got := mustDecode[userResp](t, body); got.Enabled {
			t.Error("enabled want false")
		}
	})

	t.Run("invalid role 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
			"username": "eve",
			"role":     "root",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})
}

// TestListUsers_NoTokenHash 覆盖列表包装体且响应不含 token 哈希。
func TestListUsers_NoTokenHash(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedUser(t, store, "alice", model.RoleUser, true)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/users", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if bytes.Contains(body, []byte("api_token_hash")) {
		t.Errorf("user list should not contain api_token_hash field, body: %s", body)
	}
	list := mustDecode[userListResp](t, body)
	if len(list.Users) != 2 {
		t.Fatalf("user list want 2 entries (including admin), got %d", len(list.Users))
	}
}

// TestUpdateUser_PartialAndRoleValidation 覆盖局部更新与角色校验。
func TestUpdateUser_PartialAndRoleValidation(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
		"username": "alice",
		"role":     model.RoleUser,
	})
	assertStatus(t, status, http.StatusCreated, body)
	id := mustDecode[userResp](t, body).ID

	t.Run("rename and promote", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/users/"+itoa(id), adminToken, map[string]any{
			"username": "alice-renamed",
			"role":     model.RoleAdmin,
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[userResp](t, body)
		if got.Username != "alice-renamed" || got.Role != model.RoleAdmin {
			t.Errorf("update result mismatch: %+v", got)
		}
	})

	t.Run("invalid role 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/users/"+itoa(id), adminToken, map[string]any{
			"role": "root",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("zero value enabled=false can be written", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/users/"+itoa(id), adminToken, map[string]any{
			"enabled": false,
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[userResp](t, body); got.Enabled {
			t.Error("enabled want false")
		}
	})

	t.Run("unknown id 404", func(t *testing.T) {
		status, body := doRequest(
			t,
			srv,
			http.MethodPut,
			"/admin/users/999999",
			adminToken,
			map[string]any{"username": "nope"},
		)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestRotateUser_InvalidatesOldToken 覆盖 rotate 换取新 token 且旧 token 立即失效。
func TestRotateUser_InvalidatesOldToken(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	targetID, oldToken := seedUser(t, store, "alice", model.RoleUser, true)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/users/"+itoa(targetID)+"/rotate", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	rotated := mustDecode[userResp](t, body)

	if !strings.HasPrefix(rotated.Token, model.TokenPrefixAccess) {
		t.Fatalf("rotated token should start with %q, got %q", model.TokenPrefixAccess, rotated.Token)
	}
	if rotated.Token == oldToken {
		t.Fatal("plaintext after rotate should differ from old plaintext")
	}

	if _, err := store.GetUserByTokenHash(context.Background(), model.HashToken(oldToken)); !model.IsNotFound(err) {
		t.Errorf("old token should be invalidated (want not found), got err=%v", err)
	}
	u, err := store.GetUserByTokenHash(context.Background(), model.HashToken(rotated.Token))
	if err != nil {
		t.Fatalf("new token should resolve, got err=%v", err)
	}
	if u.ID != targetID {
		t.Errorf("new token should resolve to user %d, got %d", targetID, u.ID)
	}
}

// TestDeleteUser_CascadesKeys 覆盖删除用户级联清理其名下密钥，并返回 404 于重复删除。
func TestDeleteUser_CascadesKeys(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID, _ := seedUser(t, store, "victim", model.RoleUser, true)
	modelID := createModel(t, srv, adminToken, "gpt-cascade-user", model.ModelTypeOpenAI)
	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary},
	})

	status, body := doRequest(t, srv, http.MethodDelete, "/admin/users/"+itoa(ownerID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if !mustDecode[okResp](t, body).OK {
		t.Errorf("delete response want ok=true, body: %s", body)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/keys/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
	if _, err := store.GetAPIKeyByID(context.Background(), created.ID); !model.IsNotFound(err) {
		t.Errorf("key should not exist after cascade delete, got err=%v", err)
	}

	status, body = doRequest(t, srv, http.MethodDelete, "/admin/users/"+itoa(ownerID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}

// TestUser_LastEnabledAdminGuard 覆盖「最后一个启用中的管理员」保护：
// 删除 / 降级 / 禁用都会被 409 拒绝；存在第二个启用管理员时放行。
func TestUser_LastEnabledAdminGuard(t *testing.T) {
	srv, store := newTestServer(t)
	adminID, adminToken := seedAdmin(t, store)

	t.Run("demote last admin rejected", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/admin/users/%d", adminID), adminToken,
			map[string]any{"role": model.RoleUser})
		assertStatus(t, status, http.StatusConflict, body)
		if got := decodeError(t, body); !strings.Contains(got.Error.Message, "last enabled admin") {
			t.Fatalf("error message = %q, want last-enabled-admin hint", got.Error.Message)
		}
	})

	t.Run("disable last admin rejected", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/admin/users/%d", adminID), adminToken,
			map[string]any{"enabled": false})
		assertStatus(t, status, http.StatusConflict, body)
	})

	t.Run("delete last admin rejected", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/admin/users/%d", adminID), adminToken, nil)
		assertStatus(t, status, http.StatusConflict, body)
	})

	t.Run("with a second enabled admin operations are allowed", func(t *testing.T) {
		secondID, secondToken := seedUser(t, store, "admin2", model.RoleAdmin, true)
		// 降级第一个管理员：现在允许（还剩 admin2）。此后原 token 已不是管理员。
		status, body := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/admin/users/%d", adminID), adminToken,
			map[string]any{"role": model.RoleUser})
		assertStatus(t, status, http.StatusOK, body)
		// 删除第二个（仍是最后一个启用管理员）→ 拒绝；先创建第三个再删除第二个 → 允许。
		status, body = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/admin/users/%d", secondID), secondToken, nil)
		assertStatus(t, status, http.StatusConflict, body)
		seedUser(t, store, "admin3", model.RoleAdmin, true)
		status, body = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/admin/users/%d", secondID), secondToken, nil)
		assertStatus(t, status, http.StatusOK, body)
	})
}
