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

// 设计稿缺口补齐（close-design-gaps）：单密钥用量趋势端点覆盖。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"aigate/internal/model"
)

// mustSeedKey 入库一个属于 userID 的密钥并返回 key id。
func mustSeedKey(t *testing.T, store *model.Store, userID uint) uint {
	t.Helper()
	_, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate key token: %v", err)
	}
	k := &model.APIKey{Name: "usage-key", UserID: userID, ProxyTokenHash: hash, ProxyTokenPrefix: prefix, Enabled: true}
	if err := store.CreateAPIKey(context.Background(), k, nil); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return k.ID
}

type keyUsageResp struct {
	KeyID   uint   `json:"key_id"`
	GroupBy string `json:"group_by"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Rows    []struct {
		Date         string `json:"date"`
		TotalTokens  int64  `json:"total_tokens"`
		RequestCount int64  `json:"request_count"`
	} `json:"rows"`
}

// TestUserKeyUsage_Trend：按日聚合、单 key 隔离、默认 30 天区间与显式区间。
func TestUserKeyUsage_Trend(t *testing.T) {
	srv, store := newTestServer(t)
	uid, token := seedUser(t, store, "carol", model.RoleUser, 0, true)
	keyID := mustSeedKey(t, store, uid)
	otherKeyID := mustSeedKey(t, store, uid)

	now := time.Now().UTC()
	todayLabel := model.LocalToday().Format("2006-01-02")
	yesterdayLabel := model.LocalToday().AddDate(0, 0, -1).Format("2006-01-02")

	seedUsage(t, store, uid, keyID, 0, 31, 100, now)
	seedUsage(t, store, uid, keyID, 0, 31, 50, now)       // 同日第二笔 → 合并累计
	seedUsage(t, store, uid, otherKeyID, 0, 31, 999, now) // 其他密钥 → 不得计入

	// 默认区间（最近 30 天）：今天一行（100+50 合并、2 次请求）。
	status, body := doRequest(t, srv, http.MethodGet, "/api/user/keys/"+itoa(keyID)+"/usage", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	resp := mustDecode[keyUsageResp](t, body)
	if resp.KeyID != keyID || resp.GroupBy != "date" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.End != todayLabel || resp.Start == "" || resp.Start > todayLabel {
		t.Fatalf("default range = [%s, %s], want end=%s and start<=end", resp.Start, resp.End, todayLabel)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (%+v)", len(resp.Rows), resp.Rows)
	}
	if row := resp.Rows[0]; row.Date[:10] != todayLabel || row.TotalTokens != 150 || row.RequestCount != 2 {
		t.Fatalf("row = %+v, want today total=150 requests=2", row)
	}

	// 显式区间覆盖昨天（无数据）→ 空结果；覆盖今天 → 命中。
	status, body = doRequest(t, srv, http.MethodGet,
		"/api/user/keys/"+itoa(keyID)+"/usage?start="+yesterdayLabel+"&end="+yesterdayLabel, token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if resp = mustDecode[keyUsageResp](t, body); len(resp.Rows) != 0 {
		t.Fatalf("yesterday rows = %+v, want empty", resp.Rows)
	}
	status, body = doRequest(t, srv, http.MethodGet,
		"/api/user/keys/"+itoa(keyID)+"/usage?start="+todayLabel+"&end="+todayLabel, token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if resp = mustDecode[keyUsageResp](t, body); len(resp.Rows) != 1 || resp.Rows[0].TotalTokens != 150 {
		t.Fatalf("today rows = %+v, want 150", resp.Rows)
	}

	// 其他用户访问 → 404（不泄露存在性）。
	_, tokenB := seedUser(t, store, "dave", model.RoleUser, 0, true)
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys/"+itoa(keyID)+"/usage", tokenB, nil)
	assertStatus(t, status, http.StatusNotFound, body)

	// admin 指定 user_id 可查任意密钥。
	_, adminToken := seedUser(t, store, "root", model.RoleAdmin, 0, true)
	status, body = doRequest(t, srv, http.MethodGet,
		"/api/user/keys/"+itoa(keyID)+"/usage?user_id="+itoa(uid), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[keyUsageResp](t, body); got.KeyID != keyID || len(got.Rows) != 1 {
		t.Fatalf("admin view = %+v, want 1 row for key %d", got, keyID)
	}

	// 不存在的密钥 id → 404。
	status, body = doRequest(t, srv, http.MethodGet, "/api/user/keys/99999/usage", token, nil)
	assertStatus(t, status, http.StatusNotFound, body)
}
