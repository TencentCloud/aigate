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

// 本文件实现 Anthropic Messages 协议与 OpenAI Chat Completions 协议之间的双向转换：
//   - anthropicRequestToOpenAI：Anthropic 入站请求 → OpenAI 兼容上游请求；
//   - openAIResponseToAnthropic：OpenAI 上游响应 → Anthropic 回包。
//
// 转换是有损的：Anthropic 专属字段（cache_control、thinking、top_k、metadata 等）
// 在转换路径上被丢弃；需要保真请走 Anthropic 原生直通路径（见 design D1）。

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

const (
	// anthMsgIDPrefix 是 Anthropic 消息 ID 前缀。
	anthMsgIDPrefix = "msg_"
	// anthToolIDPrefix 是 Anthropic 工具调用 ID 前缀。
	anthToolIDPrefix = "toolu_"
	// anthIDAlphabet 是随机 ID 的字符表（小写字母 + 数字，Anthropic 可接受）。
	anthIDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// --- 请求方向：Anthropic → OpenAI ---

// anthropicRequestToOpenAI 把 Anthropic Messages 请求体转换为 OpenAI chat completions 请求体。
// 校验：model 必填、max_tokens 必填且 > 0，缺失时返回错误（错误文案英文）。
func anthropicRequestToOpenAI(body []byte) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errors.New("invalid request body")
	}

	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("missing model")
	}
	maxTokens, ok := anthJSONInt(req["max_tokens"])
	if !ok {
		return nil, errors.New("missing max_tokens")
	}
	if maxTokens <= 0 {
		return nil, errors.New("invalid max_tokens")
	}

	out := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
	}

	// 直通字段：原样带过去（存在才写，避免改变无关字段语义）。
	for _, key := range []string{"stream", "temperature", "top_p"} {
		if v, exists := req[key]; exists {
			out[key] = v
		}
	}
	// stop_sequences → stop（空数组不下发，避免上游把空数组视作非法）。
	if stop, ok := req["stop_sequences"].([]any); ok && len(stop) > 0 {
		out["stop"] = stop
	}

	// system 消息置于最前（字符串直取；blocks 仅拼接 text 块，\n 分隔）。
	var messages []any
	if sys := anthropicSystemToText(req["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	if raw, ok := req["messages"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, anthropicMessageToOpenAI(m)...)
		}
	}
	if len(messages) > 0 {
		out["messages"] = messages
	}
	if tools := anthropicToolsToOpenAI(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := anthropicToolChoiceToOpenAI(req["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}

	return out, nil
}

// anthropicSystemToText 把 Anthropic system 字段（字符串或 blocks 数组）转为纯文本。
// blocks 形态下只取 text 块，其余块（如 image）忽略。
func anthropicSystemToText(v any) string {
	switch sys := v.(type) {
	case string:
		return sys
	case []any:
		var parts []string
		for _, item := range sys {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if blockType, _ := m["type"].(string); blockType != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// anthropicMessageToOpenAI 转换单条 Anthropic 消息，返回 0 条或多条 OpenAI 消息。
// user 消息中的 tool_result 会额外拆出独立的 role=tool 消息（排在文本消息之前）。
func anthropicMessageToOpenAI(m map[string]any) []any {
	role, _ := m["role"].(string)
	switch role {
	case "assistant":
		return []any{anthropicAssistantMessageToOpenAI(m["content"])}
	case "user":
		return anthropicUserMessagesToOpenAI(m["content"])
	default:
		// 未知角色：保留原样，仅做 content 形态转换（丢弃工具类块）。
		content := ""
		if s, ok := m["content"].(string); ok {
			content = s
		} else if blocks, ok := m["content"].([]any); ok {
			_, content, _ = anthropicTextAndImageParts(blocks)
		}
		return []any{map[string]any{"role": role, "content": content}}
	}
}

// anthropicAssistantMessageToOpenAI 转换 assistant 消息：text 块作为 content，
// tool_use 块收集为 OpenAI tool_calls。
func anthropicAssistantMessageToOpenAI(content any) map[string]any {
	if s, ok := content.(string); ok {
		return map[string]any{"role": "assistant", "content": s}
	}
	blocks, _ := content.([]any)
	parts, text, hasImage := anthropicTextAndImageParts(blocks)

	msg := map[string]any{"role": "assistant"}
	if hasImage {
		msg["content"] = parts
	} else {
		msg["content"] = text
	}

	var toolCalls []any
	for _, item := range blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := b["type"].(string); blockType != "tool_use" {
			continue
		}
		id, _ := b["id"].(string)
		name, _ := b["name"].(string)
		args := "{}"
		if input, exists := b["input"]; exists && input != nil {
			if raw, err := json.Marshal(input); err == nil {
				args = string(raw)
			}
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		})
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// anthropicUserMessagesToOpenAI 转换 user 消息：
//   - 每个 tool_result 块 → 一条 role=tool 消息（tool_call_id 对应 tool_use_id）；
//   - 其余 text/image 块 → 一条 user 消息（若存在）。
func anthropicUserMessagesToOpenAI(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": "user", "content": s}}
	}
	blocks, _ := content.([]any)

	var out []any
	for _, item := range blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := b["type"].(string); blockType != "tool_result" {
			continue
		}
		toolUseID, _ := b["tool_use_id"].(string)
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": toolUseID,
			"content":      anthropicToolResultToString(b["content"]),
		})
	}

	parts, text, hasImage := anthropicTextAndImageParts(blocks)
	if len(parts) > 0 {
		var userContent any = text
		if hasImage {
			userContent = parts
		}
		out = append(out, map[string]any{"role": "user", "content": userContent})
	}
	return out
}

// anthropicTextAndImageParts 从 content blocks 中提取 text / image 两类块。
// 返回 OpenAI 形态的 parts 数组、text 拼接串（\n 分隔）以及是否含 image 块。
func anthropicTextAndImageParts(blocks []any) (parts []any, text string, hasImage bool) {
	var texts []string
	for _, item := range blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch blockType, _ := b["type"].(string); blockType {
		case "text":
			t, _ := b["text"].(string)
			parts = append(parts, map[string]any{"type": "text", "text": t})
			texts = append(texts, t)
		case "image":
			if part := anthropicImageToOpenAIPart(b); part != nil {
				parts = append(parts, part)
				hasImage = true
			}
		}
	}
	return parts, strings.Join(texts, "\n"), hasImage
}

