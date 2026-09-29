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

// 白盒单测：Anthropic ↔ OpenAI 协议转换（anthropic_convert.go）。
// 断言以「字段值 / 结构」为准，不依赖字段顺序。

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 通用辅助
// ---------------------------------------------------------------------------

// anthConvMapFromBytes 解析 JSON 字节为 map（并断言可解析）。
func anthConvMapFromBytes(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", string(b), err)
	}
	return m
}

// anthConvRequest 执行请求转换并断言成功。
func anthConvRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := anthropicRequestToOpenAI([]byte(body))
	if err != nil {
		t.Fatalf("anthropicRequestToOpenAI(%s) unexpected error: %v", body, err)
	}
	return out
}

// anthConvResponse 执行响应回译并断言成功，返回解析后的 map。
func anthConvResponse(t *testing.T, body, requestedModel string) map[string]any {
	t.Helper()
	out, err := openAIResponseToAnthropic([]byte(body), requestedModel)
	if err != nil {
		t.Fatalf("openAIResponseToAnthropic(%s) unexpected error: %v", body, err)
	}
	return anthConvMapFromBytes(t, out)
}

// anthConvMessages 取转换结果中的 messages 数组。
func anthConvMessages(t *testing.T, out map[string]any) []any {
	t.Helper()
	msgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing or not array: %#v", out["messages"])
	}
	return msgs
}

// anthConvMsg 取 messages[i] 的 map。
func anthConvMsg(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	if i >= len(msgs) {
		t.Fatalf("messages[%d] out of range (len=%d)", i, len(msgs))
	}
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] not object: %#v", i, msgs[i])
	}
	return m
}

// anthConvStr 断言字段为字符串并返回。
func anthConvStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q = %#v, want string", key, m[key])
	}
	return s
}

