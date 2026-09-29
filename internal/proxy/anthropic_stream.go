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

// 本文件实现「OpenAI 兼容上游 SSE 增量 → Anthropic 事件序列」的流式翻译器。
//
// 输出事件序列（design D4）：
//   message_start → content_block_start → content_block_delta* → content_block_stop
//   → message_delta（stop_reason + output_tokens）→ message_stop
//
// 翻译器必须容忍上游任意字节切分：内部缓冲不完整的事件，只处理以空行结束的完整事件。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicStreamTranslator 把 OpenAI 格式 SSE 增量翻译为 Anthropic 事件序列并写入目标 writer。
type anthropicStreamTranslator struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	msgID   string

	// buf 缓存尚未构成完整事件（未遇到空行边界）的字节。
	buf []byte

	// wrote 标记是否已向客户端写出过任何字节（供调用方判断「未写出即失败 → 502」）。
	wrote bool

	// started 标记 message_start 是否已发出；closed 标记收尾是否已完成。
	started bool
	closed  bool

	// 当前打开的 content block 状态。
	blockOpen bool
	curIsTool bool
	curIndex  int
	nextIndex int

	// 当前 tool_use 块对应的上游 tool_calls index（用于识别同块 / 新块）。
	curToolUpstreamIndex int

	// 收尾用的结束原因与输出 token 数。
	finishReason string
	outputTokens int
}

// newAnthropicStreamTranslator 创建流式翻译器；model 为客户端请求的模型名（写入 message_start）。
func newAnthropicStreamTranslator(
	w http.ResponseWriter,
	flusher http.Flusher,
	model string,
) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{
		w:                    w,
		flusher:              flusher,
		model:                model,
		msgID:                newAnthropicMessageID(),
		curToolUpstreamIndex: -1,
		nextIndex:            0,
	}
}