// anthropicImageToOpenAIPart 把 Anthropic image 块转为 OpenAI image_url part。
// base64 source 拼成 data URI；url source 直接透传；其余形态返回 nil（丢弃）。
func anthropicImageToOpenAIPart(b map[string]any) map[string]any {
	source, ok := b["source"].(map[string]any)
	if !ok {
		return nil
	}
	var url string
	switch sourceType, _ := source["type"].(string); sourceType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if data == "" {
			return nil
		}
		url = "data:" + mediaType + ";base64," + data
	case "url":
		url, _ = source["url"].(string)
		if url == "" {
			return nil
		}
	default:
		return nil
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url},
	}
}

// anthropicToolResultToString 把 tool_result 的 content 字符串化：
// 字符串原样；blocks 数组优先拼接 text 块；其余形态退化为 JSON 序列化。
func anthropicToolResultToString(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			b, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if blockType, _ := b["type"].(string); blockType != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		return anthropicJSONString(c)
	default:
		return anthropicJSONString(c)
	}
}

// anthropicToolsToOpenAI 把 Anthropic tools 转为 OpenAI function 工具数组。
// Anthropic 的 cache_control 等扩展字段被丢弃。
func anthropicToolsToOpenAI(v any) []any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range items {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc, ok := t["description"].(string); ok && desc != "" {
			fn["description"] = desc
		}
		if schema, exists := t["input_schema"]; exists && schema != nil {
			fn["parameters"] = schema
		} else {
			fn["parameters"] = map[string]any{"type": "object"}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicToolChoiceToOpenAI 映射 Anthropic tool_choice 到 OpenAI 形态。
// auto→"auto"；any→"required"；none→"none"；tool→指定 function；其余忽略（返回 nil）。
func anthropicToolChoiceToOpenAI(v any) any {
	tc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch tcType, _ := tc["type"].(string); tcType {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := tc["name"].(string)
		if name == "" {
			return nil
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}
	default:
		return nil
	}
}

// --- 响应方向：OpenAI → Anthropic ---

// openAIResponseToAnthropic 把 OpenAI chat completion 响应体转换为 Anthropic Messages 响应体。
// requestedModel 是客户端请求的模型名（写入响应的 model 字段）。
func openAIResponseToAnthropic(body []byte, requestedModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errors.New("invalid upstream response")
	}
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, errors.New("invalid upstream response")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, errors.New("invalid upstream response")
	}

	finishReason, _ := choice["finish_reason"].(string)

	// content 块顺序：text 在前、tool_use 在后。
	content := []any{}
	message, _ := choice["message"].(map[string]any)
	if message != nil {
		if text := openAIMessageText(message["content"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		if toolCalls, ok := message["tool_calls"].([]any); ok {
			for _, item := range toolCalls {
				tc, ok := item.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tc["id"].(string)
				name, args := "", ""
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
					args, _ = fn["arguments"].(string)
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": openAIToolArgumentsToObject(args),
				})
			}
		}
	}

	out := map[string]any{
		"id":            newAnthropicMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         requestedModel,
		"content":       content,
		"stop_reason":   finishReasonToStopReason(finishReason),
		"stop_sequence": nil,
		"usage":         openAIUsageToAnthropic(resp["usage"]),
	}
	return json.Marshal(out)
}

