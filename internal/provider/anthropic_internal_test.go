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
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─── response 转换 ─────────────────────────────────────────────────────

func TestAnthropicToOpenAIIncludesCacheTokensInPromptTotals(t *testing.T) {
	ar := &anthropicResponse{
		ID:         "msg-cache",
		Role:       "assistant",
		StopReason: "end_turn",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "ok"},
		},
	}
	ar.Usage.InputTokens = 100
	ar.Usage.CacheReadInputTokens = 30
	ar.Usage.CacheCreationInputTokens = 20
	ar.Usage.OutputTokens = 10

	body, err := anthropicToOpenAI(ar)
	if err != nil {
		t.Fatalf("anthropicToOpenAI() error = %v", err)
	}

	var got struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens        int `json:"cached_tokens"`
				CacheCreationTokens int `json:"cache_creation_tokens"`
			} `json:"prompt_tokens_details"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	if got.Usage.PromptTokens != 150 || got.Usage.CompletionTokens != 10 || got.Usage.TotalTokens != 160 {
		t.Fatalf(
			"usage totals = prompt %d completion %d total %d, want prompt 150 completion 10 total 160",
			got.Usage.PromptTokens,
			got.Usage.CompletionTokens,
			got.Usage.TotalTokens,
		)
	}
	if got.Usage.PromptDetails.CachedTokens != 30 || got.Usage.PromptDetails.CacheCreationTokens != 20 {
		t.Fatalf(
			"prompt details = cached %d creation %d, want cached 30 creation 20",
			got.Usage.PromptDetails.CachedTokens,
			got.Usage.PromptDetails.CacheCreationTokens,
		)
	}
	if got.Usage.CacheReadInputTokens != 30 || got.Usage.CacheCreationInputTokens != 20 {
		t.Fatalf(
			"anthropic cache fields = read %d creation %d, want read 30 creation 20",
			got.Usage.CacheReadInputTokens,
			got.Usage.CacheCreationInputTokens,
		)
	}
}

// TestAnthropicToOpenAI_MessageBlocks 覆盖 text / thinking / redacted_thinking /
// tool_use 内容块到 OpenAI message 的映射，以及 finish_reason 的换算。
func TestAnthropicToOpenAI_MessageBlocks(t *testing.T) {
	ar := &anthropicResponse{
		ID:         "msg-1",
		StopReason: "tool_use",
		Content: []anthropicContentBlock{
			{Type: "thinking", Thinking: "reason-1"},
			{Type: "redacted_thinking"},
			{Type: "text", Text: "part-a"},
			{Type: "text", Text: "part-b"},
			{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"sf"}`)},
		},
	}
	ar.Usage.InputTokens = 5
	ar.Usage.OutputTokens = 7

	body, err := anthropicToOpenAI(ar)
	if err != nil {
		t.Fatalf("anthropicToOpenAI() error = %v", err)
	}

	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string                   `json:"role"`
				Content          string                   `json:"content"`
				ReasoningContent string                   `json:"reasoning_content"`
				ToolCalls        []map[string]interface{} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	if got.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", got.Object)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices length = %d, want 1", len(got.Choices))
	}
	choice := got.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "part-apart-b" {
		t.Fatalf("message = %+v, want assistant with concatenated text", choice.Message)
	}
	if choice.Message.ReasoningContent != "reason-1[redacted]" {
		t.Fatalf("reasoning_content = %q, want reason-1[redacted]", choice.Message.ReasoningContent)
	}
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want a single call", choice.Message.ToolCalls)
	}
	tc := choice.Message.ToolCalls[0]
	if tc["id"] != "toolu_1" || tc["type"] != "function" {
		t.Fatalf("tool_call = %+v, want id toolu_1 type function", tc)
	}
	fn, _ := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"sf"}` {
		t.Fatalf("function = %+v, want get_weather with raw arguments", fn)
	}
}

// TestAnthropicToOpenAI_EmptyToolUseInput 覆盖 tool_use 无 input 时以 "{}" 兜底。
func TestAnthropicToOpenAI_EmptyToolUseInput(t *testing.T) {
	ar := &anthropicResponse{
		ID:      "msg-2",
		Content: []anthropicContentBlock{{Type: "tool_use", ID: "t1", Name: "noop"}},
	}
	body, err := anthropicToOpenAI(ar)
	if err != nil {
		t.Fatalf("anthropicToOpenAI() error = %v", err)
	}
	if !strings.Contains(string(body), `"arguments":"{}"`) {
		t.Fatalf("body = %s, want arguments fallback to {}", body)
	}
}

func TestApplyAnthropicUsageIncludesCacheTokensInPromptTotals(t *testing.T) {
	usage := &Usage{}
	applyAnthropicUsage(usage, map[string]interface{}{
		"input_tokens":                float64(100),
		"cache_read_input_tokens":     float64(30),
		"cache_creation_input_tokens": float64(20),
	})
	applyAnthropicUsage(usage, map[string]interface{}{
		"output_tokens": float64(10),
	})

	if usage.PromptTokens != 150 || usage.CompletionTokens != 10 || usage.TotalTokens != 160 {
		t.Fatalf(
			"usage totals = prompt %d completion %d total %d, want prompt 150 completion 10 total 160",
			usage.PromptTokens,
			usage.CompletionTokens,
			usage.TotalTokens,
		)
	}
	if usage.PromptCacheReadTokens != 30 || usage.PromptCacheWriteTokens != 20 {
		t.Fatalf(
			"cache tokens = read %d write %d, want read 30 write 20",
			usage.PromptCacheReadTokens,
			usage.PromptCacheWriteTokens,
		)
	}

	applyAnthropicUsage(usage, map[string]interface{}{
		"input_tokens":  float64(110),
		"output_tokens": float64(11),
	})
	if usage.PromptTokens != 160 || usage.TotalTokens != 171 {
		t.Fatalf(
			"updated usage totals = prompt %d total %d, want prompt 160 total 171",
			usage.PromptTokens,
			usage.TotalTokens,
		)
	}
}

// TestApplyAnthropicUsage_ZeroCacheOverwrite 覆盖 cache 字段为 0 时不覆盖已有非零值。
func TestApplyAnthropicUsage_ZeroCacheOverwrite(t *testing.T) {
	usage := &Usage{}
	applyAnthropicUsage(usage, map[string]interface{}{
		"input_tokens":                float64(10),
		"cache_read_input_tokens":     float64(3),
		"cache_creation_input_tokens": float64(4),
	})
	applyAnthropicUsage(usage, map[string]interface{}{
		"input_tokens":                float64(10),
		"cache_read_input_tokens":     float64(0),
		"cache_creation_input_tokens": float64(0),
	})
	if usage.PromptCacheReadTokens != 3 || usage.PromptCacheWriteTokens != 4 {
		t.Fatalf(
			"cache = read %d write %d, want read 3 write 4 (zero must not clobber)",
			usage.PromptCacheReadTokens,
			usage.PromptCacheWriteTokens,
		)
	}
}

func TestAnthropicUsage(t *testing.T) {
	ar := &anthropicResponse{}
	ar.Usage.InputTokens = 7
	ar.Usage.OutputTokens = 3
	ar.Usage.CacheReadInputTokens = 2
	ar.Usage.CacheCreationInputTokens = 1

	got := anthropicUsage(ar)
	if got.PromptTokens != 10 || got.CompletionTokens != 3 || got.TotalTokens != 13 {
		t.Fatalf("usage = %+v, want prompt 10 completion 3 total 13", got)
	}
	if got.PromptCacheReadTokens != 2 || got.PromptCacheWriteTokens != 1 {
		t.Fatalf("cache = read %d write %d, want read 2 write 1", got.PromptCacheReadTokens, got.PromptCacheWriteTokens)
	}
}

func TestMapStopReason(t *testing.T) {
	tests := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"pause_turn":    "pause_turn", // unknown values pass through unchanged
		"":              "",
	}
	for in, want := range tests {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// ─── request 转换 ──────────────────────────────────────────────────────

func mustBuildAnthropicRequest(t *testing.T, model, body string) anthropicRequest {
	t.Helper()
	out, err := buildAnthropicRequest(model, []byte(body))
	if err != nil {
		t.Fatalf("buildAnthropicRequest() error = %v", err)
	}
	var ar anthropicRequest
	if err := json.Unmarshal(out, &ar); err != nil {
		t.Fatalf("anthropic request is not JSON: %v (%s)", err, out)
	}
	return ar
}

// asBlocks 把 message content 归一化为内容块数组（bare string 会被包装成 text 块）。
func asBlocks(t *testing.T, raw json.RawMessage) []map[string]interface{} {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("content is empty")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []map[string]interface{}{{"type": "text", "text": s}}
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("content is neither string nor block array: %s", raw)
	}
	return blocks
}

func rawAsString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("content = %s, want a bare JSON string", raw)
	}
	return s
}

func TestBuildAnthropicRequest_SystemExtraction(t *testing.T) {
	ar := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[
			{"role":"system","content":"first"},
			{"role":"system","content":[{"type":"text","text":"second"}]},
			{"role":"user","content":"hi"}
		]
	}`)

	if ar.Model != "claude-3" {
		t.Fatalf("model = %q, want claude-3", ar.Model)
	}
	if ar.System != "first\n\nsecond" {
		t.Fatalf("system = %q, want system messages joined with blank line", ar.System)
	}
	if len(ar.Messages) != 1 {
		t.Fatalf("messages length = %d, want 1 (system messages removed)", len(ar.Messages))
	}
	if ar.Messages[0].Role != "user" {
		t.Fatalf("message role = %q, want user", ar.Messages[0].Role)
	}
	// 单一纯文本块 → 直接下发裸字符串，输出更简洁。
	if got := rawAsString(t, ar.Messages[0].Content); got != "hi" {
		t.Fatalf("content = %q, want bare string \"hi\"", got)
	}
	if ar.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens = %d, want default %d", ar.MaxTokens, defaultMaxTokens)
	}
}

