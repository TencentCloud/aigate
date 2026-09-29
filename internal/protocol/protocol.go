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

// Package protocol 定义上游协议类型的共享常量。
// model（持久化与校验）与 provider（协议实现分发）都从这里取值，
// 保证单一事实源、避免字面量在多处漂移。
package protocol

// 上游协议类型取值（对应 ai_models.model_type 列）。
const (
	// TypeOpenAICompletions 是 OpenAI 兼容端点（URL 含 /v1，转发时拼 /chat/completions）。
	TypeOpenAICompletions = "openai-completions"
	// TypeAnthropicMessages 是 Anthropic Messages 协议（URL 为 host，转发时拼 /v1/messages）。
	TypeAnthropicMessages = "anthropic-messages"
)
