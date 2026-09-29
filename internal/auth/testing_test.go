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

// Package auth_test 以黑盒方式测试鉴权中间件：经 httptest 走真实 HTTP，仅依赖导出接口。
package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("auto migrate test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedUser 直接经 model 层写入用户并返回其 ID 与明文 token。
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

// doRequest 发送请求并以原文设置 Authorization 头（空串表示不带该头）。
func doRequest(t *testing.T, srv *httptest.Server, method, path, authorization string, body any) (int, []byte) {
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

// decodeError 按统一错误体解析响应。
func decodeError(t *testing.T, body []byte) httpx.ErrorBody {
	t.Helper()
	return mustDecode[httpx.ErrorBody](t, body)
}
