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

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// okChatBody 是一个结构完整的 OpenAI 兼容成功响应（usage 无 null，便于逐字节比对）。
const okChatBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-a",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// userOnlyBody 是不含 model 字段的最小 chat 请求体（应回退到 primary）。
const userOnlyBody = `{"messages":[{"role":"user","content":"hi"}]}`

// assertErrorResponse 断言统一错误响应（状态码 + error.message + error.code）。
func assertErrorResponse(t *testing.T, resp *http.Response, wantStatus int, wantMessage string) {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, wantStatus, readBody(t, resp))
	}
	body := readBody(t, resp)
	var eb httpx.ErrorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	if eb.Error.Message != wantMessage {
		t.Fatalf("error.message = %q, want %q", eb.Error.Message, wantMessage)
	}
	if eb.Error.Code != wantStatus {
		t.Fatalf("error.code = %d, want %d", eb.Error.Code, wantStatus)
	}
}

// ---------------------------------------------------------------------------
// 1. 鉴权
// ---------------------------------------------------------------------------

func TestChatCompletions_Authentication(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, validToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	disabledKey, disabledToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	if _, err := e.store.UpdateAPIKey(ctx(), disabledKey.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable api key: %v", err)
	}

	t.Run("missing authorization", func(t *testing.T) {
		resp := e.postChat(t, "", reqWithModel("mock-a"), nil)
		assertErrorResponse(t, resp, http.StatusUnauthorized, "missing API key")
	})

	t.Run("unknown token", func(t *testing.T) {
		resp := e.postChat(t, "sk-this-token-does-not-exist", reqWithModel("mock-a"), nil)
		assertErrorResponse(t, resp, http.StatusUnauthorized, "invalid API key")
	})

	t.Run("disabled key", func(t *testing.T) {
		resp := e.postChat(t, disabledToken, reqWithModel("mock-a"), nil)
		assertErrorResponse(t, resp, http.StatusUnauthorized, "invalid API key")
	})

	t.Run("valid key passes", func(t *testing.T) {
		resp := e.postChat(t, validToken, reqWithModel("mock-a"), nil)
		assertStatus(t, resp, http.StatusOK)
		if body := readBody(t, resp); body != okChatBody {
			t.Fatalf("body = %q, want upstream body echoed", body)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. 解析 / 解析模型错误分支
// ---------------------------------------------------------------------------

func TestChatCompletions_RequestErrors(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	bound := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, boundToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(bound.ID, 0))
	_, noBindingToken := mustCreateKey(t, e.store, e.user.ID, 0)

	t.Run("invalid json body", func(t *testing.T) {
		resp := e.postChat(t, boundToken, `{"model":"mock-a","messages":[`, nil)
		assertErrorResponse(t, resp, http.StatusBadRequest, "invalid json body")
	})

	t.Run("no binding and no model requested", func(t *testing.T) {
		resp := e.postChat(t, noBindingToken, userOnlyBody, nil)
		assertErrorResponse(t, resp, http.StatusBadRequest, "no available model for this key")
	})

	t.Run("explicit model not bound", func(t *testing.T) {
		resp := e.postChat(t, boundToken, reqWithModel("not-bound-model"), nil)
		assertErrorResponse(t, resp, http.StatusBadRequest, "requested model is not bound to this key")
	})

	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called on request error paths, got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// 3. max_tokens 强制
// ---------------------------------------------------------------------------

func TestChatCompletions_EnforceMaxTokens(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, cappedToken := mustCreateKey(t, e.store, e.user.ID, 100, primary(m.ID, 0))
	_, uncappedToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("caps max_tokens", func(t *testing.T) {
		resp := e.postChat(
			t,
			cappedToken,
			`{"model":"mock-a","max_tokens":5000,"messages":[{"role":"user","content":"hi"}]}`,
			nil,
		)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		body := e.rec.Body()
		if got, ok := bodyNumber(t, body, "max_tokens"); !ok || got != 100 {
			t.Fatalf("upstream max_tokens = %v (present=%v), want 100", got, ok)
		}
	})

	t.Run("caps max_completion_tokens", func(t *testing.T) {
		resp := e.postChat(
			t,
			cappedToken,
			`{"model":"mock-a","max_completion_tokens":5000,"messages":[{"role":"user","content":"hi"}]}`,
			nil,
		)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		body := e.rec.Body()
		if got, ok := bodyNumber(t, body, "max_tokens"); !ok || got != 100 {
			t.Fatalf("upstream max_tokens = %v (present=%v), want 100", got, ok)
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Fatalf("upstream body must not keep max_completion_tokens for a legacy model: %v", body)
		}
	})

	t.Run("injects max_tokens when both absent", func(t *testing.T) {
		resp := e.postChat(t, cappedToken, reqWithModel("mock-a"), nil)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		body := e.rec.Body()
		if got, ok := bodyNumber(t, body, "max_tokens"); !ok || got != 100 {
			t.Fatalf("upstream max_tokens = %v (present=%v), want 100 injected", got, ok)
		}
	})

	t.Run("key without limit does not inject", func(t *testing.T) {
		resp := e.postChat(t, uncappedToken, reqWithModel("mock-a"), nil)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		body := e.rec.Body()
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("upstream body must not contain max_tokens when key has no limit: %v", body)
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Fatalf("upstream body must not contain max_completion_tokens when key has no limit: %v", body)
		}
	})
}

// ---------------------------------------------------------------------------
// 4. 模型名替换 / 解析
// ---------------------------------------------------------------------------

func TestChatCompletions_ModelNameReplacement(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	primaryModel := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
	aliasModel := mustCreateAIModel(t, e.store, newAIModel("vendor/model:free", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0,
		primary(primaryModel.ID, 0), fallback(aliasModel.ID, 1))

	t.Run("case-insensitive match replaced by stored id", func(t *testing.T) {
		resp := e.postChat(t, token, reqWithModel("MOCK-GPT"), nil)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Body()["model"]; got != "mock-gpt" {
			t.Fatalf("upstream body.model = %v, want stored model_id %q", got, "mock-gpt")
		}
	})

	t.Run("slug alias match replaced by stored id", func(t *testing.T) {
		resp := e.postChat(t, token, reqWithModel("VENDOR-MODEL-FREE"), nil)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Body()["model"]; got != "vendor/model:free" {
			t.Fatalf("upstream body.model = %v, want stored model_id %q", got, "vendor/model:free")
		}
	})

	t.Run("unspecified model falls back to primary", func(t *testing.T) {
		resp := e.postChat(t, token, userOnlyBody, nil)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Body()["model"]; got != "mock-gpt" {
			t.Fatalf("upstream body.model = %v, want primary model_id %q", got, "mock-gpt")
		}
	})
}

// ---------------------------------------------------------------------------
// 5. 自定义头透传
// ---------------------------------------------------------------------------

func TestChatCompletions_CustomHeaderPassthrough(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := newAIModel("mock-a", e.upstream.URL)
	m.CustomHTTPHeaders = `{"X-Feature":"beta"}`
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("matching value is forwarded", func(t *testing.T) {
		resp := e.postChat(t, token, reqWithModel("mock-a"), map[string]string{"X-Feature": "beta"})
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Header("X-Feature"); got != "beta" {
			t.Fatalf("upstream X-Feature = %q, want %q", got, "beta")
		}
	})

	t.Run("mismatching value drops the header", func(t *testing.T) {
		resp := e.postChat(t, token, reqWithModel("mock-a"), map[string]string{"X-Feature": "gamma"})
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Header("X-Feature"); got != "" {
			t.Fatalf("upstream X-Feature = %q, want empty (passthrough disabled)", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. 非流式透传
// ---------------------------------------------------------------------------

func TestChatCompletions_NonStreamingPassthrough(t *testing.T) {
	t.Run("200 body is echoed verbatim", func(t *testing.T) {
		e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
		m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
		_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
		assertStatus(t, resp, http.StatusOK)
		if body := readBody(t, resp); body != okChatBody {
			t.Fatalf("client body = %q, want verbatim upstream body %q", body, okChatBody)
		}
	})

	t.Run("429 status and body are forwarded", func(t *testing.T) {
		const rateLimitedBody = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`
		e := newEnv(t, respondConstantJSON(http.StatusTooManyRequests, rateLimitedBody))
		m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
		_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
		assertStatus(t, resp, http.StatusTooManyRequests)
		if body := readBody(t, resp); body != rateLimitedBody {
			t.Fatalf("client body = %q, want %q", body, rateLimitedBody)
		}
	})
}

// ---------------------------------------------------------------------------
// 7. 流式
// ---------------------------------------------------------------------------

func TestChatCompletions_Streaming(t *testing.T) {
	const streamUsage = `{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}`

	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("mock upstream response writer does not support flushing")
			return
		}
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"content":"he"}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":"llo"}}]}` + "\n\n",
			`data: {"choices":[{"delta":{}}],"usage":` + streamUsage + `}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
	})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postChat(t, token, `{"model":"mock-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	assertStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	if !strings.Contains(body, "data: ") {
		t.Fatalf("streaming response should contain SSE data chunks, got %q", body)
	}
	if !strings.Contains(body, `"content":"he"`) || !strings.Contains(body, `"content":"llo"`) {
		t.Fatalf("streaming chunks were not forwarded verbatim, got %q", body)
	}
	if trimmed := strings.TrimSpace(body); !strings.HasSuffix(trimmed, "data: [DONE]") {
		t.Fatalf("streaming response should end with %q, got %q", "data: [DONE]", trimmed)
	}

	if got := e.rec.Body()["stream"]; got != true {
		t.Fatalf("upstream body.stream = %v, want true", got)
	}
	if got := e.rec.Header("Content-Type"); got != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", got)
	}

	// 流结束后用量应被记账。
	e.stopUsage()
	read := openReadDB(t, e.dsn)
	var row model.LLMUsageLog
	if err := read.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("load streaming usage log: %v", err)
	}
	if row.APIKeyID != key.ID || row.Model != "mock-a" {
		t.Fatalf("streaming usage log = %+v, want key_id=%d model=mock-a", row, key.ID)
	}
	if row.StatusCode != http.StatusOK {
		t.Fatalf("streaming usage status_code = %d, want 200", row.StatusCode)
	}
	if row.TotalTokens != 10 || row.PromptTokens != 4 || row.CompletionTokens != 6 {
		t.Fatalf("streaming usage tokens = %+v, want prompt=4 completion=6 total=10", row)
	}
}

// ---------------------------------------------------------------------------
// 8. 上游不可达
// ---------------------------------------------------------------------------

func TestChatCompletions_UpstreamUnreachable(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))

	// 先起再关，得到一个保证无人监听的端口。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", deadURL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	var eb httpx.ErrorBody
	body := readBody(t, resp)
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	if !strings.HasPrefix(eb.Error.Message, "failed to connect to LLM backend") {
		t.Fatalf("error.message = %q, want prefix %q", eb.Error.Message, "failed to connect to LLM backend")
	}
}

// ---------------------------------------------------------------------------
// 9. /v1/models
// ---------------------------------------------------------------------------

func TestListModels(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))

	mixedCase := mustCreateAIModel(t, e.store, newAIModel("Mock-GPT", e.upstream.URL))
	dupLower := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
	// 注意：AIModel.Enabled 带 `default:true`，GORM 在 Create 时会忽略 false 零值，
	// 因此禁用必须走 UpdateAIModel 的 map 更新（否则会静默落成启用）。
	disabled := mustCreateAIModel(t, e.store, newAIModel("mock-disabled", e.upstream.URL))
	if _, err := e.store.UpdateAIModel(ctx(), disabled.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable ai model: %v", err)
	}

	_, token := mustCreateKey(t, e.store, e.user.ID, 0,
		primary(mixedCase.ID, 0), fallback(dupLower.ID, 1), fallback(disabled.ID, 2))

	resp := e.getModels(t, token)
	assertStatus(t, resp, http.StatusOK)

	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &payload); err != nil {
		t.Fatalf("decode models payload: %v", err)
	}
	if payload.Object != "list" {
		t.Fatalf("object = %q, want %q", payload.Object, "list")
	}
	if len(payload.Data) != 1 {
		t.Fatalf(
			"data len = %d, want 1 (lowercased + deduped + disabled excluded): %+v",
			len(payload.Data),
			payload.Data,
		)
	}
	got := payload.Data[0]
	if got.ID != "mock-gpt" {
		t.Fatalf("id = %q, want lowercased %q", got.ID, "mock-gpt")
	}
	if got.Object != "model" {
		t.Fatalf("object = %q, want %q", got.Object, "model")
	}
	if got.OwnedBy != "mock-provider" {
		t.Fatalf("owned_by = %q, want %q", got.OwnedBy, "mock-provider")
	}

	t.Run("no auth", func(t *testing.T) {
		resp := e.getModels(t, "")
		assertErrorResponse(t, resp, http.StatusUnauthorized, "missing API key")
	})
}

// ---------------------------------------------------------------------------
// 11. 用量字段落库
// ---------------------------------------------------------------------------

func TestChatCompletions_UsageLogged(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
	assertStatus(t, resp, http.StatusOK)
	_ = readBody(t, resp)

	// 排空异步队列后再做表侧断言。
	e.stopUsage()
	read := openReadDB(t, e.dsn)

	var row model.LLMUsageLog
	if err := read.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if row.APIKeyID != key.ID {
		t.Fatalf("usage.api_key_id = %d, want %d", row.APIKeyID, key.ID)
	}
	if row.UserID != e.user.ID {
		t.Fatalf("usage.user_id = %d, want %d", row.UserID, e.user.ID)
	}
	if row.Model != "mock-a" {
		t.Fatalf("usage.model = %q, want %q", row.Model, "mock-a")
	}
	if row.Provider != "mock-provider" {
		t.Fatalf("usage.provider = %q, want %q", row.Provider, "mock-provider")
	}
	if row.StatusCode != http.StatusOK {
		t.Fatalf("usage.status_code = %d, want 200", row.StatusCode)
	}
	if row.PromptTokens != 3 || row.CompletionTokens != 2 || row.TotalTokens != 5 {
		t.Fatalf("usage tokens = %+v, want prompt=3 completion=2 total=5", row)
	}

	if n := countRows(t, read, &model.LLMUsageLog{}); n != 1 {
		t.Fatalf("llm_usage_logs rows = %d, want 1", n)
	}
	if n := countRows(t, read, &model.DailyUsageSummary{}); n != 1 {
		t.Fatalf("daily_usage_summaries rows = %d, want 1", n)
	}
}
