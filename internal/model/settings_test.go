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

// GetSettings 首次调用自动创建 id=1 的默认行，且不会重复创建。
func TestGetSettings_AutoCreatesDefaultsOnce(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	st, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.ID != 1 {
		t.Fatalf("ID = %d, want 1", st.ID)
	}
	if st.DefaultTokenQuotaRules != "[]" {
		t.Fatalf("DefaultTokenQuotaRules = %q, want %q", st.DefaultTokenQuotaRules, "[]")
	}
	if st.GlobalTokenQuotaRules != "[]" {
		t.Fatalf("GlobalTokenQuotaRules = %q, want %q", st.GlobalTokenQuotaRules, "[]")
	}
	if st.GlobalQuotaScope != "site" {
		t.Fatalf("GlobalQuotaScope = %q, want %q", st.GlobalQuotaScope, "site")
	}

	// 再次读取不应新建第二行。
	again, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings(second): %v", err)
	}
	if again.ID != 1 {
		t.Fatalf("second GetSettings ID = %d, want 1", again.ID)
	}
	if n := countRows(t, db, &model.Setting{}, ""); n != 1 {
		t.Fatalf("settings rows = %d, want 1", n)
	}
}

// UpdateSettings 更新后落库持久（用另一条连接回读验证）。
func TestUpdateSettings_Persists(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	// 先确保默认行存在。
	if _, err := store.GetSettings(ctx); err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	updated, err := store.UpdateSettings(ctx, map[string]any{
		"default_token_quota_rules": `[{"period":"day","limit":100}]`,
		"global_token_quota_rules":  `[{"period":"month","limit":10000}]`,
		"global_quota_scope":        "group",
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if updated.GlobalQuotaScope != "group" {
		t.Fatalf("returned GlobalQuotaScope = %q, want group", updated.GlobalQuotaScope)
	}

	// 重新读取（走 GetSettings）应沿用更新值，而不是回到默认值。
	reread, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings(after update): %v", err)
	}
	if reread.GlobalQuotaScope != "group" ||
		reread.DefaultTokenQuotaRules != `[{"period":"day","limit":100}]` ||
		reread.GlobalTokenQuotaRules != `[{"period":"month","limit":10000}]` {
		t.Fatalf("GetSettings after update = %+v, want updated values", reread)
	}

	// 表侧确认仍然是单行 id=1 且已持久化。
	var row model.Setting
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatalf("read settings row: %v", err)
	}
	if row.GlobalQuotaScope != "group" || row.DefaultTokenQuotaRules != `[{"period":"day","limit":100}]` {
		t.Fatalf("persisted settings = %+v, want updated values", row)
	}
	if n := countRows(t, db, &model.Setting{}, ""); n != 1 {
		t.Fatalf("settings rows = %d, want 1", n)
	}
}

// 分组 CRUD：默认配额规则为空串（未配置 → 回退全局默认；"[]" 才是显式不限）、列表有序、局部更新、删除。
func TestGroup_CRUDAndDefaults(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	g := &model.Group{Name: "team-a"}
	if err := store.CreateGroup(ctx, g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if g.TokenQuotaRules != "" {
		t.Fatalf("default TokenQuotaRules = %q, want empty (unset)", g.TokenQuotaRules)
	}

	got, err := store.GetGroup(ctx, g.ID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got.Name != "team-a" || got.TokenQuotaRules != "" {
		t.Fatalf("GetGroup = %+v, want name=team-a rules empty", got)
	}

	second := &model.Group{Name: "team-b", TokenQuotaRules: `[{"period":"day","limit":5}]`}
	if err := store.CreateGroup(ctx, second); err != nil {
		t.Fatalf("CreateGroup(second): %v", err)
	}

	groups, err := store.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("ListGroups len = %d, want 2", len(groups))
	}
	if groups[0].ID >= groups[1].ID {
		t.Fatalf("ListGroups not ordered by id ASC: %+v", groups)
	}

	updated, err := store.UpdateGroup(ctx, g.ID, map[string]any{"name": "team-a2"})
	if err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	if updated.Name != "team-a2" || updated.TokenQuotaRules != "" {
		t.Fatalf("UpdateGroup = %+v, want name=team-a2 rules empty", updated)
	}

	if err := store.DeleteGroup(ctx, second.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if _, err := store.GetGroup(ctx, second.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted group still readable: err = %v", err)
	}
	if _, err := store.GetGroup(ctx, 999999); !model.IsNotFound(err) {
		t.Fatalf("GetGroup(unknown) err = %v, want not found", err)
	}
}
