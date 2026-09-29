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

// Package event 实现 webhook 事件出口：事件信封 + HMAC 签名 + 进程内异步投递器。
//
// 语义（见 openspec/changes/gateway-events/design.md D1–D5）：
//   - 事件在业务动作完成后发出（转发完成 / 配额拒绝 / 密钥禁用或删除）；
//   - 投递异步、单 consumer 串行，队列满时丢弃并告警，绝不阻塞数据面；
//   - 投递使用 SSRF 防护出站，单次超时 10s，最多 3 次尝试（重试等待 1s、5s），
//     每次尝试更新投递记录，成功或耗尽后落终态。
package event

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync/atomic"
	"time"
)

// 事件类型。
const (
	// TypeRequestCompleted 一次数据面转发完成（含流式）。
	TypeRequestCompleted = "request.completed"
	// TypeQuotaExceeded 请求因配额超限被拒绝。
	TypeQuotaExceeded = "quota.exceeded"
	// TypeKeyDisabled 密钥被管理面禁用或删除。
	TypeKeyDisabled = "key.disabled"
	// TypeKeyExpired 密钥首次因过期被数据面拒绝（同一有效期内只发一次）。
	TypeKeyExpired = "key.expired"
)

// Event 是投递信封：JSON 序列化结果即投递 body（id 为投递 ID）。
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"created_at"`
	Data      map[string]any `json:"data"`
}

// Sign 计算 sha256=<hex(hmac-sha256(secret, body))>（签名覆盖原始 body）。
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// defaultDispatcher 是进程内默认投递器（由 main 装配）。
// 未装配时 Emit 为 no-op，因此埋点方无需感知投递器是否存在。
var defaultDispatcher atomic.Pointer[Dispatcher]

// SetDefault 装配进程内默认投递器；传 nil 表示卸载（Emit 退回 no-op）。
func SetDefault(d *Dispatcher) {
	defaultDispatcher.Store(d)
}

// Emit 向默认投递器提交事件；未装配时静默丢弃（nil 安全）。
func Emit(eventType string, data map[string]any) {
	d := defaultDispatcher.Load()
	if d == nil {
		return
	}
	d.Submit(Event{ID: newEventID(), Type: eventType, CreatedAt: time.Now().UTC(), Data: data})
}

// newEventID 生成事件 ID（16 字节随机 hex；随机源不可用时退化为纳秒时间戳）。
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
