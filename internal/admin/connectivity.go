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

package admin

import (
	"errors"
	"net/http"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
)

// 连通性探测的 error_kind 枚举（与 spec 一致，封闭集合）。
const (
	connectivityNetworkUnreachable = "network_unreachable"
	connectivityInvalidAPIKey      = "invalid_api_key"
	connectivityForbidden          = "forbidden"
	connectivityRateLimited        = "rate_limited"
	connectivityUpstreamServer     = "upstream_server"
	connectivityUpstreamClient     = "upstream_client"
)

// connectivityOK 是探测成功的结果：latency_ms 为一次最小对话请求的往返毫秒数。
type connectivityOK struct {
	OK        bool  `json:"ok"`
	LatencyMS int64 `json:"latency_ms"`
}

// connectivityFailure 是探测失败的结果：error_kind 为封闭枚举，message 为人类可读原因。
type connectivityFailure struct {
	OK        bool   `json:"ok"`
	ErrorKind string `json:"error_kind"`
	Message   string `json:"message"`
}

// handleModelConnectivity 实现 `POST /admin/models/{id}/connectivity`：
// 以最小对话请求（max_tokens=1）探测模型配置是否可用。
//
// 口径说明：
//   - 探测结果一律以 HTTP 200 返回，成功/失败体现在 body 的 ok 字段（探测失败不是接口调用失败）；
//   - 仅「模型不存在」(404) 与「路径 id 非法」(400) 走统一错误体；
//   - 出站请求复用 provider 的 SSRF 防护链，私网/回环是否放行由 -allow-private-upstreams 决定；
//   - 上游密钥经 model.ResolveAPIKeyForModel 解析：引用凭据时实时用凭据密钥，否则回退模型 api_key；
//     解析失败（引用加载失败 / 密文不可解）属服务端配置问题，按 500 返回并输出日志，不作为探测结果。
func handleModelConnectivity(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		m, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		// 自备 Key 模型没有共享密钥可探测：其可用性需在数据面按绑定密钥验证。
		if m.UserKeyRequired {
			httpx.WriteError(
				w,
				http.StatusBadRequest,
				"user_key_required model has no shared key to probe; verify it from the data plane with a binding key",
			)
			return
		}
		apiKey, err := model.ResolveAPIKeyForModel(r.Context(), store, m)
		if err != nil {
			writeInternalError(w, err)
			return
		}

		latency, err := provider.GetProvider(m.ModelType).
			CheckConnectivityWithChat(r.Context(), apiKey, m.URL, m.ModelID)
		if err != nil {
			httpx.WriteJSON(w, http.StatusOK, connectivityFailure{
				OK:        false,
				ErrorKind: classifyConnectivityError(err),
				Message:   err.Error(),
			})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, connectivityOK{OK: true, LatencyMS: latencyMillis(latency)})
	}
}

// classifyConnectivityError 把 provider 的错误哨兵映射为 error_kind 枚举。
// 未命中任何哨兵（理论上不会发生）按「请求未到达上游」归类。
func classifyConnectivityError(err error) string {
	switch {
	case errors.Is(err, provider.ErrInvalidAPIKey):
		return connectivityInvalidAPIKey
	case errors.Is(err, provider.ErrForbidden):
		return connectivityForbidden
	case errors.Is(err, provider.ErrRateLimited):
		return connectivityRateLimited
	case errors.Is(err, provider.ErrUpstreamServer):
		return connectivityUpstreamServer
	case errors.Is(err, provider.ErrUpstreamClient):
		return connectivityUpstreamClient
	default:
		// 含 ErrNetworkUnreachable（DNS/连接/TLS/超时/SSRF 拦截）与未知错误。
		return connectivityNetworkUnreachable
	}
}

// latencyMillis 把往返耗时取整为毫秒；耗时为正但不足 1ms 时记为 1，
// 保证成功响应满足「latency_ms 为正」的约定。
func latencyMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms == 0 && d > 0 {
		return 1
	}
	return ms
}
