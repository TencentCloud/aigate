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

package proxy_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"aigate/internal/model"
)

// bindingWithKey 构造带绑定级密钥的绑定（自备 Key 模型用）。
func bindingWithKey(aiModelID uint, role string, order int, key string) model.APIKeyModel {
	return model.APIKeyModel{AIModelID: aiModelID, Role: role, SortOrder: order, APIKey: key}
}

// mustCreateUserKeyModel 入库一个自备 Key 模型（无共享密钥）。
func mustCreateUserKeyModel(t *testing.T, store *model.Store, m *model.AIModel) *model.AIModel {
	t.Helper()
	m.APIKey = ""
	m.UserKeyRequired = true
	return mustCreateAIModel(t, store, m)
}

// TestUserKey_ForwardingUsesBindingKey：自备模型转发使用绑定密钥（按绑定隔离），
// 而不是任何模型级共享密钥。
func TestUserKey_ForwardingUsesBindingKey(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateUserKeyModel(t, e.store, newAIModel("byok-a", e.upstream.URL))
	_, token := mustCreateKey(
		t,
		e.store,
		e.user.ID,
		0,
		bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-user-owned"),
	)

	resp := e.postChat(t, token, reqWithModel("byok-a"), nil)
	assertStatus(t, resp, http.StatusOK)

	if got := e.rec.Header("Authorization"); got != "Bearer sk-user-owned" {
		t.Fatalf("upstream Authorization = %q, want the binding key", got)
	}
	if e.rec.Calls() != 1 {
		t.Fatalf("upstream calls = %d, want 1", e.rec.Calls())
	}
}

// TestUserKey_MissingBindingKeyRejected：自备模型绑定缺密钥时数据面 400（不回退、不触达上游）。
func TestUserKey_MissingBindingKeyRejected(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateUserKeyModel(t, e.store, newAIModel("byok-missing", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postChat(t, token, reqWithModel("byok-missing"), nil)
	assertStatus(t, resp, http.StatusBadRequest)
	if body := readBody(t, resp); !strings.Contains(body, "user-provided key") {
		t.Fatalf("body = %q, want a user-provided-key error", body)
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("upstream calls = %d, want 0 (request must be rejected before forwarding)", e.rec.Calls())
	}
}

// TestUserKey_ModelsListSkipsUnresolvable：/v1/models 跳过缺密钥的自备绑定。
func TestUserKey_ModelsListSkipsUnresolvable(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	okModel := mustCreateAIModel(t, e.store, newAIModel("mock-ok", e.upstream.URL))
	byokWithKey := mustCreateUserKeyModel(t, e.store, newAIModel("byok-with-key", e.upstream.URL))
	byokMissing := mustCreateUserKeyModel(t, e.store, newAIModel("byok-missing2", e.upstream.URL))

	_, token := mustCreateKey(t, e.store, e.user.ID, 0,
		primary(okModel.ID, 0),
		bindingWithKey(byokWithKey.ID, model.ModelRoleFallback, 1, "sk-present"),
		bindingWithKey(byokMissing.ID, model.ModelRoleFallback, 2, ""), // 缺密钥 → 必须跳过
	)

	resp := e.getModels(t, token)
	assertStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	if strings.Contains(body, "byok-missing2") {
		t.Fatalf("models list must skip the key-less BYOK binding, got %s", body)
	}
	if !strings.Contains(body, "byok-with-key") {
		t.Fatalf("models list should keep the BYOK binding that has a key, got %s", body)
	}
	if !strings.Contains(body, "mock-ok") {
		t.Fatalf("models list should contain the normal model, got %s", body)
	}
}

// TestKeyLevelQuota_Rejects429：密钥级配额命中 → 429 + Retry-After + 专属文案。
func TestKeyLevelQuota_Rejects429(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("quota-m", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	// 命中前先放行一次（未配置规则）。
	resp := e.postChat(t, token, reqWithModel("quota-m"), nil)
	assertStatus(t, resp, http.StatusOK)

	// 配置密钥级 day=5，并写入该密钥的窗口内用量 5 → 下一次请求超限。
	if _, err := e.store.UpdateAPIKey(ctx(), key.ID, map[string]any{
		"token_quota_rules": `[{"mode":"day","limit":5}]`,
	}); err != nil {
		t.Fatalf("update key rules: %v", err)
	}
	if err := e.store.InsertUsageLog(ctx(), &model.LLMUsageLog{
		APIKeyID:    key.ID,
		UserID:      e.user.ID,
		TotalTokens: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	resp = e.postChat(t, token, reqWithModel("quota-m"), nil)
	assertStatus(t, resp, http.StatusTooManyRequests)
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("Retry-After header missing on key-level quota rejection")
	}
	if body := readBody(t, resp); !strings.Contains(body, "Key token quota exceeded") {
		t.Fatalf("body = %q, want key-level quota message", body)
	}
	// 超限请求不得触达上游（上游计数仍为最开始那 1 次）。
	if e.rec.Calls() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (rejected request must not be forwarded)", e.rec.Calls())
	}
}
