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
	"reflect"
	"strconv"
	"testing"
)

func TestNeedsMaxCompletionTokens(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		// GPT-5 family — needs max_completion_tokens
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5-nano", true},
		{"gpt-5.1", true},
		{"gpt-5-codex", true},
		{"gpt-5.2-2026-04-01", true},
		{"GPT-5", true}, // case insensitive
		{"Gpt-5-Mini", true},

		// GPT-6 and beyond — needs max_completion_tokens (future-proof)
		{"gpt-6", true},
		{"gpt-6-mini", true},
		{"gpt-7", true},
		{"gpt-99", true},

		// GPT-4 family — uses legacy max_tokens
		{"gpt-4", false},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4.1", false},
		{"gpt-4.1-mini", false},
		{"gpt-4-turbo", false},

		// GPT-3 family — uses legacy max_tokens
		{"gpt-3.5-turbo", false},

		// o-series — needs max_completion_tokens
		{"o1", true},
		{"o1-mini", true},
		{"o1-preview", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"O1", true},

		// Non-OpenAI — uses legacy max_tokens
		{"claude-3.5-sonnet", false},
		{"deepseek-chat", false},
		{"gemini-2.0-flash", false},
	}

	for _, tt := range tests {
		got := needsMaxCompletionTokens(tt.model)
		if got != tt.expected {
			t.Errorf("needsMaxCompletionTokens(%q) = %v, want %v", tt.model, got, tt.expected)
		}
	}
}

func TestNormalizeMaxTokensParam_NewModel(t *testing.T) {
	req := map[string]interface{}{
		"max_tokens":  float64(2000),
		"temperature": float64(1),
	}
	normalizeMaxTokensParam(req, "gpt-5")

	// max_tokens should be removed
	if _, ok := req["max_tokens"]; ok {
		t.Error("max_tokens should be removed for gpt-5 model")
	}
	// max_completion_tokens should be set
	if v, ok := req["max_completion_tokens"]; !ok {
		t.Error("max_completion_tokens should be set for gpt-5 model")
	} else if v.(float64) != 2000 {
		t.Errorf("max_completion_tokens = %v, want 2000", v)
	}
	// temperature should be untouched
	if req["temperature"].(float64) != 1 {
		t.Errorf("temperature = %v, want 1", req["temperature"])
	}
}

func TestNormalizeMaxTokensParam_Legacy(t *testing.T) {
	req := map[string]interface{}{
		"max_completion_tokens": float64(2000),
		"temperature":           float64(0.7),
	}
	normalizeMaxTokensParam(req, "gpt-4o")

	// max_completion_tokens should be removed
	if _, ok := req["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should be removed for legacy model")
	}
	// max_tokens should be set
	if v, ok := req["max_tokens"]; !ok {
		t.Error("max_tokens should be set for legacy model")
	} else if v.(float64) != 2000 {
		t.Errorf("max_tokens = %v, want 2000", v)
	}
}

func TestNormalizeMaxTokensParam_NewModelExistingMCT(t *testing.T) {
	// max_completion_tokens already set → keep it, remove max_tokens
	req := map[string]interface{}{
		"max_tokens":            float64(1000),
		"max_completion_tokens": float64(2000),
	}
	normalizeMaxTokensParam(req, "gpt-5")

	if _, ok := req["max_tokens"]; ok {
		t.Error("max_tokens should be removed")
	}
	if v := req["max_completion_tokens"].(float64); v != 2000 {
		t.Errorf("max_completion_tokens = %v, want 2000 (not overwritten)", v)
	}
}

func TestNormalizeMaxTokensParam_NewModelNoTokenParam(t *testing.T) {
	req := map[string]interface{}{
		"temperature": float64(1),
	}
	normalizeMaxTokensParam(req, "gpt-5")

	// should be a no-op
	if _, ok := req["max_tokens"]; ok {
		t.Error("max_tokens should not be present")
	}
	if _, ok := req["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should not be present")
	}
}

func TestNormalizeMaxTokensParam_LegacyExistingMaxTokens(t *testing.T) {
	// Both present on a legacy model → max_tokens wins, max_completion_tokens dropped.
	req := map[string]interface{}{
		"max_tokens":            float64(1000),
		"max_completion_tokens": float64(2000),
	}
	normalizeMaxTokensParam(req, "gpt-4o")

	if v := req["max_tokens"].(float64); v != 1000 {
		t.Errorf("max_tokens = %v, want 1000 (not overwritten)", v)
	}
	if _, ok := req["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should be removed")
	}
}

func TestNormalizeMaxTokensParam_LegacyNoTokenParam(t *testing.T) {
	req := map[string]interface{}{"temperature": float64(1)}
	normalizeMaxTokensParam(req, "gpt-4o")
	if _, ok := req["max_tokens"]; ok {
		t.Error("max_tokens should not be present")
	}
	if _, ok := req["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should not be present")
	}
}

