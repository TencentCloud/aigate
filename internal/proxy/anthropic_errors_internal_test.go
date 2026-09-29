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

package proxy

// 白盒单测：/v1/messages 入站的上游错误映射（状态码 → 错误 kind、状态文案）。
// 这两条路径此前零覆盖，是 Anthropic 错误处理相关重构的安全网。

import (
	"net/http"
	"testing"
)

func TestAnthropicErrorKindForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, kindAuthenticationError},
		{http.StatusBadRequest, kindInvalidRequestError},
		{http.StatusTooManyRequests, kindRateLimitError},
		{http.StatusNotFound, kindInvalidRequestError},
		{http.StatusUnprocessableEntity, kindInvalidRequestError},
		{http.StatusInternalServerError, kindAPIError},
		{http.StatusBadGateway, kindAPIError},
		{http.StatusOK, kindAPIError}, // 非错误码回退 api_error
	}
	for _, tc := range cases {
		if got := anthropicErrorKindForStatus(tc.status); got != tc.want {
			t.Errorf("anthropicErrorKindForStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestUpstreamStatusMessage(t *testing.T) {
	if got := upstreamStatusMessage(http.StatusTooManyRequests); got != "upstream returned status 429" {
		t.Fatalf("upstreamStatusMessage(429) = %q, want %q", got, "upstream returned status 429")
	}
	if got := upstreamStatusMessage(http.StatusServiceUnavailable); got != "upstream returned status 503" {
		t.Fatalf("upstreamStatusMessage(503) = %q, want %q", got, "upstream returned status 503")
	}
}
