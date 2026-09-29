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

// Package admin_test 是管理面 API 的黑盒测试：经 admin.Register + httptest 走真实 HTTP，
// 只依赖导出接口；需要直查数据库时经 model 包 API 断言。
package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"aigate/internal/admin"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// newTestStore 建一个临时 SQLite 文件库并完成建表。
func newTestStore(t *testing.T) *model.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	store, err := model.Open(model.DBTypeSQLite, path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestServer 建库并装配真实 HTTP 服务。
func newTestServer(t *testing.T) (*httptest.Server, *model.Store) {
	t.Helper()
	store := newTestStore(t)
	mux := http.NewServeMux()
	admin.Register(mux, store)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

// newAdminServer 在测试服务中预置一个管理员用户，返回其明文 token。
func newAdminServer(t *testing.T) (*httptest.Server, *model.Store, string) {
	t.Helper()
	srv, store := newTestServer(t)
	_, token := seedAdmin(t, store)
	return srv, store, token
}

// seedUser 直接经 model 层写入用户并返回其 ID 与明文 token（用于造鉴权前置）。
func seedUser(t *testing.T, store *model.Store, name, role string, enabled bool) (uint, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	u := &model.User{Username: name, Role: role, APITokenHash: hash, APITokenPrefix: prefix, Enabled: true}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !enabled {
		// enabled 带 default 标签，false 无法直接经 Create 落库，需定向更新。
		if _, err := store.UpdateUser(context.Background(), u.ID, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("disable user: %v", err)
		}
	}
	return u.ID, plain
}

// seedAdmin 写入一个启用的管理员用户。
func seedAdmin(t *testing.T, store *model.Store) (uint, string) {
	t.Helper()
	return seedUser(t, store, "root", model.RoleAdmin, true)
}

// seedOwner 写入一个启用的普通用户（密钥属主）。
func seedOwner(t *testing.T, store *model.Store) uint {
	t.Helper()
	id, _ := seedUser(t, store, "owner", model.RoleUser, true)
	return id
}

// ---------------------------------------------------------------------------
// HTTP 辅助
// ---------------------------------------------------------------------------

// doRequest 发送请求；bearer 非空时附加 `Authorization: Bearer <token>`。
func doRequest(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) (int, []byte) {
	t.Helper()
	auth := ""
	if bearer != "" {
		auth = "Bearer " + bearer
	}
	return doRequestAuth(t, srv, method, path, auth, body)
}

// doRequestAuth 发送请求并以原文设置 Authorization 头（空串表示不带该头）。
func doRequestAuth(t *testing.T, srv *httptest.Server, method, path, authorization string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// itoa 是路径拼接用的十进制 id 转换辅助。
func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// assertStatus 断言状态码，失败时打印响应体。
func assertStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status want %d, got %d, body: %s", want, got, body)
	}
}

// mustDecode 解析 JSON 响应。
func mustDecode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode response: %v, body: %s", err, body)
	}
	return v
}

// decodeError 按统一错误体解析响应。
func decodeError(t *testing.T, body []byte) httpx.ErrorBody {
	t.Helper()
	return mustDecode[httpx.ErrorBody](t, body)
}

// ---------------------------------------------------------------------------
// 响应结构（仅测试使用）
// ---------------------------------------------------------------------------

type modelResp struct {
	ID                uint              `json:"id"`
	Provider          string            `json:"provider"`
	ModelID           string            `json:"model_id"`
	ModelName         string            `json:"model_name"`
	URL               string            `json:"url"`
	ModelType         string            `json:"model_type"`
	CustomHTTPHeaders map[string]string `json:"custom_http_headers"`
	QuotaDay          int               `json:"quota_day"`
	Enabled           bool              `json:"enabled"`
	UserKeyRequired   bool              `json:"user_key_required"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`
}

type modelListResp struct {
	Models []modelResp `json:"models"`
}

type bindingResp struct {
	AIModelID uint   `json:"ai_model_id"`
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
	Role      string `json:"role"`
	SortOrder int    `json:"sort_order"`
	HasKey    bool   `json:"has_key"`
}

