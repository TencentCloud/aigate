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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"aigate/internal/admin"
	"aigate/internal/crypto"
	"aigate/internal/model"
)

// credServer 在自建 SQLite 文件库上装配管理面服务，并回传 DSN 便于直读原始列。
func credServer(t *testing.T) (*httptest.Server, *model.Store, string, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "cred.db")
	store, err := model.Open(model.DBTypeSQLite, dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mux := http.NewServeMux()
	admin.Register(mux, store)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, token := seedAdmin(t, store)
	return srv, store, dsn, token
}

// openRawDB 打开指向同一 SQLite 文件的第二条连接（直读原始列，不经过解密逻辑）。
func openRawDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open raw db connection: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get raw db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// withTestMasterKey 启用主密钥并在测试结束时恢复禁用态。
func withTestMasterKey(t *testing.T, key string) {
	t.Helper()
	if err := crypto.Configure(key); err != nil {
		t.Fatalf("configure master key: %v", err)
	}
	t.Cleanup(func() { _ = crypto.Configure("") })
}

const testMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// credentialResp 是凭据接口的响应结构（仅测试使用）。
type credentialResp struct {
	ID          uint               `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Secrets     []model.SecretItem `json:"secrets"`
	Enabled     bool               `json:"enabled"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
}

type credentialListResp struct {
	Credentials []credentialResp `json:"credentials"`
}

// createCredential 经管理面创建凭据并返回创建响应。
func createCredential(
	t *testing.T,
	srv *httptest.Server,
	adminToken, name string,
	secrets []map[string]any,
	extra map[string]any,
) credentialResp {
	t.Helper()
	body := map[string]any{"name": name, "secrets": secrets}
	for k, v := range extra {
		body[k] = v
	}
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/credentials", adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	return mustDecode[credentialResp](t, raw)
}

// headerSecret 构造一条 header 密钥项。
func headerSecret(key, value string) map[string]any {
	return map[string]any{"key": key, "value": value, "location": "header"}
}

