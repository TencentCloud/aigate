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

// 密钥生命周期单测：有效期字段、首次过期观测的幂等，以及删除模型时的 fallback 自动提升。

import (
	"testing"
	"time"

	"aigate/internal/model"
)

// TestAPIKey_ExpiryFieldsAndMarkIfFirst 覆盖：
// 有效期可设置与清除；MarkKeyExpiredIfFirst 只在首次返回 true 且保留首次观测时间。
func TestAPIKey_ExpiryFieldsAndMarkIfFirst(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "owner")
	key, _ := mustCreateAPIKey(t, store, owner.ID)

	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	updated, err := store.UpdateAPIKey(ctx, key.ID, map[string]any{"expires_at": past})
	if err != nil {
		t.Fatalf("UpdateAPIKey(set expires_at): %v", err)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(past) {
		t.Fatalf("ExpiresAt = %v, want %v", updated.ExpiresAt, past)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	first, err := store.MarkKeyExpiredIfFirst(ctx, key.ID, now)
	if err != nil {
		t.Fatalf("MarkKeyExpiredIfFirst(first): %v", err)
	}
	if !first {
		t.Fatal("first call should report first observation")
	}
	second, err := store.MarkKeyExpiredIfFirst(ctx, key.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("MarkKeyExpiredIfFirst(second): %v", err)
	}
	if second {
		t.Fatal("second call must not report first observation")
	}

	got, err := store.GetAPIKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetAPIKeyByID: %v", err)
	}
	if got.ExpiredAt == nil || !got.ExpiredAt.Equal(now) {
		t.Fatalf("ExpiredAt = %v, want %v（首次观测时间必须保留）", got.ExpiredAt, now)
	}

	cleared, err := store.UpdateAPIKey(ctx, key.ID, map[string]any{"expires_at": nil, "expired_at": nil})
	if err != nil {
		t.Fatalf("UpdateAPIKey(clear): %v", err)
	}
	if cleared.ExpiresAt != nil || cleared.ExpiredAt != nil {
		t.Fatalf("cleared key = %+v, want nil ExpiresAt/ExpiredAt", cleared)
	}
}

// TestDeleteAIModel_PromotesFallback 覆盖删除模型时：
//   - primary 被删且仍有其他绑定 → sort_order 最小者提升为 primary；
//   - primary 被删且无剩余绑定 → 保持无绑定（不制造残留 primary）；
//   - 删除非 primary（fallback）→ 其余角色不变。
func TestDeleteAIModel_PromotesFallback(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "owner")
	a := mustCreateAIModel(t, store, "model-a")
	b := mustCreateAIModel(t, store, "model-b")
	c := mustCreateAIModel(t, store, "model-c")

	k1, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(a.ID, model.ModelRolePrimary, 0),
		binding(c.ID, model.ModelRoleFallback, 20),
		binding(b.ID, model.ModelRoleFallback, 5),
	)
	k2, _ := mustCreateAPIKey(t, store, owner.ID, binding(a.ID, model.ModelRolePrimary, 0))
	k3, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(c.ID, model.ModelRolePrimary, 0),
		binding(b.ID, model.ModelRoleFallback, 3),
	)

	if err := store.DeleteAIModel(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAIModel(a): %v", err)
	}

	// k1：sort_order 最小的 fallback（b，5）提升为 primary，c 保持 fallback。
	infos, err := store.ListKeyModels(ctx, k1.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(k1): %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("k1 bindings = %+v, want 2", infos)
	}
	roles := map[uint]string{}
	for _, in := range infos {
		roles[in.AIModelID] = in.Role
	}
	if roles[b.ID] != model.ModelRolePrimary || roles[c.ID] != model.ModelRoleFallback {
		t.Fatalf("k1 roles after delete = %+v, want b=primary c=fallback", roles)
	}

	// k2：无剩余绑定。
	if infos2, err := store.ListKeyModels(ctx, k2.ID, false); err != nil || len(infos2) != 0 {
		t.Fatalf("k2 bindings = %+v err=%v, want empty", infos2, err)
	}

	// k3：与 a 无关，角色不变；随后删除其 fallback b（非 primary）也不影响 c 的 primary。
	infos3, err := store.ListKeyModels(ctx, k3.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(k3): %v", err)
	}
	roles3 := map[uint]string{}
	for _, in := range infos3 {
		roles3[in.AIModelID] = in.Role
	}
	if roles3[c.ID] != model.ModelRolePrimary || roles3[b.ID] != model.ModelRoleFallback {
		t.Fatalf("k3 roles after deleting a = %+v, want c=primary b=fallback", roles3)
	}

	if err := store.DeleteAIModel(ctx, b.ID); err != nil {
		t.Fatalf("DeleteAIModel(b): %v", err)
	}
	infos3b, err := store.ListKeyModels(ctx, k3.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(k3 after b): %v", err)
	}
	if len(infos3b) != 1 || infos3b[0].AIModelID != c.ID || infos3b[0].Role != model.ModelRolePrimary {
		t.Fatalf("k3 after deleting fallback b = %+v, want only c(primary)", infos3b)
	}
}
