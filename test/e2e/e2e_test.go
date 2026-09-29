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

//go:build e2e

// Package e2e 是 AIGate 的端到端冒烟测试。
//
// 它只作为外部黑盒驱动被测网关二进制：进程内起 httptest mock 上游（OpenAI / Anthropic 双协议），
// 用真实 HTTP 调用网关，并直连 MySQL 断言用量落库。不导入 internal 包。
//
// 触发方式见同目录 README.md（bash test/e2e/run.sh）；未设置必要环境变量时整套用例 t.Skip。
package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const (
	envBinKey = "AIGATE_E2E_BIN"
	envDSNKey = "AIGATE_E2E_DSN"

	adminToken     = "e2e-admin-token"
	upstreamAPIKey = "sk-upstream-x"

	// e2eMasterKey 是固定测试主密钥（64 位 hex，解码 32 字节）：
	// 全部 e2e 均在「加密开启」下回归，覆盖密钥类字段的落库密文与转发时实时解密。
	e2eMasterKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// credSecretE2E 是 M3b 凭据中的 header 密钥；legacyTokenE2E 是 M3b 导入的存量明文 token。
	credSecretE2E  = "sk-cred-e2e"
	legacyTokenE2E = "sk-legacy-e2e"

	openAIModelID    = "mock-gpt"
	anthropicModelID = "mock-claude"

	openAIContent    = "hello from mock"
	anthropicContent = "hello from anthropic"

	// healthPollTimeout 是等待网关 /healthz 就绪的上限。
	healthPollTimeout = 20 * time.Second
	// usagePollTimeout 是等待用量异步落库的上限（设计 D7：用量为异步写入，需要轮询）。
	usagePollTimeout = 5 * time.Second
	// webhookPollTimeout 是等待 webhook 事件投递到达 mock 接收端的上限。
	webhookPollTimeout = 10 * time.Second
)

// e2eHTTPClient 是全部网关请求复用的客户端；超时覆盖连接 + 读取整个响应体。
var e2eHTTPClient = &http.Client{Timeout: 30 * time.Second}

// ---------------------------------------------------------------------------
// 固定响应体
// ---------------------------------------------------------------------------

const openAINonStreamBody = `{"id":"chatcmpl-mock","object":"chat.completion","choices":[{"index":0,` +
	`"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,` +
	`"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`

const anthropicNonStreamBody = `{"id":"msg_mock","type":"message","role":"assistant","content":[{"type":"text",` +
	`"text":"hello from anthropic"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":9,` +
	`"output_tokens":5}}`

// openAIStreamChunks 是 OpenAI 型 SSE 的 3 个 data 分片：2 个 content + 1 个带 usage。
var openAIStreamChunks = []string{
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant",` +
		`"content":"hello "},"finish_reason":null}]}`,
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"from mock"},` +
		`"finish_reason":null}]}`,
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`,
}

// anthropicSSEEvents 是 Anthropic 型 SSE 事件序列（event + data 成对出现）。
var anthropicSSEEvents = [][2]string{
	{
		"message_start",
		`{"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":"mock",` +
			`"content":[],"stop_reason":null,"usage":{"input_tokens":9,"output_tokens":0}}}`,
	},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	{
		"content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from anthropic"}}`,
	},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

// ---------------------------------------------------------------------------
// mock 上游
// ---------------------------------------------------------------------------

type upstreamKind string

const (
	kindOpenAI    upstreamKind = "openai"
	kindAnthropic upstreamKind = "anthropic"
)

// recordedRequest 是 mock 上游最后一次收到的请求快照。
type recordedRequest struct {
	Method  string
	Path    string
	Body    []byte
	Headers http.Header
}

// upstreamMock 是进程内的假上游：按 kind 提供 OpenAI 或 Anthropic 路由，
// 并以互斥保护记录最后一次收到的请求，供用例断言 model 名与请求体格式。
type upstreamMock struct {
	server *httptest.Server
	kind   upstreamKind

	mu   sync.Mutex
	last recordedRequest
}

func newUpstreamMock(kind upstreamKind) *upstreamMock {
	m := &upstreamMock{kind: kind}
	mux := http.NewServeMux()
	switch kind {
	case kindOpenAI:
		mux.HandleFunc("/v1/chat/completions", m.handleOpenAI)
	case kindAnthropic:
		mux.HandleFunc("/v1/messages", m.handleAnthropic)
	}
	m.server = httptest.NewServer(mux)
	return m
}

func (m *upstreamMock) URL() string { return m.server.URL }

func (m *upstreamMock) Close() { m.server.Close() }

func (m *upstreamMock) record(r *http.Request, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body, Headers: r.Header.Clone()}
}

func (m *upstreamMock) lastRequest() recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *upstreamMock) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.record(r, body)

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)

	if payload.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range openAIStreamChunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flush(flusher)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush(flusher)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, openAINonStreamBody)
}

func (m *upstreamMock) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.record(r, body)

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)

	if payload.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		for _, ev := range anthropicSSEEvents {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
			flush(flusher)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, anthropicNonStreamBody)
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// 网关进程
// ---------------------------------------------------------------------------

// syncBuffer 是并发安全的进程输出缓冲：主 goroutine 可随时读它拼装失败信息，
// 而子进程仍在后台写入。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type gatewayProcess struct {
	cmd     *exec.Cmd
	baseURL string
	out     *syncBuffer
}

// freePort 申请一个空闲的环回端口后立刻释放，交给被测进程使用，避免端口写死。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startGateway(t *testing.T, bin, dsn string) *gatewayProcess {
	t.Helper()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	out := &syncBuffer{}
	cmd := exec.Command(bin,
		"-addr", addr,
		"-db-type", "mysql",
		"-db-dsn", dsn,
		"-timezone", "Asia/Shanghai",
		"-usage-buffer", "256",
		"-quota-exceeded-status", "429",
		"-init-admin-token", adminToken,
		// 注意：-allow-private-upstreams 是布尔 flag，必须用 =true 形式；
		// 若写成「-allow-private-upstreams true」，Go flag 会把 true 当作位置参数并停止解析，
		// 之后的 flag（如 -master-key）会被静默忽略。
		"-allow-private-upstreams=true",
		"-master-key", e2eMasterKey,
	)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start gateway process (bin=%s): %v", bin, err)
	}

	gp := &gatewayProcess{cmd: cmd, baseURL: "http://" + addr, out: out}
	t.Cleanup(gp.stop)

	gp.waitHealthy(t)
	return gp
}

// stop 向子进程发 SIGTERM 并等待退出；超时后强杀，避免残留进程占用随机端口。
func (g *gatewayProcess) stop() {
	if g.cmd.Process == nil {
		return
	}
	_ = g.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- g.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = g.cmd.Process.Kill()
		<-done
	}
}

// waitHealthy 轮询 GET /healthz 直到 200；超时则打印子进程输出并终止用例。
func (g *gatewayProcess) waitHealthy(t *testing.T) {
	t.Helper()

	url := g.baseURL + "/healthz"
	deadline := time.Now().Add(healthPollTimeout)
	var lastStatus int
	var lastErr error

	for {
		status, body, err := tryGet(url)
		switch {
		case err == nil && status == http.StatusOK:
			return
		case err == nil:
			lastStatus, lastErr = status, fmt.Errorf("response body: %s", body)
		default:
			lastErr = err
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"timed out waiting for gateway /healthz to become ready (%s, last status=%d, error=%v)\n--- gateway subprocess "+
					"output ---\n%s",
				healthPollTimeout,
				lastStatus,
				lastErr,
				g.out.String(),
			)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func tryGet(url string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf(
			"skipping e2e: environment variable %s is not set (see test/e2e/README.md for prerequisites and how to run)",
			key,
		)
	}
	return v
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	return b
}

// doRequest 发送一次网关 HTTP 请求；bearer 非空时附加 Authorization 头。
func doRequest(t *testing.T, method, url, bearer string, body []byte) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("failed to build request %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s failed: %v", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read %s %s response: %v", method, url, err)
	}
	return resp.StatusCode, respBody
}

// waitFor 轮询 probe 直到返回 nil 或超时；用于等待异步副作用（健康检查、用量落库）。
func waitFor(t *testing.T, desc string, timeout time.Duration, probe func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %s, last error: %v", desc, timeout, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type createModelResponse struct {
	ID int64 `json:"id"`
}

type createKeyResponse struct {
	ID               int64  `json:"id"`
	Token            string `json:"token"`
	ProxyTokenPrefix string `json:"proxy_token_prefix"`
	Bindings         []struct {
		AIModelID int64 `json:"ai_model_id"`
		HasKey    bool  `json:"has_key"`
	} `json:"bindings"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// postChatCompletion 向数据面发起一次对话请求，返回状态码与原始响应体。
func postChatCompletion(t *testing.T, baseURL, key string, payload map[string]any) (int, []byte) {
	t.Helper()
	return doRequest(t, http.MethodPost, baseURL+"/v1/chat/completions", key, mustJSON(t, payload))
}

// ---------------------------------------------------------------------------
// 主用例
// ---------------------------------------------------------------------------

