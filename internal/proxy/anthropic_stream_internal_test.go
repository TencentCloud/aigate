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

// 白盒单测：OpenAI SSE → Anthropic 事件序列（anthropic_stream.go）。
// 断言基于「事件名顺序 + 事件载荷字段」，不逐字节比对 golden。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// anthStreamNew 构造被测翻译器；httptest.ResponseRecorder 自带 http.Flusher。
func anthStreamNew(t *testing.T, model string) (*anthropicStreamTranslator, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	return newAnthropicStreamTranslator(rec, rec, model), rec
}

// anthStreamSSE 组装一个 OpenAI 形态的 SSE 事件（data 行 + 空行结束）。
func anthStreamSSE(data string) string { return "data: " + data + "\n\n" }

// anthStreamFeed 按 size 字节一段喂入，模拟上游任意切分（含在事件中间切断）。
func anthStreamFeed(t *testing.T, tr *anthropicStreamTranslator, payload string, size int) {
	t.Helper()
	if size <= 0 {
		size = 1
	}
	for i := 0; i < len(payload); i += size {
		end := i + size
		if end > len(payload) {
			end = len(payload)
		}
		p := []byte(payload[i:end])
		n, err := tr.Write(p)
		if err != nil {
			t.Fatalf("Write returned an error: %v", err)
		}
		if n != len(p) {
			t.Fatalf("Write returned n = %d, want %d", n, len(p))
		}
	}
}

// anthStreamEvents 按出现顺序返回全部事件 (name, data)。
func anthStreamEvents(t *testing.T, raw string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				name = strings.TrimSpace(rest)
				continue
			}
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			}
		}
		if name == "" {
			t.Fatalf("output contains a block without an event line: %q", block)
		}
		out = append(out, [2]string{name, data})
	}
	return out
}

// anthStreamNames 返回事件名序列。
func anthStreamNames(events [][2]string) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev[0])
	}
	return names
}

// anthStreamWantNames 断言事件名序列完全相等。
func anthStreamWantNames(t *testing.T, events [][2]string, want []string) {
	t.Helper()
	got := anthStreamNames(events)
	if len(got) != len(want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", got, want)
		}
	}
}

// anthStreamDataOf 解析指定第 i 个同类型事件的 data 载荷为 map。
func anthStreamDataOf(t *testing.T, events [][2]string, eventName string, i int) map[string]any {
	t.Helper()
	seen := 0
	for _, ev := range events {
		if ev[0] != eventName {
			continue
		}
		if seen == i {
			var m map[string]any
			if err := json.Unmarshal([]byte(ev[1]), &m); err != nil {
				t.Fatalf("failed to parse %s data: %v (%s)", eventName, err, ev[1])
			}
			return m
		}
		seen++
	}
	t.Fatalf("output has no %s event at index %d", eventName, i)
	return nil
}

