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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 1. 鉴权
// ---------------------------------------------------------------------------

func TestAnthropicIngress_Authentication(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
	m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", e.upstream.URL))
	_, validToken := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("x-api-key accepted", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, validToken, anthropicReq("mock-claude", 64))
		assertStatus(t, resp, http.StatusOK)
		if got := readBody(t, resp); got != okAnthropicMessage {
			t.Fatalf("passthrough response must be echoed verbatim, got %q", got)
		}
	})

	t.Run("Bearer accepted", func(t *testing.T) {
		resp := e.postMessagesBearer(t, validToken, anthropicReq("mock-claude", 64))
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
	})

	t.Run("missing credential returns 401", func(t *testing.T) {
		resp := e.postMessages(t, anthropicReq("mock-claude", 64), nil)
		if msg := assertAnthropicError(t, resp, http.StatusUnauthorized, "authentication_error"); msg != "missing API key" {
			t.Fatalf("error.message = %q, want %q", msg, "missing API key")
		}
	})

	t.Run("bad key returns 401", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, "sk-this-token-does-not-exist", anthropicReq("mock-claude", 64))
		if msg := assertAnthropicError(t, resp, http.StatusUnauthorized, "authentication_error"); msg != "invalid API key" {
			t.Fatalf("error.message = %q, want %q", msg, "invalid API key")
		}
	})
}

// ---------------------------------------------------------------------------
// 2. 入站校验
// ---------------------------------------------------------------------------

