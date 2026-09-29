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

// 本文件实现 Anthropic Messages 协议的入站管线（POST /v1/messages）：
//   - 鉴权：x-api-key 优先，其次 Authorization: Bearer；错误恒为 Anthropic 风格；
//   - 双路径分派（design D1）：
//       anthropic-messages 上游 → 原生直通（仅替换 model、施加密钥 max_tokens 上限）；
//       其余上游               → Anthropic 请求转 OpenAI、OpenAI 响应/SSE 回译；
//   - 复用既有「密钥鉴权 → 模型解析 → 四级配额（全局→模型→用户→密钥）→ max_tokens 上限」链（prepareForward）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/quota"
)

const (
	// anthropicMessagesPath 是 Anthropic Messages 上游路径。
	anthropicMessagesPath = "/v1/messages"
	// defaultAnthropicVersion 是入站未携带 anthropic-version 头时的缺省版本。
	defaultAnthropicVersion = "2023-06-01"

	// 上游超时：非流式 5 分钟、流式 30 分钟（design 风险 3）。
	anthropicUpstreamTimeout = 5 * time.Minute
	anthropicStreamTimeout   = 30 * time.Minute

	// 直通流转发时的读取缓冲大小。
	anthropicStreamCopyBuffer = 32 * 1024
)

// Anthropic 入站专用错误文案与错误类型（errors.go 保持不动）。
const (
	msgModelRequired           = "model is required"
	msgMaxTokensRequired       = "max_tokens is required"
	msgInvalidUpstreamResponse = "invalid upstream response"

	kindAuthenticationError = "authentication_error"
	kindInvalidRequestError = "invalid_request_error"
	kindRateLimitError      = "rate_limit_error"
	kindAPIError            = "api_error"
)

// --- 鉴权与错误响应 ---

// authKeyAnthropic 优先 x-api-key，其次 Authorization: Bearer；失败写出 401 authentication_error。
// 已过期密钥同样按「无效凭据」口径拒绝（不暴露过期状态）。
func (h *Handler) authKeyAnthropic(w http.ResponseWriter, r *http.Request) *model.APIKey {
	token := strings.TrimSpace(r.Header.Get("x-api-key"))
	if token == "" {
		token = extractBearerToken(r)
	}
	if token == "" {
		writeAnthropicError(w, http.StatusUnauthorized, kindAuthenticationError, msgMissingAPIKey)
		return nil
	}
	key, err := h.store.GetAPIKeyByTokenHash(r.Context(), model.HashToken(token))
	if err != nil || !key.Enabled {
		writeAnthropicError(w, http.StatusUnauthorized, kindAuthenticationError, msgInvalidAPIKey)
		return nil
	}
	if rejectExpiredKey(r.Context(), h.store, key, time.Now()) {
		writeAnthropicError(w, http.StatusUnauthorized, kindAuthenticationError, msgInvalidAPIKey)
		return nil
	}
	return key
}

// writeAnthropicError 输出 {"type":"error","error":{"type":kind,"message":msg}}。
func writeAnthropicError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	})
}

// anthropicErrorKindForStatus 把 HTTP 状态码映射为 Anthropic 错误类型：
// 401→authentication_error；400→invalid_request_error；429→rate_limit_error；
// 其余 4xx→invalid_request_error；5xx→api_error。
func anthropicErrorKindForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return kindAuthenticationError
	case status == http.StatusBadRequest:
		return kindInvalidRequestError
	case status == http.StatusTooManyRequests:
		return kindRateLimitError
	case status >= http.StatusInternalServerError:
		return kindAPIError
	case status >= http.StatusBadRequest:
		return kindInvalidRequestError
	default:
		return kindAPIError
	}
}

// isAnthropicIngressRoute 判断当前请求是否命中 /v1/messages 路由。
// 以 ServeMux 记录的路由模式为准，回退到路径比较（直接调用 handler 的场景）。
func isAnthropicIngressRoute(r *http.Request) bool {
	if r.Pattern != "" {
		return strings.HasSuffix(r.Pattern, anthropicMessagesPath)
	}
	return r.URL.Path == anthropicMessagesPath
}

// writeIngressError 按当前路由写出错误：/v1/messages 用 Anthropic 风格，其余沿用统一错误体。
// 与 /v1/chat/completions 的既有行为保持一致（该路由走 httpx.WriteError 分支）。
func (h *Handler) writeIngressError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isAnthropicIngressRoute(r) {
		writeAnthropicError(w, status, anthropicErrorKindForStatus(status), message)
		return
	}
	httpx.WriteError(w, status, message)
}

