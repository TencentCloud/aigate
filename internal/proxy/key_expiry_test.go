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

package proxy_test

// 数据面过期密钥单测：两个入站口都按 401 拒绝（不暴露过期状态）；key.expired 事件只发一次。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/event"
	"aigate/internal/model"
)

// expireKey 把密钥的有效期设置为过去。
func expireKey(t *testing.T, e *env, keyID uint) {
	t.Helper()
	if _, err := e.store.UpdateAPIKey(
		ctx(), keyID,
		map[string]any{"expires_at": time.Now().Add(-time.Minute).UTC()},
	); err != nil {
		t.Fatalf("set expires_at: %v", err)
	}
}

// TestKeyExpired_RejectedOnBothIngresses 覆盖 OpenAI 与 Anthropic 两个入站口对过期密钥的 401，
// 以及「未过期密钥不受影响、上游不被触达」。
func TestKeyExpired_RejectedOnBothIngresses(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))

	expiredKey, expiredToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	expireKey(t, e, expiredKey.ID)

	resp := e.postChat(t, expiredToken, reqWithModel("mock-a"), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("openai ingress status = %d, want 401 (body=%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid API key") {
		t.Fatalf("expired key should use the invalid-credential wording, body=%s", body)
	}

	resp2 := e.postMessagesAPIKey(
		t,
		expiredToken,
		`{"model":"mock-a","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anthropic ingress status = %d, want 401", resp2.StatusCode)
	}

	// 首次拒绝已记录观测时间；上游未被触达。
	got, err := e.store.GetAPIKeyByID(ctx(), expiredKey.ID)
	if err != nil || got.ExpiredAt == nil {
		t.Fatalf("expired_at not recorded: key=%+v err=%v", got, err)
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("upstream should not be called for expired key, calls=%d", e.rec.Calls())
	}

	// 未过期密钥照常放行。
	okKey, okToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	if _, err := e.store.UpdateAPIKey(
		ctx(), okKey.ID,
		map[string]any{"expires_at": time.Now().Add(time.Hour).UTC()},
	); err != nil {
		t.Fatalf("set future expires_at: %v", err)
	}
	resp3 := e.postChat(t, okToken, reqWithModel("mock-a"), nil)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("future-expiry key should pass, status=%d", resp3.StatusCode)
	}
}

// TestKeyExpired_EventEmittedOnce 覆盖 key.expired 只在「首次过期拒绝」时发出一次。
func TestKeyExpired_EventEmittedOnce(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	expireKey(t, e, key.ID)

	// 接收端 + 订阅 + 投递器装配（newEnv 已放行私网上游，投递可走回环）。
	var hits int32
	var lastBody atomic.Value // []byte：最近一次投递的原始 body（并发安全）
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(body)
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)
	if err := e.store.CreateWebhook(ctx(), &model.Webhook{
		Name: "hook", URL: receiver.URL, Secret: "s", EventTypes: "[]", Enabled: true,
	}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	d := event.NewDispatcher(e.store, 16)
	d.Start()
	event.SetDefault(d)
	t.Cleanup(func() {
		event.SetDefault(nil)
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.Stop(stopCtx)
	})

	// 第一次：401 + 发出一次事件。
	resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first call status = %d, want 401", resp.StatusCode)
	}
	waitHits(t, &hits, 1)

	// 事件负载契约：type 与 data.key_id/name/user_id（字段改名或漂移会被此断言捕获）。
	raw, _ := lastBody.Load().([]byte)
	var envelope struct {
		Type string `json:"type"`
		Data struct {
			KeyID  uint   `json:"key_id"`
			Name   string `json:"name"`
			UserID uint   `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode key.expired envelope: %v (raw=%s)", err, raw)
	}
	if envelope.Type != "key.expired" || envelope.Data.KeyID != key.ID ||
		envelope.Data.Name != key.Name || envelope.Data.UserID != key.UserID {
		t.Fatalf("key.expired payload mismatch: %+v (want key_id=%d name=%q user_id=%d)",
			envelope, key.ID, key.Name, key.UserID)
	}

	// 第二次：仍 401，但不再产生新事件。
	resp2 := e.postChat(t, token, reqWithModel("mock-a"), nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second call status = %d, want 401", resp2.StatusCode)
	}
	time.Sleep(700 * time.Millisecond)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("key.expired deliveries = %d, want 1（同一有效期只发一次）", n)
	}
}

// waitHits 轮询等待投递计数达到 want（最多 3 秒）。
func waitHits(t *testing.T, hits *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(hits) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook deliveries = %d, want >= %d", atomic.LoadInt32(hits), want)
}
