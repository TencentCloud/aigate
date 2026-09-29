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

// Package event 的白盒单测：签名向量、投递语义（成功 / 重试 / 耗尽）、订阅过滤、
// 队列满丢弃与 Stop 排空。
//
// 夹具说明：每个用例使用独立的临时 SQLite 库；mock 接收端起在 127.0.0.1，
// 因此统一 provider.SetAllowPrivateUpstreams(true) 并在用例结束还原严格模式。
// 退避表与超时被替换为毫秒级/短超时，避免用例真实等待 1s/5s/30s。
package event

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// newTestStore 建一个隔离的临时 SQLite 库并完成建表。
func newTestStore(t *testing.T) *model.Store {
	t.Helper()
	store, err := model.Open(model.DBTypeSQLite, filepath.Join(t.TempDir(), "event-test.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("auto migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// allowPrivateUpstreams 放行回环 mock 接收端（SSRF 默认严格模式会拦截）。
func allowPrivateUpstreams(t *testing.T) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
}

// newDispatcher 组装投递器并注入毫秒级退避与短超时（Start 由用例决定）。
func newDispatcher(t *testing.T, store *model.Store, buffer int) *Dispatcher {
	t.Helper()
	d := NewDispatcher(store, buffer)
	d.backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	d.timeout = 3 * time.Second
	return d
}

// mustCreateWebhook 入库一个订阅；enabled=false 时创建后定向停用。
func mustCreateWebhook(
	t *testing.T,
	store *model.Store,
	name, url, secret, eventTypes string,
	enabled bool,
) *model.Webhook {
	t.Helper()
	w := &model.Webhook{Name: name, URL: url, Secret: secret, EventTypes: eventTypes, Enabled: true}
	if err := store.CreateWebhook(context.Background(), w); err != nil {
		t.Fatalf("create webhook %q: %v", name, err)
	}
	if !enabled {
		updated, err := store.UpdateWebhook(context.Background(), w.ID, map[string]any{"enabled": false})
		if err != nil {
			t.Fatalf("disable webhook %q: %v", name, err)
		}
		return updated
	}
	return w
}

// receivedDelivery 是 mock 接收端记录的一次投递。
type receivedDelivery struct {
	eventType  string
	deliveryID string
	signature  string
	body       []byte
}

// mockReceiver 记录收到的投递，并按 statusFor(call) 决定响应码（nil 表示恒 200）。
type mockReceiver struct {
	mu         sync.Mutex
	deliveries []receivedDelivery
	calls      int
	statusFor  func(call int) int
}

func (m *mockReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.deliveries = append(m.deliveries, receivedDelivery{
		eventType:  r.Header.Get("X-Gateway-Event"),
		deliveryID: r.Header.Get("X-Gateway-Delivery"),
		signature:  r.Header.Get("X-Gateway-Signature"),
		body:       body,
	})
	statusFor := m.statusFor
	m.mu.Unlock()

	status := http.StatusOK
	if statusFor != nil {
		status = statusFor(call)
	}
	w.WriteHeader(status)
}

// snapshot 返回已收到的投递副本。
func (m *mockReceiver) snapshot() []receivedDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]receivedDelivery, len(m.deliveries))
	copy(out, m.deliveries)
	return out
}