// writeIngressQuotaExceeded 写出配额超限响应：/v1/messages 用 Anthropic 风格（rate_limit_error），
// 其余沿用既有 OpenAI 风格错误体；两条分支都带 Retry-After 头与配置的状态码。
// 响应后发出 quota.exceeded 事件（key/resolved 提供事件维度字段）。
func (h *Handler) writeIngressQuotaExceeded(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	resolved *model.Resolved,
	st quota.Status,
) {
	if !isAnthropicIngressRoute(r) {
		h.writeQuotaExceeded(w, key, resolved, st)
		return
	}
	msg := msgUserQuotaExceeded
	switch st.Kind {
	case "global":
		msg = msgGlobalQuotaExceeded
	case "model":
		msg = msgModelQuotaExceeded
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(st.RetryAfter)))
	writeAnthropicError(w, h.quotaExceededStatus, kindRateLimitError, msg)
	emitQuotaExceeded(key, resolved, st)
}

// --- 主处理 ---

func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. 密钥鉴权（x-api-key 优先，其次 Bearer）。
	key := h.authKeyAnthropic(w, r)
	if key == nil {
		return
	}

	// 2. 读取请求体（上限 httpx.MaxBodyBytes）。
	rawBody, err := httpx.ReadBody(r)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, kindInvalidRequestError, msgBodyTooLarge)
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, kindInvalidRequestError, msgReadRequestBody)
		return
	}

	// 3. 解析 JSON（未知字段保留在 map 中，不做严格拒绝）。
	var reqBody map[string]any
	if err := json.Unmarshal(rawBody, &reqBody); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, kindInvalidRequestError, msgInvalidJSON)
		return
	}

	// 4. Anthropic 语义校验：model 必填、max_tokens 必填且 > 0。
	requestedModel := extractRequestedModel(reqBody)
	if requestedModel == "" {
		writeAnthropicError(w, http.StatusBadRequest, kindInvalidRequestError, msgModelRequired)
		return
	}
	if n, ok := anthJSONInt(reqBody["max_tokens"]); !ok || n <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, kindInvalidRequestError, msgMaxTokensRequired)
		return
	}

	// 5. 转换为 OpenAI 兼容请求体（错误文案沿用转换器）。
	openAIBody, err := anthropicRequestToOpenAI(rawBody)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, kindInvalidRequestError, err.Error())
		return
	}

	// 6. 模型解析 → 配额检查 → max_tokens 上限 → body 重写。
	resolved, rewrittenBody, ok := h.prepareForward(w, r, key, openAIBody)
	if !ok {
		return
	}
	isStream, _ := reqBody["stream"].(bool)

	// 7. 按上游类型分派：Anthropic 上游直通用入站原始 body（保真 cache_control 等）。
	if resolved.ModelType == model.ModelTypeAnthropic {
		h.forwardAnthropicPassthrough(w, r, key, resolved, rawBody, isStream, start)
		return
	}
	h.forwardViaOpenAIConversion(w, r, key, resolved, rewrittenBody, requestedModel, isStream, start)
}

// --- 直通路径：anthropic-messages 上游 ---

// forwardAnthropicPassthrough 把入站 Anthropic 请求原样转发到 Anthropic 上游，并原样回传响应。
// 请求体仅两处修改：model 替换为解析命中的上游 ID；max_tokens 施加密钥上限。
func (h *Handler) forwardAnthropicPassthrough(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	resolved *model.Resolved,
	inboundBody []byte,
	isStream bool,
	start time.Time,
) {
	outBody, err := buildAnthropicPassthroughBody(inboundBody, resolved.ModelID, key.MaxTokens)
	if err != nil {
		slog.Error("anthropic passthrough body rewrite failed", "error", err, "model", resolved.ModelID)
		writeAnthropicError(w, http.StatusInternalServerError, kindAPIError, msgInternalError)
		return
	}

	url := strings.TrimRight(resolved.URL, "/") + anthropicMessagesPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(outBody))
	if err != nil {
		slog.Error("build anthropic passthrough request failed", "error", err, "model", resolved.ModelID)
		writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgUpstreamConnect)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", resolved.APIKey)
	req.Header.Set("anthropic-version", anthropicVersionOrDefault(r))
	if beta := strings.TrimSpace(r.Header.Get("anthropic-beta")); beta != "" {
		req.Header.Set("anthropic-beta", beta)
	}
	for k, v := range extractCustomHeaders(r, resolved.CustomHTTPHeaders) {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	timeout := anthropicUpstreamTimeout
	if isStream {
		timeout = anthropicStreamTimeout
	}
	resp, err := provider.DoSSRFProtectedRequest(req, timeout)
	if err != nil {
		// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
		slog.Error("anthropic upstream request error", "error", err, "model", resolved.ModelID)
		writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgUpstreamConnect)
		return
	}
	defer resp.Body.Close()

	// 非流式（或流式请求遇到上游错误）：原样回传响应体与状态码。
	if !isStream || resp.StatusCode >= http.StatusBadRequest {
		respBody, readErr := provider.ReadUpstreamBody(resp.Body)
		if readErr != nil {
			slog.Error("read anthropic upstream response failed", "error", readErr, "model", resolved.ModelID)
			writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgInvalidUpstreamResponse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logUsage(key, resolved, anthropicUsageFromBody(respBody), resp.StatusCode, start)
		return
	}

	// 流式：SSE 头就绪后逐块转发，同时轻量扫描事件累积 usage。
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, kindAPIError, msgStreamNotSupported)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	usage := &anthropicStreamUsage{}
	buf := make([]byte, anthropicStreamCopyBuffer)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				break
			}
			flusher.Flush()
			usage.observe(chunk)
		}
		if readErr != nil {
			break
		}
	}
	h.logUsage(key, resolved, usage.usage(), resp.StatusCode, start)
}

