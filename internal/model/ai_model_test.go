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
	"reflect"
	"testing"

	"aigate/internal/model"
)

// 模型 CRUD + 零值字段落库时套用 DB 默认值（quota_day=-1、enabled=true、model_type 默认）。
func TestAIModel_CRUDAndDefaults(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	// 只填业务必填项，其余留零值，验证默认值来自建表定义。
	m := &model.AIModel{Provider: "p", ModelID: "gpt-x", ModelName: "GPT X", URL: "https://upstream.test/v1"}
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatalf("CreateAIModel: %v", err)
	}

	got, err := store.GetAIModel(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetAIModel: %v", err)
	}
	if got.QuotaDay != -1 {
		t.Fatalf("default QuotaDay = %d, want -1", got.QuotaDay)
	}
	if !got.Enabled {
		t.Fatal("default Enabled = false, want true")
	}
	if got.ModelType != model.ModelTypeOpenAI {
		t.Fatalf("default ModelType = %q, want %q", got.ModelType, model.ModelTypeOpenAI)
	}
	if got.CustomHTTPHeaders != "" {
		t.Fatalf("CustomHTTPHeaders = %q, want empty", got.CustomHTTPHeaders)
	}

	second := mustCreateAIModel(t, store, "m2")
	models, err := store.ListAIModels(ctx)
	if err != nil {
		t.Fatalf("ListAIModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ListAIModels len = %d, want 2", len(models))
	}
	if models[0].ID >= models[1].ID {
		t.Fatalf("ListAIModels not ordered by id ASC: %+v", models)
	}
	if models[1].ID != second.ID {
		t.Fatalf("ListAIModels[1].ID = %d, want %d", models[1].ID, second.ID)
	}

	if err := store.DeleteAIModel(ctx, second.ID); err != nil {
		t.Fatalf("DeleteAIModel: %v", err)
	}
	if _, err := store.GetAIModel(ctx, second.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted model still readable: err = %v", err)
	}
	if _, err := store.GetAIModel(ctx, 999999); !model.IsNotFound(err) {
		t.Fatalf("GetAIModel(unknown) err = %v, want not found", err)
	}
}

// 局部字段更新：只改传入字段，Enabled 可被显式置为 false。
func TestAIModel_UpdatePartialFields(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	m := mustCreateAIModel(t, store, "partial")
	updated, err := store.UpdateAIModel(ctx, m.ID, map[string]any{
		"model_name": "renamed",
		"enabled":    false,
	})
	if err != nil {
		t.Fatalf("UpdateAIModel: %v", err)
	}
	if updated.ModelName != "renamed" {
		t.Fatalf("ModelName = %q, want renamed", updated.ModelName)
	}
	if updated.Enabled {
		t.Fatal("Enabled should be updated to false")
	}
	// 未传入字段保持原值。
	if updated.ModelID != m.ModelID || updated.URL != m.URL {
		t.Fatalf("unexpected mutation of untouched fields: %+v", updated)
	}
	if updated.QuotaDay != m.QuotaDay {
		t.Fatalf("QuotaDay = %d, want %d", updated.QuotaDay, m.QuotaDay)
	}

	same, err := store.UpdateAIModel(ctx, m.ID, nil)
	if err != nil {
		t.Fatalf("UpdateAIModel(nil fields): %v", err)
	}
	if same.ModelName != "renamed" {
		t.Fatalf("UpdateAIModel(nil fields) ModelName = %q, want renamed", same.ModelName)
	}
}

// 删除模型清理其绑定，密钥本身保留。
func TestAIModel_DeleteClearsBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")
	m2 := mustCreateAIModel(t, store, "m2")
	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(m1.ID, model.ModelRolePrimary, 0),
		binding(m2.ID, model.ModelRoleFallback, 1),
	)

	if err := store.DeleteAIModel(ctx, m1.ID); err != nil {
		t.Fatalf("DeleteAIModel: %v", err)
	}

	if _, err := store.GetAIModel(ctx, m1.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted model still readable: err = %v", err)
	}
	infos, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != 1 || infos[0].AIModelID != m2.ID {
		t.Fatalf("bindings after model delete = %+v, want only model %d", infos, m2.ID)
	}
	if _, err := store.GetAPIKeyByID(ctx, key.ID); err != nil {
		t.Fatalf("api key should survive model delete: %v", err)
	}
}

// GetCustomHTTPHeaders 的合法 / 非法 / 空串三种输入。
func TestAIModel_GetCustomHTTPHeaders(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  map[string]string
	}{
		{name: "valid", value: `{"X-A":"1","X-B":"2"}`, want: map[string]string{"X-A": "1", "X-B": "2"}},
		{name: "empty", value: "", want: nil},
		{name: "whitespace", value: "  \n\t ", want: nil},
		{name: "invalid-json", value: `{"X-A":`, want: nil},
		{name: "not-object", value: `["X-A"]`, want: nil},
		{name: "empty-object", value: `{}`, want: nil},
		{name: "json-null", value: `null`, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := model.AIModel{CustomHTTPHeaders: tt.value}
			got := m.GetCustomHTTPHeaders()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetCustomHTTPHeaders(%q) = %#v, want %#v", tt.value, got, tt.want)
			}
		})
	}
}
