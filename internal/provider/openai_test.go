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
	"time"

	"aigate/internal/provider"
)

// 说明：本仓库 SSRF 防护始终启用；默认严格模式下指向回环 / 私网地址的 mock 上游会被
// URL 校验直接拒绝。放行私网（provider.SetAllowPrivateUpstreams(true)）后，httptest
// mock 可充当可达上游，因此下述用例覆盖 ChatCompletion / ChatCompletionStream /
// CheckConnectivityWithChat 的可达路径与请求形状、响应分类。

func TestOpenAIChatCompletion_RejectsMalformedRequestBody(t *testing.T) {
	p := &provider.OpenAIProvider{}
	for _, body := range []string{``, `{`, `not-json`} {
		result, status, err := p.ChatCompletion(
			context.Background(),
			"sk",
			"https://api.example.com/v1",
			"gpt-4o",
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

func TestOpenAIChatCompletionStream_RejectsMalformedRequestBody(t *testing.T) {
	p := &provider.OpenAIProvider{}
	rec := httptest.NewRecorder()

	result, status, err := p.ChatCompletionStream(
		context.Background(),
		"sk",
		"https://api.example.com/v1",
		"gpt-4o",
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

func TestOpenAIChatCompletion_BlocksDisallowedTargets(t *testing.T) {
	p := &provider.OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)

	for _, base := range []string{
		"http://127.0.0.1:8080/v1",
		"http://10.0.0.5/v1",
		"http://169.254.169.254/v1",
		"http://[::1]:8080/v1",
		"file:///etc/passwd",
	} {
		t.Run(base, func(t *testing.T) {
			result, status, err := p.ChatCompletion(context.Background(), "sk", base, "gpt-4o", body, nil)
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

func TestOpenAIChatCompletionStream_BlocksDisallowedTargets(t *testing.T) {
	p := &provider.OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	for _, base := range []string{
		"http://127.0.0.1:8080/v1",
		"http://10.0.0.5/v1",
		"http://[fc00::1]/v1",
		"gopher://127.0.0.1:11211/_stats",
	} {
		t.Run(base, func(t *testing.T) {
			rec := httptest.NewRecorder()
			result, status, err := p.ChatCompletionStream(
				context.Background(),
				"sk",
				base,
				"gpt-4o",
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
			// 不能把失败的响应体当作正常流写回客户端。
			if rec.Body.Len() != 0 {
				t.Fatalf("base=%q: response body should stay empty, got %q", base, rec.Body.String())
			}
		})
	}
}

// TestOpenAIChatCompletion_EmptyBaseIsRejected 覆盖 base 为空时拼出的相对 URL
// 会因缺少 scheme 被 SSRF 校验拒绝（不会 panic）。
func TestOpenAIChatCompletion_EmptyBaseIsRejected(t *testing.T) {
	p := &provider.OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, status, err := p.ChatCompletion(context.Background(), "sk", "", "gpt-4o", body, nil)
	if err == nil {
		t.Fatalf("expected error for empty base, got nil")
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}

// ---------------------------------------------------------------------------
// 可达 mock 上游（allowPrivate=true，恢复迁移前的原语义）
// ---------------------------------------------------------------------------

// TestOpenAIChatCompletion_ReachableUpstream 验证非流式转发命中 /chat/completions、
// 注入 model、带上 Bearer 鉴权，并透传 200 响应体与 usage。
func TestOpenAIChatCompletion_ReachableUpstream(t *testing.T) {
	allowPrivateForTest(t)

	var (
		gotPath   string
		gotAuth   string
		gotMethod string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(
				`{"id":"c1","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},` +
					`"choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
			),
		)
	}))
	defer srv.Close()

	p := &provider.OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result, status, err := p.ChatCompletion(context.Background(), "sk-ok", srv.URL, "gpt-4o", body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil || result.Usage.TotalTokens != 5 {
		t.Fatalf("result = %#v, want usage.total=5", result)
	}
	if gotMethod != http.MethodPost || gotPath != "/chat/completions" {
		t.Fatalf("upstream = %s %s, want POST /chat/completions", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sk-ok" {
		t.Fatalf("auth = %q, want Bearer sk-ok", gotAuth)
	}
	if gotBody["model"] != "gpt-4o" {
		t.Fatalf("body.model = %v, want gpt-4o (must be injected into the request body)", gotBody["model"])
	}
}

// TestOpenAIChatCompletionStream_ReachableUpstream 验证流式转发命中 /chat/completions、
// 注入 stream=true / stream_options，并把 SSE 分片与 usage 透传给客户端。
func TestOpenAIChatCompletionStream_ReachableUpstream(t *testing.T) {
	allowPrivateForTest(t)

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL, "gpt-4o", body, rec, rec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil || result.Usage.TotalTokens != 2 {
		t.Fatalf("result = %#v, want usage.total=2", result)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("client body should contain the forwarded chunk, got %q", rec.Body.String())
	}
	if gotBody["stream"] != true {
		t.Fatalf("upstream body.stream = %v, want true", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"]; !ok {
		t.Fatalf("upstream body should contain stream_options, got %v", gotBody)
	}
}

// TestOpenAICheckConnectivityWithChat_Success 验证探测打到 /chat/completions，
// 带上 Bearer 鉴权与最小化请求体（单个 user message、legacy max_tokens），并接受 200 响应。
func TestOpenAICheckConnectivityWithChat_Success(t *testing.T) {
	allowPrivateForTest(t)

	var (
		gotPath   string
		gotAuth   string
		gotMethod string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":""}}]}`))
	}))
	defer srv.Close()

	p := &provider.OpenAIProvider{}
	latency, err := p.CheckConnectivityWithChat(context.Background(), "sk-ok", srv.URL, "gpt-4o-mini")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if latency <= 0 {
		t.Fatalf("expected positive latency, got %v", latency)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %s, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-ok" {
		t.Fatalf("auth = %q, want Bearer sk-ok", gotAuth)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Fatalf("body.model = %v, want gpt-4o-mini", gotBody["model"])
	}
	// 旧版 GPT 模型：应发送 max_tokens，而不是 max_completion_tokens。
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Fatalf("body should contain max_tokens for legacy model: %v", gotBody)
	}
	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Fatalf("body should NOT contain max_completion_tokens for legacy model: %v", gotBody)
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages should be a single-element array, got %v", gotBody["messages"])
	}
}

// TestOpenAICheckConnectivityWithChat_NewModelMaxCompletionTokens 验证 GPT-5+ / o-series
// 在探测请求里把 max_tokens 归一化为 max_completion_tokens。
func TestOpenAICheckConnectivityWithChat_NewModelMaxCompletionTokens(t *testing.T) {
	allowPrivateForTest(t)

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &provider.OpenAIProvider{}
	if _, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL, "gpt-5"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Fatalf("max_tokens should be removed for gpt-5: %v", gotBody)
	}
	if _, ok := gotBody["max_completion_tokens"]; !ok {
		t.Fatalf("max_completion_tokens should be set for gpt-5: %v", gotBody)
	}
}

// TestOpenAICheckConnectivityWithChat_ResponseClassification 覆盖 401/403/429/5xx/4xx
// 到 sentinel 的映射，以及错误响应体 snippet 的捕获。
func TestOpenAICheckConnectivityWithChat_ResponseClassification(t *testing.T) {
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
			name:     "500 internal",
			status:   http.StatusInternalServerError,
			body:     "boom",
			wantKind: provider.ErrUpstreamServer,
			wantSnip: "boom",
		},
		{
			name:     "502 bad gateway",
			status:   http.StatusBadGateway,
			body:     "bad gateway",
			wantKind: provider.ErrUpstreamServer,
			wantSnip: "bad gateway",
		},
		{
			name:     "400 bad request",
			status:   http.StatusBadRequest,
			body:     "bad request",
			wantKind: provider.ErrUpstreamClient,
			wantSnip: "bad request",
		},
		{
			name:     "404 not found",
			status:   http.StatusNotFound,
			body:     "not found",
			wantKind: provider.ErrUpstreamClient,
			wantSnip: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.body, tt.status)
			}))
			defer srv.Close()

			p := &provider.OpenAIProvider{}
			_, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL, "gpt-4o-mini")
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

