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

// Package userapi_test 是用户面 API 的黑盒测试：经 userapi.Register + httptest 走真实 HTTP，
// 只依赖导出接口；造数经 model 包与 admin 面完成。
package userapi_test

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
	"time"

	"aigate/internal/admin"
	"aigate/internal/model"
	"aigate/internal/userapi"
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
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("migrate test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestServer 建库并装配管理员面 + 用户面的真实 HTTP 服务。
func newTestServer(t *testing.T) (*httptest.Server, *model.Store) {
	t.Helper()
	store := newTestStore(t)
	mux := http.NewServeMux()
	admin.Register(mux, store)
	userapi.Register(mux, store)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

// seedUser 直接经 model 层写入用户并返回其 ID 与明文 token。
func seedUser(t *testing.T, store *model.Store, name, role string, groupID uint, enabled bool) (uint, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	u := &model.User{
		Username:       name,
		Role:           role,
		GroupID:        groupID,
		APITokenHash:   hash,
		APITokenPrefix: prefix,
		Enabled:        true,
	}
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

// seedGroup 写入一个分组（rules 为空串表示未配置策略）。
func seedGroup(t *testing.T, store *model.Store, name, rules string) uint {
	t.Helper()
	g := &model.Group{Name: name, TokenQuotaRules: rules}
	if err := store.CreateGroup(context.Background(), g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g.ID
}

// seedUsage 写入一条用量日志并同步累计日聚合（聚合查询以日聚合表为数据源）。
func seedUsage(t *testing.T, store *model.Store, userID, keyID, groupID, modelID uint, total int, createdAt time.Time) {
	t.Helper()
	entry := &model.LLMUsageLog{
		UserID:       userID,
		APIKeyID:     keyID,
		GroupID:      groupID,
		AIModelID:    modelID,
		Model:        "test-model",
		Provider:     "mock",
		PromptTokens: total,
		TotalTokens:  total,
		StatusCode:   http.StatusOK,
		Latency:      42,
		CreatedAt:    createdAt,
	}
	if err := store.InsertUsageLog(context.Background(), entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	if err := store.UpsertDailyUsage(context.Background(), entry); err != nil {
		t.Fatalf("upsert daily usage: %v", err)
	}
}

// seedTwoUsersUsage 造两个普通用户与各自用量：
//
//	A: model31=100、model32=50；B: model31=30
func seedTwoUsersUsage(t *testing.T, store *model.Store) (uidA uint, tokenA string, uidB uint, tokenB string) {
	t.Helper()
	uidA, tokenA = seedUser(t, store, "alice", model.RoleUser, 0, true)
	uidB, tokenB = seedUser(t, store, "bob", model.RoleUser, 0, true)
	now := time.Now().UTC()
	seedUsage(t, store, uidA, 11, 0, 31, 100, now)
	seedUsage(t, store, uidA, 11, 0, 32, 50, now.Add(-time.Minute))
	seedUsage(t, store, uidB, 12, 0, 31, 30, now.Add(-2*time.Minute))
	return uidA, tokenA, uidB, tokenB
}

// ---------------------------------------------------------------------------
// HTTP 辅助
// ---------------------------------------------------------------------------

// doRequest 发送请求；bearer 非空时附加 `Authorization: Bearer <token>`。
func doRequest(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) (int, []byte) {
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
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
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
		t.Fatalf("status: want %d, got %d, response: %s", want, got, body)
	}
}

// mustDecode 解析 JSON 响应。
func mustDecode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode response: %v, response: %s", err, body)
	}
	return v
}

// ---------------------------------------------------------------------------
// 响应结构（仅测试使用）
// ---------------------------------------------------------------------------

type meResp struct {
	ID        uint   `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	GroupID   uint   `json:"group_id"`
	GroupName string `json:"group_name"`
}

type ruleUsageResp struct {
	RuleIndex   int    `json:"rule_index"`
	Mode        string `json:"mode"`
	Limit       int    `json:"limit"`
	Used        int64  `json:"used"`
	Active      bool   `json:"active"`
	PeriodStart *int64 `json:"period_start"`
	PeriodEnd   *int64 `json:"period_end"`
}

type quotaResp struct {
	UserID uint            `json:"user_id"`
	Usages []ruleUsageResp `json:"usages"`
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
