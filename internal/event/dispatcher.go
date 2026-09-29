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

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"aigate/internal/model"
	"aigate/internal/provider"
)

const (
	// defaultBuffer 是队列默认容量（-webhook-buffer 可配）。
	defaultBuffer = 256
	// deliveryTimeout 是单次投递超时。
	deliveryTimeout = 10 * time.Second
	// maxAttempts 是最多尝试次数。
	maxAttempts = 3
	// maxErrorRunes 是 last_error 列的写入上限（VARCHAR(512)）。
	maxErrorRunes = 500
	// drainLimit 是读取接收端响应体的上限（仅用于复用连接，内容不参与判定）。
	drainLimit = 64 * 1024
	// defaultPruneInterval 是投递记录清理周期（保留期见 -webhook-delivery-retention-days）。
	defaultPruneInterval = time.Hour
)

// defaultBackoffs 是默认退避表：第 n 次尝试失败后等待 defaultBackoffs[n-1] 再重试。
var defaultBackoffs = []time.Duration{time.Second, 5 * time.Second}

// Dispatcher 有界队列 + 单 consumer 的异步投递器。
//
// 队列满时 Submit 丢弃事件并计数（不阻塞调用方）；consumer 串行处理事件，
// 依次向匹配的订阅投递，投递经 SSRF 防护出站。
type Dispatcher struct {
	store   *model.Store
	ch      chan Event
	done    chan struct{}
	dropped atomic.Int64

	// 以下字段允许测试注入（生产使用包级默认值）。
	backoffs      []time.Duration
	maxAttempts   int
	timeout       time.Duration
	pruneInterval time.Duration // 投递记录清理周期（默认 1h）

	// retention > 0 时启用投递记录清理；须在 Start 之前通过 SetDeliveryRetention 配置。
	retention time.Duration

	startOnce sync.Once
	stopOnce  sync.Once

	// mu/closed 保护「Submit 发送」与「Stop 关闭」的竞争：
	// Submit 持读锁发送，Stop 持写锁置位并 close(ch)，避免 send on closed channel。
	mu     sync.RWMutex
	closed bool
}

// NewDispatcher 创建投递器（buffer < 1 时回退默认 256）。
func NewDispatcher(store *model.Store, buffer int) *Dispatcher {
	if buffer < 1 {
		buffer = defaultBuffer
	}
	return &Dispatcher{
		store:         store,
		ch:            make(chan Event, buffer),
		done:          make(chan struct{}),
		backoffs:      defaultBackoffs,
		maxAttempts:   maxAttempts,
		timeout:       deliveryTimeout,
		pruneInterval: defaultPruneInterval,
	}
}

// SetDeliveryRetention 设置投递记录保留期（≤0 = 不清理）。须在 Start 之前调用；
// 保留期 > 0 时，Start 会同时启动清理循环（启动即清一次，此后每 pruneInterval 一次）。
func (d *Dispatcher) SetDeliveryRetention(retention time.Duration) {
	if retention < 0 {
		retention = 0
	}
	d.retention = retention
}

// Start 启动消费 goroutine（重复调用为空操作）；配置了保留期时同时启动清理循环。
func (d *Dispatcher) Start() {
	d.startOnce.Do(func() {
		go d.loop()
		if d.retention > 0 {
			go d.pruneLoop()
		}
	})
}

// Stop 关闭队列并等待排空；ctx 到期仍未排空时直接返回（剩余事件由 consumer 在后台继续处理）。
// 重复调用为空操作。未调用 Start 时也会先启动 consumer，保证队列被排空且 done 一定关闭。
func (d *Dispatcher) Stop(ctx context.Context) {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.ch)
		d.mu.Unlock()
		d.Start()
		select {
		case <-d.done:
		case <-ctx.Done():
		}
	})
}

// Submit 提交一个事件；队列满或已关闭时丢弃并计数。
// 未填充 ID/CreatedAt 的事件（直接构造的场景）在此补齐。
func (d *Dispatcher) Submit(evt Event) {
	if evt.ID == "" {
		evt.ID = newEventID()
	}
	if evt.CreatedAt.IsZero() {
		evt.CreatedAt = time.Now().UTC()
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		n := d.dropped.Add(1)
		slog.Warn("webhook dispatcher stopped, dropping event", "type", evt.Type, "dropped_total", n)
		return
	}
	select {
	case d.ch <- evt:
	default:
		n := d.dropped.Add(1)
		slog.Warn("webhook event queue full, dropping event", "type", evt.Type, "dropped_total", n)
	}
}

// Dropped 返回累计丢弃的事件数。
func (d *Dispatcher) Dropped() int64 {
	return d.dropped.Load()
}

// loop 是唯一 consumer：串行排空队列。
func (d *Dispatcher) loop() {
	defer close(d.done)
	for evt := range d.ch {
		d.dispatch(evt)
	}
}

