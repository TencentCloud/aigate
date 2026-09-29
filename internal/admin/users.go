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
	"net/http"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// userResponse 是用户的对外表示：绝不含 token 哈希。
// Token 仅在创建/rotate 响应中填充一次（其余场景为空串，经 omitempty 省略）。
type userResponse struct {
	ID             uint      `json:"id"`
	Username       string    `json:"username"`
	Role           string    `json:"role"`
	GroupID        uint      `json:"group_id"`
	APITokenPrefix string    `json:"api_token_prefix"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Token          string    `json:"token,omitempty"`
}

type userCreateRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	GroupID  uint   `json:"group_id"`
	Enabled  *bool  `json:"enabled"`
}

// userUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type userUpdateRequest struct {
	Username *string `json:"username"`
	Role     *string `json:"role"`
	GroupID  *uint   `json:"group_id"`
	Enabled  *bool   `json:"enabled"`
}

func handleCreateUser(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req userCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		role := req.Role
		if role == "" {
			role = model.RoleUser
		}
		if !validRole(role) {
			httpx.WriteError(w, http.StatusBadRequest, "role must be admin or user")
			return
		}

		plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		u := &model.User{
			Username:       req.Username,
			Role:           role,
			GroupID:        req.GroupID,
			APITokenHash:   hash,
			APITokenPrefix: prefix,
			Enabled:        true,
		}
		if req.Enabled != nil {
			u.Enabled = *req.Enabled
		}
		if err := store.CreateUser(r.Context(), u); err != nil {
			writeInternalError(w, err)
			return
		}
		// enabled 带 default 标签，零值在 INSERT 时会被 GORM 跳过，显式 false 需补一次更新。
		if req.Enabled != nil && !*req.Enabled {
			updated, err := store.UpdateUser(r.Context(), u.ID, map[string]any{"enabled": false})
			if err != nil {
				writeInternalError(w, err)
				return
			}
			u = updated
		}
		resp := toUserResponse(u)
		resp.Token = plain
		httpx.WriteJSON(w, http.StatusCreated, resp)
	}
}

func handleListUsers(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := store.ListUsers(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]userResponse, 0, len(users))
		for i := range users {
			out = append(out, toUserResponse(&users[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": out})
	}
}

func handleUpdateUser(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid user id")
			return
		}
		var req userUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Role != nil && !validRole(*req.Role) {
			httpx.WriteError(w, http.StatusBadRequest, "role must be admin or user")
			return
		}
		before, err := store.GetUserByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "user")
			return
		}
		// 保护「最后一个启用中的管理员」：降级 / 禁用会让自己永久失去管理面入口。
		removesAdmin := (req.Role != nil && *req.Role != model.RoleAdmin) ||
			(req.Enabled != nil && !*req.Enabled)
		if !guardLastEnabledAdmin(w, r, store, before, removesAdmin) {
			return
		}

		fields := make(map[string]any)
		if req.Username != nil {
			fields["username"] = *req.Username
		}
		if req.Role != nil {
			fields["role"] = *req.Role
		}
		if req.GroupID != nil {
			fields["group_id"] = *req.GroupID
		}
		if req.Enabled != nil {
			fields["enabled"] = *req.Enabled
		}
		u, err := store.UpdateUser(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "user")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toUserResponse(u))
	}
}

func handleDeleteUser(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid user id")
			return
		}
		target, err := store.GetUserByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "user")
			return
		}
		// 保护「最后一个启用中的管理员」：删除后管理面将无可用入口（需重启进程重建）。
		if !guardLastEnabledAdmin(w, r, store, target, true) {
			return
		}
		if err := store.DeleteUser(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// guardLastEnabledAdmin 防止「删除 / 降级 / 禁用」最后一个启用中的管理员（命中时 409）。
// removesAdmin 表示本次操作是否撤销目标的 admin 能力（删除、role 改为 user、enabled 置 false）。
// 说明：检查与写入之间存在极小的并发窗口（两个管理员同时互相降级），单管理面场景可接受。
func guardLastEnabledAdmin(
	w http.ResponseWriter,
	r *http.Request,
	store *model.Store,
	target *model.User,
	removesAdmin bool,
) bool {
	if !removesAdmin || target.Role != model.RoleAdmin || !target.Enabled {
		return true
	}
	n, err := store.CountEnabledUsersByRole(r.Context(), model.RoleAdmin)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if n <= 1 {
		httpx.WriteError(w, http.StatusConflict, "cannot remove the last enabled admin")
		return false
	}
	return true
}

func handleRotateUser(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid user id")
			return
		}
		u, err := store.GetUserByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "user")
			return
		}
		plain, displayPrefix, err := store.RotateUserToken(r.Context(), id)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		u.APITokenPrefix = displayPrefix
		resp := toUserResponse(u)
		resp.Token = plain
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// toUserResponse 将用户实体转为对外表示（丢弃 token 哈希）。
func toUserResponse(u *model.User) userResponse {
	return userResponse{
		ID:             u.ID,
		Username:       u.Username,
		Role:           u.Role,
		GroupID:        u.GroupID,
		APITokenPrefix: u.APITokenPrefix,
		Enabled:        u.Enabled,
		CreatedAt:      u.CreatedAt,
		UpdatedAt:      u.UpdatedAt,
	}
}

// validRole 校验用户角色取值。
func validRole(role string) bool {
	return role == model.RoleAdmin || role == model.RoleUser
}
