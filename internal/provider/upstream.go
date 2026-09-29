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

// 本文件集中放置两类 Provider（OpenAI / Anthropic）共用的上游 HTTP 辅助：
// 请求构造（含 SSRF 校验与鉴权头注入）、发送、错误透传、SSE 响应头与连通性探测收尾，
// 避免同一套样板代码在多个文件中重复。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// newUpstreamRequest 构造发往上游的 POST 请求：
//   - SSRF 防护恒开：出站目标地址一律先经 validateOutboundURL 校验；
//   - 统一设置 JSON 内容类型；上游特有的鉴权头由 auth 注入（可为 nil）；
//   - 最后应用用户自定义头（不覆盖已设置的头部）。
func newUpstreamRequest(
	ctx context.Context,
	url string,
	body []byte,
	auth func(http.Header),
	customHeaders map[string]string,
) (*http.Request, error) {
	if err := validateOutboundURL(ctx, url); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, richErrorf(err, "failed to build upstream request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if auth != nil {
		auth(httpReq.Header)
	}
	applyCustomHeaders(httpReq, customHeaders)
	return httpReq, nil
}

// doUpstream 以指定超时发送上游请求；传输层错误统一包装为 "upstream request failed"。
func doUpstream(httpReq *http.Request, timeout time.Duration) (*http.Response, error) {
	resp, err := httpClientForTimeout(timeout).Do(httpReq)
	if err != nil {
		return nil, richErrorf(err, "upstream request failed")
	}
	return resp, nil
}

// forwardUpstreamError 将上游错误响应原样透传给客户端（大小受限，防异常上游回传超大 body）。
func forwardUpstreamError(w http.ResponseWriter, resp *http.Response) {
	respBody := readUpstreamErrorBody(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// setSSEHeaders 设置流式透传所需的响应头。
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// newProbeRequest 构造连通性探测请求；所有构造期错误按网络不可达归类。
// 与 newUpstreamRequest 的差异：探测路径不应用用户自定义头。
func newProbeRequest(ctx context.Context, url string, body []byte, auth func(http.Header)) (*http.Request, error) {
	if err := validateOutboundURL(ctx, url); err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if auth != nil {
		auth(httpReq.Header)
	}
	return httpReq, nil
}

// runConnectivityProbe 发送探测请求（15s 超时）并返回往返耗时与分类后的错误。
func runConnectivityProbe(httpReq *http.Request) (time.Duration, error) {
	start := time.Now()
	resp, err := httpClientForTimeout(15 * time.Second).Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		return latency, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	defer resp.Body.Close()
	if kind := classifyHTTPStatus(resp.StatusCode); kind != nil {
		return latency, &ConnectivityError{
			Kind:       kind,
			StatusCode: resp.StatusCode,
			Snippet:    readBodySnippet(resp.Body, 512),
		}
	}
	// Drain body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return latency, nil
}
