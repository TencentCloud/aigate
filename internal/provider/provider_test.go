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
	"errors"
	"net/http"
	"testing"
	"time"

	"aigate/internal/provider"
)

// TestGetProvider_AnthropicMessages verifies that GetProvider returns AnthropicProvider for anthropic-messages.
func TestGetProvider_AnthropicMessages(t *testing.T) {
	p := provider.GetProvider("anthropic-messages")
	if p == nil {
		t.Fatalf("GetProvider returned nil for anthropic-messages")
	}
	if _, ok := p.(*provider.AnthropicProvider); !ok {
		t.Fatalf("GetProvider returned %T, expected *AnthropicProvider", p)
	}
}

// TestGetProvider_OpenAICompletions verifies that GetProvider returns OpenAIProvider for openai-completions.
func TestGetProvider_OpenAICompletions(t *testing.T) {
	p := provider.GetProvider("openai-completions")
	if p == nil {
		t.Fatalf("GetProvider returned nil for openai-completions")
	}
	if _, ok := p.(*provider.OpenAIProvider); !ok {
		t.Fatalf("GetProvider returned %T, expected *OpenAIProvider", p)
	}
}

// TestGetProvider_DefaultUnknown verifies that GetProvider returns OpenAIProvider for unknown types.
func TestGetProvider_DefaultUnknown(t *testing.T) {
	p := provider.GetProvider("unknown-provider")
	if p == nil {
		t.Fatalf("GetProvider returned nil for unknown provider")
	}
	if _, ok := p.(*provider.OpenAIProvider); !ok {
		t.Fatalf("GetProvider returned %T for unknown type, expected default *OpenAIProvider", p)
	}
}

// TestGetProvider_EmptyString verifies that GetProvider returns OpenAIProvider for empty string.
func TestGetProvider_EmptyString(t *testing.T) {
	p := provider.GetProvider("")
	if p == nil {
		t.Fatalf("GetProvider returned nil for empty string")
	}
	if _, ok := p.(*provider.OpenAIProvider); !ok {
		t.Fatalf("GetProvider returned %T for empty string, expected default *OpenAIProvider", p)
	}
}

func TestConnectivityError_Error(t *testing.T) {
	cause := errors.New("dial tcp timeout")
	tests := []struct {
		name string
		err  *provider.ConnectivityError
		want string
	}{
		{
			name: "transport cause without status",
			err: &provider.ConnectivityError{
				Kind:  provider.ErrNetworkUnreachable,
				Cause: cause,
			},
			want: "network unreachable: dial tcp timeout",
		},
		{
			name: "status with snippet",
			err: &provider.ConnectivityError{
				Kind:       provider.ErrInvalidAPIKey,
				StatusCode: http.StatusUnauthorized,
				Snippet:    "bad key",
			},
			want: "invalid api key (status 401): bad key",
		},
		{
			name: "status without snippet",
			err: &provider.ConnectivityError{
				Kind:       provider.ErrForbidden,
				StatusCode: http.StatusForbidden,
			},
			want: "forbidden (status 403)",
		},
		{
			name: "kind only",
			err: &provider.ConnectivityError{
				Kind: provider.ErrRateLimited,
			},
			want: "rate limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectivityError_Unwrap(t *testing.T) {
	err := &provider.ConnectivityError{Kind: provider.ErrInvalidAPIKey}
	if !errors.Is(err, provider.ErrInvalidAPIKey) {
		t.Fatalf("errors.Is should match ErrInvalidAPIKey")
	}
	if errors.Is(err, provider.ErrForbidden) {
		t.Fatalf("errors.Is should not match ErrForbidden")
	}
}

func TestProvider_CheckConnectivityWithChat_InterfaceCompliance(t *testing.T) {
	var _ provider.Provider = (*provider.OpenAIProvider)(nil)
	var _ provider.Provider = (*provider.AnthropicProvider)(nil)
}

// allowPrivateForTest 临时放行私网 / 回环上游地址，使 httptest（127.0.0.1）mock
// 可以充当可达上游；用例结束时由 t.Cleanup 还原为默认严格模式，保证用例间隔离。
//
// 由于 SetAllowPrivateUpstreams 是包级全局开关，这些用例之间必须串行执行，
// 禁止使用 t.Parallel。
func allowPrivateForTest(t *testing.T) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
}

// --- Chat probe: 默认严格模式下的拦截 ---
//
// 默认严格模式下，指向回环 / 私网 / 元数据地址的探测都会在 URL 校验阶段被拒绝。
// 可达 mock 上游（httptest 落在 127.0.0.1）的成功路径用例位于 openai_test.go /
// anthropic_test.go，它们先用 allowPrivateForTest 放行私网再断言请求形状与响应分类。

func TestOpenAIProbe_StrictBlocksDisallowedTargets(t *testing.T) {
	p := &provider.OpenAIProvider{}
	for _, base := range []string{
		"http://127.0.0.1:8080",
		"http://10.0.0.1",
		"http://192.168.1.1",
		"http://localhost:9090",
		"http://169.254.169.254",
		"http://[::1]:8080",
		"file:///etc/passwd",
	} {
		t.Run(base, func(t *testing.T) {
			_, err := p.CheckConnectivityWithChat(context.Background(), "sk", base, "gpt-4o-mini")
			if err == nil {
				t.Fatalf("base=%q: expected SSRF rejection, got nil", base)
			}
			if !errors.Is(err, provider.ErrNetworkUnreachable) {
				t.Fatalf("base=%q: err = %v, want ErrNetworkUnreachable", base, err)
			}
			var ce *provider.ConnectivityError
			if !errors.As(err, &ce) {
				t.Fatalf("base=%q: err is not *ConnectivityError: %v", base, err)
			}
			if ce.StatusCode != 0 {
				t.Fatalf("base=%q: StatusCode = %d, want 0 (request never completed)", base, ce.StatusCode)
			}
			if ce.Cause == nil {
				t.Fatalf("base=%q: Cause should be populated with the rejection reason", base)
			}
		})
	}
}

func TestAnthropicProbe_StrictBlocksDisallowedTargets(t *testing.T) {
	p := &provider.AnthropicProvider{}
	for _, base := range []string{
		"http://127.0.0.1:9090",
		"http://10.0.0.1",
		"http://192.168.1.1",
		"http://169.254.169.254",
		"http://[::1]:8080",
		"gopher://127.0.0.1:11211/_stats",
	} {
		t.Run(base, func(t *testing.T) {
			_, err := p.CheckConnectivityWithChat(context.Background(), "sk", base, "claude")
			if err == nil {
				t.Fatalf("base=%q: expected SSRF rejection, got nil", base)
			}
			if !errors.Is(err, provider.ErrNetworkUnreachable) {
				t.Fatalf("base=%q: err = %v, want ErrNetworkUnreachable", base, err)
			}
			var ce *provider.ConnectivityError
			if !errors.As(err, &ce) {
				t.Fatalf("base=%q: err is not *ConnectivityError: %v", base, err)
			}
			if ce.Cause == nil {
				t.Fatalf("base=%q: Cause should be populated with the rejection reason", base)
			}
		})
	}
}

// TestOpenAIProbe_TransportFailure verifies that a base URL which passes URL-level
// validation but cannot be dialled is surfaced as ErrNetworkUnreachable with the
// transport cause recorded. `.invalid` is reserved by RFC 2606 and never resolves.
func TestOpenAIProbe_TransportFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p := &provider.OpenAIProvider{}
	_, err := p.CheckConnectivityWithChat(ctx, "sk", "https://AIGate-nonexistent.invalid", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected transport failure, got nil")
	}
	if !errors.Is(err, provider.ErrNetworkUnreachable) {
		t.Fatalf("err = %v, want ErrNetworkUnreachable", err)
	}
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) {
		t.Fatalf("err is not *ConnectivityError: %v", err)
	}
	if ce.Cause == nil {
		t.Fatalf("ConnectivityError.Cause should be populated for a transport failure: %#v", ce)
	}
	if ce.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0 (request never completed)", ce.StatusCode)
	}
}