func TestE2E_M1_ForwardingSmoke(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)
	anthropicMock := newUpstreamMock(kindAnthropic)
	t.Cleanup(anthropicMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	var (
		openAIModelRowID    int64
		anthropicModelRowID int64
		apiKeyPlaintext     string
		apiKeyPrefix        string
	)

	t.Run("1. create two models", func(t *testing.T) {
		openAIModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			// OpenAI 兼容端点的 base url 带 /v1，网关会拼 /chat/completions。
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		t.Logf("OpenAI-type model row id=%d", openAIModelRowID)

		anthropicModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   anthropicModelID,
			"model_name": anthropicModelID,
			// Anthropic 端点不带 /v1，网关会拼 /v1/messages。
			"url":        anthropicMock.URL(),
			"api_key":    upstreamAPIKey,
			"model_type": "anthropic-messages",
			"quota_day":  -1,
			"enabled":    true,
		})
		t.Logf("Anthropic-type model row id=%d", anthropicModelRowID)

		if openAIModelRowID == anthropicModelRowID {
			t.Errorf("the two models should get different ids, both got %d", openAIModelRowID)
		}
	})

	t.Run("2. create a key bound to the primary/fallback models", func(t *testing.T) {
		body := mustJSON(t, map[string]any{
			"name":       "e2e",
			"user_id":    1,
			"group_id":   0,
			"max_tokens": 0,
			"bindings": []map[string]any{
				{"ai_model_id": openAIModelRowID, "role": "primary"},
				{"ai_model_id": anthropicModelRowID, "role": "fallback"},
			},
		})

		status, respBody := doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, body)
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, respBody)
		}

		var resp createKeyResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, respBody)
		}
		if !strings.HasPrefix(resp.Token, "sk-") {
			t.Fatalf("the token returned by key creation should start with sk-, got: %q", resp.Token)
		}
		if resp.ProxyTokenPrefix == "" {
			t.Fatalf("key creation response is missing proxy_token_prefix, response: %s", respBody)
		}
		apiKeyPlaintext = resp.Token
		apiKeyPrefix = resp.ProxyTokenPrefix
		t.Logf("key id=%d, proxy_token_prefix=%s", resp.ID, apiKeyPrefix)
	})

	t.Run("3. the key list does not echo the plaintext", func(t *testing.T) {
		if apiKeyPlaintext == "" {
			t.Fatal("the previous step did not obtain the key plaintext, cannot assert non-echo")
		}
		status, respBody := doRequest(t, http.MethodGet, gw.baseURL+"/admin/keys", adminAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/keys expected 200, got %d, response: %s", status, respBody)
		}
		if bytes.Contains(respBody, []byte(apiKeyPlaintext)) {
			t.Fatalf("GET /admin/keys response echoed the key plaintext, response: %s", respBody)
		}
		if !bytes.Contains(respBody, []byte(apiKeyPrefix)) {
			t.Errorf("GET /admin/keys response should contain proxy_token_prefix %q, response: %s", apiKeyPrefix, respBody)
		}
	})

	t.Run("4. the model list returns the bound models", func(t *testing.T) {
		status, respBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", apiKeyPlaintext, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/models expected 200, got %d, response: %s", status, respBody)
		}
		for _, want := range []string{openAIModelID, anthropicModelID} {
			if !bytes.Contains(respBody, []byte(want)) {
				t.Errorf("GET /v1/models should contain model %q, response: %s", want, respBody)
			}
		}
	})

	t.Run("5. non-streaming forward to an OpenAI-type upstream", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("non-streaming /v1/chat/completions expected 200, got %d, response: %s", status, respBody)
		}

		var resp chatCompletionResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatalf("failed to parse chat completion response: %v, response: %s", err, respBody)
		}
		if len(resp.Choices) == 0 {
			t.Fatalf("response is missing choices, response: %s", respBody)
		}
		if resp.Choices[0].Message.Content != openAIContent {
			t.Errorf(
				"forwarded content expected %q, got %q (response: %s)",
				openAIContent,
				resp.Choices[0].Message.Content,
				respBody,
			)
		}

		got := openAIMock.lastRequest()
		if len(got.Body) == 0 {
			t.Fatal("the OpenAI mock received no request body")
		}
		var upstreamReq struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(got.Body, &upstreamReq); err != nil {
			t.Fatalf(
				"failed to parse the request body received by the OpenAI mock: %v, request body: %s",
				err,
				got.Body,
			)
		}
		if upstreamReq.Model != openAIModelID {
			t.Errorf(
				"the model received by the upstream should be %q, got %q (request body: %s)",
				openAIModelID,
				upstreamReq.Model,
				got.Body,
			)
		}
	})

	t.Run("6. streaming forward returns SSE chunks", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"stream":   true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("streaming /v1/chat/completions expected 200, got %d, response: %s", status, respBody)
		}
		if !bytes.Contains(respBody, []byte("data:")) {
			t.Errorf("streaming response should contain data: chunks, response: %s", respBody)
		}
		if !bytes.Contains(respBody, []byte("data: [DONE]")) {
			t.Errorf("streaming response should end with data: [DONE], response: %s", respBody)
		}
	})

	t.Run("7. Anthropic path request and response conversion", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    anthropicModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("Anthropic path expected 200, got %d, response: %s", status, respBody)
		}

		var resp chatCompletionResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatalf("failed to parse chat completion response: %v, response: %s", err, respBody)
		}
		if len(resp.Choices) == 0 {
			t.Fatalf("response is missing choices, response: %s", respBody)
		}
		if resp.Choices[0].Message.Content != anthropicContent {
			t.Errorf(
				"forwarded content expected %q, got %q (response: %s)",
				anthropicContent,
				resp.Choices[0].Message.Content,
				respBody,
			)
		}

		got := anthropicMock.lastRequest()
		if len(got.Body) == 0 {
			t.Fatal("the Anthropic mock received no request body")
		}
		if got.Path != "/v1/messages" {
			t.Errorf("the Anthropic upstream path should be /v1/messages, got %q", got.Path)
		}
		var upstreamReq map[string]json.RawMessage
		if err := json.Unmarshal(got.Body, &upstreamReq); err != nil {
			t.Fatalf(
				"failed to parse the request body received by the Anthropic mock: %v, request body: %s",
				err,
				got.Body,
			)
		}
		if _, ok := upstreamReq["messages"]; !ok {
			t.Errorf("the Anthropic request body should contain the messages field, request body: %s", got.Body)
		}
		if _, ok := upstreamReq["max_tokens"]; !ok {
			t.Errorf("the Anthropic request body should contain the max_tokens field, request body: %s", got.Body)
		}
	})

	t.Run("8. authentication failure returns 401", func(t *testing.T) {
		status, respBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", "sk-this-key-does-not-exist", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("accessing /v1/models with an invalid key expected 401, got %d, response: %s", status, respBody)
		}

		status, respBody = doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("accessing /v1/models without Authorization expected 401, got %d, response: %s", status, respBody)
		}
	})

	t.Run("9. usage is persisted asynchronously", func(t *testing.T) {
		db := openMySQL(t, dsn)
		waitFor(t, "MySQL reachable", usagePollTimeout, db.Ping)

		waitFor(
			t,
			"llm_usage_logs has at least 3 rows and at least one with total_tokens>0",
			usagePollTimeout,
			func() error {
				var total int
				if err := db.QueryRow(`SELECT COUNT(*) FROM llm_usage_logs`).Scan(&total); err != nil {
					return err
				}
				if total < 3 {
					return fmt.Errorf("currently %d rows, expected >= 3", total)
				}
				var positive int
				if err := db.QueryRow(`SELECT COUNT(*) FROM llm_usage_logs WHERE total_tokens > 0`).Scan(&positive); err != nil {
					return err
				}
				if positive == 0 {
					return fmt.Errorf("no usage log row with total_tokens>0 yet")
				}
				return nil
			},
		)

		waitFor(t, "daily_usage_summaries has at least 1 row with total_tokens>0", usagePollTimeout, func() error {
			var positive int
			if err := db.QueryRow(`SELECT COUNT(*) FROM daily_usage_summaries WHERE total_tokens > ` +
				`0`).Scan(&positive); err != nil {
				return err
			}
			if positive < 1 {
				return fmt.Errorf("currently %d rows with total_tokens>0, expected >= 1", positive)
			}
			return nil
		})

		var (
			model    string
			apiKeyID int64
		)
		err := db.QueryRow(`SELECT model, api_key_id FROM llm_usage_logs WHERE total_tokens > 0 LIMIT 1`).
			Scan(&model, &apiKeyID)
		if err != nil {
			t.Fatalf("failed to sample a usage log row: %v", err)
		}
		if model == "" {
			t.Error("the sampled usage log row has an empty model")
		}
		if apiKeyID == 0 {
			t.Error("the sampled usage log row has api_key_id equal to 0")
		}
	})
}

// ---------------------------------------------------------------------------
// 步骤实现
// ---------------------------------------------------------------------------