// TestCredentials_CRUD 覆盖凭据 CRUD 全链路与掩码返回。
func TestCredentials_CRUD(t *testing.T) {
	srv, _, _, adminToken := credServer(t)

	created := createCredential(t, srv, adminToken, "legacy", []map[string]any{
		headerSecret("Authorization", "sk-1234567890"),
	}, map[string]any{"description": "legacy export"})
	if created.ID == 0 {
		t.Fatal("create response missing id")
	}
	if created.Name != "legacy" || created.Description != "legacy export" || !created.Enabled {
		t.Fatalf("create response = %+v", created)
	}
	if len(created.Secrets) != 1 || created.Secrets[0].Value != "sk*********90" {
		t.Fatalf("create response should return masked secrets: %+v", created.Secrets)
	}
	if created.Secrets[0].Key != "Authorization" || created.Secrets[0].Location != model.SecretLocationHeader {
		t.Fatalf("masked item should keep key/location: %+v", created.Secrets[0])
	}

	status, raw := doRequest(t, srv, http.MethodGet, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got := mustDecode[credentialResp](t, raw)
	if got.ID != created.ID || got.Name != "legacy" {
		t.Fatalf("detail response = %+v", got)
	}

	createCredential(t, srv, adminToken, "second", []map[string]any{
		headerSecret("X-Api-Key", "abcdef"),
	}, nil)
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/credentials", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	list := mustDecode[credentialListResp](t, raw)
	if len(list.Credentials) != 2 || list.Credentials[0].ID != created.ID {
		t.Fatalf("list response = %+v", list.Credentials)
	}

	status, raw = doRequest(t, srv, http.MethodPut, "/admin/credentials/"+itoa(created.ID), adminToken, map[string]any{
		"name":    "legacy-renamed",
		"enabled": false,
		"secrets": []map[string]any{headerSecret("Authorization", "sk-rotated-secret")},
	})
	assertStatus(t, status, http.StatusOK, raw)
	updated := mustDecode[credentialResp](t, raw)
	if updated.Name != "legacy-renamed" || updated.Enabled {
		t.Fatalf("update response = %+v", updated)
	}
	if updated.Secrets[0].Value != "sk*************et" {
		t.Fatalf("mask after update = %+v", updated.Secrets)
	}

	status, raw = doRequest(t, srv, http.MethodDelete, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if ok := mustDecode[okResp](t, raw); !ok.OK {
		t.Fatalf("delete response = %s", raw)
	}
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, raw)
}

// TestCredentials_NoPlaintextEcho 覆盖响应中绝不出现明文密钥值。
func TestCredentials_NoPlaintextEcho(t *testing.T) {
	srv, _, _, adminToken := credServer(t)
	const plain = "sk-super-secret-value"
	created := createCredential(t, srv, adminToken, "masked", []map[string]any{
		headerSecret("Authorization", plain),
	}, nil)

	for _, path := range []string{"/admin/credentials", "/admin/credentials/" + itoa(created.ID)} {
		status, raw := doRequest(t, srv, http.MethodGet, path, adminToken, nil)
		assertStatus(t, status, http.StatusOK, raw)
		if strings.Contains(string(raw), plain) {
			t.Fatalf("%s response echoed plaintext secret: %s", path, raw)
		}
	}
	// 短值全部掩码。
	short := createCredential(t, srv, adminToken, "short", []map[string]any{
		headerSecret("X", "abcd"),
	}, nil)
	if short.Secrets[0].Value != "****" {
		t.Fatalf("short value mask = %q, want ****", short.Secrets[0].Value)
	}
}

// TestCredentials_EncryptedAtRest 覆盖配置主密钥后落库为密文。
func TestCredentials_EncryptedAtRest(t *testing.T) {
	srv, store, dsn, adminToken := credServer(t)
	withTestMasterKey(t, testMasterKey)
	const plain = "sk-encrypted-at-rest"

	created := createCredential(t, srv, adminToken, "enc", []map[string]any{
		headerSecret("Authorization", plain),
	}, nil)

	// 接口读回掩码；数据层读回明文。
	got, err := store.GetCredentialByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	if v, err := got.FirstHeaderSecretValue(); err != nil || v != plain {
		t.Fatalf("FirstHeaderSecretValue = (%q, %v), want decrypted plaintext", v, err)
	}

	// 直读原始列：必须是 enc:v1: 前缀密文且不含明文。
	db := openRawDB(t, dsn)
	var row model.Credential
	if err := db.First(&row, created.ID).Error; err != nil {
		t.Fatalf("read raw credential row: %v", err)
	}
	if !strings.HasPrefix(row.SecretsJSON, crypto.CiphertextPrefix) {
		t.Fatalf("persisted secrets_json not ciphertext: %q", row.SecretsJSON)
	}
	if strings.Contains(row.SecretsJSON, plain) {
		t.Fatalf("plaintext found in persisted ciphertext: %q", row.SecretsJSON)
	}
}

// TestCredentials_ValidationErrors 覆盖入参校验与不存在分支。
func TestCredentials_ValidationErrors(t *testing.T) {
	srv, _, _, adminToken := credServer(t)
	existing := createCredential(t, srv, adminToken, "existing", []map[string]any{headerSecret("A", "v")}, nil)

	tests := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{
			name:   "create-missing-name",
			method: http.MethodPost,
			path:   "/admin/credentials",
			body:   map[string]any{"secrets": []map[string]any{headerSecret("A", "v")}},
			want:   http.StatusBadRequest,
		},
		{
			name:   "create-secret-missing-key",
			method: http.MethodPost,
			path:   "/admin/credentials",
			body:   map[string]any{"name": "c", "secrets": []map[string]any{{"value": "v", "location": "header"}}},
			want:   http.StatusBadRequest,
		},
		{
			name:   "create-unsupported-location",
			method: http.MethodPost,
			path:   "/admin/credentials",
			body: map[string]any{
				"name":    "c",
				"secrets": []map[string]any{{"key": "k", "value": "v", "location": "query"}},
			},
			want: http.StatusBadRequest,
		},
		{
			name:   "create-invalid-body",
			method: http.MethodPost,
			path:   "/admin/credentials",
			body:   "not-json",
			want:   http.StatusBadRequest,
		},
		{name: "get-unknown", method: http.MethodGet, path: "/admin/credentials/999999", want: http.StatusNotFound},
		{name: "get-invalid-id", method: http.MethodGet, path: "/admin/credentials/0", want: http.StatusBadRequest},
		{
			name:   "update-unknown",
			method: http.MethodPut,
			path:   "/admin/credentials/999999",
			body:   map[string]any{"name": "x"},
			want:   http.StatusNotFound,
		},
		{
			name:   "update-empty-name",
			method: http.MethodPut,
			path:   "/admin/credentials/" + itoa(existing.ID),
			body:   map[string]any{"name": "  "},
			want:   http.StatusBadRequest,
		},
		{
			name:   "update-unsupported-location",
			method: http.MethodPut,
			path:   "/admin/credentials/" + itoa(existing.ID),
			body:   map[string]any{"secrets": []map[string]any{{"key": "k", "value": "v", "location": "body"}}},
			want:   http.StatusBadRequest,
		},
		{
			name:   "delete-unknown",
			method: http.MethodDelete,
			path:   "/admin/credentials/999999",
			want:   http.StatusNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, raw := doRequest(t, srv, tt.method, tt.path, adminToken, tt.body)
			assertStatus(t, status, tt.want, raw)
		})
	}
}

