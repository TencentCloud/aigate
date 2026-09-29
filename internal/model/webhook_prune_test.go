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

// 投递记录保留单测：过期删除（含遗留 pending）、保留期内不动、边界严格小于。

import (
	"testing"
	"time"

	"aigate/internal/model"
)

// TestPruneWebhookDeliveries 覆盖 PruneWebhookDeliveries 的删除与保留边界。
func TestPruneWebhookDeliveries(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	hook := &model.Webhook{Name: "h", URL: "https://example.com/hook", Secret: "s", EventTypes: "[]", Enabled: true}
	if err := store.CreateWebhook(ctx, hook); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	mk := func(created time.Time, status string) *model.WebhookDelivery {
		d := &model.WebhookDelivery{
			WebhookID: hook.ID, EventType: "request.completed", Payload: "{}",
			Status: status, CreatedAt: created,
		}
		if err := store.CreateWebhookDelivery(ctx, d); err != nil {
			t.Fatalf("CreateWebhookDelivery(%s): %v", status, err)
		}
		return d
	}
	mk(now.Add(-72*time.Hour), model.DeliveryStatusSuccess) // 过期
	mk(now.Add(-72*time.Hour), model.DeliveryStatusPending) // 过期（遗留 pending 也清理）
	fresh := mk(now.Add(-1*time.Hour), model.DeliveryStatusSuccess)

	n, err := store.PruneWebhookDeliveries(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneWebhookDeliveries: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned = %d, want 2", n)
	}

	rows, total, err := store.ListWebhookDeliveries(ctx, hook.ID, 1, 50)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != fresh.ID {
		t.Fatalf("remaining total=%d rows=%+v, want only fresh delivery", total, rows)
	}

	// 边界：created_at 恰好等于 cutoff 的保留（判定为严格小于）。
	n2, err := store.PruneWebhookDeliveries(ctx, rows[0].CreatedAt)
	if err != nil {
		t.Fatalf("PruneWebhookDeliveries(boundary): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("boundary prune removed %d rows, want 0", n2)
	}
}
