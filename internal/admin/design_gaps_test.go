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

// 设计稿缺口补齐（close-design-gaps）的管理面 API 覆盖：
//   - 密钥级配额 token_quota_rules（创建/更新/校验/展示）
//   - 组级全局配额 global_token_quota_rules
//   - userkey（用户自备 Key）模式的创建校验、不可变与连通性拒绝
//   - /admin/usage/export（CSV/JSON/过滤/非法格式）
//   - 绑定级密钥（has_key 只读暴露、明文不回显）

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aigate/internal/model"
)

// ---------- 密钥级配额 ----------

func TestKeyLevelQuotaRules_API(t *testing.T) {
	srv, store, admin := newAdminServer(t)
	owner := seedOwner(t, store)

	// 创建时携带规则（规范化后返回）。
	status, body := doRequest(t, srv, http.MethodPost, "/admin/keys", admin, map[string]any{
		"name": "quota-key", "user_id": owner,
		"token_quota_rules": `[{"mode":"day","limit":10}]`,
	})
	assertStatus(t, status, http.StatusCreated, body)
	created := mustDecode[keyResp](t, body)
	if created.TokenQuotaRules != `[{"mode":"day","limit":10}]` {
		t.Fatalf("token_quota_rules = %q, want normalized rule", created.TokenQuotaRules)
	}

	// 非法规则 → 400。
	status, body = doRequest(t, srv, http.MethodPost, "/admin/keys", admin, map[string]any{
		"name": "bad-key", "user_id": owner,
		"token_quota_rules": `[{"mode":"day","limit":1},{"mode":"day","limit":2}]`,
	})
	assertStatus(t, status, http.StatusBadRequest, body)
	if !strings.Contains(string(body), "token_quota_rules") {
		t.Fatalf("error should name the field, got %s", body)
	}

	// 更新：设置新规则、再清空（空串 = 不限制）。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), admin, map[string]any{
		"token_quota_rules": `[{"mode":"month","limit":999}]`,
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[keyResp](t, body).TokenQuotaRules; got != `[{"mode":"month","limit":999}]` {
		t.Fatalf("updated rules = %q", got)
	}
	status, body = doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), admin, map[string]any{
		"token_quota_rules": "",
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[keyResp](t, body).TokenQuotaRules; got != "" {
		t.Fatalf("cleared rules = %q, want empty", got)
	}

	// 列表同样返回规则字段。
	status, body = doRequest(t, srv, http.MethodGet, "/admin/keys", admin, nil)
	assertStatus(t, status, http.StatusOK, body)
	if list := mustDecode[keyListResp](t, body); len(list.Keys) != 1 {
		t.Fatalf("keys len = %d, want 1", len(list.Keys))
	}
}

// ---------- 组级全局配额 ----------

func TestGroupGlobalQuotaRules_API(t *testing.T) {
	srv, _, admin := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/user-groups", admin, map[string]any{
		"name":                     "g1",
		"token_quota_rules":        `[{"mode":"day","limit":100}]`,
		"global_token_quota_rules": `[{"mode":"day","limit":5000}]`,
	})
	assertStatus(t, status, http.StatusCreated, body)
	created := mustDecode[groupResp](t, body)
	if created.GlobalTokenQuotaRules != `[{"mode":"day","limit":5000}]` {
		t.Fatalf("global rules = %q", created.GlobalTokenQuotaRules)
	}

	// 显式不限 "[]"。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(created.ID), admin, map[string]any{
		"global_token_quota_rules": `[]`,
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[groupResp](t, body).GlobalTokenQuotaRules; got != `[]` {
		t.Fatalf("explicit unlimited = %q, want []", got)
	}

	// 非法 → 400；清空 → 空串。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(created.ID), admin, map[string]any{
		"global_token_quota_rules": `[{"mode":"day","limit":-5}]`,
	})
	assertStatus(t, status, http.StatusBadRequest, body)
	status, body = doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(created.ID), admin, map[string]any{
		"global_token_quota_rules": "",
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[groupResp](t, body).GlobalTokenQuotaRules; got != "" {
		t.Fatalf("cleared global rules = %q, want empty", got)
	}
}

// ---------- userkey 模式 ----------

