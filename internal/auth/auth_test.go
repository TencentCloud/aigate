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

package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aigate/internal/auth"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// echoUser 是挂在中间件后的桩 handler：把 context 中的当前用户回显为 JSON。
func echoUser(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, "user missing from context")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": u.ID, "role": u.Role})
}

// newAuthServer 用指定中间件包住桩 handler 起一个测试服务。
func newAuthServer(
	t *testing.T,
	store *model.Store,
	middleware func(*model.Store, http.Handler) http.Handler,
) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(middleware(store, http.HandlerFunc(echoUser)))
	t.Cleanup(srv.Close)
	return srv
}

// TestRequireAdmin_TokenAndRole 覆盖管理面鉴权：无/坏 token 401、非管理员 403、禁用 401、有效管理员放行。
func TestRequireAdmin_TokenAndRole(t *testing.T) {
	store := newTestStore(t)
	_, adminToken := seedAdmin(t, store)
	_, userToken := seedUser(t, store, "alice", model.RoleUser, true)
	_, disabledAdminToken := seedUser(t, store, "bob", model.RoleAdmin, false)
	srv := newAuthServer(t, store, auth.RequireAdmin)

	cases := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing Authorization", authorization: "", wantStatus: http.StatusUnauthorized},
		{
			name:          "Bearer but token not found",
			authorization: "Bearer no-such-token",
			wantStatus:    http.StatusUnauthorized,
		},
		{name: "invalid scheme", authorization: "Token " + adminToken, wantStatus: http.StatusUnauthorized},
		{name: "empty after Bearer", authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "regular user token", authorization: "Bearer " + userToken, wantStatus: http.StatusForbidden},
		{name: "disabled admin", authorization: "Bearer " + disabledAdminToken, wantStatus: http.StatusUnauthorized},
		{name: "valid admin", authorization: "Bearer " + adminToken, wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodGet, "/admin/anything", tc.authorization, nil)
			assertStatus(t, status, tc.wantStatus, body)
			if tc.wantStatus != http.StatusOK {
				if eb := decodeError(t, body); eb.Error.Code != tc.wantStatus {
					t.Errorf("error body code: want %d, got %d, response: %s", tc.wantStatus, eb.Error.Code, body)
				}
			}
		})
	}
}

// TestRequireUser_AllowsAdminAndUser 覆盖用户面鉴权：user/admin 均可通过且 context 注入当前用户。
func TestRequireUser_AllowsAdminAndUser(t *testing.T) {
	store := newTestStore(t)
	_, adminToken := seedAdmin(t, store)
	userID, userToken := seedUser(t, store, "alice", model.RoleUser, true)
	_, disabledToken := seedUser(t, store, "bob", model.RoleUser, false)
	srv := newAuthServer(t, store, auth.RequireUser)

	t.Run("missing token 401", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", "", nil)
		assertStatus(t, status, http.StatusUnauthorized, body)
	})

	t.Run("bad token 401", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", "Bearer garbage", nil)
		assertStatus(t, status, http.StatusUnauthorized, body)
	})

	t.Run("disabled user 401", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", "Bearer "+disabledToken, nil)
		assertStatus(t, status, http.StatusUnauthorized, body)
	})

	t.Run("regular user allowed and injected", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", "Bearer "+userToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[struct {
			ID   uint   `json:"id"`
			Role string `json:"role"`
		}](t, body)
		if got.ID != userID {
			t.Errorf("context user id: want %d, got %d", userID, got.ID)
		}
		if got.Role != model.RoleUser {
			t.Errorf("context role: want %q, got %q", model.RoleUser, got.Role)
		}
	})

	t.Run("admin also allowed", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", "Bearer "+adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
	})
}