// anthConvWantInt 断言数值字段等于 want。
func anthConvWantInt(t *testing.T, v any, want int, what string) {
	t.Helper()
	got, ok := anthJSONInt(v)
	if !ok {
		t.Fatalf("%s = %#v, want number", what, v)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

// anthConvWantStrings 断言字符串数组字段与 want 相等。
func anthConvWantStrings(t *testing.T, v any, want []string, what string) {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", what, v)
	}
	got := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		got = append(got, s)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// anthConvToolCall 取 assistant 消息里第 i 个 tool_calls 的 function 部分。
func anthConvToolCall(t *testing.T, msg map[string]any, i int) (id, name, args string) {
	t.Helper()
	calls, ok := msg["tool_calls"].([]any)
	if !ok {
		t.Fatalf("tool_calls missing or not array: %#v", msg["tool_calls"])
	}
	if i >= len(calls) {
		t.Fatalf("tool_calls[%d] out of range (len=%d)", i, len(calls))
	}
	call, ok := calls[i].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[%d] not object: %#v", i, calls[i])
	}
	if anthConvStr(t, call, "type") != "function" {
		t.Fatalf("tool_calls[%d].type = %v, want function", i, call["type"])
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[%d].function not object: %#v", i, call["function"])
	}
	id, _ = call["id"].(string)
	name, _ = fn["name"].(string)
	args, _ = fn["arguments"].(string)
	return id, name, args
}

// ---------------------------------------------------------------------------
// 请求方向
// ---------------------------------------------------------------------------

func TestAnthConvRequest_SystemStringAndPassthrough(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"claude-3","max_tokens":128,"stream":true,"temperature":0.3,"top_p":0.9,
		"system":"be brief",
		"messages":[{"role":"user","content":"hi"}]
	}`)

	if anthConvStr(t, out, "model") != "claude-3" {
		t.Fatalf("model = %v", out["model"])
	}
	anthConvWantInt(t, out["max_tokens"], 128, "max_tokens")
	if out["stream"] != true {
		t.Fatalf("stream = %#v, want true", out["stream"])
	}
	if _, ok := out["temperature"].(float64); !ok {
		t.Fatalf("temperature = %#v, want number", out["temperature"])
	}
	if _, ok := out["top_p"].(float64); !ok {
		t.Fatalf("top_p = %#v, want number", out["top_p"])
	}

	msgs := anthConvMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	sys := anthConvMsg(t, msgs, 0)
	if anthConvStr(t, sys, "role") != "system" || anthConvStr(t, sys, "content") != "be brief" {
		t.Fatalf("system message = %#v", sys)
	}
	if anthConvStr(t, anthConvMsg(t, msgs, 1), "content") != "hi" {
		t.Fatalf("user content = %#v", msgs[1])
	}
}

func TestAnthConvRequest_SystemBlocksJoinTextOnly(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":8,
		"system":[
			{"type":"text","text":"a"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
			{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	msgs := anthConvMessages(t, out)
	sys := anthConvMsg(t, msgs, 0)
	if anthConvStr(t, sys, "role") != "system" {
		t.Fatalf("first message role = %v", sys["role"])
	}
	if got := anthConvStr(t, sys, "content"); got != "a\nb" {
		t.Fatalf("system content = %q, want %q", got, "a\nb")
	}
	// system 块上的 cache_control 不得泄漏为独立字段。
	if _, exists := sys["cache_control"]; exists {
		t.Fatalf("system message must not carry cache_control: %#v", sys)
	}
}

func TestAnthConvRequest_StringContentPreserved(t *testing.T) {
	out := anthConvRequest(t, `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"plain"}]}`)
	msgs := anthConvMessages(t, out)
	if got := anthConvMsg(t, msgs, 0)["content"]; got != "plain" {
		t.Fatalf("content = %#v, want string \"plain\"", got)
	}
}

func TestAnthConvRequest_TextAndImageParts(t *testing.T) {
	base64Body := `{
		"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look at this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}
		]}]
	}`
	out := anthConvRequest(t, base64Body)
	msgs := anthConvMessages(t, out)
	msg0 := anthConvMsg(t, msgs, 0)
	content, ok := msg0["content"].([]any)
	if !ok {
		t.Fatalf("content must be a parts array when an image is present: %#v", msg0["content"])
	}
	if len(content) != 2 {
		t.Fatalf("parts len = %d, want 2", len(content))
	}
	textPart := content[0].(map[string]any)
	if anthConvStr(t, textPart, "type") != "text" || anthConvStr(t, textPart, "text") != "look at this" {
		t.Fatalf("text part = %#v", textPart)
	}
	imgPart := content[1].(map[string]any)
	if anthConvStr(t, imgPart, "type") != "image_url" {
		t.Fatalf("image part type = %#v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]any)
	if got := anthConvStr(t, imgURL, "url"); got != "data:image/png;base64,QUJD" {
		t.Fatalf("image data URI = %q", got)
	}

	// source.type=url 直通。
	urlBody := `{
		"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
		]}]
	}`
	outURL := anthConvRequest(t, urlBody)
	msgsURL := anthConvMessages(t, outURL)
	msgURL := anthConvMsg(t, msgsURL, 0)
	contentURL, ok := msgURL["content"].([]any)
	if !ok || len(contentURL) != 1 {
		t.Fatalf("url image parts = %#v", msgURL["content"])
	}
	imgURLPart := contentURL[0].(map[string]any)["image_url"].(map[string]any)
	if got := anthConvStr(t, imgURLPart, "url"); got != "https://example.com/a.png" {
		t.Fatalf("url source = %q", got)
	}
}

func TestAnthConvRequest_AssistantToolUseToToolCalls(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":16,
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"sf","n":2}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}
		]
	}`)
	msgs := anthConvMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}

	assistant := anthConvMsg(t, msgs, 0)
	if anthConvStr(t, assistant, "role") != "assistant" {
		t.Fatalf("assistant role = %v", assistant["role"])
	}
	if got := anthConvStr(t, assistant, "content"); got != "calling" {
		t.Fatalf("assistant content = %q", got)
	}
	id, name, args := anthConvToolCall(t, assistant, 0)
	if id != "toolu_1" || name != "get_weather" {
		t.Fatalf("tool_call id/name = %q/%q", id, name)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("tool_call arguments is not valid JSON: %q (%v)", args, err)
	}
	if parsed["city"] != "sf" {
		t.Fatalf("arguments.city = %#v", parsed["city"])
	}

	toolMsg := anthConvMsg(t, msgs, 1)
	if anthConvStr(t, toolMsg, "role") != "tool" {
		t.Fatalf("tool result role = %v", toolMsg["role"])
	}
	if anthConvStr(t, toolMsg, "tool_call_id") != "toolu_1" {
		t.Fatalf("tool_call_id = %#v", toolMsg["tool_call_id"])
	}
	if got := anthConvStr(t, toolMsg, "content"); got != "sunny" {
		t.Fatalf("tool content = %q", got)
	}
}

