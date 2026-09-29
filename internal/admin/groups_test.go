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
	"net/http/httptest"
	"testing"
)

// createGroup 经管理面创建分组并返回创建响应。
func createGroup(t *testing.T, srv *httptest.Server, adminToken, name string, rules any) groupResp {
	t.Helper()
	body := map[string]any{"name": name}
	if rules != nil {
		body["token_quota_rules"] = rules
	}
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/user-groups", adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	return mustDecode[groupResp](t, raw)
}

// TestCreateGroup_QuotaRulesSemantics 覆盖组策略写入语义：
// 省略/空串 = 未配置（回退全局默认）；"[]" = 显式不限；合法规则经校验规范化后落库；非法 → 400。
func TestCreateGroup_QuotaRulesSemantics(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	cases := []struct {
		name     string
		rules    any
		wantCode int
		wantRule string
	}{
		{name: "omitted rules means unconfigured", rules: nil, wantCode: http.StatusCreated, wantRule: ""},
		{name: "empty string means unconfigured", rules: "", wantCode: http.StatusCreated, wantRule: ""},
		{name: "empty array means explicitly unlimited", rules: "[]", wantCode: http.StatusCreated, wantRule: "[]"},
		{
			name:     "valid rules normalized and persisted",
			rules:    `[{"mode":"month","limit":1000000}]`,
			wantCode: http.StatusCreated,
			wantRule: `[{"mode":"month","limit":1000000}]`,
		},
		{name: "invalid JSON", rules: "not-json", wantCode: http.StatusBadRequest},
		{name: "non-array object", rules: "{}", wantCode: http.StatusBadRequest},
		{name: "null", rules: "null", wantCode: http.StatusBadRequest},
		{
			name:     "duplicate mode",
			rules:    `[{"mode":"day","limit":1},{"mode":"day","limit":2}]`,
			wantCode: http.StatusBadRequest,
		},
		{name: "limit less than -1", rules: `[{"mode":"day","limit":-2}]`, wantCode: http.StatusBadRequest},
		{name: "invalid mode", rules: `[{"mode":"week","limit":1}]`, wantCode: http.StatusBadRequest},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createGroupExpect(t, srv, adminToken, "g"+itoa(uint(i)), tc.rules, tc.wantCode, tc.wantRule)
		})
	}
}

// createGroupExpect 按期望状态码创建分组，并在成功时校验落库的规则文本。
func createGroupExpect(
	t *testing.T,
	srv *httptest.Server,
	adminToken, name string,
	rules any,
	wantCode int,
	wantRule string,
) {
	t.Helper()
	body := map[string]any{"name": name}
	if rules != nil {
		body["token_quota_rules"] = rules
	}
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/user-groups", adminToken, body)
	assertStatus(t, status, wantCode, raw)
	if wantCode != http.StatusCreated {
		return
	}
	got := mustDecode[groupResp](t, raw)
	if got.ID == 0 {
		t.Fatalf("create response missing id, body: %s", raw)
	}
	if got.TokenQuotaRules != wantRule {
		t.Errorf("token_quota_rules want %q, got %q", wantRule, got.TokenQuotaRules)
	}
}

// TestGroups_CRUD 覆盖分组的读取/局部更新/删除与不存在时的 404。
func TestGroups_CRUD(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)
	const initialRules = `[{"mode":"day","limit":1000}]`
	g := createGroup(t, srv, adminToken, "team-a", initialRules)

	t.Run("list contains the new group", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/user-groups", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		lst := mustDecode[groupListResp](t, body)
		found := false
		for _, item := range lst.UserGroups {
			if item.ID == g.ID && item.Name == "team-a" && item.TokenQuotaRules == initialRules {
				found = true
			}
		}
		if !found {
			t.Errorf("list does not contain the new group, body: %s", body)
		}
	})

	t.Run("get by id", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/user-groups/"+itoa(g.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[groupResp](t, body); got.ID != g.ID || got.Name != "team-a" {
			t.Errorf("group content mismatch, body: %s", body)
		}
	})

	t.Run("partial update of name keeps rules", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(g.ID), adminToken, map[string]any{
			"name": "team-b",
		})
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[groupResp](t, body)
		if got.Name != "team-b" || got.TokenQuotaRules != initialRules {
			t.Errorf("name/rules mismatch after partial update, body: %s", body)
		}
	})

	t.Run("update rules to explicitly unlimited", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(g.ID), adminToken, map[string]any{
			"token_quota_rules": "[]",
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[groupResp](t, body); got.TokenQuotaRules != "[]" {
			t.Errorf("token_quota_rules want [], got %q", got.TokenQuotaRules)
		}
	})

	t.Run("update rules to unconfigured", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(g.ID), adminToken, map[string]any{
			"token_quota_rules": "",
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[groupResp](t, body); got.TokenQuotaRules != "" {
			t.Errorf("token_quota_rules want empty string (unconfigured), got %q", got.TokenQuotaRules)
		}
	})

	t.Run("invalid rules update 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/user-groups/"+itoa(g.ID), adminToken, map[string]any{
			"token_quota_rules": "oops",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("unknown group 404", func(t *testing.T) {
		for _, req := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, "/admin/user-groups/9999", nil},
			{http.MethodPut, "/admin/user-groups/9999", map[string]any{"name": "x"}},
			{http.MethodDelete, "/admin/user-groups/9999", nil},
		} {
			status, body := doRequest(t, srv, req.method, req.path, adminToken, req.body)
			if status != http.StatusNotFound {
				t.Errorf("%s %s want 404, got %d, body: %s", req.method, req.path, status, body)
			}
		}
	})

	t.Run("delete returns ok and subsequent get is 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodDelete, "/admin/user-groups/"+itoa(g.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[okResp](t, body); !got.OK {
			t.Errorf("delete response want ok=true, body: %s", body)
		}
		status, body = doRequest(t, srv, http.MethodGet, "/admin/user-groups/"+itoa(g.ID), adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestDeleteGroup_DoesNotCascadeUsers 断言删除分组只删分组本身：
// 引用它的用户 group_id 保留为悬空值，用户不被级联删除。
func TestDeleteGroup_DoesNotCascadeUsers(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)
	g := createGroup(t, srv, adminToken, "team-a", `[{"mode":"day","limit":1000}]`)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/users", adminToken, map[string]any{
		"name":     "alice",
		"group_id": g.ID,
	})
	assertStatus(t, status, http.StatusCreated, body)
	userID := mustDecode[userResp](t, body).ID
	if userID == 0 {
		t.Fatalf("create user response missing id, body: %s", body)
	}

	status, body = doRequest(t, srv, http.MethodDelete, "/admin/user-groups/"+itoa(g.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)

	status, body = doRequest(t, srv, http.MethodGet, "/admin/users", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	found := false
	for _, u := range mustDecode[userListResp](t, body).Users {
		if u.ID == userID {
			found = true
			if u.GroupID != g.ID {
				t.Errorf("user group_id after group delete want %d, got %d", g.ID, u.GroupID)
			}
		}
	}
	if !found {
		t.Error("deleting a group should not cascade delete users")
	}
}