func TestBuildAnthropicRequest_ToolCallsAndToolResults(t *testing.T) {
	ar := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":"let me check","tool_calls":[
				{"id":"call/1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"sf\"}"}}
			]},
			{"role":"tool","tool_call_id":"call/1","content":"sunny"},
			{"role":"user","content":"thanks"}
		]
	}`)

	if len(ar.Messages) != 3 {
		t.Fatalf("messages length = %d, want 3 (tool result merged with following user message)", len(ar.Messages))
	}

	assistant := asBlocks(t, ar.Messages[1].Content)
	if ar.Messages[1].Role != "assistant" || len(assistant) != 2 {
		t.Fatalf(
			"assistant message = %+v (blocks %+v), want role assistant with text+tool_use",
			ar.Messages[1],
			assistant,
		)
	}
	if assistant[0]["type"] != "text" || assistant[0]["text"] != "let me check" {
		t.Fatalf("assistant text block = %+v", assistant[0])
	}
	toolUse := assistant[1]
	if toolUse["type"] != "tool_use" || toolUse["name"] != "get_weather" {
		t.Fatalf("tool_use block = %+v", toolUse)
	}
	// tool_use_id 必须被清洗成 Anthropic 允许的字符集。
	if toolUse["id"] != "call_1" {
		t.Fatalf("tool_use id = %v, want call_1 (sanitized)", toolUse["id"])
	}
	input, _ := toolUse["input"].(map[string]interface{})
	if input["city"] != "sf" {
		t.Fatalf("tool_use input = %v, want arguments preserved as object", toolUse["input"])
	}

	merged := asBlocks(t, ar.Messages[2].Content)
	if ar.Messages[2].Role != "user" || len(merged) != 2 {
		t.Fatalf("merged user message = %+v, want tool_result + text", ar.Messages[2])
	}
	if merged[0]["type"] != "tool_result" || merged[0]["tool_use_id"] != "call_1" || merged[0]["content"] != "sunny" {
		t.Fatalf("tool_result block = %+v", merged[0])
	}
	if merged[1]["type"] != "text" || merged[1]["text"] != "thanks" {
		t.Fatalf("merged text block = %+v", merged[1])
	}
}

// TestBuildAnthropicRequest_ToolResultWithBlocks 覆盖 tool 消息 content 为数组
// 时转换为 tool_result 的块内容。
func TestBuildAnthropicRequest_ToolResultWithBlocks(t *testing.T) {
	ar := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[
			{"role":"tool","tool_call_id":"t1",
				"content":[{"type":"text","text":"out"},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}
		]
	}`)

	blocks := asBlocks(t, ar.Messages[0].Content)
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Fatalf("blocks = %+v, want a single tool_result block", blocks)
	}
	inner, ok := blocks[0]["content"].([]interface{})
	if !ok || len(inner) != 2 {
		t.Fatalf("tool_result content = %v, want 2 blocks", blocks[0]["content"])
	}
	first, _ := inner[0].(map[string]interface{})
	second, _ := inner[1].(map[string]interface{})
	if first["type"] != "text" || first["text"] != "out" {
		t.Fatalf("first inner block = %+v", first)
	}
	if second["type"] != "image" {
		t.Fatalf("second inner block = %+v, want image", second)
	}
}

