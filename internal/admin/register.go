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

// Package admin 实现网关管理面 API（`/admin/*`）：模型、密钥、用户、分组、用量与全局设置的读写。
//
// 全部管理面路由统一经 auth.RequireAdmin 鉴权（仅 role=admin 的访问 token 可通过）；
// 用户面（internal/userapi）复用 auth.RequireUser + auth.UserFromContext。
// handler 只做协议编解码、鉴权编排与参数校验，业务与持久化下沉到 model 包
// （本包不直接访问数据库/不 import gorm）。
package admin

import (
	"net/http"

	"aigate/internal/auth"
	"aigate/internal/model"
)

// Register 在 mux 上注册全部管理面路由。
func Register(mux *http.ServeMux, store *model.Store) {
	m := http.NewServeMux()

	m.HandleFunc("POST /admin/models", handleCreateModel(store))
	m.HandleFunc("GET /admin/models", handleListModels(store))
	m.HandleFunc("GET /admin/models/{id}", handleGetModel(store))
	m.HandleFunc("PUT /admin/models/{id}", handleUpdateModel(store))
	m.HandleFunc("DELETE /admin/models/{id}", handleDeleteModel(store))
	m.HandleFunc("POST /admin/models/{id}/connectivity", handleModelConnectivity(store))

	m.HandleFunc("POST /admin/credentials", handleCreateCredential(store))
	m.HandleFunc("GET /admin/credentials", handleListCredentials(store))
	m.HandleFunc("GET /admin/credentials/{id}", handleGetCredential(store))
	m.HandleFunc("PUT /admin/credentials/{id}", handleUpdateCredential(store))
	m.HandleFunc("DELETE /admin/credentials/{id}", handleDeleteCredential(store))

	m.HandleFunc("POST /admin/keys", handleCreateKey(store))
	m.HandleFunc("GET /admin/keys", handleListKeys(store))
	m.HandleFunc("GET /admin/keys/{id}", handleGetKey(store))
	m.HandleFunc("PUT /admin/keys/{id}", handleUpdateKey(store))
	m.HandleFunc("DELETE /admin/keys/{id}", handleDeleteKey(store))
	m.HandleFunc("POST /admin/keys/{id}/rotate", handleRotateKey(store))

	m.HandleFunc("POST /admin/users", handleCreateUser(store))
	m.HandleFunc("GET /admin/users", handleListUsers(store))
	m.HandleFunc("PUT /admin/users/{id}", handleUpdateUser(store))
	m.HandleFunc("DELETE /admin/users/{id}", handleDeleteUser(store))
	m.HandleFunc("POST /admin/users/{id}/rotate", handleRotateUser(store))

	m.HandleFunc("POST /admin/user-groups", handleCreateGroup(store))
	m.HandleFunc("GET /admin/user-groups", handleListGroups(store))
	m.HandleFunc("GET /admin/user-groups/{id}", handleGetGroup(store))
	m.HandleFunc("PUT /admin/user-groups/{id}", handleUpdateGroup(store))
	m.HandleFunc("DELETE /admin/user-groups/{id}", handleDeleteGroup(store))

	m.HandleFunc("GET /admin/usage/summary", handleUsageSummary(store))
	m.HandleFunc("GET /admin/usage/export", handleUsageExport(store))
	m.HandleFunc("GET /admin/usage/logs", handleUsageLogs(store))

	m.HandleFunc("GET /admin/settings", handleGetSettings(store))
	m.HandleFunc("PUT /admin/settings", handleUpdateSettings(store))

	m.HandleFunc("POST /admin/webhooks", handleCreateWebhook(store))
	m.HandleFunc("GET /admin/webhooks", handleListWebhooks(store))
	m.HandleFunc("GET /admin/webhooks/{id}", handleGetWebhook(store))
	m.HandleFunc("PUT /admin/webhooks/{id}", handleUpdateWebhook(store))
	m.HandleFunc("DELETE /admin/webhooks/{id}", handleDeleteWebhook(store))
	m.HandleFunc("GET /admin/webhooks/{id}/deliveries", handleListWebhookDeliveries(store))

	m.HandleFunc("POST /admin/import", handleImport(store))

	mux.Handle("/admin/", auth.RequireAdmin(store, m))
}
