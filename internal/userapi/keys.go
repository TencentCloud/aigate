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

package userapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"aigate/internal/dto"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// 自助密钥的提示文案（与 spec/design 的措辞对齐）。
const (
	msgNoAvailableModels = "no available models, please contact an administrator"
	msgModelNotAvailable = "model not available for this user"
	msgKeyNotFound       = "key not found"
)

// userKeyResponse 是自助密钥的对外表示：绝不含 token 哈希。
// Token 仅在创建/rotate 响应中一次性填充（其余场景经 omitempty 省略）。
type userKeyResponse = dto.KeyResponse

// userKeyBindingRequest 是绑定关系请求体（与管理面口径一致，恰好一个 primary 由 model 层校验）。
type userKeyBindingRequest = dto.BindingRequest

type userKeyCreateRequest struct {
	Name      string                  `json:"name"`
	MaxTokens int                     `json:"max_tokens"`
	Bindings  []userKeyBindingRequest `json:"bindings"`
}

// userKeyUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」；
// bindings 为指针，非 nil 时整体替换（同样需通过模型范围校验）。
type userKeyUpdateRequest struct {
	Name      *string                  `json:"name"`
	MaxTokens *int                     `json:"max_tokens"`
	Enabled   *bool                    `json:"enabled"`
	Bindings  *[]userKeyBindingRequest `json:"bindings"`
}

// handleCreateUserKey 实现 `POST /api/user/keys`：数量上限 → 模型范围 → 落库的校验顺序。
func handleCreateUserKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		self, ok := currentUser(w, r)
		if !ok {
			return
		}
		target, ok := resolveUserTarget(w, r, store, self)
		if !ok {
			return
		}

		var req userKeyCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if status, msg, err := enforceKeyLimit(r.Context(), store, target.ID); err != nil {
			writeInternalError(w, err)
			return
		} else if status != 0 {
			httpx.WriteError(w, status, msg)
			return
		}
		if ok, msg, err := validateModelScope(r.Context(), store, target.ID, req.Bindings); err != nil {
			writeInternalError(w, err)
			return
		} else if !ok {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}

		plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		key := &model.APIKey{
			Name:             req.Name,
			UserID:           target.ID,
			GroupID:          target.GroupID,
			ProxyTokenHash:   hash,
			ProxyTokenPrefix: prefix,
			MaxTokens:        req.MaxTokens,
			Enabled:          true,
		}
		// 绑定结构校验（恰好一个 primary、模型存在）交由 model 层：参数错误 400，内部错误 500。
		if err := store.CreateAPIKey(r.Context(), key, dto.ToAPIKeyModels(req.Bindings)); err != nil {
			writeMutationError(w, err)
			return
		}
		resp, err := dto.BuildKeyResponse(r.Context(), store, key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		resp.Token = plain
		httpx.WriteJSON(w, http.StatusCreated, resp)
	}
}

// handleListUserKeys 实现 `GET /api/user/keys`：列出目标用户名下密钥（不含明文）。
func handleListUserKeys(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		self, ok := currentUser(w, r)
		if !ok {
			return
		}
		target, ok := resolveUserTarget(w, r, store, self)
		if !ok {
			return
		}
		keys, err := store.ListAPIKeysByOwner(r.Context(), target.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out, err := dto.ListKeyResponses(r.Context(), store, keys)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": out})
	}
}

