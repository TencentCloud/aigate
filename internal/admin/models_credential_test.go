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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/crypto"
	"aigate/internal/model"
)

// upstreamPlainKey 与 testing_test.go 的 createModel 夹具保持一致：创建模型时写入的上游明文密钥。
const upstreamPlainKey = "sk-upstream-secret"

// modelCredResp 在既有 modelResp 上补充 credential_id 字段。
type modelCredResp struct {
	modelResp
	CredentialID uint `json:"credential_id"`
}

// createModelWithCredential 经管理面创建引用凭据的模型并返回模型 ID（断言响应含 credential_id）。
func createModelWithCredential(t *testing.T, srv *httptest.Server, adminToken, modelID string, credID uint) uint {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":      "mock",
		"model_id":      modelID,
		"model_name":    modelID,
		"url":           "https://upstream.example/v1",
		"api_key":       upstreamPlainKey,
		"credential_id": credID,
		"model_type":    "openai-completions",
	})
	assertStatus(t, status, http.StatusCreated, body)
	resp := mustDecode[modelCredResp](t, body)
	if resp.CredentialID != credID {
		t.Fatalf("create response credential_id = %d, want %d, body: %s", resp.CredentialID, credID, body)
	}
	if strings.Contains(string(body), upstreamPlainKey) {
		t.Fatalf("create response echoed api_key plaintext: %s", body)
	}
	return resp.ID
}

// resolveModelKey 经数据层解析密钥命中的目标模型（走真实解析链）。
func resolveModelKey(t *testing.T, store *model.Store, keyID uint) *model.Resolved {
	t.Helper()
	ctx := context.Background()
	key, err := store.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("GetAPIKeyByID: %v", err)
	}
	got, err := store.ResolveModelForKey(ctx, key, "")
	if err != nil {
		t.Fatalf("ResolveModelForKey: %v", err)
	}
	return got
}

// TestCreateModel_WithCredentialRef 覆盖创建模型时引用凭据（>0）与响应字段。
func TestCreateModel_WithCredentialRef(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	cred := createCredential(
		t,
		srv,
		adminToken,
		"cred",
		[]map[string]any{headerSecret("Authorization", "sk-cred")},
		nil,
	)

	modelID := createModelWithCredential(t, srv, adminToken, "gpt-cred", cred.ID)

	got, err := store.GetAIModel(context.Background(), modelID)
	if err != nil {
		t.Fatalf("GetAIModel: %v", err)
	}
	if got.CredentialID != cred.ID {
		t.Fatalf("persisted credential_id = %d, want %d", got.CredentialID, cred.ID)
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/models/"+itoa(modelID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if resp := mustDecode[modelCredResp](t, body); resp.CredentialID != cred.ID {
		t.Fatalf("detail response credential_id = %d, want %d", resp.CredentialID, cred.ID)
	}
}

// TestCreateModel_UnknownCredentialRejected 覆盖创建时引用不存在的凭据 → 400。
func TestCreateModel_UnknownCredentialRejected(t *testing.T) {
	srv, _, _, adminToken := credServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"model_id":      "gpt-bad-cred",
		"url":           "https://upstream.example/v1",
		"model_type":    "openai-completions",
		"credential_id": 999999,
	})
	assertStatus(t, status, http.StatusBadRequest, body)
	if msg := decodeError(t, body).Error.Message; !strings.Contains(msg, "credential") {
		t.Fatalf("400 message does not mention unknown credential: %q", msg)
	}
}

// TestUpdateModel_CredentialRef 覆盖更新模型绑定 / 解绑凭据与非法引用。
func TestUpdateModel_CredentialRef(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	cred := createCredential(
		t,
		srv,
		adminToken,
		"cred",
		[]map[string]any{headerSecret("Authorization", "sk-cred")},
		nil,
	)
	modelID := createModel(t, srv, adminToken, "gpt-update-cred", model.ModelTypeOpenAI)

	ctx := context.Background()
	if m, err := store.GetAIModel(ctx, modelID); err != nil || m.CredentialID != 0 {
		t.Fatalf("initial credential_id = %v (err=%v), want 0", m, err)
	}

	body := map[string]any{"credential_id": cred.ID}
	status, raw := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(modelID), adminToken, body)
	assertStatus(t, status, http.StatusOK, raw)
	if resp := mustDecode[modelCredResp](t, raw); resp.CredentialID != cred.ID {
		t.Fatalf("update response credential_id = %d, want %d", resp.CredentialID, cred.ID)
	}
	if m, err := store.GetAIModel(ctx, modelID); err != nil || m.CredentialID != cred.ID {
		t.Fatalf("persisted credential_id after update = %v (err=%v), want %d", m, err, cred.ID)
	}

	// 非法引用 → 400，且不改变原绑定。
	status, raw = doRequest(
		t,
		srv,
		http.MethodPut,
		"/admin/models/"+itoa(modelID),
		adminToken,
		map[string]any{"credential_id": 999999},
	)
	assertStatus(t, status, http.StatusBadRequest, raw)
	if m, _ := store.GetAIModel(ctx, modelID); m.CredentialID != cred.ID {
		t.Fatalf("invalid update changed credential_id: %d", m.CredentialID)
	}

	// 置 0 表示解除引用。
	status, raw = doRequest(
		t,
		srv,
		http.MethodPut,
		"/admin/models/"+itoa(modelID),
		adminToken,
		map[string]any{"credential_id": 0},
	)
	assertStatus(t, status, http.StatusOK, raw)
	if resp := mustDecode[modelCredResp](t, raw); resp.CredentialID != 0 {
		t.Fatalf("credential_id after unbind = %d, want 0", resp.CredentialID)
	}
}