func TestBuildAnthropicRequest_MultiModalImages(t *testing.T) {
	ar := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/abc"}},
			{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
			{"type":"image_url","image_url":{"url":"data:broken"}},
			{"type":"image_url","image_url":"http://insecure/a.png"},
			{"type":"unknown_block","value":1}
		]}]
	}`)

	blocks := asBlocks(t, ar.Messages[0].Content)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want text + base64 image + https image", blocks)
	}

	base64Source, _ := blocks[1]["source"].(map[string]interface{})
	if blocks[1]["type"] != "image" || base64Source["type"] != "base64" ||
		base64Source["media_type"] != "image/jpeg" || base64Source["data"] != "/9j/abc" {
		t.Fatalf("base64 image block = %+v", blocks[1])
	}

	urlSource, _ := blocks[2]["source"].(map[string]interface{})
	if urlSource["type"] != "url" || urlSource["url"] != "https://example.com/a.png" {
		t.Fatalf("url image block = %+v", blocks[2])
	}
}

func TestBuildAnthropicRequest_ToolsAndToolChoice(t *testing.T) {
	tests := []struct {
		name       string
		toolChoice string
		wantType   string
		wantName   string
	}{
		{name: "auto", toolChoice: `"auto"`, wantType: "auto"},
		{name: "required → any", toolChoice: `"required"`, wantType: "any"},
		{
			name:       "named function",
			toolChoice: `{"type":"function","function":{"name":"f"}}`,
			wantType:   "tool",
			wantName:   "f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"type":"function",
					"function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}],
				"tool_choice":` + tt.toolChoice + `
			}`
			ar := mustBuildAnthropicRequest(t, "claude-3", body)

			if len(ar.Tools) != 1 {
				t.Fatalf("tools = %+v, want 1", ar.Tools)
			}
			if ar.Tools[0].Name != "f" || ar.Tools[0].Description != "d" {
				t.Fatalf("tool = %+v, want name f description d", ar.Tools[0])
			}
			if !strings.Contains(string(ar.Tools[0].InputSchema), `"type":"object"`) {
				t.Fatalf("input_schema = %s, want parameters passed through", ar.Tools[0].InputSchema)
			}
			if ar.ToolChoice == nil {
				t.Fatalf("tool_choice is nil, want %q", tt.wantType)
			}
			if ar.ToolChoice.Type != tt.wantType || ar.ToolChoice.Name != tt.wantName {
				t.Fatalf("tool_choice = %+v, want type %q name %q", ar.ToolChoice, tt.wantType, tt.wantName)
			}
		})
	}
}