// openAIMessageText 从 OpenAI message.content 提取文本（字符串或 parts 数组）。
func openAIMessageText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

// openAIToolArgumentsToObject 把 tool_calls.function.arguments 解析为输入对象；
// 空串或解析失败（含解析结果非对象）时返回空对象。
func openAIToolArgumentsToObject(args string) map[string]any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return map[string]any{}
	}
	if obj, ok := parsed.(map[string]any); ok {
		return obj
	}
	return map[string]any{}
}

// openAIUsageToAnthropic 映射用量：input_tokens=prompt_tokens、output_tokens=completion_tokens、
// cache_read_input_tokens=prompt_tokens_details.cached_tokens（该字段存在时才输出）。
func openAIUsageToAnthropic(v any) map[string]any {
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	u, ok := v.(map[string]any)
	if !ok {
		return usage
	}
	if n, ok := anthJSONInt(u["prompt_tokens"]); ok {
		usage["input_tokens"] = n
	}
	if n, ok := anthJSONInt(u["completion_tokens"]); ok {
		usage["output_tokens"] = n
	}
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := anthJSONInt(details["cached_tokens"]); ok {
			usage["cache_read_input_tokens"] = n
		}
	}
	return usage
}

// --- 公共工具 ---

// newAnthropicMessageID 生成 "msg_" 前缀的随机消息 ID。
func newAnthropicMessageID() string {
	return anthMsgIDPrefix + anthRandomSuffix(24)
}

// finishReasonToStopReason 映射结束原因：
// stop→end_turn；length→max_tokens；tool_calls→tool_use；其余（含空）→end_turn。
func finishReasonToStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthRandomSuffix 生成 n 个字符的随机后缀（crypto/rand 不可用时退化为全 0，绝不 panic）。
func anthRandomSuffix(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = '0'
		}
		return string(buf)
	}
	for i, b := range buf {
		buf[i] = anthIDAlphabet[int(b)%len(anthIDAlphabet)]
	}
	return string(buf)
}

// anthJSONInt 把 JSON 解码后的数值（float64/int/json.Number）转 int。
// 非整数、NaN/Inf 或超出 int32 范围的值一律返回 ok=false：
// 避免 float→int 转换的架构相关行为（越界值在 amd64 上会得到意外结果）。
func anthJSONInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return 0, false
		}
		if n > math.MaxInt32 || n < math.MinInt32 {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		if n > math.MaxInt32 || n < math.MinInt32 {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		if i > math.MaxInt32 || i < math.MinInt32 {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// anthropicJSONString 尽力把任意值序列化为 JSON 字符串，失败返回空串。
func anthropicJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