// anthStreamCount 统计指定事件名出现次数。
func anthStreamCount(events [][2]string, eventName string) int {
	n := 0
	for _, ev := range events {
		if ev[0] == eventName {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 纯文本流
// ---------------------------------------------------------------------------

func TestAnthStream_TextStreamEventSequence(t *testing.T) {
	// 逐字节喂入：每个事件都会被切在中间，验证内部缓冲。
	sizes := []int{1, 3, 7, 64}
	for _, size := range sizes {
		t.Run("chunk_size", func(t *testing.T) {
			tr, rec := anthStreamNew(t, "mock-claude")
			// 首片只有 role、无增量 → 不应触发 message_start。
			payload := strings.Join([]string{
				anthStreamSSE(
					`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				),
				anthStreamSSE(`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`),
				anthStreamSSE(`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`),
				anthStreamSSE(
					`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":9,` +
						`"total_tokens":12}}`,
				),
				anthStreamSSE(`[DONE]`),
			}, "")

			anthStreamFeed(t, tr, payload, size)
			if !tr.Wrote() {
				t.Fatalf("Wrote() = false, want true (events already written)")
			}
			// 收尾幂等：重复 Finish 不应追加事件。
			tr.Finish()
			tr.Finish()

			raw := rec.Body.String()
			events := anthStreamEvents(t, raw)
			anthStreamWantNames(t, events, []string{
				"message_start",
				"content_block_start",
				"content_block_delta",
				"content_block_delta",
				"content_block_stop",
				"message_delta",
				"message_stop",
			})

			// message_start 载荷。
			start := anthStreamDataOf(t, events, "message_start", 0)
			msg, ok := start["message"].(map[string]any)
			if !ok {
				t.Fatalf("message_start.message = %#v", start["message"])
			}
			if id, _ := msg["id"].(string); !strings.HasPrefix(id, "msg_") {
				t.Fatalf("message.id = %#v, want msg_ prefix", msg["id"])
			}
			if msg["model"] != "mock-claude" || msg["role"] != "assistant" || msg["type"] != "message" {
				t.Fatalf("message metadata = %#v", msg)
			}
			if msg["stop_reason"] != nil {
				t.Fatalf("message_start.message.stop_reason = %#v, want null", msg["stop_reason"])
			}
			if content, ok := msg["content"].([]any); !ok || len(content) != 0 {
				t.Fatalf("message_start.message.content = %#v, want []", msg["content"])
			}
			startUsage := msg["usage"].(map[string]any)
			anthConvWantInt(t, startUsage["input_tokens"], 0, "message_start usage.input_tokens")
			anthConvWantInt(t, startUsage["output_tokens"], 0, "message_start usage.output_tokens")

			// content_block_start（text，index 0）。
			blockStart := anthStreamDataOf(t, events, "content_block_start", 0)
			anthConvWantInt(t, blockStart["index"], 0, "content_block_start.index")
			block := blockStart["content_block"].(map[string]any)
			if block["type"] != "text" || block["text"] != "" {
				t.Fatalf("content_block = %#v, want an empty text block", block)
			}

			// 两个 text_delta。
			texts := []string{}
			for i := 0; i < 2; i++ {
				d := anthStreamDataOf(t, events, "content_block_delta", i)
				anthConvWantInt(t, d["index"], 0, "content_block_delta.index")
				delta := d["delta"].(map[string]any)
				if delta["type"] != "text_delta" {
					t.Fatalf("delta.type = %#v, want text_delta", delta["type"])
				}
				texts = append(texts, delta["text"].(string))
			}
			if strings.Join(texts, "") != "Hello" {
				t.Fatalf("concatenated text_delta = %v, want [Hel lo]", texts)
			}

			// content_block_stop。
			blockStop := anthStreamDataOf(t, events, "content_block_stop", 0)
			anthConvWantInt(t, blockStop["index"], 0, "content_block_stop.index")

			// message_delta：stop_reason + output_tokens。
			md := anthStreamDataOf(t, events, "message_delta", 0)
			delta := md["delta"].(map[string]any)
			if delta["stop_reason"] != "end_turn" {
				t.Fatalf("message_delta.delta.stop_reason = %#v, want end_turn", delta["stop_reason"])
			}
			if delta["stop_sequence"] != nil {
				t.Fatalf("message_delta.delta.stop_sequence = %#v, want null", delta["stop_sequence"])
			}
			usage := md["usage"].(map[string]any)
			anthConvWantInt(t, usage["output_tokens"], 9, "message_delta usage.output_tokens")

			// message_stop 载荷含 type。
			stop := anthStreamDataOf(t, events, "message_stop", 0)
			if stop["type"] != "message_stop" {
				t.Fatalf("message_stop = %#v", stop)
			}

			// 幂等：message_delta / message_stop 各一次，且以 message_stop 结束。
			if n := anthStreamCount(events, "message_delta"); n != 1 {
				t.Fatalf("message_delta count = %d, want 1", n)
			}
			if n := anthStreamCount(events, "message_stop"); n != 1 {
				t.Fatalf("message_stop count = %d, want 1", n)
			}
			if events[len(events)-1][0] != "message_stop" {
				t.Fatalf("last event = %s, want message_stop", events[len(events)-1][0])
			}
		})
	}
}

func TestAnthStream_TextStreamFinishWithoutDone(t *testing.T) {
	// 上游未给 [DONE]：Finish() 补发收尾。
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, anthStreamSSE(`{"choices":[{"delta":{"content":"a"}}]}`), 4)
	tr.Finish()
	tr.Finish() // 幂等

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
	if got := anthStreamDataOf(t, events, "message_delta", 0)["delta"].(map[string]any)["stop_reason"]; got != "end_turn" {
		t.Fatalf("stop_reason without finish_reason = %#v, want end_turn", got)
	}
}

// ---------------------------------------------------------------------------
// 工具调用流
// ---------------------------------------------------------------------------

func TestAnthStream_ToolCallStream(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather"` +
				`,"arguments":""}}]},"finish_reason":null}]}`,
		),
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		),
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]},"finish_reason":null}]}`,
		),
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
		anthStreamSSE(`[DONE]`),
	}, "")

	for _, size := range []int{1, 5, 4096} {
		t.Run("chunk_size", func(t *testing.T) {
			tr, rec := anthStreamNew(t, "m")
			anthStreamFeed(t, tr, payload, size)
			tr.Finish()

			events := anthStreamEvents(t, rec.Body.String())
			anthStreamWantNames(t, events, []string{
				"message_start",
				"content_block_start",
				"content_block_delta",
				"content_block_delta",
				"content_block_stop",
				"message_delta",
				"message_stop",
			})

			// tool_use 块的 content_block_start 必须带 id/name/input。
			blockStart := anthStreamDataOf(t, events, "content_block_start", 0)
			block := blockStart["content_block"].(map[string]any)
			if block["type"] != "tool_use" {
				t.Fatalf("content_block.type = %#v, want tool_use", block["type"])
			}
			if block["id"] != "call_1" || block["name"] != "get_weather" {
				t.Fatalf("tool_use block = %#v, want id=call_1 name=get_weather", block)
			}
			if input, ok := block["input"].(map[string]any); !ok || len(input) != 0 {
				t.Fatalf("tool_use.input = %#v, want {}", block["input"])
			}

			// 两个 input_json_delta 分片。
			fragments := []string{}
			for i := 0; i < 2; i++ {
				d := anthStreamDataOf(t, events, "content_block_delta", i)
				delta := d["delta"].(map[string]any)
				if delta["type"] != "input_json_delta" {
					t.Fatalf("delta.type = %#v, want input_json_delta", delta["type"])
				}
				fragments = append(fragments, delta["partial_json"].(string))
			}
			if strings.Join(fragments, "") != `{"city":"sf"}` {
				t.Fatalf("concatenated partial_json = %v", fragments)
			}

			// 收尾 stop_reason=tool_use。
			stopReason := anthStreamDataOf(t, events, "message_delta", 0)["delta"].(map[string]any)["stop_reason"]
			if stopReason != "tool_use" {
				t.Fatalf("stop_reason = %#v, want tool_use", stopReason)
			}
		})
	}
}

func TestAnthStream_TextThenToolBlocksIndexIncrement(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(`{"choices":[{"delta":{"content":"thinking"},"finish_reason":null}]}`),
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
				`"finish_reason":null}]}`,
		),
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, payload, 11)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start",
		"content_block_start", // text，index 0
		"content_block_delta", // text_delta
		"content_block_stop",  // 关闭 text 块
		"content_block_start", // tool_use，index 1
		"content_block_delta", // input_json_delta
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	firstStart := anthStreamDataOf(t, events, "content_block_start", 0)
	anthConvWantInt(t, firstStart["index"], 0, "text block index")
	secondStart := anthStreamDataOf(t, events, "content_block_start", 1)
	anthConvWantInt(t, secondStart["index"], 1, "tool_use block index")

	// text 块的 content_block_stop 必须紧邻 tool_use 块 start 之前。
	stopBeforeTool := anthStreamDataOf(t, events, "content_block_stop", 0)
	anthConvWantInt(t, stopBeforeTool["index"], 0, "closing text block index")
}

func TestAnthStream_MultipleToolCallsParallel(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"f0","arguments":"{}"}}]},` +
				`"finish_reason":null}]}`,
		),
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"f1","arguments":"{}"}}]},` +
				`"finish_reason":null}]}`,
		),
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, payload, 9)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	})
	for i := 0; i < 2; i++ {
		start := anthStreamDataOf(t, events, "content_block_start", i)
		anthConvWantInt(t, start["index"], i, "tool_use block index")
		block := start["content_block"].(map[string]any)
		if block["id"] != "c"+string(rune('0'+i)) {
			t.Fatalf("tool_use[%d].id = %#v", i, block["id"])
		}
	}
}

func TestAnthStream_ToolCallMissingIDGeneratesTooluPrefix(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`,
		),
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, payload, 3)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	block := anthStreamDataOf(t, events, "content_block_start", 0)["content_block"].(map[string]any)
	id, _ := block["id"].(string)
	if !strings.HasPrefix(id, "toolu_") || len(id) <= len("toolu_") {
		t.Fatalf("missing id should generate a toolu_ prefixed ID, got %#v", block["id"])
	}
}

