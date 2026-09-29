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

// 本文件是 /v1/messages（anthropic-ingress）特性的测试夹具，与既有 testing_test.go 同属
// proxy_test 包，可直接复用其中的 env / mock 上游 / 实体构造 helper。
//
// 新增内容：
//   - Anthropic 型上游模型构造（ModelType=anthropic-messages）；
//   - /v1/messages 请求 helper（x-api-key / Bearer 两种凭据）；
//   - Anthropic 风格错误与成功响应的断言/解析 helper；
//   - SSE 型 mock 上游与 OpenAI 流式分片序列。
package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 固定响应体与分片序列
// ---------------------------------------------------------------------------

// okAnthropicMessage 是一条结构完整的 Anthropic 成功响应（直通路径按字节原样回传）。
const okAnthropicMessage = `{"id":"msg_mock","type":"message","role":"assistant",` +
	`"content":[{"type":"text","text":"hello from anthropic"}],"model":"mock-claude",` +
	`"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":5,` +
	`"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`

// okOpenAIChatForAnthropic 是 OpenAI 成功响应，usage 含 cached_tokens，
// 用于断言「OpenAI 响应 → Anthropic 响应」的内容与 usage 映射。
const okOpenAIChatForAnthropic = `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-gpt",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,` +
	`"prompt_tokens_details":{"cached_tokens":3}}}`

// openAIStreamChunks 是转换路径流式用例的 OpenAI SSE 分片：两片文本增量 + 一片收尾（含 usage）+ [DONE]。
var openAIStreamChunks = []string{
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant",` +
		`"content":"hello "},"finish_reason":null}]}` + "\n\n",
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"from mock"},` +
		`"finish_reason":null}]}` + "\n\n",
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n",
	"data: [DONE]\n\n",
}

// anthropicSSEChunks 是直通路径流式用例的 Anthropic 事件序列（原样回传断言用）。
var anthropicSSEChunks = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock\",\"type\":\"message\"," +
		"\"role\":\"assistant\",\"model\":\"mock\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":9," +
		"\"output_tokens\":0}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":" +
		"\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":" +
		"\"text_delta\",\"text\":\"hello from anthropic\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}," +
		"\"usage\":{\"output_tokens\":5}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// ---------------------------------------------------------------------------
// 夹具构造
// ---------------------------------------------------------------------------

// newAnthropicAIModel 构造一个 Anthropic 型（anthropic-messages）上游模型配置（未入库）。
func newAnthropicAIModel(modelID, upstreamURL string) *model.AIModel {
	m := newAIModel(modelID, upstreamURL)
	m.ModelType = model.ModelTypeAnthropic
	return m
}

// mustCreateKeyWithGroup 入库一个带分组的密钥（配额用例需要 key.GroupID 命中组策略）。
func mustCreateKeyWithGroup(
	t *testing.T,
	store *model.Store,
	ownerID, groupID uint,
	maxTokens int,
	bindings ...model.APIKeyModel,
) (*model.APIKey, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate api key token: %v", err)
	}
	k := &model.APIKey{
		Name:             "test-key",
		UserID:           ownerID,
		GroupID:          groupID,
		ProxyTokenHash:   hash,
		ProxyTokenPrefix: prefix,
		MaxTokens:        maxTokens,
		Enabled:          true,
	}
	if err := store.CreateAPIKey(ctx(), k, bindings); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return k, plain
}

// respondSSE 返回一个按 chunks 逐块写出并在每块后 flush 的 mock 上游处理器。
func respondSSE(chunks []string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 请求 helper
// ---------------------------------------------------------------------------

// postMessages 向被测网关发出一次 POST /v1/messages；headers 为附加请求头（可为 nil）。
func (e *env) postMessages(t *testing.T, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build messages request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do messages request: %v", err)
	}
	return resp
}

// postMessagesAPIKey 以 x-api-key 头发起 /v1/messages 请求。
func (e *env) postMessagesAPIKey(t *testing.T, apiKey, body string) *http.Response {
	t.Helper()
	return e.postMessages(t, body, map[string]string{"x-api-key": apiKey})
}

// postMessagesBearer 以 Authorization: Bearer 头发起 /v1/messages 请求。
func (e *env) postMessagesBearer(t *testing.T, token, body string) *http.Response {
	t.Helper()
	return e.postMessages(t, body, map[string]string{"Authorization": "Bearer " + token})
}

// anthropicReq 构造一个最小 Anthropic 请求体（显式 model + max_tokens）。
func anthropicReq(modelID string, maxTokens int) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, modelID, maxTokens)
}

// readStreamBody 读取流式响应体（语义化命名，实现与 readBody 一致）。
func readStreamBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	return readBody(t, resp)
}

// ---------------------------------------------------------------------------
// 断言 / 解析 helper
// ---------------------------------------------------------------------------

// anthropicErrorBody 是 Anthropic 风格错误响应体。
type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// assertAnthropicError 断言 Anthropic 风格错误响应（状态码 + 顶层 type + error.type），返回错误消息。
func assertAnthropicError(t *testing.T, resp *http.Response, wantStatus int, wantKind string) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, wantStatus, body)
	}
	var eb anthropicErrorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode anthropic error body %q: %v", body, err)
	}
	if eb.Type != "error" {
		t.Fatalf("error body type = %q, want %q (body=%s)", eb.Type, "error", body)
	}
	if eb.Error.Type != wantKind {
		t.Fatalf("error.type = %q, want %q (body=%s)", eb.Error.Type, wantKind, body)
	}
	return eb.Error.Message
}

// anthropicMessageResponse 是 Anthropic 成功响应的解析视图。
type anthropicMessageResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// decodeAnthropicMessage 解析 Anthropic 成功响应体。
func decodeAnthropicMessage(t *testing.T, body string) anthropicMessageResponse {
	t.Helper()
	var msg anthropicMessageResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode anthropic message %q: %v", body, err)
	}
	return msg
}

// bodyHasKey 递归检查解析后的 JSON 值中是否出现指定键（用于 cache_control 等保真断言）。
func bodyHasKey(v any, key string) bool {
	switch node := v.(type) {
	case map[string]any:
		if _, ok := node[key]; ok {
			return true
		}
		for _, child := range node {
			if bodyHasKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if bodyHasKey(child, key) {
				return true
			}
		}
	}
	return false
}