// Write 接收上游（OpenAI 格式）SSE 字节；内部按事件边界解析并翻译输出。始终返回 (len(p), nil)。
func (t *anthropicStreamTranslator) Write(p []byte) (int, error) {
	// 已收尾：忽略后续输入（design D4「之后再来的输入忽略」）。
	if t.closed {
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	for !t.closed {
		end, delimLen := anthropicStreamEventBoundary(t.buf)
		if end < 0 {
			break
		}
		block := string(t.buf[:end])
		t.buf = t.buf[end+delimLen:]
		t.processEvent(block)
	}
	if t.closed {
		t.buf = nil
	}
	return len(p), nil
}

// Wrote 返回是否已向客户端写出过任何字节（调用方用于「未写出即失败 → 502」判断）。
func (t *anthropicStreamTranslator) Wrote() bool { return t.wrote }

// Finish 幂等收尾：若已开始且未结束，补发 content_block_stop / message_delta / message_stop。
func (t *anthropicStreamTranslator) Finish() { t.finalize() }

// processEvent 处理一个完整 SSE 事件块（多行 data: 会被拼接）。
func (t *anthropicStreamTranslator) processEvent(block string) {
	data := anthropicSSEDataOf(block)
	if data == "" {
		return
	}
	if data == "[DONE]" {
		t.finalize()
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// 上游分片无法解析：容忍并丢弃（不中断整条流）。
		return
	}
	t.handleChunk(chunk)
}

// handleChunk 处理单个 OpenAI 流式分片：提取文本增量、tool_calls 增量与 finish_reason。
func (t *anthropicStreamTranslator) handleChunk(chunk map[string]any) {
	// 用量可能出现在任意分片（OpenAI stream_options.include_usage 的末片）。
	if usage, ok := chunk["usage"].(map[string]any); ok {
		if n, ok := anthJSONInt(usage["completion_tokens"]); ok {
			t.outputTokens = n
		}
	}

	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}

	text := ""
	var toolCalls []any
	if delta, ok := choice["delta"].(map[string]any); ok {
		text, _ = delta["content"].(string)
		toolCalls, _ = delta["tool_calls"].([]any)
	}

	// 仅在真正有增量输出（文本或工具调用）时才发 message_start，避免空流误发。
	if text != "" || len(toolCalls) > 0 {
		t.ensureStarted()
		if text != "" {
			t.ensureTextBlock()
			t.emit("content_block_delta", map[string]any{
				"index": t.curIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		}
		for _, raw := range toolCalls {
			t.handleToolCallDelta(raw)
		}
	}

	if t.closed {
		return
	}
	if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
		// 只记录不立即收尾：OpenAI 的 stream_options.include_usage 会在 finish_reason
		// 之后追加一个 choices 为空、仅含 usage 的收尾分片；提前收尾会整片丢弃它，
		// 导致 message_delta 的 output_tokens 恒为 0。真正的收尾交给 [DONE] 或 Finish()。
		t.finishReason = reason
	}
}

// handleToolCallDelta 处理一个 tool_calls 分片：新 index 开新块，同 index 追加参数分片。
func (t *anthropicStreamTranslator) handleToolCallDelta(raw any) {
	tc, ok := raw.(map[string]any)
	if !ok {
		return
	}
	upstreamIndex := 0
	if n, ok := anthJSONInt(tc["index"]); ok {
		upstreamIndex = n
	}
	id, _ := tc["id"].(string)
	name, args := "", ""
	if fn, ok := tc["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	}

	// 新 index（或当前不在工具块）→ 关闭当前块后开新的 tool_use 块。
	// 同块后续分片中迟到的 id/name 不做补发（首片即带全是常态）。
	if !t.blockOpen || !t.curIsTool || upstreamIndex != t.curToolUpstreamIndex {
		t.closeBlock()
		t.openToolBlock(upstreamIndex, id, name)
	}

	if args != "" {
		t.emit("content_block_delta", map[string]any{
			"index": t.curIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// ensureStarted 首次有增量输出时发出 message_start。
func (t *anthropicStreamTranslator) ensureStarted() {
	if t.started {
		return
	}
	t.started = true
	t.emit("message_start", map[string]any{
		"message": map[string]any{
			"id":            t.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// ensureTextBlock 确保存在打开的 text 块；若当前是工具块则先关闭再开新块。
func (t *anthropicStreamTranslator) ensureTextBlock() {
	if t.blockOpen && !t.curIsTool {
		return
	}
	t.closeBlock()
	t.curIndex = t.nextIndex
	t.nextIndex++
	t.blockOpen = true
	t.curIsTool = false
	t.emit("content_block_start", map[string]any{
		"index":         t.curIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// openToolBlock 打开一个 tool_use 块（id 缺失时生成 toolu_ 前缀随机 ID）。
func (t *anthropicStreamTranslator) openToolBlock(upstreamIndex int, id, name string) {
	if id == "" {
		id = anthToolIDPrefix + anthRandomSuffix(24)
	}
	t.curIndex = t.nextIndex
	t.nextIndex++
	t.blockOpen = true
	t.curIsTool = true
	t.curToolUpstreamIndex = upstreamIndex
	t.emit("content_block_start", map[string]any{
		"index": t.curIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

// closeBlock 关闭当前打开的 content block（无块时为空操作）。
func (t *anthropicStreamTranslator) closeBlock() {
	if !t.blockOpen {
		return
	}
	t.emit("content_block_stop", map[string]any{"index": t.curIndex})
	t.blockOpen = false
	t.curIsTool = false
}

// finalize 收尾：关闭当前块 → message_delta → message_stop。幂等。
// 上游 200 但从未产生增量输出（空流）时也补发最小合法事件序列，
// 避免客户端拿到「200 + 空 body」的非法 SSE 流。错误中断的场景由调用方保证不调用本函数。
func (t *anthropicStreamTranslator) finalize() {
	if t.closed {
		return
	}
	t.closed = true
	t.ensureStarted()
	t.closeBlock()
	t.emit("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   finishReasonToStopReason(t.finishReason),
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": t.outputTokens},
	})
	t.emit("message_stop", map[string]any{})
}

// emit 写出一个 Anthropic SSE 事件并立即 flush。
func (t *anthropicStreamTranslator) emit(eventType string, payload map[string]any) {
	payload["type"] = eventType
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", eventType, data)
	t.wrote = true
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// anthropicStreamEventBoundary 在 b 中查找首个事件边界（空行）。
// 返回边界起始下标与分隔符长度；未找到时返回 (-1, 0)。
func anthropicStreamEventBoundary(b []byte) (int, int) {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\n':
			if i+1 < len(b) && b[i+1] == '\n' {
				return i, 2
			}
		case '\r':
			if i+3 < len(b) && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
				return i, 4
			}
		}
	}
	return -1, 0
}

// anthropicSSEDataOf 提取一个事件块中的 data 载荷（多行 data: 以 \n 拼接）。
func anthropicSSEDataOf(block string) string {
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSuffix(line, "\r")
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		lines = append(lines, strings.TrimPrefix(rest, " "))
	}
	return strings.Join(lines, "\n")
}
