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

// Package httpx 提供 HTTP 层的公共读写辅助：统一 JSON 响应与错误体。
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// MaxBodyBytes 限制请求体解码的最大字节数。
const MaxBodyBytes = 1 << 20

// ErrBodyTooLarge 表示请求体超过 MaxBodyBytes（与「JSON 非法」可区分）。
var ErrBodyTooLarge = errors.New("request body too large")

// ErrorBody 是统一错误响应体：{"error":{"message","type","code"}}。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 错误详情。
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code"`
}

// WriteJSON 写出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError 写出统一错误响应。
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Message: message, Type: "error", Code: status}})
}

// ReadBody 读取请求体（上限 MaxBodyBytes），并关闭 body；
// 超限时返回 ErrBodyTooLarge，读取失败时返回底层错误。
func ReadBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

// DecodeJSON 解码请求体（大小受限；超限与 JSON 非法使用不同的错误文案）。
func DecodeJSON(r *http.Request, v any) error {
	body, err := ReadBody(r)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			return ErrBodyTooLarge
		}
		return fmt.Errorf("read request body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid json body: %w", err)
	}
	return nil
}

// ParseUintID 解析路径参数中的正整数 ID；非法或 0 返回 false（由调用方以 400 响应）。
func ParseUintID(r *http.Request, param string) (uint, bool) {
	n, err := strconv.ParseUint(r.PathValue(param), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}
