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

package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigate/internal/provider"
)

// ---------------------------------------------------------------------------
// 响应结构（仅本文件使用）
// ---------------------------------------------------------------------------

type connectivityResp struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	ErrorKind string `json:"error_kind"`
	Message   string `json:"message"`
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// allowPrivateUpstreamsForTest 放行私网 / 回环上游（mock 落在 127.0.0.1）；
// 用例结束还原为默认严格模式，保证用例间隔离。
// 注意：该开关是包级全局量，本包用例不得使用 t.Parallel。
func allowPrivateUpstreamsForTest(t *testing.T) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
}

// createProbeModel 经管理面创建用于探测的模型（自定义 URL），返回其 ID。
func createProbeModel(t *testing.T, srv *httptest.Server, adminToken, modelID, modelType, url string) uint {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   modelID,
		"model_name": modelID,
		"url":        url,
		"api_key":    "sk-upstream-secret",
		"model_type": modelType,
	})
	assertStatus(t, status, http.StatusCreated, body)
	return mustDecode[modelResp](t, body).ID
}

// probeModel 调用连通性端点并解析响应（状态码单独返回，便于断言 404/400）。
func probeModel(t *testing.T, srv *httptest.Server, adminToken string, modelID uint) (int, connectivityResp) {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/"+itoa(modelID)+"/connectivity", adminToken, nil)
	if status != http.StatusOK {
		return status, connectivityResp{}
	}
	return status, mustDecode[connectivityResp](t, body)
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestModelConnectivity_ReachableUpstream 验证 mock 上游可达时返回 ok + 正 latency。
func TestModelConnectivity_ReachableUpstream(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	srv, _, adminToken := newAdminServer(t)

	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// 稍作延迟，保证 latency_ms 为可观测的正值。
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	id := createProbeModel(t, srv, adminToken, "m-reachable", "openai-completions", upstream.URL+"/v1")
	status, resp := probeModel(t, srv, adminToken, id)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !resp.OK {
		t.Fatalf("ok = false, message = %q", resp.Message)
	}
	if resp.LatencyMS <= 0 {
		t.Fatalf("latency_ms = %d, want > 0", resp.LatencyMS)
	}
	if resp.ErrorKind != "" || resp.Message != "" {
		t.Fatalf("success response should not carry error_kind/message: %+v", resp)
	}
	// 探测以最小对话请求（max_tokens=1）打上游的 /chat/completions，并带模型的 api_key。
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-upstream-secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer sk-upstream-secret", gotAuth)
	}
	if got, ok := gotBody["max_tokens"].(float64); !ok || got != 1 {
		t.Fatalf("probe request max_tokens = %v, want 1", gotBody["max_tokens"])
	}
}

// TestModelConnectivity_NetworkUnreachable 验证不可达地址归类为 network_unreachable。
func TestModelConnectivity_NetworkUnreachable(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	srv, _, adminToken := newAdminServer(t)

	// 127.0.0.1:1 上无监听：放行私网后仍会连接失败。
	id := createProbeModel(t, srv, adminToken, "m-down", "openai-completions", "http://127.0.0.1:1/v1")
	status, resp := probeModel(t, srv, adminToken, id)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (probe result in body)", status)
	}
	if resp.OK || resp.ErrorKind != "network_unreachable" {
		t.Fatalf("resp = %+v, want ok=false kind=network_unreachable", resp)
	}
	if resp.Message == "" {
		t.Fatalf("failure response should carry message")
	}
}

// TestModelConnectivity_PrivateUpstreamBlockedByDefault 验证默认严格模式下私网/回环被安全策略拦截。
func TestModelConnectivity_PrivateUpstreamBlockedByDefault(t *testing.T) {
	provider.SetAllowPrivateUpstreams(false) // 显式严格（默认值）
	srv, _, adminToken := newAdminServer(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("strict mode should not reach private upstream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	id := createProbeModel(t, srv, adminToken, "m-private", "openai-completions", upstream.URL+"/v1")
	status, resp := probeModel(t, srv, adminToken, id)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (probe result in body)", status)
	}
	if resp.OK || resp.ErrorKind != "network_unreachable" {
		t.Fatalf("resp = %+v, want ok=false kind=network_unreachable (SSRF blocked)", resp)
	}
}

// TestModelConnectivity_ErrorKindMapping 验证上游状态码到 error_kind 枚举的映射。
func TestModelConnectivity_ErrorKindMapping(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	srv, _, adminToken := newAdminServer(t)

	cases := []struct {
		name     string
		status   int
		wantKind string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "forbidden"},
		{"rate_limited", http.StatusTooManyRequests, "rate_limited"},
		{"upstream_server", http.StatusInternalServerError, "upstream_server"},
		{"upstream_client", http.StatusBadRequest, "upstream_client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			}))
			defer upstream.Close()

			id := createProbeModel(t, srv, adminToken, "m-"+tc.name, "openai-completions", upstream.URL+"/v1")
			status, resp := probeModel(t, srv, adminToken, id)

			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (probe result in body)", status)
			}
			if resp.OK || resp.ErrorKind != tc.wantKind {
				t.Fatalf("resp = %+v, want ok=false kind=%s", resp, tc.wantKind)
			}
			if !strings.Contains(resp.Message, "status") {
				t.Fatalf("message should contain upstream status description, got %q", resp.Message)
			}
		})
	}
}

// TestModelConnectivity_AnthropicUpstream 验证 anthropic-messages 模型走 Anthropic 探测路径。
func TestModelConnectivity_AnthropicUpstream(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	srv, _, adminToken := newAdminServer(t)

	var (
		gotPath    string
		gotAPIKey  string
		gotVersion string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`),
		)
	}))
	defer upstream.Close()

	// anthropic-messages 的 url 为 host（探测时拼 /v1/messages）。
	id := createProbeModel(t, srv, adminToken, "m-claude", "anthropic-messages", upstream.URL)
	status, resp := probeModel(t, srv, adminToken, id)

	if status != http.StatusOK || !resp.OK {
		t.Fatalf("status = %d, resp = %+v, want ok", status, resp)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-upstream-secret" {
		t.Fatalf("x-api-key = %q, want sk-upstream-secret", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatalf("anthropic-version header missing")
	}
}

// TestModelConnectivity_ModelNotFound 验证模型不存在返回 404。
func TestModelConnectivity_ModelNotFound(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/999/connectivity", adminToken, nil)
	assertStatus(t, status, http.StatusNotFound, body)
	if msg := decodeError(t, body).Error.Message; msg != "model not found" {
		t.Fatalf("message = %q, want model not found", msg)
	}
}

// TestModelConnectivity_InvalidID 验证非法路径 id 返回 400。
func TestModelConnectivity_InvalidID(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/abc/connectivity", adminToken, nil)
	assertStatus(t, status, http.StatusBadRequest, body)
}

// TestModelConnectivity_RequiresAdmin 验证连通性端点位于管理面鉴权内。
func TestModelConnectivity_RequiresAdmin(t *testing.T) {
	srv, _, _ := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/1/connectivity", "", nil)
	assertStatus(t, status, http.StatusUnauthorized, body)
}
