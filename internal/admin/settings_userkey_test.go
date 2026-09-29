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
	"net/http"
	"testing"
)

// settingsUserKeyResp 在既有 settingsResp 上补充 user_key_limit 字段。
type settingsUserKeyResp struct {
	settingsResp
	UserKeyLimit int `json:"user_key_limit"`
}

// TestSettings_UserKeyLimit 覆盖 user_key_limit 的默认值、更新生效、0=不限与负数拒绝。
func TestSettings_UserKeyLimit(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	t.Run("default 5", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsUserKeyResp](t, body).UserKeyLimit; got != 5 {
			t.Fatalf("user_key_limit default want 5, got %d", got)
		}
	})

	t.Run("update takes effect and reads back", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"user_key_limit": 1,
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsUserKeyResp](t, body).UserKeyLimit; got != 1 {
			t.Fatalf("update response user_key_limit want 1, got %d", got)
		}

		status, body = doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsUserKeyResp](t, body).UserKeyLimit; got != 1 {
			t.Fatalf("read-back user_key_limit want 1, got %d", got)
		}
	})

	t.Run("0 means unlimited and is valid", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"user_key_limit": 0,
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsUserKeyResp](t, body).UserKeyLimit; got != 0 {
			t.Fatalf("user_key_limit want 0, got %d", got)
		}
	})

	t.Run("negative rejected and original value unchanged", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"user_key_limit": -1,
		})
		assertStatus(t, status, http.StatusBadRequest, body)

		status, body = doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsUserKeyResp](t, body).UserKeyLimit; got != 0 {
			t.Fatalf("user_key_limit after invalid update want 0, got %d", got)
		}
	})
}