// pruneLoop 周期性清理过期投递记录：启动时先清一次（覆盖停机期间累积的旧记录），
// 队列排空（done 关闭）后退出。
func (d *Dispatcher) pruneLoop() {
	d.pruneOnce()
	ticker := time.NewTicker(d.pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.pruneOnce()
		case <-d.done:
			return
		}
	}
}

// pruneOnce 删除创建时间早于「现在 − 保留期」的投递记录。
func (d *Dispatcher) pruneOnce() {
	before := time.Now().UTC().Add(-d.retention)
	n, err := d.store.PruneWebhookDeliveries(context.Background(), before)
	if err != nil {
		slog.Warn("prune webhook deliveries failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("pruned webhook deliveries", "rows", n, "retention", d.retention.String())
	}
}

// dispatch 把事件投递给所有匹配的订阅（enabled 且 event_types 空或含该类型）；
// 无匹配订阅时不产生任何投递记录。
func (d *Dispatcher) dispatch(evt Event) {
	ctx := context.Background()
	hooks, err := d.store.ListEnabledWebhooks(ctx)
	if err != nil {
		slog.Error("load webhook subscriptions failed, dropping event", "error", err, "type", evt.Type)
		return
	}
	for i := range hooks {
		if !hooks[i].Subscribes(evt.Type) {
			continue
		}
		d.deliver(ctx, &hooks[i], evt)
	}
}

// deliver 向单个订阅投递事件：先落投递记录（行 ID 即投递 ID），再按退避重试。
func (d *Dispatcher) deliver(ctx context.Context, hook *model.Webhook, evt Event) {
	row := &model.WebhookDelivery{
		WebhookID: hook.ID,
		EventType: evt.Type,
		Status:    model.DeliveryStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := d.store.CreateWebhookDelivery(ctx, row); err != nil {
		slog.Error("create webhook delivery failed", "error", err, "webhook_id", hook.ID, "type", evt.Type)
		return
	}

	deliveryID := strconv.FormatUint(uint64(row.ID), 10)
	body, err := json.Marshal(Event{ID: deliveryID, Type: evt.Type, CreatedAt: evt.CreatedAt, Data: evt.Data})
	if err != nil {
		slog.Error("encode webhook delivery payload failed", "error", err, "delivery_id", deliveryID)
		return
	}
	if _, err := d.store.UpdateWebhookDelivery(ctx, row.ID, map[string]any{"payload": string(body)}); err != nil {
		slog.Error("persist webhook delivery payload failed", "error", err, "delivery_id", deliveryID)
	}

	status := model.DeliveryStatusFailed
	attempts := 0
	lastStatus := 0
	lastError := ""
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		attempts = attempt
		code, postErr := d.post(ctx, hook, evt.Type, deliveryID, body)
		if postErr == nil {
			status = model.DeliveryStatusSuccess
			lastStatus = code
			lastError = ""
			break
		}
		lastStatus = code
		lastError = truncateRunes(postErr.Error(), maxErrorRunes)
		if attempt < d.maxAttempts {
			slog.Warn("webhook delivery attempt failed, retrying",
				"delivery_id", deliveryID, "webhook_id", hook.ID, "type", evt.Type,
				"attempt", attempt, "last_status", code, "backoff", d.backoffFor(attempt))
			time.Sleep(d.backoffFor(attempt))
		}
	}

	if _, err := d.store.UpdateWebhookDelivery(ctx, row.ID, map[string]any{
		"status":      status,
		"attempts":    attempts,
		"last_status": lastStatus,
		"last_error":  lastError,
	}); err != nil {
		slog.Error("finalize webhook delivery failed", "error", err, "delivery_id", deliveryID)
		return
	}
	if status == model.DeliveryStatusFailed {
		slog.Warn("webhook delivery exhausted retries",
			"delivery_id", deliveryID, "webhook_id", hook.ID, "type", evt.Type,
			"attempts", attempts, "last_status", lastStatus, "last_error", lastError)
	}
}

// post 执行一次投递：POST 订阅 url，带签名头与信封 body；非 2xx 视为失败。
// 返回的 code 为 HTTP 状态码（网络错误时为 0）。
func (d *Dispatcher) post(
	ctx context.Context,
	hook *model.Webhook,
	eventType, deliveryID string,
	body []byte,
) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Event", eventType)
	req.Header.Set("X-Gateway-Delivery", deliveryID)
	req.Header.Set("X-Gateway-Signature", Sign(hook.Secret, body))

	resp, err := provider.DoSSRFProtectedRequest(req, d.timeout)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// backoffFor 返回第 attempt 次尝试失败后的等待时长（越界时回退最后一个值）。
func (d *Dispatcher) backoffFor(attempt int) time.Duration {
	if len(d.backoffs) == 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(d.backoffs) {
		attempt = len(d.backoffs)
	}
	return d.backoffs[attempt-1]
}

// truncateRunes 按字符截断字符串（列宽上限保护，避免超长写入失败）。
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