// TestBuildAnthropicRequest_ToolChoiceNoneOmitted 覆盖 none 与「无 tools」时不输出 tool_choice。
func TestBuildAnthropicRequest_ToolChoiceNoneOmitted(t *testing.T) {
	withNone := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],
		"tool_choice":"none"
	}`)
	if withNone.ToolChoice != nil {
		t.Fatalf("tool_choice = %+v, want nil for none", withNone.ToolChoice)
	}

	noTools := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":"auto"
	}`)
	if noTools.ToolChoice != nil {
		t.Fatalf("tool_choice = %+v, want nil when no tools are present", noTools.ToolChoice)
	}
}

func TestBuildAnthropicRequest_MaxTokensAndThinking(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantMaxTokens int
		wantBudget    int // 0 means thinking must be absent
		wantStream    bool
	}{
		{
			name:          "default when omitted",
			body:          `{"messages":[{"role":"user","content":"hi"}]}`,
			wantMaxTokens: defaultMaxTokens,
		},
		{
			name:          "explicit max_tokens",
			body:          `{"messages":[{"role":"user","content":"hi"}],"max_tokens":123}`,
			wantMaxTokens: 123,
		},
		{
			name:          "max_completion_tokens wins",
			body:          `{"messages":[{"role":"user","content":"hi"}],"max_tokens":123,"max_completion_tokens":456}`,
			wantMaxTokens: 456,
		},
		{
			name:          "reasoning_effort high adds budget on top of default",
			body:          `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
			wantMaxTokens: defaultMaxTokens + thinkingBudgetHigh,
			wantBudget:    thinkingBudgetHigh,
		},
		{
			name:          "reasoning_effort low with explicit max_tokens keeps it",
			body:          `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","max_tokens":300}`,
			wantMaxTokens: 300,
			wantBudget:    thinkingBudgetLow,
		},
		{
			name:          "reasoning_effort none is ignored",
			body:          `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
			wantMaxTokens: defaultMaxTokens,
		},
		{
			name:          "raw thinking passthrough",
			body:          `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1234}}`,
			wantMaxTokens: defaultMaxTokens,
			wantBudget:    1234,
		},
		{
			name:          "stream flag propagated",
			body:          `{"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			wantMaxTokens: defaultMaxTokens,
			wantStream:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ar := mustBuildAnthropicRequest(t, "claude-3", tt.body)
			if ar.MaxTokens != tt.wantMaxTokens {
				t.Fatalf("max_tokens = %d, want %d", ar.MaxTokens, tt.wantMaxTokens)
			}
			if ar.Stream != tt.wantStream {
				t.Fatalf("stream = %v, want %v", ar.Stream, tt.wantStream)
			}
			if tt.wantBudget == 0 {
				if len(ar.Thinking) > 0 {
					t.Fatalf("thinking = %s, want absent", ar.Thinking)
				}
				return
			}
			var got anthropicThinking
			if err := json.Unmarshal(ar.Thinking, &got); err != nil {
				t.Fatalf("thinking = %s, want valid JSON: %v", ar.Thinking, err)
			}
			if got.Type != "enabled" || got.BudgetTokens != tt.wantBudget {
				t.Fatalf("thinking = %+v, want enabled/%d", got, tt.wantBudget)
			}
		})
	}
}