// callCount 返回累计收到的请求数。
func (m *mockReceiver) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// startReceiver 启动 mock 接收端并返回其 URL 与记录器。
func startReceiver(t *testing.T, statusFor func(call int) int) (string, *mockReceiver) {
	t.Helper()
	rec := &mockReceiver{statusFor: statusFor}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// waitFor 轮询 probe 直到返回 nil 或超时。
func waitFor(t *testing.T, desc string, timeout time.Duration, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timeout after %s, last error: %v", desc, timeout, lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitDelivery 等待订阅出现指定终态的投递记录并返回它。
func waitDelivery(t *testing.T, store *model.Store, webhookID uint, wantStatus string) *model.WebhookDelivery {
	t.Helper()
	var got *model.WebhookDelivery
	waitFor(t, "delivery status "+wantStatus, 5*time.Second, func() error {
		rows, _, err := store.ListWebhookDeliveries(context.Background(), webhookID, 1, 50)
		if err != nil {
			return err
		}
		states := make([]string, 0, len(rows))
		for i := range rows {
			states = append(states, rows[i].Status)
			if rows[i].Status == wantStatus {
				got = &rows[i]
				return nil
			}
		}
		return fmt.Errorf("deliveries not in state %q: [%s]", wantStatus, strings.Join(states, ","))
	})
	return got
}

// ---------------------------------------------------------------------------
// 签名
// ---------------------------------------------------------------------------

// TestSign 用外部计算的固定向量校验签名实现（不是用被测实现自证）。
func TestSign(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		body   string
		want   string
	}{
		{
			name:   "standard vector",
			secret: "e2e-webhook-secret",
			body:   `{"hello":"world"}`,
			want:   "sha256=4d7dc940e6affd5f8589816e4dc0aa0562208e584ffb8460d6c48eee1a780c46",
		},
		{
			name:   "empty secret and empty body",
			secret: "",
			body:   "",
			want:   "sha256=b613679a0814d9ec772f95d778c35fc5ff1697c493715653c6c712144292c5ad",
		},
		{
			name:   "envelope body",
			secret: "k",
			body:   `{"id":"1","type":"request.completed"}`,
			want:   "sha256=65477a6231dcaefbdc9d0f39d93ed217c21b43f34c35aa69dfd254edec8cec9f",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sign(tc.secret, []byte(tc.body)); got != tc.want {
				t.Errorf("Sign() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 投递语义
// ---------------------------------------------------------------------------

// TestDispatcherSuccessDelivery 覆盖一次成功投递：签名头、投递头、信封 body 与终态记录。
func TestDispatcherSuccessDelivery(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	const secret = "hook-secret"
	url, rec := startReceiver(t, nil)
	hook := mustCreateWebhook(t, store, "hook", url, secret, "[]", true)

	d := newDispatcher(t, store, 8)
	d.Start()
	t.Cleanup(func() { d.Stop(context.Background()) })

	d.Submit(Event{Type: TypeRequestCompleted, Data: map[string]any{"model": "mock-gpt"}})

	row := waitDelivery(t, store, hook.ID, model.DeliveryStatusSuccess)
	if row.Attempts != 1 || row.LastStatus != http.StatusOK || row.LastError != "" {
		t.Errorf("successful delivery record mismatch: %+v", row)
	}
	if row.EventType != TypeRequestCompleted {
		t.Errorf("event_type: want %q, got %q", TypeRequestCompleted, row.EventType)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("receiver: want 1 delivery, got %d", len(got))
	}
	entry := got[0]
	if entry.eventType != TypeRequestCompleted {
		t.Errorf("X-Gateway-Event: want %q, got %q", TypeRequestCompleted, entry.eventType)
	}
	if want := Sign(secret, entry.body); entry.signature != want {
		t.Errorf("X-Gateway-Signature: want %q, got %q", want, entry.signature)
	}
	if entry.deliveryID == "" {
		t.Error("X-Gateway-Delivery is empty")
	}
	if payload := string(row.Payload); payload != string(entry.body) {
		t.Errorf("payload column should match delivery body, column=%s body=%s", payload, entry.body)
	}

	var env struct {
		ID        string         `json:"id"`
		Type      string         `json:"type"`
		CreatedAt time.Time      `json:"created_at"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(entry.body, &env); err != nil {
		t.Fatalf("decode envelope body: %v, body=%s", err, entry.body)
	}
	if env.ID != entry.deliveryID {
		t.Errorf("envelope id should be delivery ID %q, got %q", entry.deliveryID, env.ID)
	}
	if env.Type != TypeRequestCompleted {
		t.Errorf("envelope type: want %q, got %q", TypeRequestCompleted, env.Type)
	}
	if env.Data["model"] != "mock-gpt" {
		t.Errorf("envelope data mismatch: %+v", env.Data)
	}
	if env.CreatedAt.IsZero() || env.CreatedAt.Location() != time.UTC {
		t.Errorf("envelope created_at should be UTC, got %v", env.CreatedAt)
	}
}

// TestDispatcherRetryThenSuccess 覆盖首次 500、重试成功：attempts=2 且终态 success。
func TestDispatcherRetryThenSuccess(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	url, rec := startReceiver(t, func(call int) int {
		if call == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	hook := mustCreateWebhook(t, store, "hook", url, "s", "[]", true)

	d := newDispatcher(t, store, 8)
	d.Start()
	t.Cleanup(func() { d.Stop(context.Background()) })

	d.Submit(Event{Type: TypeRequestCompleted})

	row := waitDelivery(t, store, hook.ID, model.DeliveryStatusSuccess)
	if row.Attempts != 2 {
		t.Errorf("attempts after retry success: want 2, got %d", row.Attempts)
	}
	if rec.callCount() != 2 {
		t.Errorf("receiver: want 2 requests, got %d", rec.callCount())
	}
}

// TestDispatcherRetryExhausted 覆盖持续 500：attempts=3、终态 failed、保留最后一次错误。
func TestDispatcherRetryExhausted(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	url, rec := startReceiver(t, func(int) int { return http.StatusInternalServerError })
	hook := mustCreateWebhook(t, store, "hook", url, "s", "[]", true)

	d := newDispatcher(t, store, 8)
	d.Start()
	t.Cleanup(func() { d.Stop(context.Background()) })

	d.Submit(Event{Type: TypeRequestCompleted})

	row := waitDelivery(t, store, hook.ID, model.DeliveryStatusFailed)
	if row.Attempts != 3 {
		t.Errorf("attempts after exhaustion: want 3, got %d", row.Attempts)
	}
	if row.LastStatus != http.StatusInternalServerError {
		t.Errorf("last_status: want 500, got %d", row.LastStatus)
	}
	if !strings.Contains(row.LastError, "unexpected status 500") {
		t.Errorf("last_error should keep the last error, got %q", row.LastError)
	}
	if rec.callCount() != 3 {
		t.Errorf("receiver: want 3 requests, got %d", rec.callCount())
	}
}

// TestDispatcherFilters 覆盖订阅过滤（停用 / event_types 不含 / 无订阅）与「无匹配不建行」。
func TestDispatcherFilters(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	allURL, allRec := startReceiver(t, nil)
	quotaURL, quotaRec := startReceiver(t, nil)
	disabledURL, disabledRec := startReceiver(t, nil)

	all := mustCreateWebhook(t, store, "all", allURL, "s", "[]", true)
	mustCreateWebhook(t, store, "quota-only", quotaURL, "s", `["quota.exceeded"]`, true)
	mustCreateWebhook(t, store, "disabled", disabledURL, "s", "[]", false)

	d := newDispatcher(t, store, 8)
	d.Start()
	t.Cleanup(func() { d.Stop(context.Background()) })

	d.Submit(Event{Type: TypeRequestCompleted})
	waitDelivery(t, store, all.ID, model.DeliveryStatusSuccess)

	if n := allRec.callCount(); n != 1 {
		t.Errorf("receiver subscribed to all events: want 1 delivery, got %d", n)
	}
	if n := quotaRec.callCount(); n != 0 {
		t.Errorf("receiver with event_types=[quota.exceeded] should not get request.completed, got %d", n)
	}
	if n := disabledRec.callCount(); n != 0 {
		t.Errorf("disabled subscription should receive no delivery, got %d", n)
	}

	// 只有匹配的订阅产生投递记录。
	if _, total, err := store.ListWebhookDeliveries(context.Background(), all.ID, 1, 50); err != nil || total != 1 {
		t.Errorf("matching subscription: want 1 delivery, got total=%d err=%v", total, err)
	}
}

// TestDispatcherNoSubscription 覆盖「无任何匹配订阅时不产生投递记录」。
func TestDispatcherNoSubscription(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	url, rec := startReceiver(t, nil)
	mustCreateWebhook(t, store, "quota-only", url, "s", `["quota.exceeded"]`, true)

	d := newDispatcher(t, store, 8)
	d.Start()

	d.Submit(Event{Type: TypeRequestCompleted})
	d.Stop(context.Background()) // 返回即代表队列已排空

	if n := rec.callCount(); n != 0 {
		t.Errorf("unsubscribed receiver should receive no delivery, got %d", n)
	}
	hooks, err := store.ListEnabledWebhooks(context.Background())
	if err != nil {
		t.Fatalf("list enabled webhooks: %v", err)
	}
	for _, h := range hooks {
		if _, total, err := store.ListWebhookDeliveries(context.Background(), h.ID, 1, 50); err != nil || total != 0 {
			t.Errorf("no matching subscription should create no delivery row, got total=%d err=%v", total, err)
		}
	}
}

// TestDispatcherQueueFull 覆盖队列满：丢弃并计数，不阻塞调用方。
func TestDispatcherQueueFull(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 1) // 不启动 consumer，队列立即占满

	d.Submit(Event{Type: TypeRequestCompleted})
	d.Submit(Event{Type: TypeRequestCompleted})
	d.Submit(Event{Type: TypeRequestCompleted})

	if got := d.Dropped(); got != 2 {
		t.Errorf("Dropped(): want 2, got %d", got)
	}
}

// TestDispatcherStopDrains 覆盖 Stop 排空：停机后队列内事件全部投递完成。
func TestDispatcherStopDrains(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	url, rec := startReceiver(t, nil)
	hook := mustCreateWebhook(t, store, "hook", url, "s", "[]", true)

	d := newDispatcher(t, store, 64)
	d.Start()
	for i := 0; i < 5; i++ {
		d.Submit(Event{Type: TypeRequestCompleted})
	}
	d.Stop(context.Background()) // 返回即代表队列已排空

	if n := rec.callCount(); n != 5 {
		t.Errorf("receiver after Stop: want 5 deliveries, got %d", n)
	}
	_, total, err := store.ListWebhookDeliveries(context.Background(), hook.ID, 1, 50)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if total != 5 {
		t.Errorf("deliveries after Stop: want 5, got %d", total)
	}
	// 重复 Stop 不应 panic。
	d.Stop(context.Background())
}

// TestEmitDefaultDispatcher 覆盖进程内默认投递器的装配与未装配时的 nil 安全。
func TestEmitDefaultDispatcher(t *testing.T) {
	allowPrivateUpstreams(t)
	store := newTestStore(t)

	// 未装配：Emit 为 no-op，不 panic。
	SetDefault(nil)
	t.Cleanup(func() { SetDefault(nil) })
	Emit(TypeRequestCompleted, map[string]any{"api_key_id": 1})

	url, rec := startReceiver(t, nil)
	hook := mustCreateWebhook(t, store, "hook", url, "s", "[]", true)

	d := newDispatcher(t, store, 8)
	d.Start()
	SetDefault(d)
	t.Cleanup(func() { d.Stop(context.Background()) })

	Emit(TypeKeyDisabled, map[string]any{"key_id": 7, "reason": "disabled"})

	row := waitDelivery(t, store, hook.ID, model.DeliveryStatusSuccess)
	if row.EventType != TypeKeyDisabled {
		t.Errorf("event_type: want %q, got %q", TypeKeyDisabled, row.EventType)
	}
	got := rec.snapshot()
	if len(got) != 1 || got[0].eventType != TypeKeyDisabled {
		t.Fatalf("receiver: want 1 key.disabled delivery, got %+v", got)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &env); err != nil {
		t.Fatalf("decode envelope body: %v", err)
	}
	if env.Data["key_id"] != float64(7) || env.Data["reason"] != "disabled" {
		t.Errorf("envelope data mismatch: %+v", env.Data)
	}
}

// TestDispatcherSubmitAfterStopDoesNotPanic 覆盖停机后的 Submit：
// 不得 panic（send on closed channel），并计入丢弃。
func TestDispatcherSubmitAfterStopDoesNotPanic(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 8)
	d.Start()
	d.Stop(context.Background())

	before := d.Dropped()
	for i := 0; i < 3; i++ {
		d.Submit(Event{Type: TypeRequestCompleted})
	}
	if got := d.Dropped() - before; got != 3 {
		t.Fatalf("dropped after stop = %d, want 3", got)
	}
}

// TestDispatcherSubmitRacesStop 覆盖 Submit 与 Stop 并发：
// 在 -race 下验证无数据竞争、无 panic。
func TestDispatcherSubmitRacesStop(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 16)
	d.Start()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				d.Submit(Event{Type: TypeRequestCompleted})
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	d.Stop(context.Background())
	wg.Wait()
}
