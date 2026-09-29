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

	"aigate/internal/model"
)

// TestUsage_UserIsolation 覆盖 user 角色的行级隔离：只能看到自己，伪造 user_id 被忽略。
func TestUsage_UserIsolation(t *testing.T) {
	srv, store := newTestServer(t)
	uidA, tokenA, uidB, _ := seedTwoUsersUsage(t, store)

	t.Run("logs return only self", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/logs", tokenA, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[usageLogsResp](t, body)
		if got.Total != 2 {
			t.Fatalf("total: want 2 (self only), got %d, response: %s", got.Total, body)
		}
		for _, l := range got.Logs {
			if l.UserID != uidA {
				t.Errorf("should not return another user's data, got user_id=%d", l.UserID)
			}
		}
	})

	t.Run("forged user_id ignored", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/logs?user_id="+itoa(uidB), tokenA, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[usageLogsResp](t, body)
		if got.Total != 2 {
			t.Errorf("forged user_id should be ignored, total: want 2, got %d", got.Total)
		}
		for _, l := range got.Logs {
			if l.UserID != uidA {
				t.Errorf("should not return another user's data, got user_id=%d", l.UserID)
			}
		}
	})

	t.Run("summary returns only self", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/summary?group_by=user", tokenA, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[usageSummaryResp](t, body)
		var total int64
		for _, row := range got.Rows {
			if row.UserID != uidA {
				t.Errorf("summary should not include another user, got user_id=%d", row.UserID)
			}
			total += row.TotalTokens
		}
		if total != 150 {
			t.Errorf("own usage total: want 150, got %d", total)
		}
	})
}

// TestUsage_AdminCanQueryAnyUser 覆盖管理员可查询任意用户，缺省查询自己。
func TestUsage_AdminCanQueryAnyUser(t *testing.T) {
	srv, store := newTestServer(t)
	_, _, uidB, _ := seedTwoUsersUsage(t, store)
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/logs?user_id="+itoa(uidB), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[usageLogsResp](t, body)
	if got.Total != 1 {
		t.Fatalf("admin query another's logs total: want 1, got %d, response: %s", got.Total, body)
	}
	if got.Logs[0].UserID != uidB {
		t.Errorf("should return target user %d data, got %d", uidB, got.Logs[0].UserID)
	}

	t.Run("defaults to querying self", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/logs", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[usageLogsResp](t, body); got.Total != 0 {
			t.Errorf("admin with no usage should default to self (0 rows), got %d", got.Total)
		}
	})
}

// TestUsage_ByModel 覆盖 /api/user/usage/by-model：固定按模型聚合。
func TestUsage_ByModel(t *testing.T) {
	srv, store := newTestServer(t)
	_, tokenA, _, _ := seedTwoUsersUsage(t, store)

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/usage/by-model", tokenA, nil)
	assertStatus(t, status, http.StatusOK, body)

	got := mustDecode[usageSummaryResp](t, body)
	if got.GroupBy != "model" {
		t.Errorf("group_by: want model, got %q", got.GroupBy)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("by-model aggregate rows: want 2, got %d, response: %s", len(got.Rows), body)
	}
	byModel := make(map[uint]int64)
	for _, row := range got.Rows {
		byModel[row.AIModelID] = row.TotalTokens
	}
	if byModel[31] != 100 || byModel[32] != 50 {
		t.Errorf("by-model summary mismatch, got %+v", byModel)
	}
}

// TestUsage_InvalidDate400 覆盖 start/end 解析失败返回 400，合法区间正常返回。
func TestUsage_InvalidDate400(t *testing.T) {
	srv, store := newTestServer(t)
	_, tokenA, _, _ := seedTwoUsersUsage(t, store)

	invalids := []string{
		"/api/user/usage/summary?start=2026/01/01",
		"/api/user/usage/logs?end=bad",
		"/api/user/usage/by-model?start=20260101",
	}
	for _, path := range invalids {
		status, body := doRequest(t, srv, http.MethodGet, path, tokenA, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d, response: %s", path, status, body)
		}
	}

	status, body := doRequest(
		t,
		srv,
		http.MethodGet,
		"/api/user/usage/logs?start=2020-01-01&end=2099-12-31",
		tokenA,
		nil,
	)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[usageLogsResp](t, body); got.Total != 2 {
		t.Errorf("valid range should return own 2 logs, got %d", got.Total)
	}
}

// TestUsage_RequiresToken 覆盖用量接口鉴权：无 token 401。
func TestUsage_RequiresToken(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{
		"/api/user/usage/summary",
		"/api/user/usage/logs",
		"/api/user/usage/by-model",
	} {
		status, body := doRequest(t, srv, http.MethodGet, path, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d, response: %s", path, status, body)
		}
	}
}
