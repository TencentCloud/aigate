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

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/provider"
)

// AnthropicProvider 的请求 / 响应格式转换由 anthropic_internal_test.go 的白盒用例覆盖；
// 本文件覆盖：请求体解码失败、出站目标被 SSRF 拦截，以及放行私网后（allowPrivate=true）
// 经由 httptest mock 的可达路径、请求形状与响应分类。

func TestAnthropicChatCompletion_RejectsMalformedRequestBody(t *testing.T) {
	p := &provider.AnthropicProvider{}
	for _, body := range []string{``, `{`, `not-json`} {
		result, status, err := p.ChatCompletion(
			context.Background(),
			"sk",
			"https://api.example.com",
			"claude",
			[]byte(body),
			nil,
		)
		if err == nil {
			t.Fatalf("body=%q: expected error, got nil", body)
		}
		if result != nil {
			t.Fatalf("body=%q: result = %+v, want nil", body, result)
		}
		if status != 0 {
			t.Fatalf("body=%q: status = %d, want 0", body, status)
		}
		if !strings.Contains(err.Error(), "failed to decode request body") {
			t.Fatalf("body=%q: err = %v, want a decode failure", body, err)
		}
	}
}

func TestAnthropicChatCompletionStream_RejectsMalformedRequestBody(t *testing.T) {
	p := &provider.AnthropicProvider{}
	rec := httptest.NewRecorder()

	result, status, err := p.ChatCompletionStream(
		context.Background(),
		"sk",
		"https://api.example.com",
		"claude",
		[]byte(`{`),
		rec,
		rec,
		nil,
	)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil", result)
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
	if !strings.Contains(err.Error(), "failed to decode request body") {
		t.Fatalf("err = %v, want a decode failure", err)
	}
}

func TestAnthropicChatCompletion_BlocksDisallowedTargets(t *testing.T) {
	p := &provider.AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	for _, base := range []string{
		"http://127.0.0.1:9090",
		"http://192.168.1.1",
		"http://169.254.169.254",
		"http://[::1]:9090",
		"file:///etc/passwd",
	} {
		t.Run(base, func(t *testing.T) {
			result, status, err := p.ChatCompletion(context.Background(), "sk", base, "claude", body, nil)
			if err == nil {
				t.Fatalf("base=%q: expected SSRF rejection, got nil", base)
			}
			if result != nil {
				t.Fatalf("base=%q: result = %+v, want nil", base, result)
			}
			if status != 0 {
				t.Fatalf("base=%q: status = %d, want 0 (request never completed)", base, status)
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("base=%q: err = %v, want a security rejection", base, err)
			}
		})
	}
}