type keyResp struct {
	ID               uint          `json:"id"`
	Name             string        `json:"name"`
	UserID           uint          `json:"user_id"`
	GroupID          uint          `json:"group_id"`
	ProxyTokenPrefix string        `json:"proxy_token_prefix"`
	MaxTokens        int           `json:"max_tokens"`
	TokenQuotaRules  string        `json:"token_quota_rules"`
	Enabled          bool          `json:"enabled"`
	ExpiresAt        *string       `json:"expires_at"`
	ExpiredAt        *string       `json:"expired_at"`
	CreatedAt        string        `json:"created_at"`
	UpdatedAt        string        `json:"updated_at"`
	Bindings         []bindingResp `json:"bindings"`
	Token            string        `json:"token"`
}

type keyListResp struct {
	Keys []keyResp `json:"keys"`
}

type userResp struct {
	ID             uint   `json:"id"`
	Username       string `json:"username"`
	Role           string `json:"role"`
	GroupID        uint   `json:"group_id"`
	APITokenPrefix string `json:"api_token_prefix"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	Token          string `json:"token"`
}

type userListResp struct {
	Users []userResp `json:"users"`
}

type settingsResp struct {
	DefaultTokenQuotaRules string `json:"default_token_quota_rules"`
	GlobalTokenQuotaRules  string `json:"global_token_quota_rules"`
	GlobalQuotaScope       string `json:"global_quota_scope"`
}

type groupResp struct {
	ID                    uint   `json:"id"`
	Name                  string `json:"name"`
	TokenQuotaRules       string `json:"token_quota_rules"`
	GlobalTokenQuotaRules string `json:"global_token_quota_rules"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

type groupListResp struct {
	UserGroups []groupResp `json:"user_groups"`
}

type usageSummaryRowResp struct {
	Date                   string `json:"date"`
	UserID                 uint   `json:"user_id"`
	APIKeyID               uint   `json:"api_key_id"`
	AIModelID              uint   `json:"ai_model_id"`
	GroupID                uint   `json:"group_id"`
	PromptTokens           int64  `json:"prompt_tokens"`
	CompletionTokens       int64  `json:"completion_tokens"`
	TotalTokens            int64  `json:"total_tokens"`
	PromptCacheReadTokens  int64  `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int64  `json:"prompt_cache_write_tokens"`
	RequestCount           int64  `json:"request_count"`
}

type usageSummaryResp struct {
	GroupBy string                `json:"group_by"`
	Rows    []usageSummaryRowResp `json:"rows"`
}

type usageLogResp struct {
	ID                     uint   `json:"id"`
	APIKeyID               uint   `json:"api_key_id"`
	UserID                 uint   `json:"user_id"`
	GroupID                uint   `json:"group_id"`
	AIModelID              uint   `json:"ai_model_id"`
	Model                  string `json:"model"`
	Provider               string `json:"provider"`
	PromptTokens           int    `json:"prompt_tokens"`
	CompletionTokens       int    `json:"completion_tokens"`
	TotalTokens            int    `json:"total_tokens"`
	PromptCacheReadTokens  int    `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int    `json:"prompt_cache_write_tokens"`
	StatusCode             int    `json:"status_code"`
	Latency                int    `json:"latency"`
	CreatedAt              string `json:"created_at"`
}

type usageLogsResp struct {
	Logs     []usageLogResp `json:"logs"`
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

type okResp struct {
	OK bool `json:"ok"`
}

// ---------------------------------------------------------------------------
// 业务辅助
// ---------------------------------------------------------------------------

// createModel 经管理面创建模型并返回其 ID。
func createModel(t *testing.T, srv *httptest.Server, adminToken, modelID, modelType string) uint {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   modelID,
		"model_name": modelID,
		"url":        "https://upstream.example/v1",
		"api_key":    "sk-upstream-secret",
		"model_type": modelType,
	})
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[modelResp](t, body).ID
}

// createKey 经管理面创建密钥并返回创建响应。
func createKey(t *testing.T, srv *httptest.Server, adminToken string, ownerID uint, bindings []map[string]any) keyResp {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/keys", adminToken, map[string]any{
		"name":     "test-key",
		"user_id":  ownerID,
		"bindings": bindings,
	})
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[keyResp](t, body)
}