// TestCredentials_CreateDisabled 覆盖显式 enabled=false 落库（绕过 GORM 默认值跳过）。
func TestCredentials_CreateDisabled(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	created := createCredential(t, srv, adminToken, "disabled", []map[string]any{headerSecret("A", "v")},
		map[string]any{"enabled": false})
	if created.Enabled {
		t.Fatalf("create response enabled should be false: %+v", created)
	}
	stored, err := store.GetCredentialByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	if stored.Enabled {
		t.Fatal("explicit false not persisted")
	}
}

// TestCredentials_EmptySecrets 覆盖无密钥项凭据（secrets 为空数组/省略）。
func TestCredentials_EmptySecrets(t *testing.T) {
	srv, _, _, adminToken := credServer(t)
	created := createCredential(t, srv, adminToken, "empty", nil, nil)
	if len(created.Secrets) != 0 {
		t.Fatalf("empty credential secrets = %+v, want empty array", created.Secrets)
	}
	status, raw := doRequest(t, srv, http.MethodGet, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	secrets, ok := decoded["secrets"].([]any)
	if !ok || len(secrets) != 0 {
		t.Fatalf("empty credential secrets want [], got %v", decoded["secrets"])
	}
}

// TestCredentials_DeleteReferencedConflict 覆盖被模型引用时删除返回 409。
func TestCredentials_DeleteReferencedConflict(t *testing.T) {
	srv, store, _, adminToken := credServer(t)
	ownerID := seedOwner(t, store)
	created := createCredential(t, srv, adminToken, "referenced", []map[string]any{
		headerSecret("Authorization", "sk-ref"),
	}, nil)

	modelID := createModelWithCredential(t, srv, adminToken, "gpt-ref", created.ID)
	_ = createKey(t, srv, adminToken, ownerID, []map[string]any{
		{"ai_model_id": modelID, "role": model.ModelRolePrimary, "sort_order": 0},
	})

	status, raw := doRequest(t, srv, http.MethodDelete, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusConflict, raw)
	if msg := decodeError(t, raw).Error.Message; !strings.Contains(msg, "referenced") {
		t.Fatalf("409 message does not mention reference: %q", msg)
	}

	// 解除引用后可删除。
	status, raw = doRequest(
		t,
		srv,
		http.MethodPut,
		"/admin/models/"+itoa(modelID),
		adminToken,
		map[string]any{"credential_id": 0},
	)
	assertStatus(t, status, http.StatusOK, raw)
	status, raw = doRequest(t, srv, http.MethodDelete, "/admin/credentials/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
}

// TestCredentials_RoutesRequireAdmin 断言凭据路由同样挂在 RequireAdmin 之后。
func TestCredentials_RoutesRequireAdmin(t *testing.T) {
	srv, store := newTestServer(t)
	_, userToken := seedUser(t, store, "alice", model.RoleUser, true)

	requests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/credentials"},
		{http.MethodGet, "/admin/credentials"},
		{http.MethodGet, "/admin/credentials/1"},
		{http.MethodPut, "/admin/credentials/1"},
		{http.MethodDelete, "/admin/credentials/1"},
	}
	for _, req := range requests {
		status, raw := doRequest(t, srv, req.method, req.path, userToken, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s %s want 403, got %d, body: %s", req.method, req.path, status, raw)
		}
	}
}
