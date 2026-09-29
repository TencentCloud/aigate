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

// Package proxy 实现数据面转发管道：鉴权 → 模型解析 → 参数强制 → 上游转发 → 用量记账。
package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/event"
	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/quota"
)

// Handler 数据面处理器。
type Handler struct {
	store               *model.Store
	usage               *UsageWriter
	quotaExceededStatus int // 配额超限状态码（429 或 403，默认 429）
}

// NewHandler 创建数据面处理器。quotaExceededStatus 非 429/403 时回退 429。
func NewHandler(store *model.Store, usage *UsageWriter, quotaExceededStatus int) *Handler {
	if quotaExceededStatus != 429 && quotaExceededStatus != 403 {
		quotaExceededStatus = http.StatusTooManyRequests
	}
	return &Handler{store: store, usage: usage, quotaExceededStatus: quotaExceededStatus}
}

// Register 注册数据面路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /v1/messages", h.handleAnthropicMessages)
	mux.HandleFunc("GET /v1/models", h.handleModels)
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. 密钥鉴权（哈希查询）。
	key := h.authKey(w, r)
	if key == nil {
		return
	}

	// 2. 读取请求体并解析目标模型。
	body, err := httpx.ReadBody(r)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, msgBodyTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, msgReadRequestBody)
		return
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, msgInvalidJSON)
		return
	}

	// 3. 模型解析 → 配额检查 → max_tokens 强制 → body 重写（与 /v1/messages 共用）。
	resolved, modifiedBody, ok := h.prepareForward(w, r, key, reqBody)
	if !ok {
		return
	}
	isStreaming, _ := reqBody["stream"].(bool)

	// 4. 按模型类型分派到上游 provider。
	p := provider.GetProvider(resolved.ModelType)
	customHeaders := extractCustomHeaders(r, resolved.CustomHTTPHeaders)
	ctx := r.Context()

	if isStreaming {
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpx.WriteError(w, http.StatusInternalServerError, msgStreamNotSupported)
			return
		}
		sw := &streamWriter{ResponseWriter: w, flusher: flusher}
		result, statusCode, err := p.ChatCompletionStream(
			ctx,
			resolved.APIKey,
			resolved.URL,
			resolved.ModelID,
			modifiedBody,
			sw,
			flusher,
			customHeaders,
		)
		if err != nil {
			slog.Error("upstream stream error", "error", err, "model", resolved.ModelID)
			// 未写出任何数据（如连接失败）：返回 502，与非流式语义一致；
			// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
			if !sw.wrote {
				httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
			}
			return
		}
		var usage *provider.Usage
		if result != nil {
			usage = result.Usage
		}
		if result != nil || statusCode != 0 {
			h.logUsage(key, resolved, usage, statusCode, start)
		}
		return
	}

	result, statusCode, err := p.ChatCompletion(
		ctx,
		resolved.APIKey,
		resolved.URL,
		resolved.ModelID,
		modifiedBody,
		customHeaders,
	)
	if err != nil {
		// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
		slog.Error("upstream request error", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(result.Body)
	h.logUsage(key, resolved, result.Usage, statusCode, start)
}

// prepareForward 完成「模型解析 → 配额检查 → max_tokens 强制 → body 重写」；
// 失败时已写出错误响应并返回 ok=false。
//
// 返回的 body 是「model 替换为上游原始 ID、max_tokens 已施加密钥上限」后的请求体字节。
// 错误响应风格按当前路由选择（见 writeIngressError / writeIngressQuotaExceeded）。
func (h *Handler) prepareForward(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	reqBody map[string]any,
) (resolved *model.Resolved, body []byte, ok bool) {
	// 解析密钥绑定的目标模型（显式未绑定 → 400；未指定 → primary）。
	reqModelName := extractRequestedModel(reqBody)
	res, err := h.store.ResolveModelForKey(r.Context(), key, reqModelName)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrModelNotBound):
			h.writeIngressError(w, r, http.StatusBadRequest, msgModelNotBound)
		case errors.Is(err, model.ErrNoResolvableModel):
			h.writeIngressError(w, r, http.StatusBadRequest, msgNoResolvableModel)
		case errors.Is(err, model.ErrBindingKeyMissing):
			h.writeIngressError(w, r, http.StatusBadRequest, msgBindingKeyMissing)
		default:
			slog.Error("resolve model failed", "error", err, "key_id", key.ID)
			h.writeIngressError(w, r, http.StatusInternalServerError, msgInternalError)
		}
		return nil, nil, false
	}

	// 配额检查（全局 → 模型日配额 → 用户 → 密钥；首个超限即拒绝，带 Retry-After）。
	if st := quota.CheckRequest(r.Context(), h.store, key, res, time.Now()); st.Exceeded {
		h.writeIngressQuotaExceeded(w, r, key, res, st)
		return nil, nil, false
	}

	// 强制 max_tokens 上限，并把上游 model 名替换为绑定的原始 ID。
	enforceMaxTokens(reqBody, key.MaxTokens)
	reqBody["model"] = res.ModelID
	modifiedBody, err := json.Marshal(reqBody)
	if err != nil {
		h.writeIngressError(w, r, http.StatusInternalServerError, msgInvalidJSON)
		return nil, nil, false
	}
	return res, modifiedBody, true
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	key := h.authKey(w, r)
	if key == nil {
		return
	}
	infos, err := h.store.ListKeyModels(r.Context(), key.ID, true)
	if err != nil {
		slog.Error("list key models failed", "error", err, "key_id", key.ID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	seen := make(map[string]struct{}, len(infos))
	data := make([]map[string]any, 0, len(infos))
	created := time.Now().Unix()
	for _, m := range infos {
		id := strings.ToLower(m.ModelID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": m.Provider,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// writeQuotaExceeded 写出配额超限响应：按配置的状态码（默认 429）+ Retry-After 头；
// 响应后发出 quota.exceeded 事件（key/resolved 提供事件维度字段）。
func (h *Handler) writeQuotaExceeded(
	w http.ResponseWriter,
	key *model.APIKey,
	resolved *model.Resolved,
	st quota.Status,
) {
	msg := msgUserQuotaExceeded
	switch st.Kind {
	case "global":
		msg = msgGlobalQuotaExceeded
	case "model":
		msg = msgModelQuotaExceeded
	case "key":
		msg = msgKeyQuotaExceeded
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(st.RetryAfter)))
	httpx.WriteError(w, h.quotaExceededStatus, msg)
	emitQuotaExceeded(key, resolved, st)
}

// emitQuotaExceeded 发出 quota.exceeded 事件。
// 两个配额响应写出口（OpenAI 风格与 Anthropic 风格）互斥调用，事件不会重复发出。
func emitQuotaExceeded(key *model.APIKey, resolved *model.Resolved, st quota.Status) {
	event.Emit(event.TypeQuotaExceeded, map[string]any{
		"api_key_id": key.ID,
		"user_id":    key.UserID,
		"group_id":   key.GroupID,
		"model":      resolved.ModelID,
		"kind":       st.Kind,
		"limit":      st.Limit,
		"used":       st.Used,
	})
}

// retryAfterSeconds 把重试间隔换算为秒（无法推算时回退 3600s，下限 1s）。
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 3600
	}
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// authKey 解析 Bearer 密钥并校验启用状态与有效期；校验失败时直接写出 401 响应并返回 nil。
// 缺失凭据与无效凭据使用不同文案（missing / invalid），与 OpenAI 生态习惯一致；
// 已过期密钥按「无效凭据」口径拒绝（不暴露过期状态，管理员经事件与字段感知）。
func (h *Handler) authKey(w http.ResponseWriter, r *http.Request) *model.APIKey {
	token := extractBearerToken(r)
	if token == "" {
		httpx.WriteError(w, http.StatusUnauthorized, msgMissingAPIKey)
		return nil
	}
	key, err := h.store.GetAPIKeyByTokenHash(r.Context(), model.HashToken(token))
	if err != nil || !key.Enabled {
		httpx.WriteError(w, http.StatusUnauthorized, msgInvalidAPIKey)
		return nil
	}
	if rejectExpiredKey(r.Context(), h.store, key, time.Now()) {
		httpx.WriteError(w, http.StatusUnauthorized, msgInvalidAPIKey)
		return nil
	}
	return key
}

// streamWriter 记录流式路径是否已写出数据（用于判断失败时能否返回 502）。
type streamWriter struct {
	http.ResponseWriter
	flusher http.Flusher
	wrote   bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(p)
}

// Flush 透传到底层 flusher。
func (s *streamWriter) Flush() {
	s.flusher.Flush()
}

// logUsage 投递用量日志（异步、不阻塞）并发出 request.completed 事件。
// 事件 data 的 usage 与用量记账口径一致（流式路径同样在流结束后调用本函数）。
func (h *Handler) logUsage(
	key *model.APIKey,
	resolved *model.Resolved,
	usage *provider.Usage,
	statusCode int,
	start time.Time,
) {
	entry := model.LLMUsageLog{
		APIKeyID:   key.ID,
		UserID:     key.UserID,
		GroupID:    key.GroupID,
		AIModelID:  resolved.AIModelID,
		Model:      resolved.ModelID,
		Provider:   resolved.Provider,
		StatusCode: statusCode,
		Latency:    int(time.Since(start).Milliseconds()),
		CreatedAt:  time.Now().UTC(),
	}
	if usage != nil {
		entry.PromptTokens = usage.PromptTokens
		entry.CompletionTokens = usage.CompletionTokens
		entry.TotalTokens = usage.TotalTokens
		entry.PromptCacheReadTokens = usage.PromptCacheReadTokens
		entry.PromptCacheWriteTokens = usage.PromptCacheWriteTokens
	}
	h.usage.Submit(entry)

	event.Emit(event.TypeRequestCompleted, map[string]any{
		"api_key_id":  key.ID,
		"user_id":     key.UserID,
		"group_id":    key.GroupID,
		"model":       resolved.ModelID,
		"provider":    resolved.Provider,
		"status_code": statusCode,
		"latency_ms":  entry.Latency,
		"usage": map[string]any{
			"prompt_tokens":             entry.PromptTokens,
			"completion_tokens":         entry.CompletionTokens,
			"total_tokens":              entry.TotalTokens,
			"prompt_cache_read_tokens":  entry.PromptCacheReadTokens,
			"prompt_cache_write_tokens": entry.PromptCacheWriteTokens,
		},
	})
}

// extractBearerToken 从 Authorization 头提取 Bearer token。
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// extractRequestedModel 读取请求体中的 model 字段（空串表示未指定）。
func extractRequestedModel(reqBody map[string]any) string {
	if v, ok := reqBody["model"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// enforceMaxTokens 对请求体的 max_tokens / max_completion_tokens 施加密钥上限。
// 数值字段超限、非正数（部分后端把 -1 视作「不限」）时统一收敛为上限；
// 两个字段都没有有效数值时注入 max_tokens=上限。
func enforceMaxTokens(reqBody map[string]any, limit int) {
	if limit <= 0 {
		return
	}
	hasNumeric := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := reqBody[key]
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			continue
		}
		hasNumeric = true
		// 用浮点比较而非 int(f)：避免超大值在 float→int 转换上的实现定义行为（可绕过上限）。
		if f <= 0 || f > float64(limit) {
			reqBody[key] = float64(limit)
		}
	}
	if !hasNumeric {
		reqBody["max_tokens"] = float64(limit)
	}
}

// extractCustomHeaders 从入站请求提取需透传给上游的自定义头。
// 语义：任一配置头的值与请求不一致时整体不透传并告警；全部一致时按配置键透传。
func extractCustomHeaders(r *http.Request, configured map[string]string) map[string]string {
	if len(configured) == 0 {
		return nil
	}
	result := make(map[string]string, len(configured))
	for key := range configured {
		if v := r.Header.Get(key); v != "" && v == configured[key] {
			result[http.CanonicalHeaderKey(key)] = v
		} else {
			slog.Warn("custom header value mismatch, passthrough disabled", "header", key)
			return nil
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