func TestAnthConvRequest_UserToolResultsBeforeTextMessage(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"r1"},
			{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"r2a"},{"type":"text","text":"r2b"}]},
			{"type":"text","text":"and also"}
		]}]
	}`)
	msgs := anthConvMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (2×tool + 1×user)", len(msgs))
	}
	first := anthConvMsg(t, msgs, 0)
	if anthConvStr(t, first, "role") != "tool" || anthConvStr(t, first, "tool_call_id") != "t1" ||
		anthConvStr(t, first, "content") != "r1" {
		t.Fatalf("messages[0] = %#v", first)
	}
	second := anthConvMsg(t, msgs, 1)
	if anthConvStr(t, second, "role") != "tool" || anthConvStr(t, second, "tool_call_id") != "t2" {
		t.Fatalf("messages[1] = %#v", second)
	}
	if got := anthConvStr(t, second, "content"); got != "r2a\nr2b" {
		t.Fatalf("messages[1].content = %q, want %q", got, "r2a\nr2b")
	}
	third := anthConvMsg(t, msgs, 2)
	if anthConvStr(t, third, "role") != "user" || anthConvStr(t, third, "content") != "and also" {
		t.Fatalf("messages[2] = %#v", third)
	}
}

func TestAnthConvRequest_ToolsMapping(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":8,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"get_weather","description":"look up weather",
				"input_schema":{"type":"object","properties":{"city":{"type":"string"}}},
				"cache_control":{"type":"ephemeral"}},
			{"name":"no_schema"}
		]
	}`)
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want 2 items", out["tools"])
	}
	first := tools[0].(map[string]any)
	if anthConvStr(t, first, "type") != "function" {
		t.Fatalf("tools[0].type = %#v", first["type"])
	}
	fn := first["function"].(map[string]any)
	if anthConvStr(t, fn, "name") != "get_weather" || anthConvStr(t, fn, "description") != "look up weather" {
		t.Fatalf("tools[0].function = %#v", fn)
	}
	schema, ok := fn["parameters"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("tools[0].function.parameters = %#v", fn["parameters"])
	}
	if _, leaked := fn["cache_control"]; leaked {
		t.Fatalf("tools[0].function must not carry cache_control: %#v", fn)
	}
	second := tools[1].(map[string]any)["function"].(map[string]any)
	if got := second["parameters"].(map[string]any)["type"]; got != "object" {
		t.Fatalf("missing input_schema should fall back to object, got %#v", got)
	}
}

