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

// Package userapi 实现网关用户面 API（`/api/user/*`）：个人信息、本人配额与本人用量查询。
//
// 全部用户面路由统一经 auth.RequireUser 鉴权（user 与 admin 角色均可通过）。
// 行级隔离：user 角色的用量查询强制 user_id = 自己（请求中的 user_id 一律忽略）；
// admin 角色可用 `?user_id=` 查询任意用户，缺省查询自己。
// handler 只做协议编解码、鉴权编排与参数校验，业务与持久化下沉到 model / quota 包。
package userapi

import (
	"log/slog"
	"net/http"

	"aigate/internal/auth"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// Register 在 mux 上注册全部用户面路由。
func Register(mux *http.ServeMux, store *model.Store) {
	m := http.NewServeMux()

	m.HandleFunc("GET /api/user/me", handleMe(store))
	m.HandleFunc("GET /api/user/quota", handleQuota(store))
	m.HandleFunc("GET /api/user/usage/summary", handleUsageSummary(store))
	m.HandleFunc("GET /api/user/usage/logs", handleUsageLogs(store))
	m.HandleFunc("GET /api/user/usage/by-model", handleUsageByModel(store))

	m.HandleFunc("POST /api/user/keys", handleCreateUserKey(store))
	m.HandleFunc("GET /api/user/keys", handleListUserKeys(store))
	m.HandleFunc("GET /api/user/keys/{id}", handleGetUserKey(store))
	m.HandleFunc("PUT /api/user/keys/{id}", handleUpdateUserKey(store))
	m.HandleFunc("DELETE /api/user/keys/{id}", handleDeleteUserKey(store))
	m.HandleFunc("POST /api/user/keys/{id}/rotate", handleRotateUserKey(store))
	m.HandleFunc("GET /api/user/keys/{id}/usage", handleUserKeyUsage(store))

	mux.Handle("/api/user/", auth.RequireUser(store, m))
}

// currentUser 取出鉴权中间件注入的当前用户；缺失视为内部编排错误。
func currentUser(w http.ResponseWriter, r *http.Request) (*model.User, bool) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, "user missing from context")
		return nil, false
	}
	return u, true
}

// writeInternalError 统一处理用户面内部错误：详细原因记日志，对外只返回通用文案。
func writeInternalError(w http.ResponseWriter, err error) {
	slog.Error("user api internal error", "error", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
}

// writeLookupError 统一处理「查找目标」错误：不存在 → 404，其余 → 500。
func writeLookupError(w http.ResponseWriter, err error, name string) {
	if model.IsNotFound(err) {
		httpx.WriteError(w, http.StatusNotFound, name+" not found")
		return
	}
	writeInternalError(w, err)
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