func TestAnthropicChatCompletionStream_BlocksDisallowedTargets(t *testing.T) {
	p := &provider.AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	for _, base := range []string{
		"http://127.0.0.1:9090",
		"http://10.0.0.1",
		"http://100.64.1.2",
		"ldap://internal/",
	} {
		t.Run(base, func(t *testing.T) {
			rec := httptest.NewRecorder()
			result, status, err := p.ChatCompletionStream(
				context.Background(),
				"sk",
				base,
				"claude",
				body,
				rec,
				rec,
				nil,
			)
			if err == nil {
				t.Fatalf("base=%q: expected SSRF rejection, got nil", base)
			}
			if result != nil {
				t.Fatalf("base=%q: result = %+v, want nil", base, result)
			}
			if status != 0 {
				t.Fatalf("base=%q: status = %d, want 0", base, status)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("base=%q: response body should stay empty, got %q", base, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 可达 mock 上游（allowPrivate=true，恢复迁移前的原语义）
// ---------------------------------------------------------------------------

// TestAnthropicChatCompletion_ReachableUpstream 验证非流式转发命中 /v1/messages、
// 带上 x-api-key / anthropic-version，把 Anthropic 响应转成 OpenAI 格式，并补齐默认 max_tokens。
func TestAnthropicChatCompletion_ReachableUpstream(t *testing.T) {
	allowPrivateForTest(t)

	var (
		gotPath    string
		gotAPIKey  string
		gotVersion string
		gotBody    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(
				`{"id":"msg_1","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,` +
					`"output_tokens":2}}`,
			),
		)
	}))
	defer srv.Close()

	p := &provider.AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result, status, err := p.ChatCompletion(
		context.Background(),
		"sk-anthropic",
		srv.URL,
		"claude-3-5-haiku",
		body,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil || result.Usage.TotalTokens != 5 {
		t.Fatalf("result = %#v, want usage.total=5", result)
	}
	if !strings.Contains(string(result.Body), "hello") {
		t.Fatalf("converted body should contain the text block, got %s", result.Body)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %s, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-anthropic" {
		t.Fatalf("x-api-key = %q, want sk-anthropic", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatalf("anthropic-version header must be set")
	}
	// Anthropic 要求 max_tokens，调用方未提供时必须补默认值。
	if v, ok := gotBody["max_tokens"].(float64); !ok || int(v) != 4096 {
		t.Fatalf("body.max_tokens = %v, want 4096", gotBody["max_tokens"])
	}
}

// TestAnthropicChatCompletionStream_ReachableUpstream 验证流式转发命中 /v1/messages，
// 并把 Anthropic SSE 事件转换为 OpenAI 风格分片（含 [DONE] 与 usage）。
func TestAnthropicChatCompletionStream_ReachableUpstream(t *testing.T) {
	allowPrivateForTest(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(
			[]byte(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"," +
					"\"usage\":{\"input_tokens\":3}}}\n\n",
			),
		)
		_, _ = w.Write(
			[]byte(
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\"," +
					"\"text\":\"hello\"}}\n\n",
			),
		)
		_, _ = w.Write(
			[]byte(
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}," +
					"\"usage\":{\"output_tokens\":2}}\n\n",
			),
		)
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result, status, err := p.ChatCompletionStream(
		context.Background(),
		"sk",
		srv.URL,
		"claude-3-5-haiku",
		body,
		rec,
		rec,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil || result.Usage.CompletionTokens != 2 {
		t.Fatalf("result = %#v, want completion tokens 2", result)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("client body should contain the converted text delta, got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("client body should contain the DONE sentinel, got %q", rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %s, want /v1/messages", gotPath)
	}
}

// TestAnthropicCheckConnectivityWithChat_Success 验证探测打到 /v1/messages，
// 带上 x-api-key / anthropic-version 与 max_tokens=1 的最小请求体。
func TestAnthropicCheckConnectivityWithChat_Success(t *testing.T) {
	allowPrivateForTest(t)

	var (
		gotPath    string
		gotAPIKey  string
		gotVersion string
		gotMethod  string
		gotBody    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","content":[{"type":"text","text":""}]}`))
	}))
	defer srv.Close()

	p := &provider.AnthropicProvider{}
	latency, err := p.CheckConnectivityWithChat(
		context.Background(),
		"sk-anthropic",
		srv.URL,
		"claude-3-5-haiku-20241022",
	)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if latency <= 0 {
		t.Fatalf("expected positive latency, got %v", latency)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %s, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-anthropic" {
		t.Fatalf("x-api-key = %q, want sk-anthropic", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatalf("anthropic-version header must be set")
	}
	if gotBody["model"] != "claude-3-5-haiku-20241022" {
		t.Fatalf("body.model = %v, want claude-3-5-haiku-20241022", gotBody["model"])
	}
	if v, ok := gotBody["max_tokens"].(float64); !ok || int(v) != 1 {
		t.Fatalf("body.max_tokens = %v, want 1", gotBody["max_tokens"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages should be a single-element array, got %v", gotBody["messages"])
	}
}

// TestAnthropicCheckConnectivityWithChat_ResponseClassification 覆盖 401/403/429/5xx/4xx
// 到 sentinel 的映射，以及错误响应体 snippet 的捕获。
func TestAnthropicCheckConnectivityWithChat_ResponseClassification(t *testing.T) {
	allowPrivateForTest(t)

	tests := []struct {
		name     string
		status   int
		body     string
		wantKind error
		wantSnip string
	}{
		{
			name:     "401 unauthorized",
			status:   http.StatusUnauthorized,
			body:     "bad key",
			wantKind: provider.ErrInvalidAPIKey,
			wantSnip: "bad key",
		},
		{
			name:     "403 forbidden",
			status:   http.StatusForbidden,
			body:     "no access",
			wantKind: provider.ErrForbidden,
			wantSnip: "no access",
		},
		{
			name:     "429 rate limited",
			status:   http.StatusTooManyRequests,
			body:     "slow down",
			wantKind: provider.ErrRateLimited,
			wantSnip: "slow down",
		},
		{
			name:     "502 bad gateway",
			status:   http.StatusBadGateway,
			body:     "boom",
			wantKind: provider.ErrUpstreamServer,
			wantSnip: "boom",
		},
		{
			name:     "400 bad request",
			status:   http.StatusBadRequest,
			body:     "bad request",
			wantKind: provider.ErrUpstreamClient,
			wantSnip: "bad request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.body, tt.status)
			}))
			defer srv.Close()

			p := &provider.AnthropicProvider{}
			_, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL, "claude")
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, tt.wantKind) {
				t.Fatalf("err = %v, want %v", err, tt.wantKind)
			}
			var ce *provider.ConnectivityError
			if !errors.As(err, &ce) {
				t.Fatalf("err is not *ConnectivityError: %v", err)
			}
			if ce.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", ce.StatusCode, tt.status)
			}
			if !strings.Contains(ce.Snippet, tt.wantSnip) {
				t.Fatalf("snippet = %q, want it to contain %q", ce.Snippet, tt.wantSnip)
			}
		})
	}
}

// TestAnthropicCheckConnectivityWithChat_NoAPIKeyHeader 验证 apiKey 为空时不发送 x-api-key，
// 但 anthropic-version 始终携带。
func TestAnthropicCheckConnectivityWithChat_NoAPIKeyHeader(t *testing.T) {
	allowPrivateForTest(t)

	var (
		sawAPIKey  bool
		sawVersion bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			sawAPIKey = true
		}
		if r.Header.Get("anthropic-version") != "" {
			sawVersion = true
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &provider.AnthropicProvider{}
	if _, err := p.CheckConnectivityWithChat(context.Background(), "", srv.URL, "claude"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawAPIKey {
		t.Fatalf("x-api-key header must not be set when apiKey is empty")
	}
	if !sawVersion {
		t.Fatalf("anthropic-version header must always be set")
	}
}

// TestAnthropicChatCompletionStream_UpstreamErrorPassthrough 覆盖 Anthropic 流式请求命中上游非 200：
// 状态码与错误体原样回传客户端、不返回结果对象（这条路径此前零覆盖）。
func TestAnthropicChatCompletionStream_UpstreamErrorPassthrough(t *testing.T) {
	allowPrivateForTest(t)

	const upstreamBody = `{"type":"error","error":{"type":"api_error","message":"overloaded"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result, status, err := p.ChatCompletionStream(
		context.Background(),
		"sk",
		srv.URL,
		"claude-3-5-haiku",
		body,
		rec,
		rec,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != upstreamBody {
		t.Fatalf("client response = %d %q, want 503 passthrough %q", rec.Code, rec.Body.String(), upstreamBody)
	}
}