// TestRoundTrip 模拟调用方写入 max_completion_tokens 后，provider 对
// 新旧模型各自归一化参数名的行为。
func TestRoundTrip(t *testing.T) {
	orig := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096}`

	var req map[string]interface{}
	if err := json.Unmarshal([]byte(orig), &req); err != nil {
		t.Fatal(err)
	}

	// Provider normalizes
	normalizeMaxTokensParam(req, req["model"].(string))

	// For gpt-5: max_completion_tokens stays, max_tokens is absent
	if _, ok := req["max_tokens"]; ok {
		t.Error("gpt-5: max_tokens should be absent")
	}
	if v, ok := req["max_completion_tokens"]; !ok || v.(float64) != 4096 {
		t.Error("gpt-5: max_completion_tokens should be 4096")
	}

	// Round-trip for legacy model
	orig2 := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096}`

	var req2 map[string]interface{}
	json.Unmarshal([]byte(orig2), &req2)
	normalizeMaxTokensParam(req2, req2["model"].(string))

	// For gpt-4o: max_completion_tokens removed, max_tokens present
	if _, ok := req2["max_completion_tokens"]; ok {
		t.Error("gpt-4o: max_completion_tokens should be absent")
	}
	if v, ok := req2["max_tokens"]; !ok || v.(float64) != 4096 {
		t.Error("gpt-4o: max_tokens should be 4096")
	}
}

// TestGptVersionExtraction covers the version regex against various model names.
func TestGptVersionExtraction(t *testing.T) {
	tests := []struct {
		model     string
		wantMatch bool
		wantMajor int
	}{
		{"gpt-5", true, 5},
		{"gpt-5-mini", true, 5},
		{"gpt-5.1", true, 5},
		{"gpt-6", true, 6},
		{"gpt-7", true, 7},
		{"gpt-10", true, 10},
		{"gpt-99-turbo", true, 99},
		{"gpt-4", true, 4},
		{"gpt-4o", true, 4},
		{"gpt-4.1", true, 4},
		{"gpt-3.5-turbo", true, 3},
		{"gpt-3", true, 3},
		// No match:
		{"o1", false, 0},
		{"claude-3", false, 0},
		{"deepseek-chat", false, 0},
		{"gpt5", false, 0}, // no dash
		{"GPT-5", true, 5}, // case insensitive (ToLower used before matching)
	}

	if gptVersionRx == nil {
		t.Fatal("gptVersionRx is nil")
	}

	for _, tt := range tests {
		m := gptVersionRx.FindStringSubmatch(tt.model)
		if tt.wantMatch {
			if m == nil {
				t.Errorf("gptVersionRx.FindStringSubmatch(%q) = nil, want match", tt.model)
				continue
			}
			v, err := strconv.Atoi(m[1])
			if err != nil {
				t.Errorf("gptVersionRx.FindStringSubmatch(%q) version parse error: %v", tt.model, err)
				continue
			}
			if v != tt.wantMajor {
				t.Errorf("gptVersionRx.FindStringSubmatch(%q) major = %d, want %d", tt.model, v, tt.wantMajor)
			}
		} else {
			if m != nil {
				t.Errorf("gptVersionRx.FindStringSubmatch(%q) = %v, want nil", tt.model, m)
			}
		}
	}
}

// Ensure the regex compiles (Go init-time panic if regex is invalid).
func TestGptVersionRxCompiles(t *testing.T) {
	if gptVersionRx == nil {
		t.Fatal("gptVersionRx did not compile")
	}
	// Verify it's actually a *regexp.Regexp
	if reflect.TypeOf(gptVersionRx).String() != "*regexp.Regexp" {
		t.Fatalf("gptVersionRx is %T, want *regexp.Regexp", gptVersionRx)
	}
}

func TestExtractOpenAIStreamUsageCacheTokens(t *testing.T) {
	chunk := `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{` +
		`"cached_tokens":60,"cache_write_tokens":7}}}`
	usage := extractOpenAIUsage(chunk, nil)
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.PromptCacheReadTokens != 60 || usage.PromptCacheWriteTokens != 7 {
		t.Fatalf(
			"cache tokens = read %d write %d, want read 60 write 7",
			usage.PromptCacheReadTokens,
			usage.PromptCacheWriteTokens,
		)
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
		t.Fatalf(
			"token totals = %d/%d/%d, want 100/20/120",
			usage.PromptTokens,
			usage.CompletionTokens,
			usage.TotalTokens,
		)
	}
}

func TestExtractOpenAIUsageProviderPriorityOrder(t *testing.T) {
	chunk := `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":4,` +
		`"input_tokens_details":{"cached_tokens":5},"prompt_tokens_details":{"cached_tokens":6,"cache_write_tokens":14,` +
		`"cache_creation_tokens":13},"prompt_cache_hit_tokens":70}}`
	usage := extractOpenAIUsage(chunk, nil)
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.PromptCacheReadTokens != 5 || usage.PromptCacheWriteTokens != 14 {
		t.Fatalf(
			"cache tokens = read %d write %d, want read 5 write 14",
			usage.PromptCacheReadTokens,
			usage.PromptCacheWriteTokens,
		)
	}
}

