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
	"testing"

	"aigate/internal/model"
)

// 创建用户：角色缺省为 user、Enabled 缺省为 true；按 id / token 哈希可查，列表与计数正确。
func TestUser_CreateDefaultsAndQueries(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	alice, alicePlain := mustCreateUserWithToken(t, store, "alice")

	if alice.Role != model.RoleUser {
		t.Fatalf("default role = %q, want %q", alice.Role, model.RoleUser)
	}
	if !alice.Enabled {
		t.Fatal("newly created user should be enabled by default")
	}
	if alice.APITokenPrefix == "" {
		t.Fatal("token prefix should be populated")
	}

	byID, err := store.GetUserByID(ctx, alice.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if byID.ID != alice.ID || byID.Username != "alice" {
		t.Fatalf("GetUserByID = %+v, want id=%d name=alice", byID, alice.ID)
	}

	byHash, err := store.GetUserByTokenHash(ctx, model.HashToken(alicePlain))
	if err != nil {
		t.Fatalf("GetUserByTokenHash: %v", err)
	}
	if byHash.ID != alice.ID {
		t.Fatalf("GetUserByTokenHash id = %d, want %d", byHash.ID, alice.ID)
	}

	if _, err := store.GetUserByTokenHash(ctx, model.HashToken("gk-not-a-real-token")); !model.IsNotFound(err) {
		t.Fatalf("GetUserByTokenHash(unknown) err = %v, want not found", err)
	}

	bob := mustCreateUser(t, store, "bob")
	admin := &model.User{Username: "root", Role: model.RoleAdmin}
	if err := store.CreateUser(ctx, admin); err != nil {
		t.Fatalf("CreateUser(admin): %v", err)
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("ListUsers len = %d, want 3", len(users))
	}
	for i := 1; i < len(users); i++ {
		if users[i-1].ID >= users[i].ID {
			t.Fatalf("ListUsers not ordered by id ASC: %+v", users)
		}
	}

	userCount, err := store.CountUsersByRole(ctx, model.RoleUser)
	if err != nil {
		t.Fatalf("CountUsersByRole(user): %v", err)
	}
	if userCount != 2 {
		t.Fatalf("CountUsersByRole(user) = %d, want 2", userCount)
	}
	adminCount, err := store.CountUsersByRole(ctx, model.RoleAdmin)
	if err != nil {
		t.Fatalf("CountUsersByRole(admin): %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("CountUsersByRole(admin) = %d, want 1", adminCount)
	}
	if bob.ID == 0 {
		t.Fatal("bob should have been persisted")
	}

	if _, err := store.GetUserByID(ctx, 999999); !model.IsNotFound(err) {
		t.Fatalf("GetUserByID(unknown) err = %v, want not found", err)
	}
}

// 更新用户返回更新后的实体，且只改传入字段。
func TestUser_UpdateReturnsUpdatedEntity(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	u := mustCreateUser(t, store, "before")

	updated, err := store.UpdateUser(ctx, u.ID, map[string]any{
		"username": "after",
		"role":     model.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.Username != "after" || updated.Role != model.RoleAdmin {
		t.Fatalf("UpdateUser = %+v, want name=after role=admin", updated)
	}
	if updated.APITokenHash != u.APITokenHash {
		t.Fatalf("UpdateUser changed token hash: %q -> %q", u.APITokenHash, updated.APITokenHash)
	}

	// 空 fields 时仅回读，不报错。
	same, err := store.UpdateUser(ctx, u.ID, nil)
	if err != nil {
		t.Fatalf("UpdateUser(nil fields): %v", err)
	}
	if same.Username != "after" {
		t.Fatalf("UpdateUser(nil fields) name = %q, want after", same.Username)
	}
}

// 删除用户级联删除其名下密钥与绑定。
func TestUser_DeleteCascadesKeysAndBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	keeper := mustCreateUser(t, store, "keeper")
	m1 := mustCreateAIModel(t, store, "m1")
	m2 := mustCreateAIModel(t, store, "m2")

	ownerKey, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(m1.ID, model.ModelRolePrimary, 0),
		binding(m2.ID, model.ModelRoleFallback, 1),
	)
	keeperKey, _ := mustCreateAPIKey(t, store, keeper.ID, binding(m1.ID, model.ModelRolePrimary, 0))

	if err := store.DeleteUser(ctx, owner.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	if _, err := store.GetUserByID(ctx, owner.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted user still readable: err = %v", err)
	}
	if _, err := store.GetAPIKeyByID(ctx, ownerKey.ID); !model.IsNotFound(err) {
		t.Fatalf("owner key should be deleted: err = %v", err)
	}
	ownerBindings, err := store.ListKeyModels(ctx, ownerKey.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(owner): %v", err)
	}
	if len(ownerBindings) != 0 {
		t.Fatalf("owner bindings = %+v, want none", ownerBindings)
	}

	// 其他用户的密钥不受影响。
	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != keeperKey.ID {
		t.Fatalf("ListAPIKeys = %+v, want only keeper key %d", keys, keeperKey.ID)
	}
	keeperBindings, err := store.ListKeyModels(ctx, keeperKey.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(keeper): %v", err)
	}
	if len(keeperBindings) != 1 {
		t.Fatalf("keeper bindings = %+v, want 1", keeperBindings)
	}
}

// 换发用户 token 后旧哈希失效、新哈希可查。
func TestUser_RotateTokenInvalidatesOldHash(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	u, oldPlain := mustCreateUserWithToken(t, store, "rotator")
	oldHash := model.HashToken(oldPlain)

	newPlain, display, err := store.RotateUserToken(ctx, u.ID)
	if err != nil {
		t.Fatalf("RotateUserToken: %v", err)
	}
	if newPlain == oldPlain {
		t.Fatal("rotated token equals old token")
	}
	if len(newPlain) == 0 || len(display) == 0 {
		t.Fatalf("RotateUserToken returned empty plain/display: %q / %q", newPlain, display)
	}
	if len(display) != 8+len("…")+4 {
		t.Fatalf("display = %q, want first-8 + … + last-4 format", display)
	}

	if _, err := store.GetUserByTokenHash(ctx, oldHash); !model.IsNotFound(err) {
		t.Fatalf("old token hash should be invalid: err = %v", err)
	}
	got, err := store.GetUserByTokenHash(ctx, model.HashToken(newPlain))
	if err != nil {
		t.Fatalf("new token hash lookup: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("new token hash resolved id = %d, want %d", got.ID, u.ID)
	}
	if got.APITokenPrefix != display {
		t.Fatalf("stored token prefix = %q, want %q", got.APITokenPrefix, display)
	}
}

// 禁用用户仍可通过 token 哈希查到（启用与否由鉴权层判断）。
func TestUser_DisabledUserStillQueryable(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	u, plain := mustCreateUserWithToken(t, store, "disabled")
	if _, err := store.UpdateUser(ctx, u.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	got, err := store.GetUserByTokenHash(ctx, model.HashToken(plain))
	if err != nil {
		t.Fatalf("disabled user must still be findable by token hash: %v", err)
	}
	if got.Enabled {
		t.Fatal("user should be disabled")
	}
	byID, err := store.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID(disabled): %v", err)
	}
	if byID.Enabled {
		t.Fatal("GetUserByID should return Enabled=false")
	}
}
