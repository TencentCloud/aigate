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

package event

// 投递记录保留：白盒单测（直接注入保留期与清理周期字段）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aigate/internal/model"
)

// TestDispatcher_PrunesExpiredDeliveries 覆盖投递器启动即清一次的行为：
// 超出保留期的记录被清掉，保留期内的记录不受影响；周期性清理同样生效。
func TestDispatcher_PrunesExpiredDeliveries(t *testing.T) {
	store := newTestStore(t)
	hook := mustCreateWebhook(t, store, "hook", "http://127.0.0.1:1/never", "", "[]", true)

	now := time.Now().UTC()
	old := &model.WebhookDelivery{
		WebhookID: hook.ID,
		EventType: "request.completed",
		Payload:   "{}",
		Status:    model.DeliveryStatusSuccess,
		CreatedAt: now.Add(-2 * time.Hour),
	}
	fresh := &model.WebhookDelivery{
		WebhookID: hook.ID,
		EventType: "request.completed",
		Payload:   "{}",
		Status:    model.DeliveryStatusSuccess,
		CreatedAt: now.Add(-time.Minute),
	}
	for _, d := range []*model.WebhookDelivery{old, fresh} {
		if err := store.CreateWebhookDelivery(context.Background(), d); err != nil {
			t.Fatalf("CreateWebhookDelivery: %v", err)
		}
	}

	d := NewDispatcher(store, 4)
	d.retention = time.Hour                 // 保留 1 小时（白盒注入）
	d.pruneInterval = 20 * time.Millisecond // 缩短周期，覆盖周期性清理路径
	d.Start()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.Stop(stopCtx)
	})

	waitFor(t, "expired delivery pruned", 2*time.Second, func() error {
		rows, total, err := store.ListWebhookDeliveries(context.Background(), hook.ID, 1, 10)
		if err != nil {
			return err
		}
		if total != 1 || len(rows) != 1 || rows[0].ID != fresh.ID {
			return fmt.Errorf("remaining total=%d rows=%d, want only fresh delivery", total, len(rows))
		}
		return nil
	})
}

// TestDispatcher_RetentionDisabled 覆盖保留期 ≤ 0 时不启动清理：
// 超出任意时间长度的历史记录保持不变。
func TestDispatcher_RetentionDisabled(t *testing.T) {
	store := newTestStore(t)
	hook := mustCreateWebhook(t, store, "hook", "http://127.0.0.1:1/never", "", "[]", true)

	old := &model.WebhookDelivery{
		WebhookID: hook.ID,
		EventType: "request.completed",
		Payload:   "{}",
		Status:    model.DeliveryStatusSuccess,
		CreatedAt: time.Now().UTC().Add(-1000 * time.Hour),
	}
	if err := store.CreateWebhookDelivery(context.Background(), old); err != nil {
		t.Fatalf("CreateWebhookDelivery: %v", err)
	}

	d := NewDispatcher(store, 4)
	d.SetDeliveryRetention(0) // 显式关闭（负数同样归一为 0）
	d.pruneInterval = 10 * time.Millisecond
	d.Start()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.Stop(stopCtx)
	})

	time.Sleep(80 * time.Millisecond)
	_, total, err := store.ListWebhookDeliveries(context.Background(), hook.ID, 1, 10)
	if err != nil {
		t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	if total != 1 {
		t.Fatalf("retention=0 must keep all deliveries, total=%d want 1", total)
	}
}