// ---------------------------------------------------------------------------
// 收尾触发与幂等
// ---------------------------------------------------------------------------

func TestAnthStream_FinishReasonBeforeDoneWins(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(`{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`),
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"length"}]}`),
		anthStreamSSE(`[DONE]`),
	}, "")
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, payload, 2)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	if n := anthStreamCount(events, "message_delta"); n != 1 {
		t.Fatalf("message_delta count = %d, want 1", n)
	}
	if n := anthStreamCount(events, "message_stop"); n != 1 {
		t.Fatalf("message_stop count = %d, want 1", n)
	}
	delta := anthStreamDataOf(t, events, "message_delta", 0)["delta"].(map[string]any)
	if got := delta["stop_reason"]; got != "max_tokens" {
		t.Fatalf("stop_reason = %#v, want max_tokens", got)
	}
}

func TestAnthStream_DoneBeforeFinishReasonWins(t *testing.T) {
	payload := strings.Join([]string{
		anthStreamSSE(`{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`),
		anthStreamSSE(`[DONE]`),
		// [DONE] 之后的分片必须被忽略。
		anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"length"}]}`),
	}, "")
	tr, rec := anthStreamNew(t, "m")
	anthStreamFeed(t, tr, payload, 6)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
	if got := anthStreamDataOf(t, events, "message_delta", 0)["delta"].(map[string]any)["stop_reason"]; got != "end_turn" {
		t.Fatalf("stop_reason = %#v, want end_turn ([DONE] arrived first, later finish_reason ignored)", got)
	}
}

