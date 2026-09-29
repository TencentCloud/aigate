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

package provider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusOK, want: nil},
		{status: http.StatusNoContent, want: nil},
		{status: http.StatusFound, want: nil},
		{status: http.StatusUnauthorized, want: ErrInvalidAPIKey},
		{status: http.StatusForbidden, want: ErrForbidden},
		{status: http.StatusTooManyRequests, want: ErrRateLimited},
		{status: http.StatusBadRequest, want: ErrUpstreamClient},
		{status: http.StatusNotFound, want: ErrUpstreamClient},
		{status: http.StatusInternalServerError, want: ErrUpstreamServer},
		{status: http.StatusBadGateway, want: ErrUpstreamServer},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			got := classifyHTTPStatus(tt.status)
			if !errors.Is(got, tt.want) {
				t.Fatalf("classifyHTTPStatus(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestReadBodySnippet_TrimsCollapsesAndDrains(t *testing.T) {
	r := strings.NewReader(" \nalpha\r\nbeta\n ")

	if got := readBodySnippet(r, 1024); got != "alpha  beta" {
		t.Fatalf("readBodySnippet() = %q, want %q", got, "alpha  beta")
	}

	remaining, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll remaining body: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("body should be drained, remaining %q", remaining)
	}
}

func TestReadBodySnippet_LimitDefaultAndDrains(t *testing.T) {
	r := strings.NewReader("abcdef")
	if got := readBodySnippet(r, 3); got != "abc" {
		t.Fatalf("readBodySnippet() = %q, want %q", got, "abc")
	}
	remaining, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll remaining body: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("body should be drained after limit, remaining %q", remaining)
	}

	r = strings.NewReader(strings.Repeat("a", 600))
	got := readBodySnippet(r, 0)
	if got != strings.Repeat("a", 512) {
		t.Fatalf("readBodySnippet() length = %d, want 512", len(got))
	}
	remaining, err = io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll remaining body: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("body should be drained after default limit, remaining length %d", len(remaining))
	}
}

// ─── applyCustomHeaders ─────────────────────────────────────────────────

func TestApplyCustomHeaders_SetsNewHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	headers := map[string]string{"X-Custom": "value1", "X-Request-Id": "req-123"}
	applyCustomHeaders(req, headers)

	if v := req.Header.Get("X-Custom"); v != "value1" {
		t.Errorf("X-Custom = %q, want %q", v, "value1")
	}
	if v := req.Header.Get("X-Request-Id"); v != "req-123" {
		t.Errorf("X-Request-Id = %q, want %q", v, "req-123")
	}
}

func TestApplyCustomHeaders_DoesNotOverwriteExisting(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	req.Header.Set("Content-Type", "application/json")
	// 试图覆盖 Content-Type，但已存在，不应覆盖
	headers := map[string]string{"Content-Type": "text/plain"}
	applyCustomHeaders(req, headers)

	if v := req.Header.Get("Content-Type"); v != "application/json" {
		t.Errorf("existing header must not be overwritten: Content-Type = %q, want %q", v, "application/json")
	}
}

func TestApplyCustomHeaders_NilMap(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	applyCustomHeaders(req, nil)
	// 不应 panic
	if len(req.Header) != 0 {
		t.Errorf("nil headers must not set any request header, got=%v", req.Header)
	}
}

func TestApplyCustomHeaders_EmptyMap(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	applyCustomHeaders(req, map[string]string{})
	if len(req.Header) != 0 {
		t.Errorf("empty map must not set any request header, got=%v", req.Header)
	}
}

// ─── richErrorf ────────────────────────────────────────────────────────

// TestRichErrorf_WrapsSentinel 验证英文文案错误仍通过 %w 保留底层 sentinel，
// 使 errors.Is 可以命中（例如调用方依赖 errors.Is(err, errSSRFBlocked) 判定拦截）。
func TestRichErrorf_WrapsSentinel(t *testing.T) {
	err := richErrorf(errSSRFBlocked, "url scheme not allowed: %s", "file")
	if err == nil {
		t.Fatal("richErrorf returned nil")
	}
	if !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("errors.Is(err, errSSRFBlocked) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "url scheme not allowed: file") {
		t.Fatalf("error message = %q, want it to contain the english template", err.Error())
	}
	if !strings.Contains(err.Error(), errSSRFBlocked.Error()) {
		t.Fatalf("error message = %q, want it to contain the sentinel text", err.Error())
	}
}

func TestRichErrorf_NilBase(t *testing.T) {
	err := richErrorf(nil, "boom")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("richErrorf(nil, ...) = %v, want %q", err, "boom")
	}
}

// ─── small helpers ─────────────────────────────────────────────────────

func TestFirstPositiveInt(t *testing.T) {
	tests := []struct {
		name   string
		values []int
		want   int
	}{
		{name: "empty", values: nil, want: 0},
		{name: "all zero", values: []int{0, 0}, want: 0},
		{name: "first wins", values: []int{5, 7}, want: 5},
		{name: "skips zeros", values: []int{0, 3, 9}, want: 3},
		{name: "single", values: []int{42}, want: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstPositiveInt(tt.values...); got != tt.want {
				t.Fatalf("firstPositiveInt(%v) = %d, want %d", tt.values, got, tt.want)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	oa := &OpenAIProvider{}
	if got := oa.baseURL("https://api.openai.com/v1/"); got != "https://api.openai.com/v1" {
		t.Fatalf("openai baseURL() = %q, want trailing slash trimmed", got)
	}
	if got := oa.baseURL(""); got != "" {
		t.Fatalf("openai baseURL(\"\") = %q, want empty", got)
	}

	an := &AnthropicProvider{}
	if got := an.baseURL("https://proxy.internal/anthropic/"); got != "https://proxy.internal/anthropic" {
		t.Fatalf("anthropic baseURL() = %q, want trailing slash trimmed", got)
	}
	// 空 apiBase 必须回落到官方默认端点。
	if got := an.baseURL(""); got != anthropicDefaultBase {
		t.Fatalf("anthropic baseURL(\"\") = %q, want %q", got, anthropicDefaultBase)
	}
}

// TestReadUpstreamErrorBody 覆盖上游错误体读取：正常透传 + 超过上限（1 MiB）时截断。
func TestReadUpstreamErrorBody(t *testing.T) {
	small := `{"error":"boom"}`
	if got := string(readUpstreamErrorBody(strings.NewReader(small))); got != small {
		t.Fatalf("passthrough = %q, want %q", got, small)
	}
	big := strings.Repeat("x", maxUpstreamErrorBodyBytes+1024)
	if got := readUpstreamErrorBody(strings.NewReader(big)); len(got) != maxUpstreamErrorBodyBytes {
		t.Fatalf("capped length = %d, want %d", len(got), maxUpstreamErrorBodyBytes)
	}
}