// buildAnthropicPassthroughBody 复刻入站请求体，仅替换 model 并施加密钥 max_tokens 上限。
func buildAnthropicPassthroughBody(inboundBody []byte, upstreamModelID string, maxTokensLimit int) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(inboundBody, &req); err != nil {
		return nil, err
	}
	req["model"] = upstreamModelID
	if maxTokensLimit > 0 {
		if n, ok := anthJSONInt(req["max_tokens"]); ok {
			if n > maxTokensLimit {
				req["max_tokens"] = maxTokensLimit
			}
		} else if f, ok := req["max_tokens"].(float64); ok && f > float64(maxTokensLimit) {
			// 超大/非整数值（anthJSONInt 判非法）：同样收敛到上限，不留绕过口子。
			req["max_tokens"] = maxTokensLimit
		}
	}
	return json.Marshal(req)
}

// anthropicVersionOrDefault 返回入站 anthropic-version 头，缺省时回退默认版本。
func anthropicVersionOrDefault(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("anthropic-version")); v != "" {
		return v
	}
	return defaultAnthropicVersion
}

// --- 转换路径：OpenAI 兼容上游 ---

// forwardViaOpenAIConversion 把已转换为 OpenAI 格式的请求交给 provider 管线，
// 并把响应（或 SSE 事件流）回译为 Anthropic 格式。
func (h *Handler) forwardViaOpenAIConversion(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	resolved *model.Resolved,
	openAIBody []byte,
	requestedModel string,
	isStream bool,
	start time.Time,
) {
	p := provider.GetProvider(resolved.ModelType)
	customHeaders := extractCustomHeaders(r, resolved.CustomHTTPHeaders)
	ctx := r.Context()

	if isStream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAnthropicError(w, http.StatusInternalServerError, kindAPIError, msgStreamNotSupported)
			return
		}
		tw := &translationWriter{
			anthropicStreamTranslator: newAnthropicStreamTranslator(w, flusher, requestedModel),
			w:                         w,
		}
		result, statusCode, err := p.ChatCompletionStream(
			ctx,
			resolved.APIKey,
			resolved.URL,
			resolved.ModelID,
			openAIBody,
			tw,
			flusher,
			customHeaders,
		)
		if err != nil {
			// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
			slog.Error("upstream stream error", "error", err, "model", resolved.ModelID)
			// 未写出任何数据（如连接失败）：返回 502，与非流式语义一致。
			if !tw.Wrote() {
				writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgUpstreamConnect)
				return
			}
			// 已写出部分事件后中断：不补发收尾事件，让客户端感知到流不完整。
			return
		}
		if statusCode >= http.StatusBadRequest && !tw.Wrote() {
			h.logUsage(key, resolved, streamResultUsage(result), statusCode, start)
			writeAnthropicError(
				w,
				statusCode,
				anthropicErrorKindForStatus(statusCode),
				upstreamStatusMessage(statusCode),
			)
			return
		}
		tw.Finish()
		h.logUsage(key, resolved, streamResultUsage(result), statusCode, start)
		return
	}

	result, statusCode, err := p.ChatCompletion(
		ctx,
		resolved.APIKey,
		resolved.URL,
		resolved.ModelID,
		openAIBody,
		customHeaders,
	)
	if err != nil {
		// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
		slog.Error("upstream request error", "error", err, "model", resolved.ModelID)
		writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgUpstreamConnect)
		return
	}
	if statusCode != http.StatusOK {
		h.logUsage(key, resolved, completionResultUsage(result), statusCode, start)
		writeAnthropicError(w, statusCode, anthropicErrorKindForStatus(statusCode), upstreamStatusMessage(statusCode))
		return
	}
	body, err := openAIResponseToAnthropic(result.Body, requestedModel)
	if err != nil {
		h.logUsage(key, resolved, completionResultUsage(result), http.StatusBadGateway, start)
		writeAnthropicError(w, http.StatusBadGateway, kindAPIError, msgInvalidUpstreamResponse)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	h.logUsage(key, resolved, completionResultUsage(result), http.StatusOK, start)
}