// TestBuildAnthropicRequest_UnknownRoleMappedToUser 覆盖未知 role 归一到 user。
func TestBuildAnthropicRequest_UnknownRoleMappedToUser(t *testing.T) {
	ar := mustBuildAnthropicRequest(t, "claude-3", `{
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"developer","content":"be nice"}
		]
	}`)

	if len(ar.Messages) != 1 {
		t.Fatalf("messages length = %d, want 1 (consecutive user roles merged)", len(ar.Messages))
	}
	blocks := asBlocks(t, ar.Messages[0].Content)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want both messages merged", blocks)
	}
}

func TestBuildAnthropicRequest_InvalidJSON(t *testing.T) {
	if _, err := buildAnthropicRequest("claude-3", []byte(`{`)); err == nil {
		t.Fatalf("expected decode error, got nil")
	}
}

// ─── 转换小函数 ────────────────────────────────────────────────────────

func TestConvertToolChoiceToAnthropic(t *testing.T) {
	tests := []struct {
		name     string
		in       interface{}
		wantNil  bool
		wantType string
		wantName string
	}{
		{name: "auto", in: "auto", wantType: "auto"},
		{name: "required", in: "required", wantType: "any"},
		{name: "none", in: "none", wantNil: true},
		{name: "unknown string", in: "whatever", wantType: "auto"},
		{
			name:     "named function",
			in:       map[string]interface{}{"function": map[string]interface{}{"name": "f"}},
			wantType: "tool",
			wantName: "f",
		},
		{
			name:     "function without name",
			in:       map[string]interface{}{"function": map[string]interface{}{}},
			wantType: "auto",
		},
		{name: "map without function", in: map[string]interface{}{"type": "function"}, wantType: "auto"},
		{name: "nil", in: nil, wantNil: true},
		{name: "number", in: 42, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToolChoiceToAnthropic(tt.in)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("convertToolChoiceToAnthropic(%v) = %+v, want nil", tt.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("convertToolChoiceToAnthropic(%v) = nil, want %+v", tt.in, tt.wantType)
			}
			if got.Type != tt.wantType || got.Name != tt.wantName {
				t.Fatalf(
					"convertToolChoiceToAnthropic(%v) = %+v, want type %q name %q",
					tt.in,
					got,
					tt.wantType,
					tt.wantName,
				)
			}
		})
	}
}