// ---------------------------------------------------------------------------
// 空流与边界
// ---------------------------------------------------------------------------

func TestAnthStream_EmptyStreams(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{name: "no input at all", payload: ""},
		{name: "only [DONE]", payload: anthStreamSSE(`[DONE]`)},
		{name: "only empty chunk", payload: anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":null}]}`)},
		{
			name:    "only role chunk",
			payload: anthStreamSSE(`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`),
		},
		{
			name:    "only usage chunk",
			payload: anthStreamSSE(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`),
		},
		{name: "invalid JSON", payload: anthStreamSSE(`{not json`)},
		{name: "no trailing blank line (incomplete event)", payload: `data: [DONE]`},
		{
			name:    "only finish_reason without delta",
			payload: anthStreamSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, rec := anthStreamNew(t, "m")
			anthStreamFeed(t, tr, tc.payload, 1)
			tr.Finish()
			tr.Finish() // 幂等，且不得 panic

			// 空流也必须补发最小合法事件序列（避免客户端拿到「200 + 空 body」的 SSE 流）。
			events := anthStreamEvents(t, rec.Body.String())
			anthStreamWantNames(t, events, []string{"message_start", "message_delta", "message_stop"})
			if !tr.Wrote() {
				t.Fatalf("Wrote() = false, want true")
			}
			md := anthStreamDataOf(t, events, "message_delta", 0)
			delta, _ := md["delta"].(map[string]any)
			if got := delta["stop_reason"]; got != "end_turn" {
				t.Fatalf("stop_reason = %v, want end_turn", got)
			}
			if msgStart := anthStreamDataOf(t, events, "message_start", 0); msgStart["message"] == nil {
				t.Fatalf("message_start payload missing message object: %v", msgStart)
			}
		})
	}

	t.Run("usage chunk still reported in message_delta", func(t *testing.T) {
		tr, rec := anthStreamNew(t, "m")
		anthStreamFeed(t, tr, anthStreamSSE(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`), 1)
		tr.Finish()

		events := anthStreamEvents(t, rec.Body.String())
		md := anthStreamDataOf(t, events, "message_delta", 0)
		usage, _ := md["usage"].(map[string]any)
		if got, _ := usage["output_tokens"].(float64); got != 2 {
			t.Fatalf("output_tokens = %v, want 2 (usage 分片即使无 choices 也要被采纳)", usage["output_tokens"])
		}
	})
}

func TestAnthStream_WroteFlag(t *testing.T) {
	tr, _ := anthStreamNew(t, "m")
	if tr.Wrote() {
		t.Fatalf("initial Wrote() = true, want false")
	}
	// 不完整事件（无空行）不产生输出。
	if _, err := tr.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}`)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if tr.Wrote() {
		t.Fatalf("Wrote() = true after incomplete event, want false")
	}
	if _, err := tr.Write([]byte("\n\n")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after complete event, want true")
	}
}

func TestAnthStream_CRLFDelimitedEvents(t *testing.T) {
	tr, rec := anthStreamNew(t, "m")
	payload := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	anthStreamFeed(t, tr, payload, 3)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
}

// TestAnthStream_UsageChunkAfterFinishReason 覆盖标准 OpenAI 分片顺序：
// finish_reason 分片之后另有一个 choices 为空、仅含 usage 的收尾分片
// （stream_options.include_usage 的语义），该分片的 usage 必须被采纳进 message_delta ——
// finish_reason 不得触发提前收尾（否则 output_tokens 恒为 0）。
func TestAnthStream_UsageChunkAfterFinishReason(t *testing.T) {
	tr, rec := anthStreamNew(t, "m")
	payload := anthStreamSSE(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		anthStreamSSE(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		anthStreamSSE(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`) +
		anthStreamSSE(`[DONE]`)
	anthStreamFeed(t, tr, payload, 7)
	tr.Finish()

	events := anthStreamEvents(t, rec.Body.String())
	anthStreamWantNames(t, events, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
	md := anthStreamDataOf(t, events, "message_delta", 0)
	usage, _ := md["usage"].(map[string]any)
	if got, _ := usage["output_tokens"].(float64); got != 7 {
		t.Fatalf("output_tokens = %v, want 7 (usage 分片必须被采纳)", usage["output_tokens"])
	}
	delta, _ := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", delta["stop_reason"])
	}
}