// TestOpenAIProbe_InvalidURL verifies that an unparseable base URL is rejected
// before any connection attempt, with the underlying cause preserved.
func TestOpenAIProbe_InvalidURL(t *testing.T) {
	p := &provider.OpenAIProvider{}
	_, err := p.CheckConnectivityWithChat(context.Background(), "sk", "http://invalid\x7fhost", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected error for invalid URL, got nil")
	}
	if !errors.Is(err, provider.ErrNetworkUnreachable) {
		t.Fatalf("err = %v, want ErrNetworkUnreachable", err)
	}
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) {
		t.Fatalf("err is not *ConnectivityError: %v", err)
	}
	if ce.Cause == nil {
		t.Fatalf("ConnectivityError.Cause should be populated for invalid URL: %#v", ce)
	}
	if ce.StatusCode != 0 {
		t.Fatalf("ConnectivityError.StatusCode = %d, want 0 (request never completed)", ce.StatusCode)
	}
}

func TestAnthropicProbe_InvalidURL(t *testing.T) {
	p := &provider.AnthropicProvider{}
	_, err := p.CheckConnectivityWithChat(context.Background(), "sk", "http://invalid\x7fhost", "claude")
	if err == nil {
		t.Fatalf("expected error for invalid URL, got nil")
	}
	if !errors.Is(err, provider.ErrNetworkUnreachable) {
		t.Fatalf("err = %v, want ErrNetworkUnreachable", err)
	}
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) {
		t.Fatalf("err is not *ConnectivityError: %v", err)
	}
	if ce.Cause == nil {
		t.Fatalf("ConnectivityError.Cause should be populated for invalid URL: %#v", ce)
	}
}

// TestAnthropicProbe_DefaultBaseWhenEmpty verifies that an empty apiBase falls back
// to the Anthropic default endpoint. 该端点无法在单测里 stub（本用例不打开私网放行，
// 回环 mock 仍被 SSRF 保护拒绝），因此这里只断言探测确实发起了上游请求、并通过
// *ConnectivityError 汇报结果，而不是在本地失败。baseURL() 对空 base 的
// 回落行为由 provider_internal_test.go 的白盒用例精确覆盖。
func TestAnthropicProbe_DefaultBaseWhenEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	p := &provider.AnthropicProvider{}
	_, err := p.CheckConnectivityWithChat(ctx, "sk", "", "claude")
	if err == nil {
		t.Skip("network reachable to api.anthropic.com — cannot assert default base in this env")
	}
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConnectivityError (probe must reach an upstream attempt)", err)
	}
}
