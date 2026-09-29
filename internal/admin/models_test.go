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
	"testing"

	"aigate/internal/model"
)

// TestCreateModel_DefaultsAndNoKeyEcho 覆盖创建默认值（quota_day=-1、enabled=true）与不回显 api_key。
func TestCreateModel_DefaultsAndNoKeyEcho(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   "gpt-test",
		"model_name": "GPT Test",
		"url":        "https://upstream.example/v1",
		"api_key":    "sk-upstream-secret",
		"model_type": "openai-completions",
	})
	assertStatus(t, status, http.StatusCreated, body)

	got := mustDecode[modelResp](t, body)
	if got.ID == 0 {
		t.Fatalf("create response missing id, body: %s", body)
	}
	if got.QuotaDay != -1 {
		t.Errorf("quota_day default want -1, got %d", got.QuotaDay)
	}
	if !got.Enabled {
		t.Error("enabled default want true")
	}
	if got.CustomHTTPHeaders != nil {
		t.Errorf("custom_http_headers default want null, got %v", got.CustomHTTPHeaders)
	}
	if bytes.Contains(body, []byte("sk-upstream-secret")) {
		t.Errorf("create response echoed api_key plaintext, body: %s", body)
	}
	if len(got.CreatedAt) == 0 || len(got.UpdatedAt) == 0 {
		t.Errorf("response should contain created_at/updated_at, body: %s", body)
	}
}

// TestCreateModel_CustomHeaders 覆盖 custom_http_headers 对象透传。
func TestCreateModel_CustomHeaders(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"model_id":            "gpt-headers",
		"url":                 "https://upstream.example/v1",
		"model_type":          "openai-completions",
		"custom_http_headers": map[string]string{"X-Trace": "on"},
	})
	assertStatus(t, status, http.StatusCreated, body)

	got := mustDecode[modelResp](t, body)
	if got.CustomHTTPHeaders["X-Trace"] != "on" {
		t.Errorf("custom_http_headers want X-Trace=on, got %v", got.CustomHTTPHeaders)
	}
}

// TestCreateModel_ExplicitZeroValues 覆盖显式 enabled=false / quota_day=0 落库（不被库默认值覆盖）。
func TestCreateModel_ExplicitZeroValues(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"model_id":   "gpt-zero",
		"url":        "https://upstream.example/v1",
		"model_type": "openai-completions",
		"quota_day":  0,
		"enabled":    false,
	})
	assertStatus(t, status, http.StatusCreated, body)

	got := mustDecode[modelResp](t, body)
	if got.QuotaDay != 0 {
		t.Errorf("quota_day explicit 0 should persist as 0, got %d", got.QuotaDay)
	}
	if got.Enabled {
		t.Error("enabled explicit false should persist as false")
	}

	stored, err := store.GetAIModel(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if stored.QuotaDay != 0 || stored.Enabled {
		t.Errorf("persisted zero values mismatch: quota_day=%d enabled=%v", stored.QuotaDay, stored.Enabled)
	}
}

// TestCreateModel_RequiredFields 覆盖必填与枚举校验（均 400 且不落库）。
func TestCreateModel_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "missing model_id",
			body: map[string]any{"url": "https://upstream.example/v1", "model_type": "openai-completions"},
		},
		{
			name: "missing url",
			body: map[string]any{"model_id": "gpt-x", "model_type": "openai-completions"},
		},
		{
			name: "missing model_type",
			body: map[string]any{"model_id": "gpt-x", "url": "https://upstream.example/v1"},
		},
		{
			name: "invalid model_type",
			body: map[string]any{"model_id": "gpt-x", "url": "https://upstream.example/v1", "model_type": "gemini"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, adminToken := newAdminServer(t)
			status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, tc.body)
			assertStatus(t, status, http.StatusBadRequest, body)
			if eb := decodeError(t, body); eb.Error.Message == "" {
				t.Errorf("error response missing message, body: %s", body)
			}

			status, body = doRequest(t, srv, http.MethodGet, "/admin/models", adminToken, nil)
			assertStatus(t, status, http.StatusOK, body)
			if list := mustDecode[modelListResp](t, body); len(list.Models) != 0 {
				t.Errorf("validation failure should not persist, got list %v", list.Models)
			}
		})
	}
}

