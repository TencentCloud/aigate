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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	anthropicDefaultBase = "https://api.anthropic.com"
	anthropicVersion     = "2023-06-01"

	// Default thinking budget_tokens per reasoning_effort level (matching LiteLLM).
	thinkingBudgetMinimal = 128
	thinkingBudgetLow     = 1024
	thinkingBudgetMedium  = 2048
	thinkingBudgetHigh    = 4096
	defaultMaxTokens      = 4096
)

// AnthropicProvider converts between OpenAI chat completion format and
// the Anthropic Messages API.
type AnthropicProvider struct{}

// --- request / response types ---

type anthropicRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	System     string               `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	Stream     bool                 `json:"stream,omitempty"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   json.RawMessage      `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// --- helpers ---

func (p *AnthropicProvider) baseURL(apiBase string) string {
	if apiBase != "" {
		return strings.TrimRight(apiBase, "/")
	}
	return anthropicDefaultBase
}

// convertToolChoiceToAnthropic converts OpenAI tool_choice to Anthropic format.
func convertToolChoiceToAnthropic(tc interface{}) *anthropicToolChoice {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "required":
			return &anthropicToolChoice{Type: "any"}
		case "none":
			return nil // Anthropic doesn't send tool_choice for "none"; omit it.
		default:
			return &anthropicToolChoice{Type: "auto"}
		}
	case map[string]interface{}:
		// {"type":"function","function":{"name":"X"}}
		if fn, ok := v["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				return &anthropicToolChoice{Type: "tool", Name: name}
			}
		}
		return &anthropicToolChoice{Type: "auto"}
	default:
		return nil
	}
}

// mergeConsecutiveAnthropicMessages merges consecutive messages with the same role
// by combining their content arrays. Anthropic requires alternating user/assistant roles.
func mergeConsecutiveAnthropicMessages(msgs []anthropicMessage) []anthropicMessage {
	if len(msgs) == 0 {
		return msgs
	}
	var merged []anthropicMessage
	for _, msg := range msgs {
		if len(merged) > 0 && merged[len(merged)-1].Role == msg.Role {
			// Merge content arrays.
			var prevContent []json.RawMessage
			var currContent []json.RawMessage

			// Try to unmarshal as array; if it's a string, wrap in a text block.
			if err := json.Unmarshal(merged[len(merged)-1].Content, &prevContent); err != nil {
				// It's a plain string — wrap it.
				var s string
				json.Unmarshal(merged[len(merged)-1].Content, &s)
				block, _ := json.Marshal([]map[string]interface{}{{"type": "text", "text": s}})
				json.Unmarshal(block, &prevContent)
			}
			if err := json.Unmarshal(msg.Content, &currContent); err != nil {
				var s string
				json.Unmarshal(msg.Content, &s)
				block, _ := json.Marshal([]map[string]interface{}{{"type": "text", "text": s}})
				json.Unmarshal(block, &currContent)
			}

			combined := append(prevContent, currContent...)
			merged[len(merged)-1].Content, _ = json.Marshal(combined)
		} else {
			merged = append(merged, msg)
		}
	}
	return merged
}

// mapReasoningEffort converts an OpenAI reasoning_effort string to an Anthropic
// thinking parameter. Returns nil if the effort is "none" or unrecognised.
// Budget tokens follow LiteLLM defaults: minimal=128, low=1024, medium=2048, high=4096.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

func mapReasoningEffort(effort string) json.RawMessage {
	var budget int
	switch strings.ToLower(effort) {
	case "minimal":
		budget = thinkingBudgetMinimal
	case "low":
		budget = thinkingBudgetLow
	case "medium":
		budget = thinkingBudgetMedium
	case "high":
		budget = thinkingBudgetHigh
	default:
		return nil
	}
	raw, _ := json.Marshal(anthropicThinking{Type: "enabled", BudgetTokens: budget})
	return raw
}

// sanitizeToolUseID replaces characters not matching ^[a-zA-Z0-9_-]+ with underscores.
// Anthropic requires tool_use_id to only contain alphanumeric characters, underscores, and hyphens.
var reInvalidToolID = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeToolUseID(id string) string {
	s := reInvalidToolID.ReplaceAllString(id, "_")
	if s == "" {
		return "tool_use_id"
	}
	return s
}

// parseDataURI splits "data:image/jpeg;base64,/9j/..." into ("image/jpeg", "/9j/...").
// Returns ("", "") when the input is not a valid base64 data URI.
func parseDataURI(uri string) (mediaType, data string) {
	// Must start with "data:"
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", ""
	}
	// Split on ";base64,"
	idx := strings.Index(rest, ";base64,")
	if idx < 0 {
		return "", ""
	}
	mt := rest[:idx]
	// Unescape "\/" → "/"
	mt = strings.ReplaceAll(mt, "\\/", "/")
	d := rest[idx+len(";base64,"):]
	return mt, d
}

// convertImageURLToAnthropic converts an OpenAI image_url value (string or
// {"url":"...","detail":"..."}) to an Anthropic image content block map.
//
// Decision tree:
//   - data URI          → base64 source (parse media_type + data)
//   - https:// URL      → url source  {"type":"url","url":"..."}
//   - http:// URL       → return nil (insecure; caller should skip or error)
func convertImageURLToAnthropic(imageURLValue interface{}) map[string]interface{} {
	var rawURL string
	switch v := imageURLValue.(type) {
	case string:
		rawURL = v
	case map[string]interface{}:
		rawURL, _ = v["url"].(string)
	}
	if rawURL == "" {
		return nil
	}

	// base64 data URI
	if strings.HasPrefix(rawURL, "data:") {
		mediaType, data := parseDataURI(rawURL)
		if mediaType == "" || data == "" {
			return nil
		}
		return map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}

	// HTTPS URL — pass directly (Anthropic supports url source)
	if strings.HasPrefix(rawURL, "https://") {
		return map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type": "url",
				"url":  rawURL,
			},
		}
	}

	// HTTP (insecure) or unrecognised scheme — skip silently.
	return nil
}

// convertContentBlocks converts an OpenAI content array ([]interface{}) into
// a slice of Anthropic content block maps.
//
// Supported OpenAI block types → Anthropic equivalents:
//   - type=text      → {"type":"text","text":"..."}
//   - type=image_url → {"type":"image","source":{...}}   (via convertImageURLToAnthropic)
//
// Other block types are dropped (they have no Anthropic equivalent in the
// chat-completions context).
func convertContentBlocks(items []interface{}) []map[string]interface{} {
	var blocks []map[string]interface{}
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			text, _ := m["text"].(string)
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": text,
			})
		case "image_url":
			if block := convertImageURLToAnthropic(m["image_url"]); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// contentToAnthropicBlocks converts an OpenAI message content value (string or
// array) into a slice of Anthropic content block maps.
//
// If the content is a plain string it is wrapped in a single text block.
// If it is an array each block is converted via convertContentBlocks.
func contentToAnthropicBlocks(content interface{}) []map[string]interface{} {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []map[string]interface{}{{"type": "text", "text": v}}
	case []interface{}:
		return convertContentBlocks(v)
	default:
		return nil
	}
}

// toolResultContentToAnthropic converts the content of an OpenAI tool message
// into an Anthropic tool_result content value.
//
//   - If the content is a plain string, it is returned as-is (Anthropic accepts
//     a bare string for tool_result content).
//   - If it is an array, each item is converted to a text or image block; the
//     result is returned as a slice (Anthropic also accepts an array here).
func toolResultContentToAnthropic(content interface{}) interface{} {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		blocks := convertContentBlocks(v)
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	default:
		return ""
	}
}

// oaiInputToolCall 是输入消息中的工具调用项（OpenAI 请求子集）。
type oaiInputToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiInputMessage 是输入消息（OpenAI 请求子集）。
type oaiInputMessage struct {
	Role       string             `json:"role"`
	Content    interface{}        `json:"content"`
	ToolCalls  []oaiInputToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

// oaiInputTool 是输入工具定义（OpenAI 请求子集）。
type oaiInputTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// oaiChatRequest 是 buildAnthropicRequest 解析的 OpenAI 请求子集。
type oaiChatRequest struct {
	Messages            []oaiInputMessage `json:"messages"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	Tools               []oaiInputTool    `json:"tools,omitempty"`
	ToolChoice          interface{}       `json:"tool_choice,omitempty"`
	Thinking            json.RawMessage   `json:"thinking,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
}

// buildAnthropicRequest converts an OpenAI-format request body into the Anthropic Messages API format.
func buildAnthropicRequest(model string, reqBody []byte) ([]byte, error) {
	var oai oaiChatRequest
	if err := json.Unmarshal(reqBody, &oai); err != nil {
		return nil, richErrorf(err, "failed to decode request body")
	}

	ar := anthropicRequest{Model: model}
	convertToolsToAnthropic(&oai, &ar)

	// Extract system messages and build message list.
	var systemParts []string
	for _, m := range oai.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, contentToString(m.Content))
			continue
		}
		ar.Messages = append(ar.Messages, convertMessageToAnthropic(m))
	}
	if len(systemParts) > 0 {
		ar.System = strings.Join(systemParts, "\n\n")
	}

	// Merge consecutive same-role messages (required by Anthropic).
	ar.Messages = mergeConsecutiveAnthropicMessages(ar.Messages)

	// Anthropic requires max_tokens; use defaultMaxTokens when the caller omits it.
	ar.MaxTokens = resolveAnthropicMaxTokens(&oai)
	ar.Stream = oai.Stream
	applyReasoningEffort(&oai, &ar)

	return json.Marshal(ar)
}

// convertToolsToAnthropic 转换工具定义；tool_choice 仅在存在工具时透传。
func convertToolsToAnthropic(oai *oaiChatRequest, ar *anthropicRequest) {
	for _, t := range oai.Tools {
		ar.Tools = append(ar.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	if oai.ToolChoice != nil && len(ar.Tools) > 0 {
		ar.ToolChoice = convertToolChoiceToAnthropic(oai.ToolChoice)
	}
}

// convertMessageToAnthropic 转换单条非 system 消息：
// assistant+tool_calls → text/tool_use 块；tool 结果 → tool_result 块；其余角色归一化后按普通消息转换。
func convertMessageToAnthropic(m oaiInputMessage) anthropicMessage {
	switch {
	case m.Role == "assistant" && len(m.ToolCalls) > 0:
		return anthropicMessage{Role: "assistant", Content: assistantToolCallBlocks(m)}
	case m.Role == "tool":
		return anthropicMessage{Role: "user", Content: toolResultBlocks(m)}
	default:
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user" // Map unknown roles to user.
		}
		return anthropicMessage{Role: role, Content: plainMessageContent(m.Content)}
	}
}

// assistantToolCallBlocks 构造 assistant 消息的 text + tool_use 内容块。
func assistantToolCallBlocks(m oaiInputMessage) json.RawMessage {
	var blocks []map[string]interface{}
	text := contentToString(m.Content)
	if text != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}
	for _, tc := range m.ToolCalls {
		var args json.RawMessage
		if tc.Function.Arguments != "" {
			args = json.RawMessage(tc.Function.Arguments)
		} else {
			args = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    sanitizeToolUseID(tc.ID),
			"name":  tc.Function.Name,
			"input": args,
		})
	}
	contentJSON, _ := json.Marshal(blocks)
	return contentJSON
}

// toolResultBlocks 构造 tool 结果消息的 tool_result 内容块。
// 内容可能为纯字符串，也可能是包含 text/image 块的数组。
func toolResultBlocks(m oaiInputMessage) json.RawMessage {
	toolContent := toolResultContentToAnthropic(m.Content)
	toolContentJSON, _ := json.Marshal(toolContent)

	block := []map[string]interface{}{
		{
			"type":        "tool_result",
			"tool_use_id": sanitizeToolUseID(m.ToolCallID),
			"content":     json.RawMessage(toolContentJSON),
		},
	}
	contentJSON, _ := json.Marshal(block)
	return contentJSON
}

// plainMessageContent 构造普通消息内容：单文本块以裸字符串发送（输出更干净），
// 多块时发送块数组，无内容时发送空串。
func plainMessageContent(content interface{}) json.RawMessage {
	blocks := contentToAnthropicBlocks(content)
	var contentJSON []byte
	switch {
	case len(blocks) == 1 && blocks[0]["type"] == "text":
		contentJSON, _ = json.Marshal(blocks[0]["text"])
	case len(blocks) > 0:
		contentJSON, _ = json.Marshal(blocks)
	default:
		contentJSON, _ = json.Marshal("")
	}
	return contentJSON
}

// resolveAnthropicMaxTokens 解析 max_tokens：max_completion_tokens 优先，其次 max_tokens，
// 调用方省略时使用 defaultMaxTokens（Anthropic 必填）。
func resolveAnthropicMaxTokens(oai *oaiChatRequest) int {
	switch {
	case oai.MaxCompletionTokens != nil:
		return *oai.MaxCompletionTokens
	case oai.MaxTokens != nil:
		return *oai.MaxTokens
	default:
		return defaultMaxTokens
	}
}

// applyReasoningEffort 应用 thinking 配置：reasoning_effort 优先于直接透传（对齐 LiteLLM 行为）；
// 用户未显式设置 max_tokens 时，按思考预算自动调整为 budget + defaultMaxTokens。
func applyReasoningEffort(oai *oaiChatRequest, ar *anthropicRequest) {
	userSetMaxTokens := oai.MaxTokens != nil || oai.MaxCompletionTokens != nil
	switch {
	case oai.ReasoningEffort != "":
		mapped := mapReasoningEffort(oai.ReasoningEffort)
		if mapped == nil {
			return
		}
		ar.Thinking = mapped
		if !userSetMaxTokens {
			var tp anthropicThinking
			json.Unmarshal(mapped, &tp)
			if tp.BudgetTokens > 0 {
				ar.MaxTokens = tp.BudgetTokens + defaultMaxTokens
			}
		}
	case len(oai.Thinking) > 0:
		ar.Thinking = oai.Thinking
	}
}

// contentToString extracts plain text from an OpenAI content value.
// Used for system messages and the text prefix of assistant tool_calls messages,
// where only the textual portion is needed.
func contentToString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	case nil:
		return ""
	default:
		b, _ := json.Marshal(content)
		return string(b)
	}
}

// mapStopReason converts Anthropic stop_reason to OpenAI finish_reason.
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

// anthropicToOpenAI converts an Anthropic response to OpenAI format.
func anthropicToOpenAI(ar *anthropicResponse) ([]byte, error) {
	var textParts []string
	var thinkingParts []string
	var toolCalls []map[string]interface{}

	for _, block := range ar.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				thinkingParts = append(thinkingParts, block.Thinking)
			}
		case "redacted_thinking":
			thinkingParts = append(thinkingParts, "[redacted]")
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_use":
			argsStr := "{}"
			if len(block.Input) > 0 {
				argsStr = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   block.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      block.Name,
					"arguments": argsStr,
				},
			})
		}
	}

	content := strings.Join(textParts, "")

	message := map[string]interface{}{
		"role":    "assistant",
		"content": content,
	}
	if len(thinkingParts) > 0 {
		message["reasoning_content"] = strings.Join(thinkingParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	promptTokens := ar.Usage.InputTokens + ar.Usage.CacheReadInputTokens + ar.Usage.CacheCreationInputTokens

	oai := map[string]interface{}{
		"id":      ar.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": mapStopReason(ar.StopReason),
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": ar.Usage.OutputTokens,
			"total_tokens":      promptTokens + ar.Usage.OutputTokens,
			"prompt_tokens_details": map[string]interface{}{
				"cached_tokens":         ar.Usage.CacheReadInputTokens,
				"cache_creation_tokens": ar.Usage.CacheCreationInputTokens,
			},
			"cache_read_input_tokens":     ar.Usage.CacheReadInputTokens,
			"cache_creation_input_tokens": ar.Usage.CacheCreationInputTokens,
		},
	}
	return json.Marshal(oai)
}

// NewAnthropicUsage 按统一口径组装 Anthropic 用量：Prompt 含 cache 读/写计数、Total = 输入 + 输出。
// provider 的转换路径与 proxy 的直通路径共用本函数，保证两处记账口径不会漂移。
func NewAnthropicUsage(inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) *Usage {
	promptTokens := inputTokens + cacheReadTokens + cacheCreationTokens
	return &Usage{
		PromptTokens:           promptTokens,
		CompletionTokens:       outputTokens,
		TotalTokens:            promptTokens + outputTokens,
		PromptCacheReadTokens:  cacheReadTokens,
		PromptCacheWriteTokens: cacheCreationTokens,
	}
}

func anthropicUsage(ar *anthropicResponse) *Usage {
	return NewAnthropicUsage(
		ar.Usage.InputTokens,
		ar.Usage.OutputTokens,
		ar.Usage.CacheReadInputTokens,
		ar.Usage.CacheCreationInputTokens,
	)
}

// --- Provider interface ---

func (p *AnthropicProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (*CompletionResult, int, error) {
	body, err := buildAnthropicRequest(model, reqBody)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.postAnthropicMessages(ctx, apiKey, apiBase, body, customHeaders)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBytes, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream response")
	}

	if resp.StatusCode != http.StatusOK {
		return &CompletionResult{Body: respBytes}, resp.StatusCode, nil
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBytes, &ar); err != nil {
		return &CompletionResult{Body: respBytes}, resp.StatusCode, nil
	}

	oaiBody, err := anthropicToOpenAI(&ar)
	if err != nil {
		return &CompletionResult{Body: respBytes}, resp.StatusCode, nil
	}
	return &CompletionResult{Body: oaiBody, Usage: anthropicUsage(&ar)}, http.StatusOK, nil
}

// postAnthropicMessages 向 Anthropic Messages 端点发送已构造的请求体（调用方负责关闭响应体）。
func (p *AnthropicProvider) postAnthropicMessages(
	ctx context.Context,
	apiKey, apiBase string,
	body []byte,
	customHeaders map[string]string,
) (*http.Response, error) {
	httpReq, err := newUpstreamRequest(ctx, p.baseURL(apiBase)+"/v1/messages", body, anthropicAuthHeaders(apiKey), customHeaders)
	if err != nil {
		return nil, err
	}
	return doUpstream(httpReq, 5*time.Minute)
}

// anthropicAuthHeaders 注入 Anthropic 上游所需的鉴权与版本头。
func anthropicAuthHeaders(apiKey string) func(http.Header) {
	return func(h http.Header) {
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", anthropicVersion)
	}
}

func (p *AnthropicProvider) ChatCompletionStream(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	w http.ResponseWriter,
	flusher http.Flusher,
	customHeaders map[string]string,
) (*StreamResult, int, error) {
	// Force streaming in the request.
	var raw map[string]interface{}
	if err := json.Unmarshal(reqBody, &raw); err != nil {
		return nil, 0, richErrorf(err, "failed to decode request body")
	}
	raw["stream"] = true
	modifiedReq, _ := json.Marshal(raw)

	body, err := buildAnthropicRequest(model, modifiedReq)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.postAnthropicMessages(ctx, apiKey, apiBase, body, customHeaders)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		forwardUpstreamError(w, resp)
		return nil, resp.StatusCode, nil
	}

	setSSEHeaders(w)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	translator := newAnthropicStreamTranslator(w, flusher)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var event map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		eventType, _ := event["type"].(string)
		translator.handleEvent(eventType, event)
	}
	// 读取中断（连接重置、单行超限等）必须显式报错：否则调用方会把截断当作正常结束。
	if err := scanner.Err(); err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream stream")
	}
	return &StreamResult{Usage: translator.usage}, http.StatusOK, nil
}

// anthropicStreamTranslator 将 Anthropic 上游的 SSE 事件翻译为 OpenAI 流式分片写往客户端。
type anthropicStreamTranslator struct {
	w               http.ResponseWriter
	flusher         http.Flusher
	messageID       string
	chunkIndex      int
	usage           *Usage
	toolCallIdx     int
	currentToolID   string
	currentToolName string
	inToolUse       bool
	inThinking      bool
}

// newAnthropicStreamTranslator 创建流式翻译器（工具序号从 -1 起计数）。
func newAnthropicStreamTranslator(w http.ResponseWriter, flusher http.Flusher) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{w: w, flusher: flusher, usage: &Usage{}, toolCallIdx: -1}
}

// writeChunk 写出一个 OpenAI 分片并递增序号。
func (t *anthropicStreamTranslator) writeChunk(chunk map[string]interface{}) {
	writeSSEChunk(t.w, t.flusher, chunk)
	t.chunkIndex++
}

// handleEvent 按 Anthropic 事件类型分发处理。
func (t *anthropicStreamTranslator) handleEvent(eventType string, event map[string]interface{}) {
	switch eventType {
	case "message_start":
		t.handleMessageStart(event)
	case "content_block_start":
		t.handleContentBlockStart(event)
	case "content_block_delta":
		t.handleContentBlockDelta(event)
	case "content_block_stop":
		t.handleContentBlockStop()
	case "message_delta":
		t.handleMessageDelta(event)
	case "message_stop":
		fmt.Fprintf(t.w, "data: [DONE]\n\n")
		t.flusher.Flush()
	}
}

// handleMessageStart 提取消息 ID 与输入用量，并补发初始 role 分片。
func (t *anthropicStreamTranslator) handleMessageStart(event map[string]interface{}) {
	if msg, ok := event["message"].(map[string]interface{}); ok {
		if id, ok := msg["id"].(string); ok {
			t.messageID = id
		}
		if u, ok := msg["usage"].(map[string]interface{}); ok {
			applyAnthropicUsage(t.usage, u)
		}
	}
	t.writeChunk(buildOpenAIStreamChunk(t.messageID, t.chunkIndex, "assistant", "", ""))
}

// handleContentBlockStart 处理 thinking / tool_use 内容块的开块事件。
func (t *anthropicStreamTranslator) handleContentBlockStart(event map[string]interface{}) {
	cb, ok := event["content_block"].(map[string]interface{})
	if !ok {
		return
	}
	blockType, _ := cb["type"].(string)
	switch blockType {
	case "thinking", "redacted_thinking":
		t.inThinking = true
	case "tool_use":
		t.toolCallIdx++
		t.currentToolID, _ = cb["id"].(string)
		t.currentToolName, _ = cb["name"].(string)
		t.inToolUse = true
		t.writeChunk(buildToolCallStartChunk(t.messageID, t.chunkIndex, t.toolCallIdx, t.currentToolID, t.currentToolName))
	}
}

// handleContentBlockDelta 处理文本 / 思考 / 工具参数增量并转译为 OpenAI 分片。
func (t *anthropicStreamTranslator) handleContentBlockDelta(event map[string]interface{}) {
	delta, ok := event["delta"].(map[string]interface{})
	if !ok {
		return
	}
	deltaType, _ := delta["type"].(string)
	switch deltaType {
	case "thinking_delta":
		if !t.inThinking {
			return
		}
		if text, _ := delta["thinking"].(string); text != "" {
			t.writeChunk(buildReasoningStreamChunk(t.messageID, t.chunkIndex, text))
		}
	case "signature_delta":
		// Signature is not forwarded to the client.
	case "text_delta":
		if text, _ := delta["text"].(string); text != "" {
			t.writeChunk(buildOpenAIStreamChunk(t.messageID, t.chunkIndex, "", text, ""))
		}
	case "input_json_delta":
		if partial, _ := delta["partial_json"].(string); partial != "" && t.inToolUse {
			t.writeChunk(buildToolCallDeltaChunk(t.messageID, t.chunkIndex, t.toolCallIdx, partial))
		}
	}
}

// handleContentBlockStop 关闭当前内容块：清空工具状态、退出 thinking 状态。
func (t *anthropicStreamTranslator) handleContentBlockStop() {
	if t.inToolUse {
		t.inToolUse = false
		t.currentToolID = ""
		t.currentToolName = ""
	}
	t.inThinking = false
}

// handleMessageDelta 提取 stop_reason 与输出用量，补发含 usage 的收尾分片。
func (t *anthropicStreamTranslator) handleMessageDelta(event map[string]interface{}) {
	finishReason := ""
	if delta, ok := event["delta"].(map[string]interface{}); ok {
		if sr, ok := delta["stop_reason"].(string); ok {
			finishReason = mapStopReason(sr)
		}
	}
	if u, ok := event["usage"].(map[string]interface{}); ok {
		applyAnthropicUsage(t.usage, u)
	}
	t.usage.TotalTokens = t.usage.PromptTokens + t.usage.CompletionTokens
	t.writeChunk(buildOpenAIStreamChunkWithUsage(t.messageID, t.chunkIndex, finishReason, t.usage))
}

// newOpenAIChunk 构造 OpenAI 流式分片的公共封装（单个 choice、index=0）。
func newOpenAIChunk(id string, delta map[string]interface{}, finishReason interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
}

// buildToolCallStartChunk creates an OpenAI streaming chunk for the start of a tool call.
func buildToolCallStartChunk(id string, chunkIdx, toolIdx int, toolCallID, funcName string) map[string]interface{} {
	delta := map[string]interface{}{
		"tool_calls": []map[string]interface{}{
			{
				"index": toolIdx,
				"id":    toolCallID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      funcName,
					"arguments": "",
				},
			},
		},
	}
	return newOpenAIChunk(id, delta, nil)
}

// buildToolCallDeltaChunk creates an OpenAI streaming chunk for a partial tool call argument.
func buildToolCallDeltaChunk(id string, chunkIdx, toolIdx int, argsFragment string) map[string]interface{} {
	delta := map[string]interface{}{
		"tool_calls": []map[string]interface{}{
			{
				"index": toolIdx,
				"function": map[string]interface{}{
					"arguments": argsFragment,
				},
			},
		},
	}
	return newOpenAIChunk(id, delta, nil)
}

// buildReasoningStreamChunk creates an OpenAI streaming chunk carrying reasoning_content in the delta.
func buildReasoningStreamChunk(id string, chunkIdx int, reasoning string) map[string]interface{} {
	return newOpenAIChunk(id, map[string]interface{}{
		"reasoning_content": reasoning,
	}, nil)
}

func buildOpenAIStreamChunk(id string, index int, role, content, finishReason string) map[string]interface{} {
	delta := map[string]interface{}{}
	if role != "" {
		delta["role"] = role
	}
	if content != "" {
		delta["content"] = content
	}
	var fr interface{}
	if finishReason != "" {
		fr = finishReason
	}
	return newOpenAIChunk(id, delta, fr)
}

func applyAnthropicUsage(usage *Usage, u map[string]interface{}) {
	inputTokens := usage.PromptTokens - usage.PromptCacheReadTokens - usage.PromptCacheWriteTokens
	if inputTokens < 0 {
		inputTokens = usage.PromptTokens
	}
	if v, ok := u["input_tokens"].(float64); ok {
		inputTokens = int(v)
	}
	if v, ok := u["output_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
	}
	if v, ok := u["cache_read_input_tokens"].(float64); ok {
		iv := int(v)
		if iv > 0 || usage.PromptCacheReadTokens == 0 {
			usage.PromptCacheReadTokens = iv
		}
	}
	if v, ok := u["cache_creation_input_tokens"].(float64); ok {
		iv := int(v)
		if iv > 0 || usage.PromptCacheWriteTokens == 0 {
			usage.PromptCacheWriteTokens = iv
		}
	}
	usage.PromptTokens = inputTokens + usage.PromptCacheReadTokens + usage.PromptCacheWriteTokens
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
}

func buildOpenAIStreamChunkWithUsage(id string, index int, finishReason string, usage *Usage) map[string]interface{} {
	chunk := newOpenAIChunk(id, map[string]interface{}{}, finishReason)
	chunk["usage"] = map[string]interface{}{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens":         usage.PromptCacheReadTokens,
			"cache_creation_tokens": usage.PromptCacheWriteTokens,
		},
		"cache_read_input_tokens":     usage.PromptCacheReadTokens,
		"cache_creation_input_tokens": usage.PromptCacheWriteTokens,
	}
	return chunk
}

func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, chunk map[string]interface{}) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// CheckConnectivityWithChat probes the Anthropic Messages API by issuing a
// minimal POST /v1/messages request (one user message, max_tokens=1).
//
// This probe validates both network reachability, the apiKey, and that the
// supplied model name is actually invocable.
//
// On success it returns the round-trip latency and a nil error. On failure
// it returns a *ConnectivityError wrapping one of the Err* sentinels defined
// in provider.go; callers can use errors.Is to classify the failure.
func (p *AnthropicProvider) CheckConnectivityWithChat(
	ctx context.Context,
	apiKey, apiBase, model string,
) (time.Duration, error) {
	body, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"max_tokens": 1,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		return 0, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}

	httpReq, err := newProbeRequest(ctx, p.baseURL(apiBase)+"/v1/messages", body, func(h http.Header) {
		if apiKey != "" {
			h.Set("x-api-key", apiKey)
		}
		h.Set("anthropic-version", anthropicVersion)
	})
	if err != nil {
		return 0, err
	}
	return runConnectivityProbe(httpReq)
}
