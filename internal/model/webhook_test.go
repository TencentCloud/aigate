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

package model_test

import (
	"testing"
	"time"

	"aigate/internal/model"
)

// newWebhook 构造一个订阅全部事件的启用订阅（未入库）。
func newWebhook(name string) *model.Webhook {
	return &model.Webhook{Name: name, URL: "https://hooks.example/events", Secret: "s3cret", Enabled: true}
}

// TestWebhookCRUD 覆盖订阅的创建 / 查询 / 更新 / 删除，以及 event_types 的归一化与解析。
func TestWebhookCRUD(t *testing.T) {
	store := newTestStore(t)

	w := newWebhook("hook-a")
	w.EventTypes = "" // 空 → 落库为 "[]"（订阅全部）
	if err := store.CreateWebhook(testCtx(), w); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if w.ID == 0 {
		t.Fatal("create webhook: id not backfilled")
	}
	if w.EventTypes != "[]" {
		t.Errorf("empty event_types should persist as %q, got %q", "[]", w.EventTypes)
	}
	types, err := w.GetEventTypes()
	if err != nil {
		t.Fatalf("get event types: %v", err)
	}
	if len(types) != 0 {
		t.Errorf("empty event_types should parse to an empty list, got %v", types)
	}

	// secret 以原值落库（掩码只发生在管理面响应）。
	got, err := store.GetWebhook(testCtx(), w.ID)
	if err != nil {
		t.Fatalf("get webhook: %v", err)
	}
	if got.Secret != "s3cret" || got.URL != w.URL || !got.Enabled {
		t.Errorf("get webhook fields mismatch: %+v", got)
	}

	// 更新：event_types。
	updated, err := store.UpdateWebhook(testCtx(), w.ID, map[string]any{
		"event_types": `["quota.exceeded"]`,
	})
	if err != nil {
		t.Fatalf("update webhook: %v", err)
	}
	types, err = updated.GetEventTypes()
	if err != nil {
		t.Fatalf("get event types after update: %v", err)
	}
	if len(types) != 1 || types[0] != "quota.exceeded" {
		t.Errorf("event_types = %v, want [quota.exceeded]", types)
	}
	if updated.Subscribes("request.completed") {
		t.Error("event_types=[quota.exceeded] must not subscribe to request.completed")
	}
	if !updated.Subscribes("quota.exceeded") {
		t.Error("event_types=[quota.exceeded] must subscribe to quota.exceeded")
	}

	// 更新：停用后不再订阅任何事件。
	updated, err = store.UpdateWebhook(testCtx(), w.ID, map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("disable webhook: %v", err)
	}
	if updated.Enabled {
		t.Error("update webhook: enabled should be false")
	}
	if updated.Subscribes("quota.exceeded") {
		t.Error("disabled webhook must not receive any event")
	}

	list, err := store.ListWebhooks(testCtx())
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(list) != 1 || list[0].ID != w.ID {
		t.Fatalf("list webhooks mismatch: %+v", list)
	}

	if err := store.DeleteWebhook(testCtx(), w.ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if _, err := store.GetWebhook(testCtx(), w.ID); !model.IsNotFound(err) {
		t.Errorf("get after delete should return not found, got %v", err)
	}
}

// TestWebhookGetEventTypesInvalid 覆盖 event_types 非法 JSON：解析报错且不参与投递。
func TestWebhookGetEventTypesInvalid(t *testing.T) {
	w := &model.Webhook{ID: 7, EventTypes: "{not-json"}
	if _, err := w.GetEventTypes(); err == nil {
		t.Fatal("invalid event_types should return an error")
	}
	if w.Subscribes("request.completed") {
		t.Error("event_types parse failure must not subscribe to any event")
	}
}

// TestWebhookDeleteCascade 覆盖删除订阅时级联清理其投递记录。
func TestWebhookDeleteCascade(t *testing.T) {
	store := newTestStore(t)

	target := newWebhook("hook-target")
	other := newWebhook("hook-other")
	for _, w := range []*model.Webhook{target, other} {
		if err := store.CreateWebhook(testCtx(), w); err != nil {
			t.Fatalf("create webhook: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		mustCreateDelivery(t, store, target.ID, "request.completed")
	}
	mustCreateDelivery(t, store, other.ID, "request.completed")

	if err := store.DeleteWebhook(testCtx(), target.ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}

	rows, total, err := store.ListWebhookDeliveries(testCtx(), target.ID, 1, 50)
	if err != nil {
		t.Fatalf("list deliveries after delete: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("cascade delete must leave no deliveries, got total=%d rows=%d", total, len(rows))
	}
	_, otherTotal, err := store.ListWebhookDeliveries(testCtx(), other.ID, 1, 50)
	if err != nil {
		t.Fatalf("list other deliveries: %v", err)
	}
	if otherTotal != 1 {
		t.Errorf("other webhook's deliveries must not be deleted, got total=%d", otherTotal)
	}
}

// TestListWebhookDeliveriesPagination 覆盖投递记录按时间倒序分页。
func TestListWebhookDeliveriesPagination(t *testing.T) {
	store := newTestStore(t)

	w := newWebhook("hook-page")
	if err := store.CreateWebhook(testCtx(), w); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	base := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		d := &model.WebhookDelivery{
			WebhookID: w.ID,
			EventType: "request.completed",
			Payload:   `{"id":"1"}`,
			Status:    model.DeliveryStatusPending,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := store.CreateWebhookDelivery(testCtx(), d); err != nil {
			t.Fatalf("create delivery: %v", err)
		}
	}

	page1, total, err := store.ListWebhookDeliveries(testCtx(), w.ID, 1, 2)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1: want 2 rows, got %d", len(page1))
	}
	// 最新创建的排在最前（i=4 → i=3）。
	if !page1[0].CreatedAt.Equal(base.Add(4*time.Minute)) || !page1[1].CreatedAt.Equal(base.Add(3*time.Minute)) {
		t.Errorf("page 1 should be the two newest in desc order, got %v / %v", page1[0].CreatedAt, page1[1].CreatedAt)
	}

	page3, _, err := store.ListWebhookDeliveries(testCtx(), w.ID, 3, 2)
	if err != nil {
		t.Fatalf("list page 3: %v", err)
	}
	if len(page3) != 1 || !page3[0].CreatedAt.Equal(base) {
		t.Errorf("page 3 should be the oldest row, got %+v", page3)
	}

	// 越界页返回空列表（total 仍为全量）。
	page9, total9, err := store.ListWebhookDeliveries(testCtx(), w.ID, 9, 2)
	if err != nil {
		t.Fatalf("list page 9: %v", err)
	}
	if len(page9) != 0 || total9 != 5 {
		t.Errorf("out-of-range page should be empty with total=5, got rows=%d total=%d", len(page9), total9)
	}
}

// TestListEnabledWebhooks 覆盖 enabled 过滤与投递记录的终态更新。
func TestListEnabledWebhooks(t *testing.T) {
	store := newTestStore(t)

	on := newWebhook("hook-on")
	off := newWebhook("hook-off")
	for _, w := range []*model.Webhook{on, off} {
		if err := store.CreateWebhook(testCtx(), w); err != nil {
			t.Fatalf("create webhook: %v", err)
		}
	}
	if _, err := store.UpdateWebhook(testCtx(), off.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable webhook: %v", err)
	}

	enabled, err := store.ListEnabledWebhooks(testCtx())
	if err != nil {
		t.Fatalf("list enabled webhooks: %v", err)
	}
	if len(enabled) != 1 || enabled[0].ID != on.ID {
		t.Fatalf("enabled filter mismatch: %+v", enabled)
	}

	d := mustCreateDelivery(t, store, on.ID, "request.completed")
	updated, err := store.UpdateWebhookDelivery(testCtx(), d.ID, map[string]any{
		"status":      model.DeliveryStatusFailed,
		"attempts":    3,
		"last_status": 500,
		"last_error":  "unexpected status 500",
	})
	if err != nil {
		t.Fatalf("update delivery: %v", err)
	}
	if updated.Status != model.DeliveryStatusFailed || updated.Attempts != 3 || updated.LastStatus != 500 {
		t.Errorf("delivery terminal state mismatch: %+v", updated)
	}
	if updated.LastError != "unexpected status 500" {
		t.Errorf("last_error should keep the last error, got %q", updated.LastError)
	}
}

// mustCreateDelivery 落一条 pending 投递记录并返回实体。
func mustCreateDelivery(t *testing.T, store *model.Store, webhookID uint, eventType string) *model.WebhookDelivery {
	t.Helper()
	d := &model.WebhookDelivery{
		WebhookID: webhookID,
		EventType: eventType,
		Payload:   `{"id":"1"}`,
		Status:    model.DeliveryStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateWebhookDelivery(testCtx(), d); err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	return d
}
