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
	"strconv"
	"strings"
	"time"
)

// OpenAIProvider handles any OpenAI-compatible endpoint.
type OpenAIProvider struct{}

var gptVersionRx = regexp.MustCompile(`(?i)^gpt-(\d+)`)

// needsMaxCompletionTokens 判断模型是否用 max_completion_tokens 代替 max_tokens。
// GPT-5+（gpt-6/7…自动覆盖）和 o-series（o1/o3/o4…）。
func needsMaxCompletionTokens(model string) bool {
	lower := strings.ToLower(model)
	if m := gptVersionRx.FindStringSubmatch(lower); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v >= 5
	}
	return len(lower) > 1 && lower[0] == 'o' && lower[1] >= '0' && lower[1] <= '9'
}

// normalizeMaxTokensParam 按模型统一 max_tokens / max_completion_tokens 参数名。
func normalizeMaxTokensParam(req map[string]interface{}, model string) {
	if needsMaxCompletionTokens(model) {
		if mt, ok := req["max_tokens"]; ok {
			if _, ok := req["max_completion_tokens"]; !ok {
				req["max_completion_tokens"] = mt
			}
			delete(req, "max_tokens")
		}
		return
	}
	if mct, ok := req["max_completion_tokens"]; ok {
		if _, ok := req["max_tokens"]; !ok {
			req["max_tokens"] = mct
		}
		delete(req, "max_completion_tokens")
	}
}

// sanitizeUsage finds the "usage" object in a response JSON body and replaces
// any null token counts with 0. Returns the original body unchanged if parsing
// fails or no usage field is present.
func sanitizeUsage(respBody []byte) []byte {
	var resp map[string]interface{}
	if json.Unmarshal(respBody, &resp) != nil {
		return respBody
	}
	usageRaw, ok := resp["usage"]
	if !ok || usageRaw == nil {
		return respBody
	}
	usage, ok := usageRaw.(map[string]interface{})
	if !ok {
		return respBody
	}

	changed := false
	tokenFields := []string{"prompt_tokens", "completion_tokens", "total_tokens"}
	for _, field := range tokenFields {
		if v, exists := usage[field]; exists && v == nil {
			usage[field] = 0
			changed = true
		}
	}
	if !changed {
		return respBody
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

func (p *OpenAIProvider) baseURL(apiBase string) string {
	return strings.TrimRight(apiBase, "/")
}

func (p *OpenAIProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (*CompletionResult, int, error) {
	resp, err := p.postChatCompletion(ctx, apiKey, apiBase, model, reqBody, false, customHeaders)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream response")
	}

	// Sanitize null usage values from non-standard endpoints.
	if resp.StatusCode == http.StatusOK {
		respBody = sanitizeUsage(respBody)
	}
	return &CompletionResult{Body: respBody, Usage: extractOpenAIUsage(string(respBody), nil)}, resp.StatusCode, nil
}

// postChatCompletion 构造并发送上游 /chat/completions 请求（调用方负责关闭响应体）。
func (p *OpenAIProvider) postChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	stream bool,
	customHeaders map[string]string,
) (*http.Response, error) {
	body, err := buildOpenAIRequest(model, reqBody, stream)
	if err != nil {
		return nil, err
	}
	httpReq, err := newUpstreamRequest(ctx, p.baseURL(apiBase)+"/chat/completions", body, func(h http.Header) {
		h.Set("Authorization", "Bearer "+apiKey)
	}, customHeaders)
	if err != nil {
		return nil, err
	}
	return doUpstream(httpReq, 5*time.Minute)
}

// buildOpenAIRequest 深拷贝入站请求体并注入模型名；stream=true 时追加流式与用量字段。
func buildOpenAIRequest(model string, reqBody []byte, stream bool) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return nil, richErrorf(err, "failed to decode request body")
	}
	req["model"] = model
	if stream {
		req["stream"] = true
		req["stream_options"] = map[string]interface{}{"include_usage": true}
	}
	normalizeMaxTokensParam(req, model)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, richErrorf(err, "failed to encode request body")
	}
	return body, nil
}

func (p *OpenAIProvider) ChatCompletionStream(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	w http.ResponseWriter,
	flusher http.Flusher,
	customHeaders map[string]string,
) (*StreamResult, int, error) {
	resp, err := p.postChatCompletion(ctx, apiKey, apiBase, model, reqBody, true, customHeaders)
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

	var usage *Usage
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data != "[DONE]" {
				usage = extractOpenAIUsage(data, usage)
			}
		}
	}
	// 读取中断（连接重置、单行超限等）必须显式报错：否则调用方会把截断当作正常结束。
	if err := scanner.Err(); err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream stream")
	}
	return &StreamResult{Usage: usage}, http.StatusOK, nil
}

// extractOpenAIUsage parses OpenAI-compatible usage from streaming chunks or
// non-streaming response bodies.
func extractOpenAIUsage(data string, prev *Usage) *Usage {
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			CachedTokens     int `json:"cached_tokens"` // Moonshot/Kimi.
			PromptDetails    struct {
				CachedTokens        int `json:"cached_tokens"`         // OpenAI Chat Completions.
				CacheWriteTokens    int `json:"cache_write_tokens"`    // OpenRouter.
				CacheCreationTokens int `json:"cache_creation_tokens"` // LiteLLM.
			} `json:"prompt_tokens_details"`
			InputDetails struct {
				CachedTokens int `json:"cached_tokens"` // OpenAI Responses.
			} `json:"input_tokens_details"`
			PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // DeepSeek.
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
		cacheRead := firstPositiveInt(
			chunk.Usage.InputDetails.CachedTokens,
			chunk.Usage.PromptDetails.CachedTokens,
			chunk.Usage.CachedTokens,
			chunk.Usage.PromptCacheHitTokens,
		)
		cacheWrite := firstPositiveInt(
			chunk.Usage.PromptDetails.CacheWriteTokens,
			chunk.Usage.PromptDetails.CacheCreationTokens,
		)
		return &Usage{
			PromptTokens:           chunk.Usage.PromptTokens,
			CompletionTokens:       chunk.Usage.CompletionTokens,
			TotalTokens:            chunk.Usage.TotalTokens,
			PromptCacheReadTokens:  cacheRead,
			PromptCacheWriteTokens: cacheWrite,
		}
	}
	return prev
}

func firstPositiveInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// CheckConnectivityWithChat probes an OpenAI-compatible endpoint by issuing a
// minimal POST /chat/completions request (one user message, max_tokens=1).
//
// This probe validates both network reachability, the apiKey, and that the
// supplied model name is actually invocable.
//
// On success it returns the round-trip latency and a nil error. On failure
// it returns a *ConnectivityError wrapping one of the Err* sentinels defined
// in provider.go; callers can use errors.Is to classify the failure.
func (p *OpenAIProvider) CheckConnectivityWithChat(
	ctx context.Context,
	apiKey, apiBase, model string,
) (time.Duration, error) {
	req := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
		"stream":     false,
	}
	normalizeMaxTokensParam(req, model)
	body, err := json.Marshal(req)
	if err != nil {
		return 0, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}

	httpReq, err := newProbeRequest(ctx, p.baseURL(apiBase)+"/chat/completions", body, func(h http.Header) {
		if apiKey != "" {
			h.Set("Authorization", "Bearer "+apiKey)
		}
	})
	if err != nil {
		return 0, err
	}
	return runConnectivityProbe(httpReq)
}
