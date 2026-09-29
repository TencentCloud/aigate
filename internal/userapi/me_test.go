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
	"bytes"
	"net/http"
	"testing"

	"aigate/internal/model"
)

// TestMe_RequiresToken 覆盖用户面鉴权：无 token / 坏 token / 禁用用户均 401，普通用户放行。
func TestMe_RequiresToken(t *testing.T) {
	srv, store := newTestServer(t)
	_, token := seedUser(t, store, "alice", model.RoleUser, 0, true)
	_, disabledToken := seedUser(t, store, "bob", model.RoleUser, 0, false)

	cases := []struct {
		name   string
		bearer string
		want   int
	}{
		{name: "no token", bearer: "", want: http.StatusUnauthorized},
		{name: "bad token", bearer: "garbage", want: http.StatusUnauthorized},
		{name: "disabled user", bearer: disabledToken, want: http.StatusUnauthorized},
		{name: "valid user", bearer: token, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", tc.bearer, nil)
			assertStatus(t, status, tc.want, body)
		})
	}
}

// TestMe_ReturnsIdentityWithoutSecrets 覆盖个人信息：字段齐全、含分组名、不含 token 哈希。
func TestMe_ReturnsIdentityWithoutSecrets(t *testing.T) {
	srv, store := newTestServer(t)
	gid := seedGroup(t, store, "team-a", "[]")
	uid, token := seedUser(t, store, "alice", model.RoleUser, gid, true)

	status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", token, nil)
	assertStatus(t, status, http.StatusOK, body)

	got := mustDecode[meResp](t, body)
	if got.ID != uid {
		t.Errorf("id: want %d, got %d", uid, got.ID)
	}
	if got.Username != "alice" || got.Role != model.RoleUser {
		t.Errorf("name/role mismatch, got %+v", got)
	}
	if got.GroupID != gid || got.GroupName != "team-a" {
		t.Errorf("group info: want %d/team-a, got %d/%q", gid, got.GroupID, got.GroupName)
	}
	if bytes.Contains(body, []byte("api_token_hash")) {
		t.Errorf("response should not contain api_token_hash: %s", body)
	}

	t.Run("empty group name when no group", func(t *testing.T) {
		_, token := seedUser(t, store, "carol", model.RoleUser, 0, true)
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", token, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[meResp](t, body)
		if got.GroupID != 0 || got.GroupName != "" {
			t.Errorf("no group: want group_id=0 and empty group_name, got %+v", got)
		}
	})

	t.Run("empty group name when group dangling", func(t *testing.T) {
		_, token := seedUser(t, store, "dave", model.RoleUser, 4242, true)
		status, body := doRequest(t, srv, http.MethodGet, "/api/user/me", token, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[meResp](t, body)
		if got.GroupID != 4242 || got.GroupName != "" {
			t.Errorf("dangling group: want group_id=4242 and empty group_name, got %+v", got)
		}
	})
}