// TestListAndGetModel 覆盖列表包装体与详情查询。
func TestListAndGetModel(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)
	firstID := createModel(t, srv, adminToken, "gpt-a", model.ModelTypeOpenAI)
	secondID := createModel(t, srv, adminToken, "claude-b", model.ModelTypeAnthropic)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/models", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	list := mustDecode[modelListResp](t, body)
	if len(list.Models) != 2 {
		t.Fatalf("list want 2 entries, got %d, body: %s", len(list.Models), body)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/models/"+itoa(secondID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[modelResp](t, body)
	if got.ID != secondID || got.ModelID != "claude-b" || got.ModelType != model.ModelTypeAnthropic {
		t.Errorf("detail mismatch with create: %+v", got)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/models/"+itoa(firstID+secondID+999), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}

// TestUpdateModel_PartialUpdate 覆盖局部更新：未出现的字段不变、api_key 空串视为不变、零值可写。
func TestUpdateModel_PartialUpdate(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   "gpt-original",
		"model_name": "Original",
		"url":        "https://upstream.example/v1",
		"api_key":    "sk-original",
		"model_type": "openai-completions",
		"quota_day":  100,
	})
	assertStatus(t, status, http.StatusCreated, body)
	id := mustDecode[modelResp](t, body).ID

	t.Run("rename only, rest unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(id), adminToken, map[string]any{
			"model_name": "Renamed",
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[modelResp](t, body)
		if got.ModelName != "Renamed" {
			t.Errorf("model_name want Renamed, got %q", got.ModelName)
		}
		if got.URL != "https://upstream.example/v1" || got.ModelID != "gpt-original" || got.Provider != "mock" {
			t.Errorf("unset fields should not change, got %+v", got)
		}
		if got.QuotaDay != 100 || !got.Enabled {
			t.Errorf("quota_day/enabled should not change, got quota_day=%d enabled=%v", got.QuotaDay, got.Enabled)
		}
	})

	t.Run("api_key empty string means unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(id), adminToken, map[string]any{
			"api_key": "",
		})
		assertStatus(t, status, http.StatusOK, body)

		stored, err := store.GetAIModel(context.Background(), id)
		if err != nil {
			t.Fatalf("get model: %v", err)
		}
		if stored.APIKey != "sk-original" {
			t.Errorf("api_key empty string should not overwrite, got %q", stored.APIKey)
		}
	})

	t.Run("api_key non-empty can be updated", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(id), adminToken, map[string]any{
			"api_key": "sk-updated",
		})
		assertStatus(t, status, http.StatusOK, body)

		stored, err := store.GetAIModel(context.Background(), id)
		if err != nil {
			t.Fatalf("get model: %v", err)
		}
		if stored.APIKey != "sk-updated" {
			t.Errorf("api_key want updated to sk-updated, got %q", stored.APIKey)
		}
	})

	t.Run("zero values can be written", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(id), adminToken, map[string]any{
			"enabled":   false,
			"quota_day": 0,
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[modelResp](t, body)
		if got.Enabled {
			t.Error("enabled want false")
		}
		if got.QuotaDay != 0 {
			t.Errorf("quota_day want 0, got %d", got.QuotaDay)
		}
	})

	t.Run("invalid model_type 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(id), adminToken, map[string]any{
			"model_type": "gemini",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("unknown id 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/models/999999", adminToken, map[string]any{
			"model_name": "nope",
		})
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestDeleteModel_CascadesBindings 覆盖删除模型返回 ok 并级联清理密钥绑定。
func TestDeleteModel_CascadesBindings(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ownerID := seedOwner(t, store)
	modelID := createModel(t, srv, adminToken, "gpt-cascade", model.ModelTypeOpenAI)

	created := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary, "sort_order": 0},
	})
	if len(created.Bindings) != 1 {
		t.Fatalf("bindings want 1 entry, got %d", len(created.Bindings))
	}

	status, body := doRequest(t, srv, http.MethodDelete, "/admin/models/"+itoa(modelID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if !mustDecode[okResp](t, body).OK {
		t.Errorf("delete response want ok=true, body: %s", body)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/keys/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[keyResp](t, body); len(got.Bindings) != 0 {
		t.Errorf("bindings should be cleaned up after model delete, got %v", got.Bindings)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/models/"+itoa(modelID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)

	status, body = doRequest(t, srv, http.MethodDelete, "/admin/models/"+itoa(modelID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}
