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
	"log/slog"
	"net/http"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// writeInternalError 统一处理管理面内部错误：详细原因记日志，对外只返回通用文案（不回显底层错误细节）。
func writeInternalError(w http.ResponseWriter, err error) {
	slog.Error("admin api internal error", "error", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
}

// writeMutationError 把 model 层写操作错误映射为响应：
// 参数校验类（model.ValidationError）→ 400 原文；其余（事务/驱动错误）→ 500 通用文案。
func writeMutationError(w http.ResponseWriter, err error) {
	if model.IsValidationError(err) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeInternalError(w, err)
}