// createModel 调用管理面创建模型，断言 201 + 响应含 id + 不回显 api_key 明文。
func createModel(t *testing.T, baseURL, adminAuth string, payload map[string]any) int64 {
	t.Helper()

	status, respBody := doRequest(t, http.MethodPost, baseURL+"/admin/models", adminAuth, mustJSON(t, payload))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/models expected 201, got %d, response: %s", status, respBody)
	}

	var resp createModelResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("failed to parse /admin/models response: %v, response: %s", err, respBody)
	}
	if resp.ID == 0 {
		t.Fatalf("POST /admin/models response is missing id, response: %s", respBody)
	}
	if bytes.Contains(respBody, []byte(upstreamAPIKey)) {
		t.Errorf("POST /admin/models response echoed the api_key plaintext, response: %s", respBody)
	}
	return resp.ID
}

// openMySQL 打开直连 MySQL 的连接池（供断言用量落库）。
func openMySQL(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("failed to open MySQL connection (check %s): %v", envDSNKey, err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ---------------------------------------------------------------------------
// M2：配额超限（组策略 / 显式不限）、用量查询、用户面行级隔离
// ---------------------------------------------------------------------------

// m2UserResponse 是 POST /admin/users 响应中本用例需要的最小字段。
type m2UserResponse struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
}

// m2CreateGroup 创建分组并返回 id；rules 为空串表示不传（未配置 → 回退全局默认）。
func m2CreateGroup(t *testing.T, baseURL, adminAuth, name, rules string) int64 {
	t.Helper()
	payload := map[string]any{"name": name}
	if rules != "" {
		payload["token_quota_rules"] = rules
	}
	status, body := doRequest(t, http.MethodPost, baseURL+"/admin/user-groups", adminAuth, mustJSON(t, payload))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/user-groups(%s) expected 201, got %d, response: %s", name, status, body)
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.ID == 0 {
		t.Fatalf("failed to parse /admin/user-groups response: %v, response: %s", err, body)
	}
	return resp.ID
}

// m2CreateKey 创建数据面密钥并返回（id, 一次性明文）。
func m2CreateKey(t *testing.T, baseURL, adminAuth string, ownerID, groupID, modelID int64) (int64, string) {
	t.Helper()
	status, body := doRequest(t, http.MethodPost, baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
		"name":     "m2-key",
		"user_id":  ownerID,
		"group_id": groupID,
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
	}))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, body)
	}
	var resp createKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Token == "" {
		t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
	}
	return resp.ID, resp.Token
}