// TestModelCredential_ResolveLiveAndFallback 覆盖凭据优先、实时生效与禁用回退。
func TestModelCredential_ResolveLiveAndFallback(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	ownerID := seedOwner(t, store)
	cred := createCredential(t, srv, adminToken, "cred", []map[string]any{
		headerSecret("Authorization", "sk-from-credential"),
	}, nil)
	modelID := createModelWithCredential(t, srv, adminToken, "gpt-live", cred.ID)
	key := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary, "sort_order": 0},
	})

	// 凭据优先。
	if got := resolveModelKey(t, store, key.ID); got.APIKey != "sk-from-credential" {
		t.Fatalf("resolved key = %q, want credential value", got.APIKey)
	}

	// 改凭据后立即生效（无快照）。
	status, raw := doRequest(t, srv, http.MethodPut, "/admin/credentials/"+itoa(cred.ID), adminToken, map[string]any{
		"secrets": []map[string]any{headerSecret("Authorization", "sk-credential-v2")},
	})
	assertStatus(t, status, http.StatusOK, raw)
	if got := resolveModelKey(t, store, key.ID); got.APIKey != "sk-credential-v2" {
		t.Fatalf("resolved key = %q, want updated credential value", got.APIKey)
	}

	// 禁用凭据 → 回退模型自身 api_key。
	status, raw = doRequest(
		t,
		srv,
		http.MethodPut,
		"/admin/credentials/"+itoa(cred.ID),
		adminToken,
		map[string]any{"enabled": false},
	)
	assertStatus(t, status, http.StatusOK, raw)
	if got := resolveModelKey(t, store, key.ID); got.APIKey != upstreamPlainKey {
		t.Fatalf("resolved key = %q, want fallback to model api_key", got.APIKey)
	}

	// 重新启用 → 回到凭据值。
	status, raw = doRequest(
		t,
		srv,
		http.MethodPut,
		"/admin/credentials/"+itoa(cred.ID),
		adminToken,
		map[string]any{"enabled": true},
	)
	assertStatus(t, status, http.StatusOK, raw)
	if got := resolveModelKey(t, store, key.ID); got.APIKey != "sk-credential-v2" {
		t.Fatalf("resolved key after re-enable = %q, want credential value", got.APIKey)
	}
}

// TestModelCredential_DecryptFailureFailsRequest 覆盖换主密钥后凭据不可解 → 解析报错（不静默回退）。
func TestModelCredential_DecryptFailureFailsRequest(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	withTestMasterKey(t, testMasterKey)
	ownerID := seedOwner(t, store)
	cred := createCredential(t, srv, adminToken, "cred-enc", []map[string]any{
		headerSecret("Authorization", "sk-encrypted-cred"),
	}, nil)
	modelID := createModelWithCredential(t, srv, adminToken, "gpt-enc", cred.ID)
	key := createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary, "sort_order": 0},
	})
	if got := resolveModelKey(t, store, key.ID); got.APIKey != "sk-encrypted-cred" {
		t.Fatalf("resolved key = %q, want decrypted credential value", got.APIKey)
	}

	// 更换主密钥：凭据与模型 api_key 均不可解 → 必须报错而不是回退。
	if err := crypto.Configure("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatalf("configure another master key: %v", err)
	}
	entity, err := store.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetAPIKeyByID: %v", err)
	}
	if got, err := store.ResolveModelForKey(context.Background(), entity, ""); err == nil {
		t.Fatalf("resolve after key rotation want error, got success: %+v", got)
	}
}

// TestCreateModel_APIKeyEncryptedAtRest 覆盖模型 api_key 的落库密文（直读原始列）。
func TestCreateModel_APIKeyEncryptedAtRest(t *testing.T) {
	srv, _, dsn, adminToken := credServer(t)
	withTestMasterKey(t, testMasterKey)

	modelID := createModel(t, srv, adminToken, "gpt-enc-row", model.ModelTypeOpenAI)

	db := openRawDB(t, dsn)
	var row model.AIModel
	if err := db.First(&row, modelID).Error; err != nil {
		t.Fatalf("read raw model row: %v", err)
	}
	if !strings.HasPrefix(row.APIKey, crypto.CiphertextPrefix) {
		t.Fatalf("persisted api_key not ciphertext: %q", row.APIKey)
	}
	if strings.Contains(row.APIKey, upstreamPlainKey) {
		t.Fatalf("plaintext found in persisted ciphertext: %q", row.APIKey)
	}
}