func TestAnthConvRequest_ToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		// want 为 nil 表示输出不应包含 tool_choice
		want     string // "auto" / "required" / "none" / "function" / ""
		wantName string
	}{
		{name: "auto", body: `{"type":"auto"}`, want: "auto"},
		{name: "any→required", body: `{"type":"any"}`, want: "required"},
		{name: "none", body: `{"type":"none"}`, want: "none"},
		{
			name:     "tool→function",
			body:     `{"type":"tool","name":"get_weather"}`,
			want:     "function",
			wantName: "get_weather",
		},
		{name: "unknown ignored", body: `{"type":"whatever"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc.body + `}`
			out := anthConvRequest(t, body)
			got, exists := out["tool_choice"]
			if tc.want == "" {
				if exists {
					t.Fatalf("tool_choice should be ignored, got %#v", got)
				}
				return
			}
			if !exists {
				t.Fatalf("tool_choice missing")
			}
			if tc.want == "function" {
				m, ok := got.(map[string]any)
				if !ok {
					t.Fatalf("tool_choice = %#v, want object", got)
				}
				if anthConvStr(t, m, "type") != "function" {
					t.Fatalf("tool_choice.type = %#v", m["type"])
				}
				fn := m["function"].(map[string]any)
				if anthConvStr(t, fn, "name") != tc.wantName {
					t.Fatalf("tool_choice.function.name = %#v", fn["name"])
				}
				return
			}
			if s, ok := got.(string); !ok || s != tc.want {
				t.Fatalf("tool_choice = %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestAnthConvRequest_StopSequencesToStop(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":8,"stop_sequences":["STOP","END"],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	anthConvWantStrings(t, out["stop"], []string{"STOP", "END"}, "stop")

	// 空数组不下发 stop（避免上游把空数组视作非法）。
	empty := anthConvRequest(
		t,
		`{"model":"m","max_tokens":8,"stop_sequences":[],"messages":[{"role":"user","content":"hi"}]}`,
	)
	if _, exists := empty["stop"]; exists {
		t.Fatalf("empty stop_sequences must not produce a stop field: %#v", empty["stop"])
	}
}

func TestAnthConvRequest_Validation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantWord string
	}{
		{name: "missing model", body: `{"max_tokens":8,"messages":[]}`, wantWord: "model"},
		{name: "blank model", body: `{"model":"  ","max_tokens":8,"messages":[]}`, wantWord: "model"},
		{name: "missing max_tokens", body: `{"model":"m","messages":[]}`, wantWord: "max_tokens"},
		{name: "zero max_tokens", body: `{"model":"m","max_tokens":0,"messages":[]}`, wantWord: "max_tokens"},
		{name: "negative max_tokens", body: `{"model":"m","max_tokens":-3,"messages":[]}`, wantWord: "max_tokens"},
		{name: "bad json", body: `{`, wantWord: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := anthropicRequestToOpenAI([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantWord != "" && !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error message %q does not contain %q", err.Error(), tc.wantWord)
			}
		})
	}
}

func TestAnthConvRequest_UnknownFieldsDropped(t *testing.T) {
	out := anthConvRequest(t, `{
		"model":"m","max_tokens":8,
		"top_k":40,
		"metadata":{"user_id":"u1","custom_ext":{"a":1}},
		"thinking":{"type":"enabled","budget_tokens":1024},
		"service_tier":"standard",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}},
			{"type":"thinking","thinking":"..."},
			{"type":"unknown_future_block","x":1}
		]}]
	}`)
	for _, key := range []string{"top_k", "metadata", "thinking", "service_tier"} {
		if _, exists := out[key]; exists {
			t.Fatalf("anthropic-only/unknown field %q must not appear in the OpenAI request: %#v", key, out[key])
		}
	}
	msgs := anthConvMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	if got := anthConvStr(t, anthConvMsg(t, msgs, 0), "content"); got != "hi" {
		t.Fatalf("content = %q, want \"hi\" (thinking/unknown blocks should be dropped)", got)
	}
	if _, exists := msgs[0].(map[string]any)["cache_control"]; exists {
		t.Fatalf("message-level cache_control must not appear")
	}
}

// ---------------------------------------------------------------------------
// 响应方向
// ---------------------------------------------------------------------------

func TestAnthConvResponse_TextResponse(t *testing.T) {
	out := anthConvResponse(t, `{
		"id":"chatcmpl-1","object":"chat.completion",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
	}`, "claude-x")

	id := anthConvStr(t, out, "id")
	if !strings.HasPrefix(id, "msg_") || len(id) <= len("msg_") {
		t.Fatalf("id = %q, want msg_ prefix and non-empty", id)
	}
	if anthConvStr(t, out, "type") != "message" || anthConvStr(t, out, "role") != "assistant" {
		t.Fatalf("type/role = %#v/%#v", out["type"], out["role"])
	}
	if anthConvStr(t, out, "model") != "claude-x" {
		t.Fatalf("model = %#v, want the client-requested model name", out["model"])
	}
	if out["stop_sequence"] != nil {
		t.Fatalf("stop_sequence = %#v, want null", out["stop_sequence"])
	}
	if anthConvStr(t, out, "stop_reason") != "end_turn" {
		t.Fatalf("stop_reason = %#v", out["stop_reason"])
	}

	content, ok := out["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want a single text block", out["content"])
	}
	block := content[0].(map[string]any)
	if anthConvStr(t, block, "type") != "text" || anthConvStr(t, block, "text") != "hello" {
		t.Fatalf("content[0] = %#v", block)
	}

	usage := out["usage"].(map[string]any)
	anthConvWantInt(t, usage["input_tokens"], 11, "usage.input_tokens")
	anthConvWantInt(t, usage["output_tokens"], 7, "usage.output_tokens")
	if _, exists := usage["cache_read_input_tokens"]; exists {
		t.Fatalf("cache_read_input_tokens must be omitted without prompt_tokens_details: %#v", usage)
	}
}

func TestAnthConvResponse_ToolCalls(t *testing.T) {
	out := anthConvResponse(t, `{
		"choices":[{"index":0,"message":{
			"role":"assistant","content":"let me check",
			"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"sf\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"not-json"}}
			]
		},"finish_reason":"tool_calls"}]
	}`, "claude-x")

	if anthConvStr(t, out, "stop_reason") != "tool_use" {
		t.Fatalf("stop_reason = %#v, want tool_use", out["stop_reason"])
	}
	content, ok := out["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("content len = %d, want 3 (text + 2×tool_use)", len(content))
	}
	// 顺序：text 在前，tool_use 在后。
	if anthConvStr(t, content[0].(map[string]any), "type") != "text" {
		t.Fatalf("content[0] should be text: %#v", content[0])
	}
	first := content[1].(map[string]any)
	if anthConvStr(t, first, "type") != "tool_use" || anthConvStr(t, first, "id") != "call_1" ||
		anthConvStr(t, first, "name") != "get_weather" {
		t.Fatalf("content[1] = %#v", first)
	}
	input, ok := first["input"].(map[string]any)
	if !ok || input["city"] != "sf" {
		t.Fatalf("tool_use.input = %#v", first["input"])
	}
	// 非法 arguments → 空对象。
	second := content[2].(map[string]any)
	input2, ok := second["input"].(map[string]any)
	if !ok || len(input2) != 0 {
		t.Fatalf("invalid arguments should fall back to {}, got %#v", second["input"])
	}
}

func TestAnthConvResponse_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{reason: "stop", want: "end_turn"},
		{reason: "length", want: "max_tokens"},
		{reason: "tool_calls", want: "tool_use"},
		{reason: "content_filter", want: "end_turn"},
		{reason: "", want: "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if got := finishReasonToStopReason(tc.reason); got != tc.want {
				t.Fatalf("finishReasonToStopReason(%q) = %q, want %q", tc.reason, got, tc.want)
			}
			body := `{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":` + jsonQuote(
				tc.reason,
			) + `}]}`
			out := anthConvResponse(t, body, "m")
			if got := anthConvStr(t, out, "stop_reason"); got != tc.want {
				t.Fatalf("response stop_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnthConvResponse_UsageCachedTokens(t *testing.T) {
	out := anthConvResponse(t, `{
		"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,
			"prompt_tokens_details":{"cached_tokens":64}}
	}`, "m")
	usage := out["usage"].(map[string]any)
	anthConvWantInt(t, usage["input_tokens"], 100, "usage.input_tokens")
	anthConvWantInt(t, usage["output_tokens"], 5, "usage.output_tokens")
	anthConvWantInt(t, usage["cache_read_input_tokens"], 64, "usage.cache_read_input_tokens")

	// cached_tokens 显式为 0 时也应输出（保持口径一致）。
	out0 := anthConvResponse(t, `{
		"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}
	}`, "m")
	usage0 := out0["usage"].(map[string]any)
	anthConvWantInt(t, usage0["cache_read_input_tokens"], 0, "usage.cache_read_input_tokens")
}

func TestAnthConvResponse_ToolUseOnlyWithoutText(t *testing.T) {
	out := anthConvResponse(t, `{
		"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":""}}
		]},"finish_reason":"tool_calls"}]
	}`, "m")
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1 (empty content yields no text block)", len(content))
	}
	block := content[0].(map[string]any)
	if anthConvStr(t, block, "type") != "tool_use" {
		t.Fatalf("content[0] = %#v", block)
	}
	if input, ok := block["input"].(map[string]any); !ok || len(input) != 0 {
		t.Fatalf("empty arguments should yield {}, got %#v", block["input"])
	}
}

func TestAnthConvResponse_PartsContent(t *testing.T) {
	out := anthConvResponse(t, `{
		"choices":[{"message":{"role":"assistant","content":[
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		]},"finish_reason":"stop"}]
	}`, "m")
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	if got := anthConvStr(t, content[0].(map[string]any), "text"); got != "ab" {
		t.Fatalf("concatenated parts text = %q, want %q", got, "ab")
	}
}

func TestAnthConvResponse_InvalidUpstream(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `hello`},
		{name: "no choices", body: `{"id":"x"}`},
		{name: "empty choices", body: `{"choices":[]}`},
		{name: "choices not array", body: `{"choices":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := openAIResponseToAnthropic([]byte(tc.body), "m"); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestNewAnthropicMessageID(t *testing.T) {
	a := newAnthropicMessageID()
	b := newAnthropicMessageID()
	if !strings.HasPrefix(a, "msg_") || len(a) <= len("msg_") {
		t.Fatalf("id = %q, want msg_ prefix and non-empty", a)
	}
	if a == b {
		t.Fatalf("two generated IDs must differ: %q", a)
	}
}

// jsonQuote 把字符串编码为 JSON 字面量（用于拼接测试请求体）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
