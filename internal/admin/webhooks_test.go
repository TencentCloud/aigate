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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 响应结构（仅测试使用）
// ---------------------------------------------------------------------------

type webhookResp struct {
	ID         uint     `json:"id"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Secret     string   `json:"secret"`
	EventTypes []string `json:"event_types"`
	Enabled    bool     `json:"enabled"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

type webhookListResp struct {
	Webhooks []webhookResp `json:"webhooks"`
}

type deliveryResp struct {
	ID         uint   `json:"id"`
	WebhookID  uint   `json:"webhook_id"`
	EventType  string `json:"event_type"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	LastStatus int    `json:"last_status"`
	LastError  string `json:"last_error"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type deliveryListResp struct {
	Deliveries []deliveryResp `json:"deliveries"`
	Total      int64          `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// createWebhook 经管理面创建订阅并断言 201。
func createWebhook(t *testing.T, srv *httptest.Server, token string, payload map[string]any) webhookResp {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/webhooks", token, payload)
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[webhookResp](t, body)
}

// seedDeliveries 直接经数据层写入 count 条投递记录（created_at 依次递增）。
func seedDeliveries(t *testing.T, store *model.Store, webhookID uint, count int) []uint {
	t.Helper()
	base := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	ids := make([]uint, 0, count)
	for i := 0; i < count; i++ {
		d := &model.WebhookDelivery{
			WebhookID: webhookID,
			EventType: "request.completed",
			Payload:   `{"id":"x"}`,
			Status:    model.DeliveryStatusSuccess,
			Attempts:  1,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := store.CreateWebhookDelivery(context.Background(), d); err != nil {
			t.Fatalf("seed delivery: %v", err)
		}
		ids = append(ids, d.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestWebhookCRUD 覆盖订阅 CRUD、secret 掩码与落库原值。
func TestWebhookCRUD(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	const plainSecret = "s3cret-value"

	var created webhookResp

	t.Run("create returns 201 with masked secret", func(t *testing.T) {
		created = createWebhook(t, srv, adminToken, map[string]any{
			"name":        "hook-a",
			"url":         "https://hooks.example/events",
			"secret":      plainSecret,
			"event_types": []string{"request.completed"},
		})
		if created.ID == 0 {
			t.Fatal("create response missing id")
		}
		if created.Secret == plainSecret {
			t.Fatalf("create response echoed secret plaintext: %q", created.Secret)
		}
		if want := model.MaskSecret(plainSecret); created.Secret != want {
			t.Errorf("secret mask want %q, got %q", want, created.Secret)
		}
		if len(created.EventTypes) != 1 || created.EventTypes[0] != "request.completed" {
			t.Errorf("event_types mismatch: %+v", created.EventTypes)
		}
		if !created.Enabled {
			t.Error("new subscription should be enabled by default")
		}

		// 数据库保存原值（掩码只发生在响应侧）。
		row, err := store.GetWebhook(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("get webhook from store: %v", err)
		}
		if row.Secret != plainSecret {
			t.Errorf("persisted secret want original %q, got %q", plainSecret, row.Secret)
		}
	})

	t.Run("detail and list do not echo plaintext", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/webhooks/"+itoa(created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[webhookResp](t, body)
		if got.Secret != model.MaskSecret(plainSecret) {
			t.Errorf("detail secret mask mismatch: %q", got.Secret)
		}

		status, body = doRequest(t, srv, http.MethodGet, "/admin/webhooks", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		list := mustDecode[webhookListResp](t, body)
		if len(list.Webhooks) != 1 || list.Webhooks[0].ID != created.ID {
			t.Fatalf("list mismatch: %+v", list.Webhooks)
		}
		if bytes.Contains(body, []byte(plainSecret)) {
			t.Errorf("list response echoed secret plaintext: %s", body)
		}
	})

	t.Run("update name and enable state", func(t *testing.T) {
		status, body := doRequest(
			t,
			srv,
			http.MethodPut,
			"/admin/webhooks/"+itoa(created.ID),
			adminToken,
			map[string]any{
				"name":    "hook-renamed",
				"enabled": false,
			},
		)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[webhookResp](t, body)
		if got.Name != "hook-renamed" || got.Enabled {
			t.Errorf("update result mismatch: %+v", got)
		}
		if got.Secret != model.MaskSecret(plainSecret) {
			t.Errorf("mask should stay when secret not updated, got %q", got.Secret)
		}
	})

	t.Run("get after delete is 404", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodDelete, "/admin/webhooks/"+itoa(created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if !mustDecode[okResp](t, body).OK {
			t.Errorf("delete response should be ok=true, body: %s", body)
		}

		status, body = doRequest(t, srv, http.MethodGet, "/admin/webhooks/"+itoa(created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	})
}

// TestWebhookUpdateSecretSemantics 覆盖 PUT 的 secret 语义：空串保持不变、非空串覆盖。
func TestWebhookUpdateSecretSemantics(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)

	created := createWebhook(t, srv, adminToken, map[string]any{
		"name":   "hook-secret",
		"url":    "https://hooks.example/events",
		"secret": "orig-secret",
	})

	status, body := doRequest(t, srv, http.MethodPut, "/admin/webhooks/"+itoa(created.ID), adminToken, map[string]any{
		"secret": "",
	})
	assertStatus(t, status, http.StatusOK, body)
	row, err := store.GetWebhook(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if row.Secret != "orig-secret" {
		t.Errorf("empty string secret should stay unchanged, got %q", row.Secret)
	}

	status, body = doRequest(t, srv, http.MethodPut, "/admin/webhooks/"+itoa(created.ID), adminToken, map[string]any{
		"secret": "rotated-secret",
	})
	assertStatus(t, status, http.StatusOK, body)
	row, err = store.GetWebhook(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if row.Secret != "rotated-secret" {
		t.Errorf("non-empty secret should overwrite, got %q", row.Secret)
	}
	if got := mustDecode[webhookResp](t, body).Secret; got != model.MaskSecret("rotated-secret") {
		t.Errorf("update response secret mask mismatch: %q", got)
	}
}

// TestWebhookURLValidation 覆盖 url 仅接受 http/https（创建与更新）。
func TestWebhookURLValidation(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	for _, bad := range []string{"", "   ", "ftp://hooks.example/events", "hooks.example/events", "http://"} {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/webhooks", adminToken, map[string]any{
			"name": "bad-url",
			"url":  bad,
		})
		if status != http.StatusBadRequest {
			t.Errorf("create url=%q want 400, got %d, body: %s", bad, status, body)
		}
		if !bytes.Contains(body, []byte("http")) {
			t.Errorf("400 message should mention http(s), body: %s", body)
		}
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/webhooks", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if list := mustDecode[webhookListResp](t, body); len(list.Webhooks) != 0 {
		t.Errorf("invalid url should not persist, got: %+v", list.Webhooks)
	}

	// 更新路径同样校验，失败时原值不变。
	created := createWebhook(t, srv, adminToken, map[string]any{
		"name": "hook-url",
		"url":  "https://hooks.example/events",
	})
	status, body = doRequest(t, srv, http.MethodPut, "/admin/webhooks/"+itoa(created.ID), adminToken, map[string]any{
		"url": "not-a-url",
	})
	assertStatus(t, status, http.StatusBadRequest, body)

	status, body = doRequest(t, srv, http.MethodGet, "/admin/webhooks/"+itoa(created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[webhookResp](t, body); got.URL != "https://hooks.example/events" {
		t.Errorf("url should stay unchanged after rejected update, got %q", got.URL)
	}
}

// TestWebhookEventTypesValidation 覆盖 event_types 的数组与元素类型校验。
func TestWebhookEventTypesValidation(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	cases := []struct {
		name    string
		payload any
	}{
		{"element not a string", []any{"request.completed", 1}},
		{"empty string element", []string{"request.completed", "  "}},
		{"not an array", "request.completed"},
	}
	for _, tc := range cases {
		status, body := doRequest(t, srv, http.MethodPost, "/admin/webhooks", adminToken, map[string]any{
			"name":        "bad-events",
			"url":         "https://hooks.example/events",
			"event_types": tc.payload,
		})
		if status != http.StatusBadRequest {
			t.Errorf("%s want 400, got %d, body: %s", tc.name, status, body)
		}
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/webhooks", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if list := mustDecode[webhookListResp](t, body); len(list.Webhooks) != 0 {
		t.Errorf("invalid event_types should not persist, got: %+v", list.Webhooks)
	}

	// 省略 event_types = 订阅全部（响应为空数组）。
	created := createWebhook(t, srv, adminToken, map[string]any{
		"name": "hook-all",
		"url":  "https://hooks.example/events",
	})
	if created.EventTypes == nil || len(created.EventTypes) != 0 {
		t.Errorf("omitted event_types should return empty array, got %+v", created.EventTypes)
	}

	// 更新为显式列表。
	status, body = doRequest(t, srv, http.MethodPut, "/admin/webhooks/"+itoa(created.ID), adminToken, map[string]any{
		"event_types": []string{"quota.exceeded"},
	})
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[webhookResp](t, body); len(got.EventTypes) != 1 || got.EventTypes[0] != "quota.exceeded" {
		t.Errorf("event_types update mismatch: %+v", got.EventTypes)
	}
}

// TestWebhookDeliveryPagination 覆盖投递记录分页（默认值、上限、倒序）。
func TestWebhookDeliveryPagination(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)

	created := createWebhook(t, srv, adminToken, map[string]any{
		"name": "hook-deliveries",
		"url":  "https://hooks.example/events",
	})
	ids := seedDeliveries(t, store, created.ID, 3)

	path := "/admin/webhooks/" + itoa(created.ID) + "/deliveries"

	t.Run("default pagination is 1/50", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, path, adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[deliveryListResp](t, body)
		if got.Total != 3 || got.Page != 1 || got.PageSize != 50 || len(got.Deliveries) != 3 {
			t.Fatalf("default pagination mismatch: %+v", got)
		}
		if got.Deliveries[0].ID != ids[2] {
			t.Errorf(
				"should be sorted by time desc (newest first), first row id want %d, got %d",
				ids[2],
				got.Deliveries[0].ID,
			)
		}
		if got.Deliveries[0].WebhookID != created.ID || got.Deliveries[0].Status != model.DeliveryStatusSuccess {
			t.Errorf("delivery row fields mismatch: %+v", got.Deliveries[0])
		}
		if got.Deliveries[0].EventType != "request.completed" || got.Deliveries[0].Attempts != 1 {
			t.Errorf("delivery row event_type/attempts mismatch: %+v", got.Deliveries[0])
		}
		if got.Deliveries[0].CreatedAt == "" || got.Deliveries[0].UpdatedAt == "" {
			t.Errorf("delivery row should contain time fields: %+v", got.Deliveries[0])
		}
	})

	t.Run("pagination returns corresponding records", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, path+"?page=1&page_size=2", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		page1 := mustDecode[deliveryListResp](t, body)
		if page1.Total != 3 || page1.Page != 1 || page1.PageSize != 2 || len(page1.Deliveries) != 2 {
			t.Fatalf("page 1 mismatch: %+v", page1)
		}
		if page1.Deliveries[0].ID != ids[2] || page1.Deliveries[1].ID != ids[1] {
			t.Errorf("page 1 order mismatch: %+v", page1.Deliveries)
		}

		status, body = doRequest(t, srv, http.MethodGet, path+"?page=2&page_size=2", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		page2 := mustDecode[deliveryListResp](t, body)
		if len(page2.Deliveries) != 1 || page2.Deliveries[0].ID != ids[0] {
			t.Errorf("page 2 mismatch: %+v", page2.Deliveries)
		}
	})

	t.Run("page_size upper limit 500 and invalid pagination returns 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, path+"?page_size=9999", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[deliveryListResp](t, body); got.PageSize != 500 {
			t.Errorf("page_size upper limit want 500, got %d", got.PageSize)
		}

		for _, q := range []string{"?page=0", "?page=abc", "?page_size=0", "?page_size=-1"} {
			status, body := doRequest(t, srv, http.MethodGet, path+q, adminToken, nil)
			if status != http.StatusBadRequest {
				t.Errorf("pagination param %q want 400, got %d, body: %s", q, status, body)
			}
		}
	})

	t.Run("no deliveries returns empty array", func(t *testing.T) {
		other := createWebhook(t, srv, adminToken, map[string]any{
			"name": "hook-empty",
			"url":  "https://hooks.example/other",
		})
		status, body := doRequest(
			t,
			srv,
			http.MethodGet,
			"/admin/webhooks/"+itoa(other.ID)+"/deliveries",
			adminToken,
			nil,
		)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[deliveryListResp](t, body)
		if got.Total != 0 || len(got.Deliveries) != 0 {
			t.Errorf("no deliveries should be empty: %+v", got)
		}
	})
}

// TestWebhookErrors 覆盖 404（不存在）与 400（非法 id）以及未鉴权访问。
func TestWebhookErrors(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	for _, tc := range []struct {
		method string
		path   string
		body   any
		want   int
	}{
		{http.MethodGet, "/admin/webhooks/9999", nil, http.StatusNotFound},
		{http.MethodPut, "/admin/webhooks/9999", map[string]any{"name": "x"}, http.StatusNotFound},
		{http.MethodDelete, "/admin/webhooks/9999", nil, http.StatusNotFound},
		{http.MethodGet, "/admin/webhooks/9999/deliveries", nil, http.StatusNotFound},
		{http.MethodGet, "/admin/webhooks/abc", nil, http.StatusBadRequest},
		{http.MethodGet, "/admin/webhooks/0", nil, http.StatusBadRequest},
	} {
		status, body := doRequest(t, srv, tc.method, tc.path, adminToken, tc.body)
		if status != tc.want {
			t.Errorf("%s %s want %d, got %d, body: %s", tc.method, tc.path, tc.want, status, body)
		}
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/webhooks", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unauthenticated access want 401, got %d, body: %s", status, body)
	}
}