// m2PostChatWithHeader 发起非流式对话请求并返回（状态码, 响应头, 响应体）。
func m2PostChatWithHeader(t *testing.T, baseURL, key string, payload map[string]any) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s failed: %v", req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestE2E_M2_QuotaAndUsage(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	modelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":   "mock",
		"model_id":   openAIModelID,
		"model_name": openAIModelID,
		"url":        mock.URL() + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
	})

	var (
		userID   int64
		userTok  string
		limitedG int64
		unlimG   int64
	)

	t.Run("1. create the user and the groups (limited day=5 / explicitly unlimited)", func(t *testing.T) {
		limitedG = m2CreateGroup(t, gw.baseURL, adminAuth, "m2-limited", `[{"mode":"day","limit":5}]`)
		unlimG = m2CreateGroup(t, gw.baseURL, adminAuth, "m2-unlimited", `[]`)

		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m2-user", "role": "user", "group_id": limitedG,
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 || u.Token == "" {
			t.Fatalf("failed to parse /admin/users response: %v, response: %s", err, body)
		}
		userID, userTok = u.ID, u.Token
		t.Logf("user id=%d, groups limited=%d / unlimited=%d", userID, limitedG, unlimG)
	})

	t.Run(
		"2. limited-group key: the first request is allowed, exceeding the quota returns 429 (with Retry-After)",
		func(t *testing.T) {
			_, key := m2CreateKey(t, gw.baseURL, adminAuth, userID, limitedG, modelID)

			status, _, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
				"model":    openAIModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("the first request from the limited group expected 200, got %d, response: %s", status, body)
			}

			// 日限 5，本次即将记入 18 tokens；等待异步落库后再发第二次。
			waitFor(t, "the limited-group usage is persisted", usagePollTimeout, func() error {
				st, b := doRequest(
					t,
					http.MethodGet,
					fmt.Sprintf("%s/admin/usage/logs?group_id=%d", gw.baseURL, limitedG),
					adminAuth,
					nil,
				)
				if st != http.StatusOK {
					return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
				}
				var resp struct {
					Total int64 `json:"total"`
				}
				if err := json.Unmarshal(b, &resp); err != nil {
					return err
				}
				if resp.Total < 1 {
					return fmt.Errorf("the usage of this group is not persisted yet (total=%d)", resp.Total)
				}
				return nil
			})

			status, header, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
				"model":    openAIModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi again"}},
			})
			if status != http.StatusTooManyRequests {
				t.Fatalf("the exceeding request expected 429, got %d, response: %s", status, body)
			}
			if !bytes.Contains(body, []byte("User token quota exceeded")) {
				t.Errorf("the 429 response should contain %q, response: %s", "User token quota exceeded", body)
			}
			var secs int
			ra := header.Get("Retry-After")
			if _, err := fmt.Sscanf(ra, "%d", &secs); err != nil || secs <= 0 {
				t.Errorf("Retry-After should be a positive number of seconds, got %q (err=%v)", ra, err)
			}
		},
	)

	t.Run("3. keys in the explicitly unlimited group are not affected by the policy", func(t *testing.T) {
		_, key := m2CreateKey(t, gw.baseURL, adminAuth, userID, unlimG, modelID)
		status, _, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the request from the explicitly unlimited group expected 200, got %d, response: %s", status, body)
		}
	})

	t.Run("4. admin usage queries (summary / logs)", func(t *testing.T) {
		waitFor(t, "all usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/logs?page_size=50", adminAuth, nil)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 2 {
				return fmt.Errorf("the usage logs have not reached 2 entries yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, b := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/summary?group_by=model", adminAuth, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /admin/usage/summary expected 200, got %d, response: %s", st, b)
		}
		var resp struct {
			GroupBy string `json:"group_by"`
			Rows    []struct {
				AIModelID    int64 `json:"ai_model_id"`
				TotalTokens  int64 `json:"total_tokens"`
				RequestCount int64 `json:"request_count"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("failed to parse summary response: %v, response: %s", err, b)
		}
		if resp.GroupBy != "model" || len(resp.Rows) == 0 {
			t.Fatalf("summary should be grouped by model and contain rows, got: %+v", resp)
		}
		if resp.Rows[0].TotalTokens <= 0 || resp.Rows[0].RequestCount <= 0 {
			t.Errorf("summary rows should contain a positive token count and request count, got: %+v", resp.Rows[0])
		}
	})

	t.Run("5. user-facing API: identity / quota view / row-level isolation", func(t *testing.T) {
		st, b := doRequest(t, http.MethodGet, gw.baseURL+"/api/user/me", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/me expected 200, got %d, response: %s", st, b)
		}
		var me struct {
			ID      int64 `json:"id"`
			GroupID int64 `json:"group_id"`
		}
		if err := json.Unmarshal(b, &me); err != nil || me.ID != userID || me.GroupID != limitedG {
			t.Fatalf("unexpected me response: %+v (err=%v), response: %s", me, err, b)
		}

		// 配额视图：解析组策略（day/5），用量为该用户全部用量（>0）。
		st, b = doRequest(t, http.MethodGet, gw.baseURL+"/api/user/quota", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/quota expected 200, got %d, response: %s", st, b)
		}
		var q struct {
			UserID int64 `json:"user_id"`
			Usages []struct {
				Mode   string `json:"mode"`
				Limit  int    `json:"limit"`
				Used   int64  `json:"used"`
				Active bool   `json:"active"`
			} `json:"usages"`
		}
		if err := json.Unmarshal(b, &q); err != nil {
			t.Fatalf("failed to parse /api/user/quota response: %v, response: %s", err, b)
		}
		if q.UserID != userID || len(q.Usages) != 1 {
			t.Fatalf("unexpected quota view: %+v, response: %s", q, b)
		}
		if u := q.Usages[0]; u.Mode != "day" || u.Limit != 5 || u.Used <= 0 || !u.Active {
			t.Errorf("unexpected quota entry: %+v", u)
		}

		// 行级隔离：user 角色传他人 user_id 被忽略，仍只返回自己的记录。
		st, b = doRequest(
			t,
			http.MethodGet,
			gw.baseURL+"/api/user/usage/logs?user_id=999999&page_size=50",
			userTok,
			nil,
		)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/usage/logs expected 200, got %d, response: %s", st, b)
		}
		var logs struct {
			Total int64 `json:"total"`
			Logs  []struct {
				UserID int64 `json:"user_id"`
			} `json:"logs"`
		}
		if err := json.Unmarshal(b, &logs); err != nil {
			t.Fatalf("failed to parse the user-facing log response: %v, response: %s", err, b)
		}
		if logs.Total < 2 {
			t.Errorf("the user-facing logs should have at least 2 entries (total=%d)", logs.Total)
		}
		for i, row := range logs.Logs {
			if row.UserID != userID {
				t.Errorf("the user-facing log entry[%d] contains another user's data, user_id=%d", i, row.UserID)
			}
		}

		// 管理员可显式查询指定用户。
		st, b = doRequest(
			t,
			http.MethodGet,
			fmt.Sprintf("%s/api/user/usage/logs?user_id=%d&page_size=50", gw.baseURL, userID),
			adminAuth,
			nil,
		)
		if st != http.StatusOK {
			t.Fatalf("the admin query for user logs expected 200, got %d, response: %s", st, b)
		}
		var adminLogs struct {
			Total int64 `json:"total"`
		}
		if err := json.Unmarshal(b, &adminLogs); err != nil || adminLogs.Total < 2 {
			t.Fatalf(
				"the admin query should return >=2 logs for that user: %+v (err=%v), response: %s",
				adminLogs,
				err,
				b,
			)
		}

		// by-model 固定按模型聚合。
		st, b = doRequest(t, http.MethodGet, gw.baseURL+"/api/user/usage/by-model", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/usage/by-model expected 200, got %d, response: %s", st, b)
		}
		var byModel struct {
			GroupBy string `json:"group_by"`
			Rows    []struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(b, &byModel); err != nil || byModel.GroupBy != "model" || len(byModel.Rows) == 0 ||
			byModel.Rows[0].TotalTokens <= 0 {
			t.Fatalf("unexpected by-model response: %+v (err=%v), response: %s", byModel, err, b)
		}
	})

	t.Run("6. key-level quota: token_quota_rules on the key -> 429 with Retry-After", func(t *testing.T) {
		// 独立用户（无分组，避免用户级规则干扰），仅密钥级 day=5。
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m2-keyquota-user", "role": "user",
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 {
			t.Fatalf("failed to parse /admin/users response: %v, response: %s", err, body)
		}

		status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
			"name": "m2-keyquota", "user_id": u.ID,
			"token_quota_rules": `[{"mode":"day","limit":5}]`,
			"bindings":          []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys (with rules) expected 201, got %d, response: %s", status, body)
		}
		var created createKeyResponse
		if err := json.Unmarshal(body, &created); err != nil || created.Token == "" {
			t.Fatalf("failed to parse key response: %v, response: %s", err, body)
		}

		st, _, b := m2PostChatWithHeader(t, gw.baseURL, created.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if st != http.StatusOK {
			t.Fatalf("the first key-level request expected 200, got %d, response: %s", st, b)
		}
		// 日限 5，本次即将记入 18 tokens；等待该密钥用量异步落库后再发第二次。
		waitFor(t, "the key usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?api_key_id=%d", gw.baseURL, created.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the key usage is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, header, b := m2PostChatWithHeader(t, gw.baseURL, created.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if st != http.StatusTooManyRequests {
			t.Fatalf("the key-level exceeding request expected 429, got %d, response: %s", st, b)
		}
		if !bytes.Contains(b, []byte("Key token quota exceeded")) {
			t.Errorf("the 429 response should contain %q, response: %s", "Key token quota exceeded", b)
		}
		if ra := header.Get("Retry-After"); ra == "" {
			t.Error("Retry-After should be present on a key-level quota rejection")
		}
	})

	t.Run("7. group-level global quota override: the group's own rules apply and count per group", func(t *testing.T) {
		// 组 G 显式全局规则（day=5）；站点保持默认不限 → 只有组规则生效。
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/user-groups", adminAuth, mustJSON(t, map[string]any{
			"name":                     "m2-goverride",
			"global_token_quota_rules": `[{"mode":"day","limit":5}]`,
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/user-groups expected 201, got %d, response: %s", status, body)
		}
		var g struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(body, &g); err != nil || g.ID == 0 {
			t.Fatalf("failed to parse group response: %v, response: %s", err, body)
		}

		status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m2-goverride-user", "role": "user", "group_id": g.ID,
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 || u.Token == "" {
			t.Fatalf("failed to parse user response: %v, response: %s", err, body)
		}

		_, key := m2CreateKey(t, gw.baseURL, adminAuth, u.ID, g.ID, modelID)

		st, _, b := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if st != http.StatusOK {
			t.Fatalf("the first request under the group override expected 200, got %d, response: %s", st, b)
		}
		waitFor(t, "the group usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?group_id=%d", gw.baseURL, g.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the group usage is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, _, b = m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if st != http.StatusTooManyRequests {
			t.Fatalf("the group-override exceeding request expected 429, got %d, response: %s", st, b)
		}
		if !bytes.Contains(b, []byte("Global token quota exceeded")) {
			t.Errorf("the 429 response should contain %q, response: %s", "Global token quota exceeded", b)
		}
	})
}

// ---------------------------------------------------------------------------
// M3：Anthropic 入站（POST /v1/messages）
// ---------------------------------------------------------------------------

// postAnthropicMessage 用 x-api-key 头向数据面发起一次 /v1/messages 请求，
// 返回（状态码, 响应头, 响应体）。apiKey 为空时不带凭据。
func postAnthropicMessage(t *testing.T, baseURL, apiKey string, payload map[string]any) (int, http.Header, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("failed to build the /v1/messages request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}

	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s failed: %v", req.URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read the /v1/messages response: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestE2E_M3_AnthropicIngress(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)
	anthropicMock := newUpstreamMock(kindAnthropic)
	t.Cleanup(anthropicMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	var (
		apiKeyPlaintext     string
		openAIModelRowID    int64
		anthropicModelRowID int64
	)

	t.Run("1. create two models and a key (primary=OpenAI / fallback=Anthropic)", func(t *testing.T) {
		openAIModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		anthropicModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   anthropicModelID,
			"model_name": anthropicModelID,
			"url":        anthropicMock.URL(),
			"api_key":    upstreamAPIKey,
			"model_type": "anthropic-messages",
			"quota_day":  -1,
			"enabled":    true,
		})

		status, respBody := doRequest(
			t,
			http.MethodPost,
			gw.baseURL+"/admin/keys",
			adminAuth,
			mustJSON(t, map[string]any{
				"name":       "m3-key",
				"user_id":    1,
				"group_id":   0,
				"max_tokens": 0,
				"bindings": []map[string]any{
					{"ai_model_id": openAIModelRowID, "role": "primary"},
					{"ai_model_id": anthropicModelRowID, "role": "fallback"},
				},
			}),
		)
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, respBody)
		}
		var resp createKeyResponse
		if err := json.Unmarshal(respBody, &resp); err != nil || resp.Token == "" {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, respBody)
		}
		apiKeyPlaintext = resp.Token
		t.Logf("M3 key id=%d", resp.ID)
	})

	t.Run("2. non-streaming conversion path (OpenAI upstream -> Anthropic response)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("non-streaming /v1/messages expected 200, got %d, response: %s", status, body)
		}

		var resp struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("failed to parse the Anthropic response: %v, response: %s", err, body)
		}
		if resp.Type != "message" {
			t.Errorf("response type expected %q, got %q (response: %s)", "message", resp.Type, body)
		}
		if resp.Role != "assistant" {
			t.Errorf("response role expected %q, got %q (response: %s)", "assistant", resp.Role, body)
		}
		if !strings.HasPrefix(resp.ID, "msg_") {
			t.Errorf("response id expected the msg_ prefix, got %q", resp.ID)
		}
		if resp.StopReason != "end_turn" {
			t.Errorf("response stop_reason expected %q, got %q (response: %s)", "end_turn", resp.StopReason, body)
		}
		if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != openAIContent {
			t.Errorf(
				"response content expected a single text block %q, got %+v (response: %s)",
				openAIContent,
				resp.Content,
				body,
			)
		}
		if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
			t.Errorf("response usage expected input=11 output=7, got %+v (response: %s)", resp.Usage, body)
		}

		got := openAIMock.lastRequest()
		if got.Path != "/v1/chat/completions" {
			t.Errorf("the OpenAI upstream path should be /v1/chat/completions, got %q", got.Path)
		}
	})

	t.Run("3. streaming conversion path (OpenAI SSE -> Anthropic event sequence)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("streaming /v1/messages expected 200, got %d, response: %s", status, body)
		}
		for _, want := range []string{"event: message_start", "text_delta", "event: message_stop"} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("the streaming response should contain %q, response: %s", want, body)
			}
		}
	})

	t.Run("4. passthrough path (Anthropic upstream, cache_control preserved)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      anthropicModelID,
			"max_tokens": 64,
			"system": []map[string]any{
				{"type": "text", "text": "be brief", "cache_control": map[string]any{"type": "ephemeral"}},
			},
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("passthrough /v1/messages expected 200, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte(anthropicContent)) {
			t.Errorf(
				"the passthrough response should contain the upstream text %q, response: %s",
				anthropicContent,
				body,
			)
		}

		got := anthropicMock.lastRequest()
		if got.Path != "/v1/messages" {
			t.Errorf("the Anthropic upstream path should be /v1/messages, got %q", got.Path)
		}
		if !bytes.Contains(got.Body, []byte("cache_control")) {
			t.Errorf(
				"the request body received by the Anthropic upstream should preserve cache_control, request body: %s",
				got.Body,
			)
		}
		if !bytes.Contains(got.Body, []byte(`"model":"`+anthropicModelID+`"`)) {
			t.Errorf(
				"the model received by the Anthropic upstream should be %q, request body: %s",
				anthropicModelID,
				got.Body,
			)
		}
		if headers := got.Headers; headers.Get("x-api-key") != upstreamAPIKey {
			t.Errorf("Anthropic upstream x-api-key expected %q, got %q", upstreamAPIKey, headers.Get("x-api-key"))
		}
		if got := got.Headers.Get("anthropic-version"); got == "" {
			t.Error("the Anthropic upstream is missing the anthropic-version header")
		}
	})

	t.Run("5. missing max_tokens returns 400 / a bad key returns 401", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("missing max_tokens expected 400, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("invalid_request_error")) {
			t.Errorf("the 400 response should be invalid_request_error, response: %s", body)
		}

		status, _, body = postAnthropicMessage(t, gw.baseURL, "sk-this-key-does-not-exist", map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("a bad key expected 401, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("authentication_error")) {
			t.Errorf("the 401 response should be authentication_error, response: %s", body)
		}
	})

	t.Run("6. /v1/messages usage is persisted asynchronously", func(t *testing.T) {
		db := openMySQL(t, dsn)
		waitFor(t, "MySQL reachable", usagePollTimeout, db.Ping)

		waitFor(
			t,
			fmt.Sprintf("/v1/messages usage is persisted (ai_model_id=%d)", openAIModelRowID),
			usagePollTimeout,
			func() error {
				var rows int
				err := db.QueryRow(
					`SELECT COUNT(*) FROM llm_usage_logs WHERE ai_model_id = ? AND total_tokens > 0`,
					openAIModelRowID,
				).Scan(&rows)
				if err != nil {
					return err
				}
				if rows < 1 {
					return fmt.Errorf("currently %d rows, expected >= 1", rows)
				}
				return nil
			},
		)
	})
}

// ---------------------------------------------------------------------------
// M3b：凭据加密 / 存量导入 / 连通性 / 用户自助密钥（gateway-ops 收口）
// ---------------------------------------------------------------------------

// connectivityResult 是连通性探测端点（POST /admin/models/{id}/connectivity）的响应。
type connectivityResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	ErrorKind string `json:"error_kind"`
	Message   string `json:"message"`
}

// importSummaryResponse 是 POST /admin/import 响应中本用例需要的最小字段。
type importSummaryResponse struct {
	DryRun  bool `json:"dry_run"`
	Summary struct {
		Created int `json:"created"`
		Skipped int `json:"skipped"`
		Errors  int `json:"errors"`
	} `json:"summary"`
}

// selfKeyListResponse 是 GET /api/user/keys 的响应。
type selfKeyListResponse struct {
	Keys []struct {
		ID     int64 `json:"id"`
		UserID int64 `json:"user_id"`
	} `json:"keys"`
}

// m3bCount 统计表行数，用于导入预演/幂等的前后比对（表名为用例内固定字面量，需反引号避开保留字）。
func m3bCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatalf("failed to count rows in %s: %v", table, err)
	}
	return n
}

// m3bCreateCredential 经管理面创建一条 header 密钥凭据并返回其 ID。
func m3bCreateCredential(t *testing.T, baseURL, adminAuth, name, secret string) int64 {
	t.Helper()
	status, body := doRequest(t, http.MethodPost, baseURL+"/admin/credentials", adminAuth, mustJSON(t, map[string]any{
		"name": name,
		"secrets": []map[string]any{
			{"key": "Authorization", "value": secret, "location": "header"},
		},
	}))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/credentials expected 201, got %d, response: %s", status, body)
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.ID == 0 {
		t.Fatalf("failed to parse /admin/credentials response: %v, response: %s", err, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("the credential response echoed the plaintext secret: %s", body)
	}
	return resp.ID
}

// m3bImportPayload 构造 M3b 的导入载荷：新分组 + 新用户 + 新模型 + 引用该模型的新密钥。
func m3bImportPayload(dryRun bool, upstreamURL string) map[string]any {
	return map[string]any{
		"dry_run": dryRun,
		"user_groups": []map[string]any{
			{"name": "m3b-g", "token_quota_rules": "[]"},
		},
		"users": []map[string]any{
			{"username": "m3b-u", "role": "user", "group": "m3b-g"},
		},
		"models": []map[string]any{
			{
				"provider": "mock", "model_id": "m3b-legacy", "model_name": "m3b-legacy",
				"url": upstreamURL + "/v1", "api_key": "sk-legacy-upstream",
				"model_type": "openai-completions", "quota_day": -1, "enabled": true,
			},
		},
		"keys": []map[string]any{
			{
				"name": "m3b-legacy-key", "user": "m3b-u", "group": "m3b-g", "max_tokens": 0,
				"token":    legacyTokenE2E,
				"bindings": []map[string]any{{"model": "m3b-legacy", "role": "primary"}},
			},
		},
	}
}

// m3bCreateSelfKey 以用户身份经 /api/user/keys 创建一把绑定指定模型的密钥。
func m3bCreateSelfKey(t *testing.T, baseURL, userToken string, modelRowID int64, name string) (int, []byte) {
	t.Helper()
	return doRequest(t, http.MethodPost, baseURL+"/api/user/keys", userToken, mustJSON(t, map[string]any{
		"name":       name,
		"max_tokens": 0,
		"bindings":   []map[string]any{{"ai_model_id": modelRowID, "role": "primary"}},
	}))
}

func TestE2E_M3b_GatewayOps(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openMySQL(t, dsn)
	waitFor(t, "MySQL reachable", usagePollTimeout, db.Ping)

	var (
		credModelRowID int64
		downModelRowID int64
	)
	const credModelID = "mock-cred-e2e"

	t.Run(
		"1. credential + encrypted storage: forwarding uses the credential secret and stores it as ciphertext",
		func(t *testing.T) {
			credID := m3bCreateCredential(t, gw.baseURL, adminAuth, "m3b-cred", credSecretE2E)
			credModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider":      "mock",
				"model_id":      credModelID,
				"model_name":    credModelID,
				"url":           openAIMock.URL() + "/v1",
				"api_key":       "",
				"credential_id": credID,
				"model_type":    "openai-completions",
				"quota_day":     -1,
				"enabled":       true,
			})

			status, body := doRequest(
				t,
				http.MethodPost,
				gw.baseURL+"/admin/keys",
				adminAuth,
				mustJSON(t, map[string]any{
					"name": "m3b-cred-key", "user_id": 1,
					"bindings": []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
				}),
			)
			if status != http.StatusCreated {
				t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, body)
			}
			var key createKeyResponse
			if err := json.Unmarshal(body, &key); err != nil || key.Token == "" {
				t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
			}

			// 转发成功，且 mock 收到的是凭据中的 header 密钥。
			status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
				"model":    credModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf(
					"forwarding to a model that references a credential expected 200, got %d, response: %s",
					status,
					respBody,
				)
			}
			got := openAIMock.lastRequest()
			if auth := got.Headers.Get("Authorization"); auth != "Bearer "+credSecretE2E {
				t.Errorf(
					"upstream Authorization expected the credential value %q, got %q",
					"Bearer "+credSecretE2E,
					auth,
				)
			}

			// 直连 MySQL：凭据落库为密文，且不出现明文。
			var secrets string
			if err := db.QueryRow(`SELECT secrets_json FROM credentials WHERE id = ?`, credID).Scan(&secrets); err != nil {
				t.Fatalf("failed to read credentials.secrets_json directly: %v", err)
			}
			if !strings.HasPrefix(secrets, "enc:v1:") {
				t.Errorf("credentials.secrets_json expected ciphertext with the enc:v1: prefix, got %q", secrets)
			}
			if strings.Contains(secrets, credSecretE2E) {
				t.Errorf("credentials.secrets_json contains plaintext: %q", secrets)
			}

			// 引用凭据的模型自身 api_key 为空（密钥只在凭据里）。
			var modelAPIKey string
			if err := db.QueryRow(`SELECT api_key FROM ai_models WHERE id = ?`, credModelRowID).Scan(&modelAPIKey); err != nil {
				t.Fatalf("failed to read ai_models.api_key directly: %v", err)
			}
			if modelAPIKey != "" {
				t.Errorf("a model that references a credential should have an empty api_key, got %q", modelAPIKey)
			}
		},
	)

	t.Run(
		"2. legacy import: dry run persists nothing, the real run takes effect, a rerun is idempotent, and the imported t"+
			"oken can forward",
		func(t *testing.T) {
			tables := []string{"user_groups", "users", "ai_models", "api_keys"}
			before := make(map[string]int, len(tables))
			for _, tb := range tables {
				before[tb] = m3bCount(t, db, tb)
			}

			// 预演：只给计划，不写库。
			status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(true, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("POST /admin/import(dry_run) expected 200, got %d, response: %s", status, body)
			}
			var dry importSummaryResponse
			if err := json.Unmarshal(body, &dry); err != nil {
				t.Fatalf("failed to parse the dry_run response: %v, response: %s", err, body)
			}
			if !dry.DryRun || dry.Summary.Created <= 0 {
				t.Fatalf("dry_run expected created>0, got %+v", dry)
			}
			for _, tb := range tables {
				if got := m3bCount(t, db, tb); got != before[tb] {
					t.Errorf("dry_run should not persist anything: %s row count %d -> %d", tb, before[tb], got)
				}
			}

			// 正式导入：四类实体各新建一条。
			status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(false, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("POST /admin/import expected 200, got %d, response: %s", status, body)
			}
			var real importSummaryResponse
			if err := json.Unmarshal(body, &real); err != nil {
				t.Fatalf("failed to parse the real import response: %v, response: %s", err, body)
			}
			if real.Summary.Created != 4 || real.Summary.Errors != 0 {
				t.Fatalf("the real import expected created=4 errors=0, got %+v", real.Summary)
			}
			for _, tb := range tables {
				if got := m3bCount(t, db, tb); got <= before[tb] {
					t.Errorf(
						"after the real import the row count of %s did not increase: %d -> %d",
						tb,
						before[tb],
						got,
					)
				}
			}

			// 用导入的明文 token 转发成功。
			status, respBody := postChatCompletion(t, gw.baseURL, legacyTokenE2E, map[string]any{
				"model":    "m3b-legacy",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("forwarding with the imported key expected 200, got %d, response: %s", status, respBody)
			}

			// 重跑：全部跳过、无新建。
			status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(false, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("the repeated import expected 200, got %d, response: %s", status, body)
			}
			var again importSummaryResponse
			if err := json.Unmarshal(body, &again); err != nil {
				t.Fatalf("failed to parse the repeated import response: %v, response: %s", err, body)
			}
			if again.Summary.Created != 0 || again.Summary.Skipped <= 0 {
				t.Errorf("the repeated import expected created=0 skipped>0, got %+v", again.Summary)
			}
		},
	)

	t.Run(
		"3. connectivity: a reachable model returns latency, an unreachable one is classified as network_unreachable",
		func(t *testing.T) {
			status, body := doRequest(t, http.MethodPost,
				fmt.Sprintf("%s/admin/models/%d/connectivity", gw.baseURL, credModelRowID), adminAuth, nil)
			if status != http.StatusOK {
				t.Fatalf("the connectivity probe expected 200, got %d, response: %s", status, body)
			}
			var okResp connectivityResult
			if err := json.Unmarshal(body, &okResp); err != nil {
				t.Fatalf("failed to parse the connectivity response: %v, response: %s", err, body)
			}
			if !okResp.OK || okResp.LatencyMS < 1 {
				t.Errorf("the reachable model expected ok=true and latency_ms>=1, got %+v", okResp)
			}

			downModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider":   "mock",
				"model_id":   "mock-down-e2e",
				"model_name": "mock-down-e2e",
				"url":        "http://127.0.0.1:1/v1",
				"api_key":    "sk-down-upstream",
				"model_type": "openai-completions",
				"quota_day":  -1,
				"enabled":    true,
			})
			status, body = doRequest(t, http.MethodPost,
				fmt.Sprintf("%s/admin/models/%d/connectivity", gw.baseURL, downModelRowID), adminAuth, nil)
			if status != http.StatusOK {
				t.Fatalf(
					"the probe of the unreachable model expected 200 (the result is in the body), got %d, response: %s",
					status,
					body,
				)
			}
			var down connectivityResult
			if err := json.Unmarshal(body, &down); err != nil {
				t.Fatalf("failed to parse the connectivity response: %v, response: %s", err, body)
			}
			if down.OK || down.ErrorKind != "network_unreachable" {
				t.Errorf("the unreachable model expected ok=false kind=network_unreachable, got %+v", down)
			}
		},
	)

	t.Run("4. self-service keys: creation / scope narrowing / limit / row-level isolation", func(t *testing.T) {
		// 建用户 U，并由管理员为其建一把带绑定的密钥（模型 = 凭据模型）。
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m3b-self-user", "role": "user",
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 || u.Token == "" {
			t.Fatalf("failed to parse /admin/users response: %v, response: %s", err, body)
		}

		status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
			"name": "m3b-self-seed", "user_id": u.ID,
			"bindings": []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
		}))
		if status != http.StatusCreated {
			t.Fatalf("the admin creating a key for U expected 201, got %d, response: %s", status, body)
		}

		// U 自助创建（绑定同一模型）→ 201 + 一次性明文。
		status, body = m3bCreateSelfKey(t, gw.baseURL, u.Token, credModelRowID, "self-1")
		if status != http.StatusCreated {
			t.Fatalf("self-service creation expected 201, got %d, response: %s", status, body)
		}
		var created createKeyResponse
		if err := json.Unmarshal(body, &created); err != nil || !strings.HasPrefix(created.Token, "sk-") {
			t.Fatalf("self-service creation should return a one-time sk- plaintext: %v, response: %s", err, body)
		}

		// 绑定未获批模型（不可达模型不属于 U 的任何已有密钥）→ 400。
		status, body = m3bCreateSelfKey(t, gw.baseURL, u.Token, downModelRowID, "self-bad")
		if status != http.StatusBadRequest {
			t.Fatalf("binding an unapproved model expected 400, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("model not available for this user")) {
			t.Errorf("the 400 message should indicate that the model is unavailable, response: %s", body)
		}

		// 列表只含自己的密钥。
		status, body = doRequest(t, http.MethodGet, gw.baseURL+"/api/user/keys", u.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /api/user/keys expected 200, got %d, response: %s", status, body)
		}
		var list selfKeyListResponse
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("failed to parse /api/user/keys response: %v, response: %s", err, body)
		}
		if len(list.Keys) != 2 {
			t.Fatalf("the self-service list expected 2 entries (seeded + self-service), got %+v", list.Keys)
		}
		for i := range list.Keys {
			if list.Keys[i].UserID != u.ID {
				t.Errorf("the self-service list entry[%d] contains another user's key: %+v", i, list.Keys[i])
			}
		}

		// 上限：把 user_key_limit 设为 1 后再次创建 → 400。
		status, body = doRequest(t, http.MethodPut, gw.baseURL+"/admin/settings", adminAuth,
			mustJSON(t, map[string]any{"user_key_limit": 1}))
		if status != http.StatusOK {
			t.Fatalf("PUT /admin/settings expected 200, got %d, response: %s", status, body)
		}
		status, body = m3bCreateSelfKey(t, gw.baseURL, u.Token, credModelRowID, "self-limit")
		if status != http.StatusBadRequest {
			t.Fatalf("creation beyond the limit expected 400, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("key limit reached")) {
			t.Errorf("the 400 message should indicate that the limit has been reached, response: %s", body)
		}
	})

	t.Run("5. key TTL: an expired key is rejected and recorded; clearing the TTL restores access", func(t *testing.T) {
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
			"name": "m3b-ttl-key", "user_id": 1,
			"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"bindings":   []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys (past expires_at) expected 201, got %d, response: %s", status, body)
		}
		var key createKeyResponse
		if err := json.Unmarshal(body, &key); err != nil || key.Token == "" {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
		}

		// 数据面：过期密钥按「无效凭据」口径拒绝。
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("an expired key expected 401, got %d, response: %s", status, respBody)
		}

		// 管理面：expires_at 与首次过期观测（expired_at）均已记录。
		status, body = doRequest(t, http.MethodGet, fmt.Sprintf("%s/admin/keys/%d", gw.baseURL, key.ID), adminAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/keys/{id} expected 200, got %d, response: %s", status, body)
		}
		var detail struct {
			ExpiresAt *string `json:"expires_at"`
			ExpiredAt *string `json:"expired_at"`
		}
		if err := json.Unmarshal(body, &detail); err != nil || detail.ExpiresAt == nil || detail.ExpiredAt == nil {
			t.Fatalf("the admin detail should expose expires_at and expired_at, response: %s (err=%v)", body, err)
		}

		// 非法格式 → 400；显式 null 清除有效期 → 恢复访问。
		status, body = doRequest(t, http.MethodPut, fmt.Sprintf("%s/admin/keys/%d", gw.baseURL, key.ID), adminAuth,
			mustJSON(t, map[string]any{"expires_at": "not-a-timestamp"}))
		if status != http.StatusBadRequest {
			t.Fatalf("an invalid expires_at expected 400, got %d, response: %s", status, body)
		}
		status, body = doRequest(t, http.MethodPut, fmt.Sprintf("%s/admin/keys/%d", gw.baseURL, key.ID), adminAuth,
			mustJSON(t, map[string]any{"expires_at": nil}))
		if status != http.StatusOK {
			t.Fatalf("clearing expires_at expected 200, got %d, response: %s", status, body)
		}
		status, respBody = postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("after clearing the TTL the key should work again, got %d, response: %s", status, respBody)
		}
	})

	t.Run("6. deleting the primary model promotes the remaining fallback binding", func(t *testing.T) {
		modelA := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider": "mock", "model_id": "m3b-promote-a", "model_name": "m3b-promote-a",
			"url": openAIMock.URL() + "/v1", "api_key": "sk-mock-upstream",
			"model_type": "openai-completions", "quota_day": -1, "enabled": true,
		})
		modelB := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider": "mock", "model_id": "m3b-promote-b", "model_name": "m3b-promote-b",
			"url": openAIMock.URL() + "/v1", "api_key": "sk-mock-upstream",
			"model_type": "openai-completions", "quota_day": -1, "enabled": true,
		})
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
			"name": "m3b-promote-key", "user_id": 1,
			"bindings": []map[string]any{
				{"ai_model_id": modelA, "role": "primary"},
				{"ai_model_id": modelB, "role": "fallback", "sort_order": 5},
			},
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, body)
		}
		var key createKeyResponse
		if err := json.Unmarshal(body, &key); err != nil || key.Token == "" {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
		}

		// 删除 primary 模型 → 剩余 fallback 自动提升；未指定 model 的请求应命中 B。
		status, body = doRequest(
			t,
			http.MethodDelete,
			fmt.Sprintf("%s/admin/models/%d", gw.baseURL, modelA),
			adminAuth,
			nil,
		)
		if status != http.StatusOK {
			t.Fatalf("DELETE /admin/models/{id} expected 200, got %d, response: %s", status, body)
		}
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("forwarding after promotion expected 200, got %d, response: %s", status, respBody)
		}
		var sent map[string]any
		if err := json.Unmarshal(openAIMock.lastRequest().Body, &sent); err != nil {
			t.Fatalf("failed to parse the recorded upstream body: %v", err)
		}
		if sent["model"] != "m3b-promote-b" {
			t.Errorf("the promoted binding should be used, upstream model = %v, want m3b-promote-b", sent["model"])
		}
	})

	t.Run(
		"7. user-key (BYOK) model: the binding key is used for forwarding; a missing binding key is rejected",
		func(t *testing.T) {
			byokModelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider": "mock", "model_id": "m3b-byok", "model_name": "m3b-byok",
				"url": openAIMock.URL() + "/v1", "api_key": "",
				"model_type": "openai-completions", "quota_day": -1, "enabled": true,
				"user_key_required": true,
			})

			// 带绑定密钥的密钥：创建响应只暴露 has_key，不回显明文。
			status, body := doRequest(
				t,
				http.MethodPost,
				gw.baseURL+"/admin/keys",
				adminAuth,
				mustJSON(t, map[string]any{
					"name": "m3b-byok-key", "user_id": 1,
					"bindings": []map[string]any{
						{"ai_model_id": byokModelID, "role": "primary", "api_key": "sk-byok-e2e"},
					},
				}),
			)
			if status != http.StatusCreated {
				t.Fatalf("POST /admin/keys (BYOK) expected 201, got %d, response: %s", status, body)
			}
			if bytes.Contains(body, []byte("sk-byok-e2e")) {
				t.Fatalf("the binding key plaintext must never be echoed, response: %s", body)
			}
			var byokKey createKeyResponse
			if err := json.Unmarshal(body, &byokKey); err != nil || byokKey.Token == "" {
				t.Fatalf("failed to parse BYOK key response: %v, response: %s", err, body)
			}
			if len(byokKey.Bindings) != 1 || !byokKey.Bindings[0].HasKey {
				t.Fatalf("the create response should flag has_key=true: %+v", byokKey.Bindings)
			}

			// 转发使用绑定密钥（而非任何共享密钥）。
			status, respBody := postChatCompletion(t, gw.baseURL, byokKey.Token, map[string]any{
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("the BYOK forward expected 200, got %d, response: %s", status, respBody)
			}
			if got := openAIMock.lastRequest().Headers.Get("Authorization"); got != "Bearer sk-byok-e2e" {
				t.Fatalf("the upstream Authorization = %q, want the per-binding key", got)
			}

			// 缺绑定密钥的密钥：数据面 400、带专用文案。
			status, body = doRequest(
				t,
				http.MethodPost,
				gw.baseURL+"/admin/keys",
				adminAuth,
				mustJSON(t, map[string]any{
					"name": "m3b-byok-key2", "user_id": 1,
					"bindings": []map[string]any{{"ai_model_id": byokModelID, "role": "primary"}},
				}),
			)
			if status != http.StatusCreated {
				t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, body)
			}
			var noKeyKey createKeyResponse
			if err := json.Unmarshal(body, &noKeyKey); err != nil || noKeyKey.Token == "" {
				t.Fatalf("failed to parse key response: %v, response: %s", err, body)
			}
			status, respBody = postChatCompletion(t, gw.baseURL, noKeyKey.Token, map[string]any{
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusBadRequest {
				t.Fatalf("a key-less BYOK binding expected 400, got %d, response: %s", status, respBody)
			}
			if !bytes.Contains(respBody, []byte("user-provided key")) {
				t.Errorf("the 400 response should explain the missing user-provided key, response: %s", respBody)
			}
		},
	)

	t.Run("8. usage export (CSV/JSON) and single-key usage trend", func(t *testing.T) {
		// 导出：CSV 表头 + JSON 结构 + 非法格式 400（此时 M3b 已产生多条用量）。
		status, body := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export", adminAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/usage/export expected 200, got %d, response: %s", status, body)
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "id,api_key_id,user_id,group_id") {
			t.Fatalf("unexpected CSV header/first rows: %q", lines[0])
		}
		status, body = doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export?format=json", adminAuth, nil)
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"logs"`)) {
			t.Fatalf("GET /admin/usage/export?format=json expected 200 with logs, got %d, response: %s", status, body)
		}
		status, _ = doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export?format=xml", adminAuth, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("an invalid export format expected 400, got %d", status)
		}

		// 单密钥趋势：建用户 + 密钥 + 一次真实转发，再按用户 token 查询。
		status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m3b-trend-user", "role": "user",
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 || u.Token == "" {
			t.Fatalf("failed to parse user response: %v, response: %s", err, body)
		}
		trendKeyID, keyTok := m2CreateKey(t, gw.baseURL, adminAuth, u.ID, 0, credModelRowID)
		status, respBody := postChatCompletion(t, gw.baseURL, keyTok, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "trend"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the trend sample request expected 200, got %d, response: %s", status, respBody)
		}
		// 等用量落库后查询该用户自己的密钥趋势（行级隔离路径）。
		waitFor(t, "the trend usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?user_id=%d", gw.baseURL, u.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("usage not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})
		status, body = doRequest(
			t,
			http.MethodGet,
			fmt.Sprintf("%s/api/user/keys/%d/usage", gw.baseURL, trendKeyID),
			u.Token,
			nil,
		)
		if status != http.StatusOK {
			t.Fatalf("GET /api/user/keys/{id}/usage expected 200, got %d, response: %s", status, body)
		}
		var trend struct {
			KeyID uint `json:"key_id"`
			Rows  []struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(body, &trend); err != nil || len(trend.Rows) == 0 || trend.Rows[0].TotalTokens <= 0 {
			t.Fatalf("unexpected trend response: %+v (err=%v), response: %s", trend, err, body)
		}
	})
}

// ---------------------------------------------------------------------------
// M4：webhook 事件出口（订阅 / 签名投递 / 三类事件 / 投递可查）
// ---------------------------------------------------------------------------

// webhookSecretE2E 是 M4 订阅的固定 secret（用于复算 HMAC 签名）。
const webhookSecretE2E = "e2e-webhook-secret"

// webhookDelivery 是 mock 接收端记录的一次投递。
type webhookDelivery struct {
	EventType  string
	DeliveryID string
	Signature  string
	Body       []byte
}

// webhookReceiver 是进程内的假 webhook 接收端，记录收到的投递并恒返回 200。
type webhookReceiver struct {
	server *httptest.Server

	mu  sync.Mutex
	got []webhookDelivery
}

func newWebhookReceiver() *webhookReceiver {
	r := &webhookReceiver{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, webhookDelivery{
			EventType:  req.Header.Get("X-Gateway-Event"),
			DeliveryID: req.Header.Get("X-Gateway-Delivery"),
			Signature:  req.Header.Get("X-Gateway-Signature"),
			Body:       body,
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *webhookReceiver) URL() string { return r.server.URL }

func (r *webhookReceiver) Close() { r.server.Close() }

// snapshot 返回已收到的投递副本。
func (r *webhookReceiver) snapshot() []webhookDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]webhookDelivery, len(r.got))
	copy(out, r.got)
	return out
}

// waitEvent 轮询等待指定类型的投递到达，返回该投递。
func (r *webhookReceiver) waitEvent(t *testing.T, eventType string) webhookDelivery {
	t.Helper()
	var found webhookDelivery
	waitFor(t, fmt.Sprintf("the webhook received the %s event", eventType), webhookPollTimeout, func() error {
		got := r.snapshot()
		for _, d := range got {
			if d.EventType == eventType {
				found = d
				return nil
			}
		}
		return fmt.Errorf("no %s received yet (currently %d deliveries received)", eventType, len(got))
	})
	return found
}

// e2eSign 复算投递签名（黑盒实现，语义与网关一致：sha256=<hex(hmac-sha256(secret, body))>）。
func e2eSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// e2eEnvelope 是事件信封（仅取用例需要断言的字段）。
type e2eEnvelope struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt string         `json:"created_at"`
	Data      map[string]any `json:"data"`
}