// translationWriter 让流式翻译器可作为 http.ResponseWriter 传给 provider
// （Header/WriteHeader 透传底层，Write 交给翻译器）。
type translationWriter struct {
	*anthropicStreamTranslator
	w http.ResponseWriter
}

func (tw *translationWriter) Header() http.Header  { return tw.w.Header() }
func (tw *translationWriter) WriteHeader(code int) { tw.w.WriteHeader(code) }

// upstreamStatusMessage 是上游非 200 时的错误文案。
func upstreamStatusMessage(statusCode int) string {
	return fmt.Sprintf("upstream returned status %d", statusCode)
}

// completionResultUsage 返回非流式结果中的用量（结果为 nil 时返回 nil）。
func completionResultUsage(result *provider.CompletionResult) *provider.Usage {
	if result == nil {
		return nil
	}
	return result.Usage
}

// streamResultUsage 返回流式结果中的用量（结果为 nil 时返回 nil）。
func streamResultUsage(result *provider.StreamResult) *provider.Usage {
	if result == nil {
		return nil
	}
	return result.Usage
}

// --- 用量解析（直通路径） ---

// anthropicUsageFields 是 Anthropic usage 对象中的四个 token 字段。
type anthropicUsageFields struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// toProviderUsage 组装记账口径的用量；统一走 provider.NewAnthropicUsage，避免两处口径漂移。
func (u anthropicUsageFields) toProviderUsage() *provider.Usage {
	return provider.NewAnthropicUsage(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
}

// anthropicUsageFromBody 从非流式 Anthropic 响应体中解析 usage；无 usage 时返回 nil。
func anthropicUsageFromBody(body []byte) *provider.Usage {
	var parsed struct {
		Usage *anthropicUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Usage == nil {
		return nil
	}
	return parsed.Usage.toProviderUsage()
}

// anthropicStreamUsage 从 Anthropic SSE 事件流中累积 usage：
// message_start.message.usage 提供 input/cache 计数，message_delta.usage 提供 output_tokens。
type anthropicStreamUsage struct {
	buf    []byte
	fields anthropicUsageFields
	seen   bool
}

// observe 接收一段任意切分的 SSE 字节，按行解析出完整事件。
func (u *anthropicStreamUsage) observe(p []byte) {
	u.buf = append(u.buf, p...)
	for {
		idx := bytes.IndexByte(u.buf, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimSuffix(string(u.buf[:idx]), "\r")
		u.buf = u.buf[idx+1:]
		u.observeLine(line)
	}
}

// observeLine 解析单行 SSE：仅关注 data: 行中的 message_start / message_delta 事件。
func (u *anthropicStreamUsage) observeLine(line string) {
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return
	}
	data := strings.TrimSpace(rest)
	if data == "" || data == "[DONE]" {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage map[string]any `json:"usage"`
		} `json:"message"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			u.apply(event.Message.Usage)
		}
	case "message_delta":
		u.apply(event.Usage)
	}
}

// apply 就出现的字段覆盖累积值（字段缺失时保留旧值）。
func (u *anthropicStreamUsage) apply(usage map[string]any) {
	if usage == nil {
		return
	}
	if n, ok := anthJSONInt(usage["input_tokens"]); ok {
		u.fields.InputTokens = n
		u.seen = true
	}
	if n, ok := anthJSONInt(usage["output_tokens"]); ok {
		u.fields.OutputTokens = n
		u.seen = true
	}
	if n, ok := anthJSONInt(usage["cache_read_input_tokens"]); ok {
		u.fields.CacheReadInputTokens = n
		u.seen = true
	}
	if n, ok := anthJSONInt(usage["cache_creation_input_tokens"]); ok {
		u.fields.CacheCreationInputTokens = n
		u.seen = true
	}
}

// usage 返回累积用量；流中从未出现任何 token 字段时返回 nil。
func (u *anthropicStreamUsage) usage() *provider.Usage {
	if !u.seen {
		return nil
	}
	return u.fields.toProviderUsage()
}