// handleGetUserKey 实现 `GET /api/user/keys/{id}`。
func handleGetUserKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _, ok := loadOwnedKey(w, r, store)
		if !ok {
			return
		}
		resp, err := dto.BuildKeyResponse(r.Context(), store, key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// handleUpdateUserKey 实现 `PUT /api/user/keys/{id}`：bindings 非 nil 时整体替换（先校验模型范围）。
func handleUpdateUserKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, target, ok := loadOwnedKey(w, r, store)
		if !ok {
			return
		}
		var req userKeyUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Bindings != nil {
			if ok, msg, err := validateModelScope(r.Context(), store, target.ID, *req.Bindings); err != nil {
				writeInternalError(w, err)
				return
			} else if !ok {
				httpx.WriteError(w, http.StatusBadRequest, msg)
				return
			}
			if err := store.ReplaceBindings(r.Context(), key.ID, dto.ToAPIKeyModels(*req.Bindings)); err != nil {
				writeMutationError(w, err)
				return
			}
		}
		fields := make(map[string]any)
		if req.Name != nil {
			fields["name"] = *req.Name
		}
		if req.MaxTokens != nil {
			fields["max_tokens"] = *req.MaxTokens
		}
		if req.Enabled != nil {
			fields["enabled"] = *req.Enabled
		}
		updated, err := store.UpdateAPIKey(r.Context(), key.ID, fields)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		resp, err := dto.BuildKeyResponse(r.Context(), store, updated)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// handleDeleteUserKey 实现 `DELETE /api/user/keys/{id}`。
func handleDeleteUserKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _, ok := loadOwnedKey(w, r, store)
		if !ok {
			return
		}
		if err := store.DeleteAPIKey(r.Context(), key.ID); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleRotateUserKey 实现 `POST /api/user/keys/{id}/rotate`：返回新的一次性明文。
func handleRotateUserKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _, ok := loadOwnedKey(w, r, store)
		if !ok {
			return
		}
		plain, displayPrefix, err := store.RotateAPIKey(r.Context(), key.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		key.ProxyTokenPrefix = displayPrefix
		resp, err := dto.BuildKeyResponse(r.Context(), store, key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		resp.Token = plain
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// resolveUserTarget 解析目标用户并统一处理错误：user 角色强制 self（quota.go 的既有口径），
// admin 可用 `?user_id=` 指定任意用户。
func resolveUserTarget(
	w http.ResponseWriter,
	r *http.Request,
	store *model.Store,
	self *model.User,
) (*model.User, bool) {
	target, err := resolveTargetUser(r, store, self)
	if err != nil {
		if errors.Is(err, errInvalidUserID) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid user_id")
			return nil, false
		}
		writeLookupError(w, err, "user")
		return nil, false
	}
	return target, true
}

// loadOwnedKey 解析路径 id 并加载目标用户名下的密钥：
// 「不存在」与「非本人」统一 404，避免通过 id 探测他人密钥是否存在。
func loadOwnedKey(w http.ResponseWriter, r *http.Request, store *model.Store) (*model.APIKey, *model.User, bool) {
	self, ok := currentUser(w, r)
	if !ok {
		return nil, nil, false
	}
	target, ok := resolveUserTarget(w, r, store, self)
	if !ok {
		return nil, nil, false
	}
	id, ok := httpx.ParseUintID(r, "id")
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "invalid key id")
		return nil, nil, false
	}
	key, err := store.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		if model.IsNotFound(err) {
			httpx.WriteError(w, http.StatusNotFound, msgKeyNotFound)
			return nil, nil, false
		}
		writeInternalError(w, err)
		return nil, nil, false
	}
	if key.UserID != target.ID {
		httpx.WriteError(w, http.StatusNotFound, msgKeyNotFound)
		return nil, nil, false
	}
	return key, target, true
}

// enforceKeyLimit 按 settings.user_key_limit（默认 5，0 = 不限）约束用户名下密钥数量。
// 返回 (状态码, 提示文案, error)：状态码为 0 表示放行，error 非 nil 表示内部错误。
func enforceKeyLimit(ctx context.Context, store *model.Store, ownerID uint) (int, string, error) {
	st, err := store.GetSettings(ctx)
	if err != nil {
		return 0, "", err
	}
	limit := st.UserKeyLimit
	if limit <= 0 {
		return 0, "", nil
	}
	count, err := store.CountAPIKeysByOwner(ctx, ownerID)
	if err != nil {
		return 0, "", err
	}
	if count >= int64(limit) {
		return http.StatusBadRequest, fmt.Sprintf("key limit reached (limit %d)", limit), nil
	}
	return 0, "", nil
}

// validateModelScope 校验请求绑定均落在「该用户名下任意已有密钥的绑定模型集合」内。
// 返回 (是否通过, 提示文案, error)。
func validateModelScope(
	ctx context.Context,
	store *model.Store,
	ownerID uint,
	bindings []userKeyBindingRequest,
) (bool, string, error) {
	allowed, err := allowedModelIDs(ctx, store, ownerID)
	if err != nil {
		return false, "", err
	}
	if len(allowed) == 0 {
		return false, msgNoAvailableModels, nil
	}
	for _, b := range bindings {
		if _, ok := allowed[b.AIModelID]; !ok {
			return false, msgModelNotAvailable, nil
		}
	}
	return true, "", nil
}

// allowedModelIDs 计算用户可绑定的模型集合：该用户名下所有已有密钥的绑定模型并集
// （一次 JOIN 查询取回，避免按密钥逐条查询）。
func allowedModelIDs(ctx context.Context, store *model.Store, ownerID uint) (map[uint]struct{}, error) {
	ids, err := store.ListModelIDsByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	return allowed, nil
}