// decodeEnvelope 解析投递 body 为事件信封。
func decodeEnvelope(t *testing.T, body []byte) e2eEnvelope {
	t.Helper()
	var env e2eEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse the event envelope: %v, body=%s", err, body)
	}
	return env
}

func TestE2E_M4_Webhooks(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openMySQL(t, dsn)
	waitFor(t, "MySQL reachable", usagePollTimeout, db.Ping)

	receiver := newWebhookReceiver()
	t.Cleanup(receiver.Close)

	var (
		webhookID int64
		modelID   int64

		limitedGroup int64
		userID       int64
		limitedKeyID int64
		limitedKey   string
	)

	t.Run("1. create the subscription (secret is masked, event_types subscribes to all)", func(t *testing.T) {
		modelID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})

		status, body := doRequest(
			t,
			http.MethodPost,
			gw.baseURL+"/admin/webhooks",
			adminAuth,
			mustJSON(t, map[string]any{
				"name":        "e2e-hook",
				"url":         receiver.URL(),
				"secret":      webhookSecretE2E,
				"event_types": []string{},
			}),
		)
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/webhooks expected 201, got %d, response: %s", status, body)
		}
		var resp struct {
			ID         int64    `json:"id"`
			Secret     string   `json:"secret"`
			EventTypes []string `json:"event_types"`
		}
		if err := json.Unmarshal(body, &resp); err != nil || resp.ID == 0 {
			t.Fatalf("failed to parse /admin/webhooks response: %v, response: %s", err, body)
		}
		if bytes.Contains(body, []byte(webhookSecretE2E)) {
			t.Fatalf("the creation response echoed the secret plaintext, response: %s", body)
		}
		if resp.Secret == "" {
			t.Fatalf("the creation response should return a masked secret, response: %s", body)
		}
		if len(resp.EventTypes) != 0 {
			t.Errorf("empty event_types should return an empty array (subscribe to all), got %+v", resp.EventTypes)
		}
		webhookID = resp.ID
		t.Logf("webhook id=%d url=%s", webhookID, receiver.URL())
	})

	t.Run("2. a normal forward -> request.completed (signature is verifiable)", func(t *testing.T) {
		_, key := m2CreateKey(t, gw.baseURL, adminAuth, 1, 0, modelID)
		status, body := postChatCompletion(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the non-streaming forward expected 200, got %d, response: %s", status, body)
		}

		got := receiver.waitEvent(t, "request.completed")
		if got.DeliveryID == "" {
			t.Error("the X-Gateway-Delivery header is missing")
		}
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("signature verification failed: expected %q, got %q", want, got.Signature)
		}

		env := decodeEnvelope(t, got.Body)
		if env.Type != "request.completed" || env.ID != got.DeliveryID || env.CreatedAt == "" {
			t.Errorf("unexpected envelope fields: %+v (delivery ID header=%q)", env, got.DeliveryID)
		}
		if env.Data["model"] != openAIModelID {
			t.Errorf("data.model expected %q, got %v", openAIModelID, env.Data["model"])
		}
		usage, ok := env.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("data.usage should be an object, got %#v", env.Data["usage"])
		}
		if total, _ := usage["total_tokens"].(float64); total <= 0 {
			t.Errorf("data.usage.total_tokens should be > 0, got %#v", usage)
		}
	})

	t.Run("3. quota exceeded -> quota.exceeded (kind=user)", func(t *testing.T) {
		limitedGroup = m2CreateGroup(t, gw.baseURL, adminAuth, "m4-limited", `[{"mode":"day","limit":5}]`)

		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
			"username": "m4-user", "role": "user", "group_id": limitedGroup,
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/users expected 201, got %d, response: %s", status, body)
		}
		var u m2UserResponse
		if err := json.Unmarshal(body, &u); err != nil || u.ID == 0 {
			t.Fatalf("failed to parse /admin/users response: %v, response: %s", err, body)
		}
		userID = u.ID

		limitedKeyID, limitedKey = m2CreateKey(t, gw.baseURL, adminAuth, userID, limitedGroup, modelID)

		status, _, body = m2PostChatWithHeader(t, gw.baseURL, limitedKey, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the first request from the limited group expected 200, got %d, response: %s", status, body)
		}
		// 日限 5，本次记入 18 tokens；等异步落库后再发第二次触发超限。
		waitFor(t, "the limited-group usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?group_id=%d", gw.baseURL, limitedGroup),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the usage of this group is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		status, _, body = m2PostChatWithHeader(t, gw.baseURL, limitedKey, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("the exceeding request expected 429, got %d, response: %s", status, body)
		}

		got := receiver.waitEvent(t, "quota.exceeded")
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("quota.exceeded signature verification failed: expected %q, got %q", want, got.Signature)
		}
		env := decodeEnvelope(t, got.Body)
		if env.Data["kind"] != "user" {
			t.Errorf("data.kind expected user, got %v", env.Data["kind"])
		}
		if id, _ := env.Data["api_key_id"].(float64); int64(id) != limitedKeyID {
			t.Errorf("data.api_key_id expected %d, got %v", limitedKeyID, env.Data["api_key_id"])
		}
		if env.Data["model"] != openAIModelID {
			t.Errorf("data.model expected %q, got %v", openAIModelID, env.Data["model"])
		}
		if limit, _ := env.Data["limit"].(float64); limit <= 0 {
			t.Errorf("data.limit should be > 0, got %v", env.Data["limit"])
		}
	})

	t.Run("4. disabling the key -> key.disabled (reason=disabled)", func(t *testing.T) {
		status, body := doRequest(
			t,
			http.MethodPut,
			fmt.Sprintf("%s/admin/keys/%d", gw.baseURL, limitedKeyID),
			adminAuth,
			mustJSON(t, map[string]any{"enabled": false}),
		)
		if status != http.StatusOK {
			t.Fatalf("PUT /admin/keys/{id} expected 200, got %d, response: %s", status, body)
		}

		got := receiver.waitEvent(t, "key.disabled")
		env := decodeEnvelope(t, got.Body)
		if id, _ := env.Data["key_id"].(float64); int64(id) != limitedKeyID {
			t.Errorf("data.key_id expected %d, got %v", limitedKeyID, env.Data["key_id"])
		}
		if env.Data["reason"] != "disabled" {
			t.Errorf("data.reason expected disabled, got %v", env.Data["reason"])
		}
		if owner, _ := env.Data["user_id"].(float64); int64(owner) != userID {
			t.Errorf("data.user_id expected %d, got %v", userID, env.Data["user_id"])
		}
	})

	t.Run("5. delivery records are queryable (total>=3 and at least one success)", func(t *testing.T) {
		status, body := doRequest(t, http.MethodGet,
			fmt.Sprintf("%s/admin/webhooks/%d/deliveries?page=1&page_size=50", gw.baseURL, webhookID), adminAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/webhooks/{id}/deliveries expected 200, got %d, response: %s", status, body)
		}
		var resp struct {
			Deliveries []struct {
				ID         int64  `json:"id"`
				WebhookID  int64  `json:"webhook_id"`
				EventType  string `json:"event_type"`
				Status     string `json:"status"`
				Attempts   int    `json:"attempts"`
				LastStatus int    `json:"last_status"`
				CreatedAt  string `json:"created_at"`
			} `json:"deliveries"`
			Total    int64 `json:"total"`
			Page     int   `json:"page"`
			PageSize int   `json:"page_size"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("failed to parse the delivery records response: %v, response: %s", err, body)
		}
		if resp.Total < 3 {
			t.Fatalf("delivery records expected >= 3, got %d (response: %s)", resp.Total, body)
		}
		if resp.Page != 1 || resp.PageSize != 50 || len(resp.Deliveries) == 0 {
			t.Errorf("unexpected pagination fields: %+v", resp)
		}

		successes := 0
		for i, d := range resp.Deliveries {
			if d.WebhookID != webhookID {
				t.Errorf("the delivery record[%d] webhook_id expected %d, got %d", i, webhookID, d.WebhookID)
			}
			if d.CreatedAt == "" {
				t.Errorf("the delivery record[%d] is missing created_at", i)
			}
			if d.Status == "success" {
				successes++
				if d.Attempts < 1 || d.LastStatus != http.StatusOK {
					t.Errorf("the successful delivery[%d] should have attempts>=1 and last_status=200, got %+v", i, d)
				}
			}
		}
		if successes == 0 {
			t.Errorf("there should be a delivery record with status=success, response: %s", body)
		}

		// 直连 MySQL 复核投递记录确实落库。
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = `+
			`?`, webhookID).Scan(&rows); err != nil {
			t.Fatalf("failed to read webhook_deliveries directly: %v", err)
		}
		if rows < 3 {
			t.Errorf("webhook_deliveries expected >= 3 rows, got %d", rows)
		}
	})

	t.Run("6. an expired key -> key.expired (delivered once per validity window)", func(t *testing.T) {
		status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
			"name": "m4-expired-key", "user_id": 1,
			"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"bindings":   []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
		}))
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys (past expires_at) expected 201, got %d, response: %s", status, body)
		}
		var key createKeyResponse
		if err := json.Unmarshal(body, &key); err != nil || key.Token == "" {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
		}

		// 首次使用：401 + 一次 key.expired。
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("an expired key expected 401, got %d, response: %s", status, respBody)
		}
		got := receiver.waitEvent(t, "key.expired")
		env := decodeEnvelope(t, got.Body)
		if env.Data["key_id"] == nil || env.Data["name"] == nil {
			t.Errorf("key.expired data should carry key_id/name, got %#v", env.Data)
		}
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("signature verification failed for key.expired: expected %q, got %q", want, got.Signature)
		}

		// 再次使用：仍 401，但不重复投递。
		postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		time.Sleep(700 * time.Millisecond)
		count := 0
		for _, d := range receiver.snapshot() {
			if d.EventType == "key.expired" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("key.expired deliveries = %d, want 1 (once per validity window)", count)
		}
	})
}