func TestUserKeyModel_API(t *testing.T) {
	srv, store, admin := newAdminServer(t)

	// 合法创建：无共享密钥。
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", admin, map[string]any{
		"provider":          "byok",
		"model_id":          "byok-model",
		"model_name":        "BYOK",
		"url":               "https://upstream.example/v1",
		"model_type":        "openai-completions",
		"user_key_required": true,
	})
	assertStatus(t, status, http.StatusCreated, body)
	m := mustDecode[modelResp](t, body)
	if !m.UserKeyRequired {
		t.Fatal("user_key_required should be true in response")
	}

	// 自备模型不得携带共享 api_key。
	status, body = doRequest(t, srv, http.MethodPost, "/admin/models", admin, map[string]any{
		"provider": "byok", "model_id": "byok-bad", "url": "https://u.example/v1",
		"model_type": "openai-completions", "user_key_required": true, "api_key": "sk-shared",
	})
	assertStatus(t, status, http.StatusBadRequest, body)

	// 自备模型不得引用凭据。
	cred := createCredential(t, srv, admin, "c1", []map[string]any{headerSecret("Authorization", "Bearer x")}, nil)
	status, body = doRequest(t, srv, http.MethodPost, "/admin/models", admin, map[string]any{
		"provider": "byok", "model_id": "byok-bad2", "url": "https://u.example/v1",
		"model_type": "openai-completions", "user_key_required": true, "credential_id": cred.ID,
	})
	assertStatus(t, status, http.StatusBadRequest, body)

	// 模式不可变更。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(m.ID), admin, map[string]any{
		"user_key_required": false,
	})
	assertStatus(t, status, http.StatusBadRequest, body)

	// 自备模型不得经更新写入共享密钥。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(m.ID), admin, map[string]any{
		"api_key": "sk-shared-2",
	})
	assertStatus(t, status, http.StatusBadRequest, body)

	// 连通性探测明确拒绝。
	status, body = doRequest(t, srv, http.MethodPost, "/admin/models/"+itoa(m.ID)+"/connectivity", admin, nil)
	assertStatus(t, status, http.StatusBadRequest, body)
	if !strings.Contains(strings.ToLower(string(body)), "user_key_required") &&
		!strings.Contains(strings.ToLower(string(body)), "shared key") {
		t.Fatalf("connectivity error should explain the BYOK limitation, got %s", body)
	}

	_ = store
}

// ---------- 绑定级密钥（只写不读回） ----------

func TestBindingKey_API_WriteOnlyAndHasKey(t *testing.T) {
	srv, store, admin := newAdminServer(t)
	owner := seedOwner(t, store)

	// 建一个自备模型。
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", admin, map[string]any{
		"provider": "byok", "model_id": "bk-model", "url": "https://u.example/v1",
		"model_type": "openai-completions", "user_key_required": true,
	})
	assertStatus(t, status, http.StatusCreated, body)
	modelID := mustDecode[modelResp](t, body).ID

	// 创建密钥并携带绑定密钥：响应中不出现明文，仅 has_key=true。
	status, body = doRequest(t, srv, http.MethodPost, "/admin/keys", admin, map[string]any{
		"name": "bk-key", "user_id": owner,
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary", "api_key": "sk-user-owned-secret"}},
	})
	assertStatus(t, status, http.StatusCreated, body)
	if strings.Contains(string(body), "sk-user-owned-secret") {
		t.Fatal("binding key plaintext must never be echoed")
	}
	created := mustDecode[keyResp](t, body)
	if len(created.Bindings) != 1 || !created.Bindings[0].HasKey {
		t.Fatalf("bindings = %+v, want has_key=true", created.Bindings)
	}

	// 整体替换且未携带密钥 → has_key=false（从库中复查）。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/keys/"+itoa(created.ID), admin, map[string]any{
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[keyResp](t, body).Bindings; len(got) != 1 || got[0].HasKey {
		t.Fatalf("bindings after replace = %+v, want has_key=false", got)
	}
}

// ---------- usage export ----------

func TestUsageExport_API(t *testing.T) {
	srv, store, admin := newAdminServer(t)
	ownerA := seedOwner(t, store)
	ownerB := seedOwner(t, store)

	seed := func(userID uint, tokens int) {
		if err := store.InsertUsageLog(context.Background(), &model.LLMUsageLog{
			UserID: userID, TotalTokens: tokens, StatusCode: 200,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	seed(ownerA, 11)
	seed(ownerA, 22)
	seed(ownerB, 33)

	// CSV：表头 + 3 行；过滤 user_id 只剩 2 行。
	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/export", admin, nil)
	assertStatus(t, status, http.StatusOK, body)
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 4 { // 表头 + 3 条
		t.Fatalf("csv rows = %d, want 4", len(rows))
	}
	if rows[0][0] != "id" || len(rows[1]) != len(rows[0]) {
		t.Fatalf("csv header/width unexpected: %v", rows[0])
	}

	status, body = doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/admin/usage/export?format=csv&user_id=%d", ownerA), admin, nil)
	assertStatus(t, status, http.StatusOK, body)
	rows, _ = csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if len(rows) != 3 {
		t.Fatalf("filtered csv rows = %d, want 3", len(rows))
	}

	// JSON：{"logs":[...]} 且可解析。
	status, body = doRequest(t, srv, http.MethodGet, "/admin/usage/export?format=json", admin, nil)
	assertStatus(t, status, http.StatusOK, body)
	var decoded struct {
		Logs []map[string]any `json:"logs"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("parse json export: %v\n%s", err, body)
	}
	if len(decoded.Logs) != 3 {
		t.Fatalf("json logs = %d, want 3", len(decoded.Logs))
	}
	if _, ok := decoded.Logs[0]["total_tokens"]; !ok {
		t.Fatalf("json log missing total_tokens: %v", decoded.Logs[0])
	}

	// 非法格式 → 400。
	status, body = doRequest(t, srv, http.MethodGet, "/admin/usage/export?format=xml", admin, nil)
	assertStatus(t, status, http.StatusBadRequest, body)
}