func TestParseDataURI(t *testing.T) {
	tests := []struct {
		name      string
		uri       string
		wantMedia string
		wantData  string
	}{
		{name: "jpeg", uri: "data:image/jpeg;base64,/9j/abcd", wantMedia: "image/jpeg", wantData: "/9j/abcd"},
		{
			name:      "escaped slash in media type",
			uri:       `data:image\/png;base64,AAAA`,
			wantMedia: "image/png",
			wantData:  "AAAA",
		},
		{name: "not a data uri", uri: "https://example.com/a.png", wantMedia: "", wantData: ""},
		{name: "missing base64 marker", uri: "data:image/png,plain", wantMedia: "", wantData: ""},
		{name: "empty", uri: "", wantMedia: "", wantData: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			media, data := parseDataURI(tt.uri)
			if media != tt.wantMedia || data != tt.wantData {
				t.Fatalf("parseDataURI(%q) = (%q, %q), want (%q, %q)", tt.uri, media, data, tt.wantMedia, tt.wantData)
			}
		})
	}
}

func TestSanitizeToolUseID(t *testing.T) {
	tests := map[string]string{
		"call_1":          "call_1",
		"abc-123_XYZ":     "abc-123_XYZ",
		"call/1":          "call_1",
		"a b":             "a_b",
		"":                "tool_use_id",
		"emoji-\u2705-id": "emoji-_-id",
	}
	for in, want := range tests {
		if got := sanitizeToolUseID(in); got != want {
			t.Errorf("sanitizeToolUseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapReasoningEffort(t *testing.T) {
	tests := map[string]int{
		"minimal": thinkingBudgetMinimal,
		"low":     thinkingBudgetLow,
		"medium":  thinkingBudgetMedium,
		"high":    thinkingBudgetHigh,
		"HIGH":    thinkingBudgetHigh,
	}
	for in, wantBudget := range tests {
		raw := mapReasoningEffort(in)
		if raw == nil {
			t.Fatalf("mapReasoningEffort(%q) = nil, want budget %d", in, wantBudget)
		}
		var thinking anthropicThinking
		if err := json.Unmarshal(raw, &thinking); err != nil {
			t.Fatalf("mapReasoningEffort(%q) = %s, want valid JSON: %v", in, raw, err)
		}
		if thinking.Type != "enabled" || thinking.BudgetTokens != wantBudget {
			t.Errorf("mapReasoningEffort(%q) = %+v, want enabled/%d", in, thinking, wantBudget)
		}
	}

	for _, in := range []string{"none", "", "extreme"} {
		if got := mapReasoningEffort(in); got != nil {
			t.Errorf("mapReasoningEffort(%q) = %s, want nil", in, got)
		}
	}
}

func TestMergeConsecutiveAnthropicMessages(t *testing.T) {
	if got := mergeConsecutiveAnthropicMessages(nil); got != nil {
		t.Fatalf("mergeConsecutiveAnthropicMessages(nil) = %+v, want nil", got)
	}

	msgs := []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "user", Content: json.RawMessage(`"b"`)},
		{Role: "user", Content: json.RawMessage(`"c"`)},
		{Role: "assistant", Content: json.RawMessage(`"d"`)},
	}
	merged := mergeConsecutiveAnthropicMessages(msgs)
	if len(merged) != 2 {
		t.Fatalf("merged length = %d, want 2", len(merged))
	}
	blocks := asBlocks(t, merged[0].Content)
	if len(blocks) != 3 {
		t.Fatalf("merged first message blocks = %+v, want 3", blocks)
	}
	if merged[1].Role != "assistant" {
		t.Fatalf("second role = %q, want assistant", merged[1].Role)
	}
}

func TestContentToString(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want string
	}{
		{name: "string", in: "plain", want: "plain"},
		{name: "nil", in: nil, want: ""},
		{name: "text array", in: []interface{}{
			map[string]interface{}{"type": "text", "text": "a"},
			map[string]interface{}{"type": "image_url", "image_url": "x"},
			map[string]interface{}{"type": "text", "text": "b"},
		}, want: "ab"},
		{name: "other type", in: float64(42), want: "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentToString(tt.in); got != tt.want {
				t.Fatalf("contentToString(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestContentToAnthropicBlocks(t *testing.T) {
	if got := contentToAnthropicBlocks(""); got != nil {
		t.Fatalf("contentToAnthropicBlocks(\"\") = %+v, want nil", got)
	}
	if got := contentToAnthropicBlocks(nil); got != nil {
		t.Fatalf("contentToAnthropicBlocks(nil) = %+v, want nil", got)
	}
	if got := contentToAnthropicBlocks(123); got != nil {
		t.Fatalf("contentToAnthropicBlocks(123) = %+v, want nil", got)
	}

	blocks := contentToAnthropicBlocks([]interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
	})
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" {
		t.Fatalf("blocks = %+v, want single text block", blocks)
	}

	// 非 map 元素必须被跳过而不是 panic。
	if got := convertContentBlocks([]interface{}{"nope", 7}); len(got) != 0 {
		t.Fatalf("convertContentBlocks() = %+v, want empty", got)
	}
}

func TestToolResultContentToAnthropic(t *testing.T) {
	if got := toolResultContentToAnthropic("plain"); got != "plain" {
		t.Fatalf("toolResultContentToAnthropic(string) = %v, want plain", got)
	}
	if got := toolResultContentToAnthropic(nil); got != "" {
		t.Fatalf("toolResultContentToAnthropic(nil) = %v, want empty string", got)
	}
	if got := toolResultContentToAnthropic([]interface{}{"nope"}); got != "" {
		t.Fatalf("toolResultContentToAnthropic(no blocks) = %v, want empty string", got)
	}

	got := toolResultContentToAnthropic([]interface{}{
		map[string]interface{}{"type": "text", "text": "out"},
	})
	if blocks, ok := got.([]map[string]interface{}); !ok || len(blocks) != 1 || blocks[0]["text"] != "out" {
		t.Fatalf("toolResultContentToAnthropic(blocks) = %#v, want single text block", got)
	}
}

// ─── 流式 chunk 构造 ───────────────────────────────────────────────────

func TestWriteSSEChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEChunk(rec, rec, map[string]interface{}{"id": "abc"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body = %q, want SSE data frame with trailing blank line", body)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")), &got); err != nil {
		t.Fatalf("chunk payload is not JSON: %v (%q)", err, body)
	}
	if got["id"] != "abc" {
		t.Fatalf("chunk id = %v, want abc", got["id"])
	}
}