// TestOpenAICheckConnectivityWithChat_SnippetTruncated 验证超长错误响应体被截断为
// 不超过 512 字节的单行 snippet。
func TestOpenAICheckConnectivityWithChat_SnippetTruncated(t *testing.T) {
	allowPrivateForTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 2000)))
	}))
	defer srv.Close()

	p := &provider.OpenAIProvider{}
	_, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL, "gpt-4o-mini")
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) {
		t.Fatalf("err is not *ConnectivityError: %v", err)
	}
	if len(ce.Snippet) != 512 {
		t.Fatalf("len(snippet) = %d, want 512 (truncated)", len(ce.Snippet))
	}
	if strings.ContainsAny(ce.Snippet, "\r\n") {
		t.Fatalf("snippet should be single-line, got %q", ce.Snippet)
	}
}

// TestOpenAICheckConnectivityWithChat_NoAPIKeyHeader 验证 apiKey 为空时不发送 Authorization 头。
func TestOpenAICheckConnectivityWithChat_NoAPIKeyHeader(t *testing.T) {
	allowPrivateForTest(t)

	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &provider.OpenAIProvider{}
	if _, err := p.CheckConnectivityWithChat(context.Background(), "", srv.URL, "gpt-4o-mini"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawAuth {
		t.Fatalf("Authorization header must not be set when apiKey is empty")
	}
}

// TestOpenAICheckConnectivityWithChat_ContextCanceled 验证 ctx 取消时探测以
// ErrNetworkUnreachable 结束。
func TestOpenAICheckConnectivityWithChat_ContextCanceled(t *testing.T) {
	allowPrivateForTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 阻塞到测试 ctx 被取消。
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	p := &provider.OpenAIProvider{}
	_, err := p.CheckConnectivityWithChat(ctx, "sk", srv.URL, "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected error after ctx cancel, got nil")
	}
	if !errors.Is(err, provider.ErrNetworkUnreachable) {
		t.Fatalf("err = %v, want ErrNetworkUnreachable", err)
	}
}

// TestOpenAICheckConnectivityWithChat_NetworkUnreachable 验证放行私网后，指向未监听端口
// 的探测仍以传输层错误结束。
func TestOpenAICheckConnectivityWithChat_NetworkUnreachable(t *testing.T) {
	allowPrivateForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := &provider.OpenAIProvider{}
	_, err := p.CheckConnectivityWithChat(ctx, "sk", "http://127.0.0.1:1", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, provider.ErrNetworkUnreachable) {
		t.Fatalf("err = %v, want ErrNetworkUnreachable", err)
	}
}

// TestOpenAIChatCompletionStream_UpstreamErrorPassthrough 覆盖流式请求命中上游非 200：
// 状态码与错误体原样回传客户端、不返回结果对象（这条路径此前零覆盖）。
func TestOpenAIChatCompletionStream_UpstreamErrorPassthrough(t *testing.T) {
	allowPrivateForTest(t)

	const upstreamBody = `{"error":{"message":"tpm rate limit exceeded","code":"20072"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.OpenAIProvider{}
	result, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL, "glm-5",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil（失败流不产生 usage 结果）", result)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want 429", rec.Code)
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Fatalf("client body = %q, want 上游错误体原样透传 %q", got, upstreamBody)
	}
}