func TestAnthropicIngress_Validation(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
	m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("missing model", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
		if msg := assertAnthropicError(t, resp, http.StatusBadRequest, "invalid_request_error"); msg != "model is required" {
			t.Fatalf("error.message = %q, want %q", msg, "model is required")
		}
	})

	t.Run("missing max_tokens", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token, `{"model":"mock-claude","messages":[{"role":"user","content":"hi"}]}`)
		if msg := assertAnthropicError(
			t, resp, http.StatusBadRequest, "invalid_request_error",
		); msg != "max_tokens is required" {
			t.Fatalf("error.message = %q, want %q", msg, "max_tokens is required")
		}
	})

	t.Run("max_tokens not positive", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-claude", 0))
		assertAnthropicError(t, resp, http.StatusBadRequest, "invalid_request_error")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token, `{"model":"mock-claude","max_tokens":`)
		if msg := assertAnthropicError(t, resp, http.StatusBadRequest, "invalid_request_error"); msg != "invalid json body" {
			t.Fatalf("error.message = %q, want %q", msg, "invalid json body")
		}
	})

	t.Run("unknown fields ignored", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token,
			`{"model":"mock-claude","max_tokens":64,"metadata":{"user_id":"u1","custom":true},`+
				`"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
	})

	if got := e.rec.Calls(); got != 1 {
		t.Fatalf("only the unknown-fields-ignored case should reach upstream, got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 配额超限
// ---------------------------------------------------------------------------

func TestAnthropicIngress_QuotaExceeded(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
	m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", e.upstream.URL))

	group := &model.Group{Name: "anthropic-limited", TokenQuotaRules: `[{"mode":"day","limit":5}]`}
	if err := e.store.CreateGroup(ctx(), group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	key, token := mustCreateKeyWithGroup(t, e.store, e.user.ID, group.ID, 0, primary(m.ID, 0))

	// 预置一条该分组用量，使 day 限额（5）立即超限。
	if err := e.store.InsertUsageLog(ctx(), &model.LLMUsageLog{
		APIKeyID:    key.ID,
		UserID:      e.user.ID,
		GroupID:     group.ID,
		AIModelID:   m.ID,
		Model:       "mock-claude",
		Provider:    "mock-provider",
		TotalTokens: 100,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}

	t.Run("/v1/messages returns anthropic-style 429", func(t *testing.T) {
		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-claude", 64))
		if msg := assertAnthropicError(
			t, resp, http.StatusTooManyRequests, "rate_limit_error",
		); msg != "User token quota exceeded" {
			t.Fatalf("error.message = %q, want %q", msg, "User token quota exceeded")
		}
		secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || secs <= 0 {
			t.Fatalf(
				"Retry-After should be a positive integer number of seconds, got %q (err=%v)",
				resp.Header.Get("Retry-After"),
				err,
			)
		}
		if got := e.rec.Calls(); got != 0 {
			t.Fatalf("quota exceeded must not reach upstream, got %d calls", got)
		}
	})

	t.Run("/v1/chat/completions keeps the existing openai-style error body", func(t *testing.T) {
		resp := e.postChat(t, token, reqWithModel("mock-claude"), nil)
		assertErrorResponse(t, resp, http.StatusTooManyRequests, "User token quota exceeded")
	})
}

// ---------------------------------------------------------------------------
// 4. 直通路径（anthropic-messages 上游）
// ---------------------------------------------------------------------------

func TestAnthropicIngress_Passthrough(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
	// 上游 model_id 与客户端请求名不同（大小写 + slug 差异），用于断言模型名替换。
	m := mustCreateAIModel(t, e.store, newAnthropicAIModel("claude-x/legacy:beta", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("cache_control fidelity, model replacement and headers", func(t *testing.T) {
		const body = `{"model":"CLAUDE-X-LEGACY-BETA","max_tokens":128,` +
			`"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],` +
			`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

		resp := e.postMessagesAPIKey(t, token, body)
		assertStatus(t, resp, http.StatusOK)
		if got := readBody(t, resp); got != okAnthropicMessage {
			t.Fatalf("passthrough response must be echoed verbatim, got %q", got)
		}

		up := e.rec.Body()
		if up["model"] != "claude-x/legacy:beta" {
			t.Fatalf("upstream body.model = %v, want bound upstream ID %q", up["model"], "claude-x/legacy:beta")
		}
		if !bodyHasKey(up["system"], "cache_control") {
			t.Fatalf("upstream request body system should keep cache_control, got %v", up["system"])
		}
		if !bodyHasKey(up["messages"], "cache_control") {
			t.Fatalf("upstream request body messages should keep cache_control, got %v", up["messages"])
		}
		if got := e.rec.Header("x-api-key"); got != "upstream-secret" {
			t.Fatalf("upstream x-api-key = %q, want %q", got, "upstream-secret")
		}
		if got := e.rec.Header("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("upstream anthropic-version = %q, want default %q", got, "2023-06-01")
		}
		if got := e.rec.Header("Content-Type"); got != "application/json" {
			t.Fatalf("upstream Content-Type = %q, want application/json", got)
		}
	})

	t.Run("anthropic-version / anthropic-beta passthrough", func(t *testing.T) {
		resp := e.postMessages(t, anthropicReq("claude-x/legacy:beta", 64), map[string]string{
			"x-api-key":         token,
			"anthropic-version": "2023-01-01",
			"anthropic-beta":    "prompt-caching-2024-07-31",
		})
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got := e.rec.Header("anthropic-version"); got != "2023-01-01" {
			t.Fatalf("upstream anthropic-version = %q, want passthrough %q", got, "2023-01-01")
		}
		if got := e.rec.Header("anthropic-beta"); got != "prompt-caching-2024-07-31" {
			t.Fatalf("upstream anthropic-beta = %q, want passthrough", got)
		}
	})

	t.Run("max_tokens capped by key limit", func(t *testing.T) {
		_, cappedToken := mustCreateKey(t, e.store, e.user.ID, 100, primary(m.ID, 0))
		resp := e.postMessagesAPIKey(t, cappedToken, anthropicReq("claude-x/legacy:beta", 5000))
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		if got, ok := bodyNumber(t, e.rec.Body(), "max_tokens"); !ok || got != 100 {
			t.Fatalf("upstream max_tokens = %v (present=%v), want 100", got, ok)
		}
	})
}

// TestAnthropicIngress_PassthroughStreaming 断言直通路径下上游 Anthropic SSE 事件序列原样回传客户端。
func TestAnthropicIngress_PassthroughStreaming(t *testing.T) {
	e := newEnv(t, respondSSE(anthropicSSEChunks))
	m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postMessagesAPIKey(
		t,
		token,
		`{"model":"mock-claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	)
	assertStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("streaming Content-Type = %q, want text/event-stream", got)
	}
	if got, want := readStreamBody(t, resp), strings.Join(anthropicSSEChunks, ""); got != want {
		t.Fatalf("passthrough streaming response must match upstream byte for byte\n got=%q\nwant=%q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 5. 转换路径：非流式
// ---------------------------------------------------------------------------

func TestAnthropicIngress_ConversionNonStreaming(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okOpenAIChatForAnthropic))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postMessagesAPIKey(t, token, anthropicReq("MOCK-GPT", 64))
	assertStatus(t, resp, http.StatusOK)
	msg := decodeAnthropicMessage(t, readBody(t, resp))

	if msg.Type != "message" || msg.Role != "assistant" {
		t.Fatalf("response type/role = %q/%q, want message/assistant", msg.Type, msg.Role)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Fatalf("response id = %q, want msg_ prefix", msg.ID)
	}
	if msg.Model != "MOCK-GPT" {
		t.Fatalf("response model = %q, want the client-requested model name %q", msg.Model, "MOCK-GPT")
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("response stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "hello from mock" {
		t.Fatalf("unexpected response content: %+v", msg.Content)
	}
	if msg.Usage.InputTokens != 11 || msg.Usage.OutputTokens != 7 || msg.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v, want input=11 output=7 cache_read=3", msg.Usage)
	}

	// 上游收到的是 OpenAI 兼容请求（model 已替换为上游 ID、max_tokens 已落实）。
	up := e.rec.Body()
	if up["model"] != "mock-gpt" {
		t.Fatalf("upstream body.model = %v, want %q", up["model"], "mock-gpt")
	}
	if got, ok := bodyNumber(t, up, "max_tokens"); !ok || got != 64 {
		t.Fatalf("upstream max_tokens = %v (present=%v), want 64", got, ok)
	}
	if _, ok := up["messages"]; !ok {
		t.Fatalf("upstream request body should contain messages: %v", up)
	}
}

// ---------------------------------------------------------------------------
// 6. 转换路径：流式
// ---------------------------------------------------------------------------

func TestAnthropicIngress_ConversionStreaming(t *testing.T) {
	e := newEnv(t, respondSSE(openAIStreamChunks))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postMessagesAPIKey(t, token,
		`{"model":"mock-gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("streaming Content-Type = %q, want text/event-stream", got)
	}

	stream := readStreamBody(t, resp)
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		"hello ",
		"from mock",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("streaming response missing %q\nresponse: %s", want, stream)
		}
	}
	if !strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Fatalf("message_delta should contain stop_reason=end_turn\nresponse: %s", stream)
	}
	if !strings.Contains(stream, `"output_tokens":7`) {
		t.Fatalf("message_delta should contain output_tokens=7\nresponse: %s", stream)
	}

	// 事件顺序：message_start → content_block_start → message_delta → message_stop。
	order := []string{
		"event: message_start",
		"event: content_block_start",
		"event: message_delta",
		"event: message_stop",
	}
	prev := -1
	for _, ev := range order {
		idx := strings.Index(stream, ev)
		if idx <= prev {
			t.Fatalf("unexpected event order (%q at %d, previous event at %d)\nresponse: %s", ev, idx, prev, stream)
		}
		prev = idx
	}
}

// ---------------------------------------------------------------------------
// 7. 上游不可达
// ---------------------------------------------------------------------------

func TestAnthropicIngress_UpstreamUnreachable(t *testing.T) {
	// 先起再关，得到一个保证无人监听的端口。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	t.Run("conversion path returns 502", func(t *testing.T) {
		e := newEnv(t, respondConstantJSON(http.StatusOK, okOpenAIChatForAnthropic))
		m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", deadURL))
		_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-gpt", 64))
		msg := assertAnthropicError(t, resp, http.StatusBadGateway, "api_error")
		if !strings.HasPrefix(msg, "failed to connect to LLM backend") {
			t.Fatalf("error.message = %q, want prefix %q", msg, "failed to connect to LLM backend")
		}
	})

	t.Run("passthrough path returns 502", func(t *testing.T) {
		e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
		m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", deadURL))
		_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-claude", 64))
		msg := assertAnthropicError(t, resp, http.StatusBadGateway, "api_error")
		if !strings.HasPrefix(msg, "failed to connect to LLM backend") {
			t.Fatalf("error.message = %q, want prefix %q", msg, "failed to connect to LLM backend")
		}
	})
}

// ---------------------------------------------------------------------------
// 8. 用量记账（配额与用量的复用）
// ---------------------------------------------------------------------------

func TestAnthropicIngress_UsageLogged(t *testing.T) {
	t.Run("conversion path non-streaming", func(t *testing.T) {
		e := newEnv(t, respondConstantJSON(http.StatusOK, okOpenAIChatForAnthropic))
		m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
		key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-gpt", 64))
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		read := waitUsageLogRows(t, e.dsn, 1)
		if n := countRows(t, read, &model.LLMUsageLog{}); n != 1 {
			t.Fatalf("llm_usage_logs rows = %d, want 1", n)
		}
		row := latestUsageLog(t, read)
		if row.APIKeyID != key.ID || row.UserID != e.user.ID || row.Model != "mock-gpt" ||
			row.Provider != "mock-provider" {
			t.Fatalf("usage log identity fields mismatch: %+v", row)
		}
		if row.StatusCode != http.StatusOK {
			t.Fatalf("usage status_code = %d, want 200", row.StatusCode)
		}
		if row.PromptTokens != 11 || row.CompletionTokens != 7 || row.TotalTokens != 18 ||
			row.PromptCacheReadTokens != 3 {
			t.Fatalf("usage tokens mismatch: %+v", row)
		}
	})

	t.Run("conversion path streaming", func(t *testing.T) {
		e := newEnv(t, respondSSE(openAIStreamChunks))
		m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
		key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postMessagesAPIKey(t, token,
			`{"model":"mock-gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readStreamBody(t, resp)

		read := waitUsageLogRows(t, e.dsn, 1)
		row := latestUsageLog(t, read)
		if row.APIKeyID != key.ID || row.Model != "mock-gpt" || row.StatusCode != http.StatusOK {
			t.Fatalf("usage log identity fields mismatch: %+v", row)
		}
		if row.PromptTokens != 11 || row.CompletionTokens != 7 || row.TotalTokens != 18 {
			t.Fatalf("streaming usage tokens mismatch: %+v", row)
		}
	})

	t.Run("passthrough path non-streaming", func(t *testing.T) {
		e := newEnv(t, respondConstantJSON(http.StatusOK, okAnthropicMessage))
		m := mustCreateAIModel(t, e.store, newAnthropicAIModel("mock-claude", e.upstream.URL))
		key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

		resp := e.postMessagesAPIKey(t, token, anthropicReq("mock-claude", 64))
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		read := waitUsageLogRows(t, e.dsn, 1)
		row := latestUsageLog(t, read)
		if row.APIKeyID != key.ID || row.Model != "mock-claude" || row.StatusCode != http.StatusOK {
			t.Fatalf("usage log identity fields mismatch: %+v", row)
		}
		// 记账口径：prompt = input + cache_read + cache_creation = 9+2+1 = 12；total = 12+5 = 17
		// （与 provider/anthropic.go 的 anthropicUsage 对齐，cache 命中同样计入配额消耗）。
		if row.PromptTokens != 12 || row.CompletionTokens != 5 || row.TotalTokens != 17 {
			t.Fatalf("passthrough usage tokens mismatch: %+v", row)
		}
		if row.PromptCacheReadTokens != 2 || row.PromptCacheWriteTokens != 1 {
			t.Fatalf("passthrough cache usage mismatch: %+v", row)
		}
	})
}

// waitUsageLogRows 轮询等待 llm_usage_logs 至少 want 行（用量为异步写入，避免与写入器竞态）。
func waitUsageLogRows(t *testing.T, dsn string, want int64) *gorm.DB {
	t.Helper()
	db := openReadDB(t, dsn)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int64
		if err := db.Model(&model.LLMUsageLog{}).Count(&n).Error; err == nil && n >= want {
			return db
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d llm_usage_logs rows", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// latestUsageLog 读取最新一条请求级用量日志（调用前需已排空异步队列）。
func latestUsageLog(t *testing.T, db *gorm.DB) model.LLMUsageLog {
	t.Helper()
	var row model.LLMUsageLog
	if err := db.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	return row
}