func TestBuildOpenAIStreamChunk(t *testing.T) {
	chunk := buildOpenAIStreamChunk("id-1", 0, "assistant", "", "")

	if chunk["id"] != "id-1" || chunk["object"] != "chat.completion.chunk" {
		t.Fatalf("chunk = %+v, want id id-1 object chat.completion.chunk", chunk)
	}
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices = %+v, want 1", choices)
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if delta["role"] != "assistant" {
		t.Fatalf("delta = %+v, want role assistant", delta)
	}
	if _, ok := delta["content"]; ok {
		t.Fatalf("delta = %+v, content should be omitted when empty", delta)
	}
	if v, ok := choice["finish_reason"]; !ok || v != nil {
		t.Fatalf("finish_reason = %v, want explicit null", choice["finish_reason"])
	}

	withText := buildOpenAIStreamChunk("id-2", 0, "", "hello", "stop")
	choice2, _ := withText["choices"].([]interface{})[0].(map[string]interface{})
	delta2, _ := choice2["delta"].(map[string]interface{})
	if delta2["content"] != "hello" {
		t.Fatalf("delta = %+v, want content hello", delta2)
	}
	if choice2["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice2["finish_reason"])
	}
}

func TestBuildToolCallChunks(t *testing.T) {
	start := buildToolCallStartChunk("id", 0, 2, "call_2", "get_weather")
	choice, _ := start["choices"].([]interface{})[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	calls, _ := delta["tool_calls"].([]map[string]interface{})
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v, want 1", calls)
	}
	if calls[0]["index"] != 2 || calls[0]["id"] != "call_2" || calls[0]["type"] != "function" {
		t.Fatalf("tool_call = %+v, want index 2 id call_2 type function", calls[0])
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != "" {
		t.Fatalf("function = %+v, want name get_weather and empty arguments", fn)
	}

	partial := buildToolCallDeltaChunk("id", 0, 2, `{"city":`)
	choice2, _ := partial["choices"].([]interface{})[0].(map[string]interface{})
	delta2, _ := choice2["delta"].(map[string]interface{})
	calls2, _ := delta2["tool_calls"].([]map[string]interface{})
	fn2, _ := calls2[0]["function"].(map[string]interface{})
	if calls2[0]["index"] != 2 || fn2["arguments"] != `{"city":` {
		t.Fatalf("partial tool_call = %+v, want index 2 and argument fragment", calls2[0])
	}
	if _, ok := calls2[0]["id"]; ok {
		t.Fatalf("partial tool_call = %+v, id should be omitted", calls2[0])
	}
}

func TestBuildReasoningStreamChunk(t *testing.T) {
	chunk := buildReasoningStreamChunk("id", 0, "thinking...")
	choice, _ := chunk["choices"].([]interface{})[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if delta["reasoning_content"] != "thinking..." {
		t.Fatalf("delta = %+v, want reasoning_content", delta)
	}
}

func TestBuildOpenAIStreamChunkWithUsage(t *testing.T) {
	usage := &Usage{
		PromptTokens:           10,
		CompletionTokens:       4,
		TotalTokens:            14,
		PromptCacheReadTokens:  2,
		PromptCacheWriteTokens: 3,
	}
	chunk := buildOpenAIStreamChunkWithUsage("id", 0, "stop", usage)

	choice, _ := chunk["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	got, _ := chunk["usage"].(map[string]interface{})
	if got["prompt_tokens"] != 10 || got["completion_tokens"] != 4 || got["total_tokens"] != 14 {
		t.Fatalf("usage = %+v, want 10/4/14", got)
	}
	details, _ := got["prompt_tokens_details"].(map[string]interface{})
	if details["cached_tokens"] != 2 || details["cache_creation_tokens"] != 3 {
		t.Fatalf("prompt_tokens_details = %+v, want cached 2 creation 3", details)
	}
	if got["cache_read_input_tokens"] != 2 || got["cache_creation_input_tokens"] != 3 {
		t.Fatalf("anthropic cache fields = %+v", got)
	}
}