// TestExtractOpenAIUsage_Fallbacks 覆盖非 OpenAI 供应商各自的 cache 字段。
func TestExtractOpenAIUsage_Fallbacks(t *testing.T) {
	tests := []struct {
		name      string
		chunk     string
		wantRead  int
		wantWrite int
	}{
		{
			name:      "moonshot cached_tokens",
			chunk:     `{"usage":{"prompt_tokens":1,"cached_tokens":4}}`,
			wantRead:  4,
			wantWrite: 0,
		},
		{
			name:      "openai prompt_tokens_details",
			chunk:     `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":6}}}`,
			wantRead:  6,
			wantWrite: 0,
		},
		{
			name:      "deepseek prompt_cache_hit_tokens",
			chunk:     `{"usage":{"prompt_tokens":1,"prompt_cache_hit_tokens":70}}`,
			wantRead:  70,
			wantWrite: 0,
		},
		{
			name:      "openrouter cache_write_tokens",
			chunk:     `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cache_write_tokens":7}}}`,
			wantRead:  0,
			wantWrite: 7,
		},
		{
			name:      "litellm cache_creation_tokens",
			chunk:     `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cache_creation_tokens":8}}}`,
			wantRead:  0,
			wantWrite: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := extractOpenAIUsage(tt.chunk, nil)
			if usage == nil {
				t.Fatal("usage is nil")
			}
			if usage.PromptCacheReadTokens != tt.wantRead || usage.PromptCacheWriteTokens != tt.wantWrite {
				t.Fatalf("cache = read %d write %d, want read %d write %d",
					usage.PromptCacheReadTokens, usage.PromptCacheWriteTokens, tt.wantRead, tt.wantWrite)
			}
		})
	}
}

// TestExtractOpenAIUsage_KeepsPrevious 覆盖 chunk 中没有 usage 时沿用上一次结果的行为。
func TestExtractOpenAIUsage_KeepsPrevious(t *testing.T) {
	prev := &Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}

	for _, data := range []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"usage":null}`,
		`not-json`,
	} {
		if got := extractOpenAIUsage(data, prev); got != prev {
			t.Fatalf("extractOpenAIUsage(%q) = %+v, want previous %+v", data, got, prev)
		}
	}
	if got := extractOpenAIUsage(`{"choices":[]}`, nil); got != nil {
		t.Fatalf("extractOpenAIUsage with nil prev = %+v, want nil", got)
	}
}

// TestSanitizeUsage 覆盖把 usage 中的 null token 替换为 0 的清洗行为。
func TestSanitizeUsage(t *testing.T) {
	t.Run("null tokens become zero", func(t *testing.T) {
		in := `{"id":"x","usage":{"prompt_tokens":null,"completion_tokens":null,"total_tokens":null,"other":1}}`
		out := sanitizeUsage([]byte(in))

		var got map[string]interface{}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("sanitized body is not JSON: %v (%s)", err, out)
		}
		usage := got["usage"].(map[string]interface{})
		for _, f := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if v, ok := usage[f].(float64); !ok || v != 0 {
				t.Fatalf("usage[%s] = %v, want 0", f, usage[f])
			}
		}
		if v := usage["other"].(float64); v != 1 {
			t.Fatalf("usage[other] = %v, want 1 (untouched)", v)
		}
	})

	t.Run("partial null tokens", func(t *testing.T) {
		in := `{"usage":{"prompt_tokens":10,"completion_tokens":null,"total_tokens":10}}`
		out := sanitizeUsage([]byte(in))

		var got map[string]interface{}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("sanitized body is not JSON: %v", err)
		}
		usage := got["usage"].(map[string]interface{})
		if usage["prompt_tokens"].(float64) != 10 || usage["completion_tokens"].(float64) != 0 {
			t.Fatalf("usage = %v, want prompt 10 completion 0", usage)
		}
	})

	t.Run("no nulls keeps original bytes", func(t *testing.T) {
		in := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
		if got := sanitizeUsage(in); string(got) != string(in) {
			t.Fatalf("sanitizeUsage() = %s, want original bytes", got)
		}
	})

	t.Run("no usage keeps original bytes", func(t *testing.T) {
		in := []byte(`{"id":"x","choices":[]}`)
		if got := sanitizeUsage(in); string(got) != string(in) {
			t.Fatalf("sanitizeUsage() = %s, want original bytes", got)
		}
	})

	t.Run("null usage keeps original bytes", func(t *testing.T) {
		in := []byte(`{"id":"x","usage":null}`)
		if got := sanitizeUsage(in); string(got) != string(in) {
			t.Fatalf("sanitizeUsage() = %s, want original bytes", got)
		}
	})

	t.Run("usage not an object keeps original bytes", func(t *testing.T) {
		in := []byte(`{"id":"x","usage":"nope"}`)
		if got := sanitizeUsage(in); string(got) != string(in) {
			t.Fatalf("sanitizeUsage() = %s, want original bytes", got)
		}
	})

	t.Run("invalid json keeps original bytes", func(t *testing.T) {
		in := []byte(`{not json`)
		if got := sanitizeUsage(in); string(got) != string(in) {
			t.Fatalf("sanitizeUsage() = %s, want original bytes", got)
		}
	})
}
