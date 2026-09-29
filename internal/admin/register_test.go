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

	"aigate/internal/model"
)

// TestAdminRoutes_RequireAdminToken 断言管理面路由整体挂在 RequireAdmin 之后：
// 普通用户 token 访问任一管理面端点均被拒（403）。
func TestAdminRoutes_RequireAdminToken(t *testing.T) {
	srv, store := newTestServer(t)
	_, userToken := seedUser(t, store, "alice", model.RoleUser, true)

	requests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/models"},
		{http.MethodGet, "/admin/keys"},
		{http.MethodGet, "/admin/users"},
		{http.MethodGet, "/admin/user-groups"},
		{http.MethodGet, "/admin/usage/summary"},
		{http.MethodGet, "/admin/usage/logs"},
		{http.MethodGet, "/admin/settings"},
	}
	for _, req := range requests {
		status, body := doRequest(t, srv, req.method, req.path, userToken, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s %s want 403, got %d, body: %s", req.method, req.path, status, body)
		}
	}
}
