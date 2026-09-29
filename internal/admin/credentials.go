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
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// credentialResponse 是凭据的对外表示：secrets 中的 value 一律为掩码，绝不回显明文。
type credentialResponse struct {
	ID          uint               `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Secrets     []model.SecretItem `json:"secrets"`
	Enabled     bool               `json:"enabled"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// credentialCreateRequest 是创建请求；enabled 缺省为 true。
type credentialCreateRequest struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Secrets     []model.SecretItem `json:"secrets"`
	Enabled     *bool              `json:"enabled"`
}

// credentialUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type credentialUpdateRequest struct {
	Name        *string             `json:"name"`
	Description *string             `json:"description"`
	Secrets     *[]model.SecretItem `json:"secrets"`
	Enabled     *bool               `json:"enabled"`
}

func handleCreateCredential(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req credentialCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			httpx.WriteError(w, http.StatusBadRequest, "name is required")
			return
		}
		secrets, err := normalizeSecrets(req.Secrets)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		c := &model.Credential{
			Name:        req.Name,
			Description: req.Description,
			Enabled:     true,
		}
		if err := c.SetSecrets(secrets); err != nil {
			writeInternalError(w, err)
			return
		}
		if err := store.CreateCredential(r.Context(), c); err != nil {
			writeInternalError(w, err)
			return
		}
		// enabled 带 default 标签，显式 false 在 INSERT 时会被 GORM 跳过，创建后补一次定向更新。
		if req.Enabled != nil && !*req.Enabled {
			updated, err := store.UpdateCredential(r.Context(), c.ID, map[string]any{"enabled": false})
			if err != nil {
				writeInternalError(w, err)
				return
			}
			c = updated
		}
		writeCredentialResponse(w, http.StatusCreated, c)
	}
}

func handleListCredentials(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListCredentials(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]credentialResponse, 0, len(list))
		for i := range list {
			resp, err := toCredentialResponse(&list[i])
			if err != nil {
				writeInternalError(w, err)
				return
			}
			out = append(out, resp)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
	}
}

func handleGetCredential(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid credential id")
			return
		}
		c, err := store.GetCredentialByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "credential")
			return
		}
		writeCredentialResponse(w, http.StatusOK, c)
	}
}

func handleUpdateCredential(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid credential id")
			return
		}
		current, err := store.GetCredentialByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "credential")
			return
		}
		var req credentialUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}

		fields := make(map[string]any)
		if req.Name != nil {
			if strings.TrimSpace(*req.Name) == "" {
				httpx.WriteError(w, http.StatusBadRequest, "name must not be empty")
				return
			}
			fields["name"] = *req.Name
		}
		if req.Description != nil {
			fields["description"] = *req.Description
		}
		if req.Enabled != nil {
			fields["enabled"] = *req.Enabled
		}
		if req.Secrets != nil {
			// 防「GET（返回掩码）→ 直接 PUT 回写」把真实密钥覆盖成掩码串：
			// 值等于现有掩码的条目视为未修改，回填现有明文；其余条目照常替换。
			existing, err := current.GetSecrets()
			if err != nil {
				writeInternalError(w, err)
				return
			}
			secrets, err := normalizeSecrets(preserveMaskedSecrets(*req.Secrets, existing))
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			sealed, err := model.SealSecrets(secrets)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			fields["secrets_json"] = sealed
		}

		c, err := store.UpdateCredential(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "credential")
			return
		}
		writeCredentialResponse(w, http.StatusOK, c)
	}
}

// handleDeleteCredential 删除凭据；仍被模型引用时拒绝（409），提示先解除引用。
func handleDeleteCredential(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid credential id")
			return
		}
		if _, err := store.GetCredentialByID(r.Context(), id); err != nil {
			writeLookupError(w, err, "credential")
			return
		}
		refs, err := store.CountModelCredentialRefs(r.Context(), id)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if refs > 0 {
			httpx.WriteError(w, http.StatusConflict,
				fmt.Sprintf("credential is referenced by %d model(s); detach them before deleting", refs))
			return
		}
		if err := store.DeleteCredential(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// writeCredentialResponse 掩码后写出凭据响应（掩码失败视为内部错误）。
func writeCredentialResponse(w http.ResponseWriter, status int, c *model.Credential) {
	resp, err := toCredentialResponse(c)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	httpx.WriteJSON(w, status, resp)
}

// toCredentialResponse 将凭据实体转为对外表示（secrets 仅返回掩码值）。
func toCredentialResponse(c *model.Credential) (credentialResponse, error) {
	secrets, err := c.GetMaskedSecrets()
	if err != nil {
		return credentialResponse{}, err
	}
	return credentialResponse{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		Secrets:     secrets,
		Enabled:     c.Enabled,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}, nil
}

// preserveMaskedSecrets 把「值恰等于现有密钥掩码」的入参条目视为未修改，回填现有明文：
// 客户端 GET 拿到的是掩码（如 sk-1****6789），若原样 PUT 回写会把真实密钥覆盖成掩码串。
// 非掩码值原样保留（正常替换）；新增 key 不受影响。
func preserveMaskedSecrets(incoming, existing []model.SecretItem) []model.SecretItem {
	if len(existing) == 0 {
		return incoming
	}
	maskByKey := make(map[string]string, len(existing))
	plainByKey := make(map[string]string, len(existing))
	for _, it := range existing {
		maskByKey[it.Key] = model.MaskSecret(it.Value)
		plainByKey[it.Key] = it.Value
	}
	out := make([]model.SecretItem, len(incoming))
	copy(out, incoming)
	for i := range out {
		mask, ok := maskByKey[out[i].Key]
		if !ok || mask == "" {
			continue
		}
		if out[i].Value == mask {
			out[i].Value = plainByKey[out[i].Key]
		}
	}
	return out
}

// normalizeSecrets 校验并规范化密钥项：
// key 必填；location 仅支持 header（省略时按 header 补齐）；value 允许为空。
func normalizeSecrets(items []model.SecretItem) ([]model.SecretItem, error) {
	out := make([]model.SecretItem, 0, len(items))
	for i, it := range items {
		key := strings.TrimSpace(it.Key)
		if key == "" {
			return nil, fmt.Errorf("secrets[%d].key is required", i)
		}
		loc := strings.ToLower(strings.TrimSpace(it.Location))
		if loc == "" {
			loc = model.SecretLocationHeader
		}
		if loc != model.SecretLocationHeader {
			return nil, fmt.Errorf("secrets[%d].location must be %q", i, model.SecretLocationHeader)
		}
		out = append(out, model.SecretItem{Key: key, Value: it.Value, Location: loc})
	}
	return out, nil
}

// credentialExists 判断凭据是否存在；不存在返回 (false, nil)，其它错误原样返回。
func credentialExists(ctx context.Context, store *model.Store, id uint) (bool, error) {
	if _, err := store.GetCredentialByID(ctx, id); err != nil {
		if model.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup credential %d: %w", id, err)
	}
	return true, nil
}
