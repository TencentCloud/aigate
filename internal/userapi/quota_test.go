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

package userapi_test

import (
	"net/http"
	"testing"
	"time"

	"aigate/internal/model"
)

// TestQuota_UserSeesOwnRules 覆盖本人配额视图：规则来自所属分组，含窗口边界与已用量。
func TestQuota_UserSeesOwnRules(t *testing.T) {
	srv, store := newTestServer(t)
	gid := seedGroup(t, store, "team-a", `[{"mode":"day","limit":1000}]`)
	uid, token := seedUser(t, store, "alice", model.RoleUser, gid, true)
	seedUsage(t, store, uid, 11, gid, 31, 100, time.Now().UTC())

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota", token, nil)
	assertStatus(t, status, http.StatusOK, body)

	got := mustDecode[quotaResp](t, body)
	if got.UserID != uid {
		t.Errorf("user_id: want %d, got %d", uid, got.UserID)
	}
	if len(got.Usages) != 1 {
		t.Fatalf("rule count: want 1, got %d, response: %s", len(got.Usages), body)
	}
	u := got.Usages[0]
	if u.Mode != "day" || u.Limit != 1000 || !u.Active || u.Used != 100 {
		t.Errorf("quota view mismatch, got %+v", u)
	}
	if u.PeriodStart == nil || u.PeriodEnd == nil {
		t.Errorf("active rule should have period start/end, got %+v", u)
	}
}

// TestQuota_UserCannotQueryOthers 覆盖行级隔离：user 角色传他人 user_id 被忽略，仍返回自己。
func TestQuota_UserCannotQueryOthers(t *testing.T) {
	srv, store := newTestServer(t)
	gidA := seedGroup(t, store, "team-a", `[{"mode":"day","limit":1000}]`)
	gidB := seedGroup(t, store, "team-b", `[{"mode":"month","limit":2000}]`)
	uidA, tokenA := seedUser(t, store, "alice", model.RoleUser, gidA, true)
	uidB, _ := seedUser(t, store, "bob", model.RoleUser, gidB, true)

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota?user_id="+itoa(uidB), tokenA, nil)
	assertStatus(t, status, http.StatusOK, body)

	got := mustDecode[quotaResp](t, body)
	if got.UserID != uidA {
		t.Fatalf("user role querying another should be ignored and return self (%d), got %d", uidA, got.UserID)
	}
	if len(got.Usages) != 1 || got.Usages[0].Mode != "day" {
		t.Errorf("should return rules of own group, got %+v", got.Usages)
	}
}

// TestQuota_AdminQueriesAnyUser 覆盖管理员可用 ?user_id= 查询任意用户，缺省查询自己。
func TestQuota_AdminQueriesAnyUser(t *testing.T) {
	srv, store := newTestServer(t)
	gid := seedGroup(t, store, "team-b", `[{"mode":"month","limit":2000}]`)
	uidB, _ := seedUser(t, store, "bob", model.RoleUser, gid, true)
	adminID, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota?user_id="+itoa(uidB), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[quotaResp](t, body)
	if got.UserID != uidB || len(got.Usages) != 1 || got.Usages[0].Mode != "month" {
		t.Errorf("admin querying another's quota mismatch, got %+v", got)
	}

	t.Run("defaults to querying self", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[quotaResp](t, body); got.UserID != adminID {
			t.Errorf("default should query self (%d), got %d", adminID, got.UserID)
		}
	})

	t.Run("invalid user_id returns 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota?user_id=abc", adminToken, nil)
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("nonexistent user_id returns 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/quota?user_id=9999", adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}
