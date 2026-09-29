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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aigate/internal/dto"
	"aigate/internal/event"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// key.disabled 事件的原因取值。
const (
	keyDisabledReasonDisabled = "disabled"
	keyDisabledReasonDeleted  = "deleted"
)

// keyResponse 是密钥的对外表示：绝不含明文 token 与 token 哈希。
// Token 仅在创建/rotate 响应中填充一次（其余场景为空串，经 omitempty 省略）。
type keyResponse = dto.KeyResponse

// bindingRequest 是绑定关系请求体。
type bindingRequest = dto.BindingRequest

type keyCreateRequest struct {
	Name            string           `json:"name"`
	UserID          uint             `json:"user_id"`
	GroupID         uint             `json:"group_id"`
	MaxTokens       int              `json:"max_tokens"`
	TokenQuotaRules *string          `json:"token_quota_rules"` // 可选：密钥级周期配额（空串 = 不限制）
	ExpiresAt       *string          `json:"expires_at"`        // 可选，RFC3339；缺省/null = 永不过期
	Bindings        []bindingRequest `json:"bindings"`
}

// keyUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」；
// bindings 为指针，非 nil 时整体替换（含空数组 → 清空绑定）；
// expires_at 为 RawMessage：缺省 = 不变，显式 null = 清除。
type keyUpdateRequest struct {
	Name            *string           `json:"name"`
	MaxTokens       *int              `json:"max_tokens"`
	TokenQuotaRules *string           `json:"token_quota_rules"`
	Enabled         *bool             `json:"enabled"`
	ExpiresAt       json.RawMessage   `json:"expires_at"`
	Bindings        *[]bindingRequest `json:"bindings"`
}

func handleCreateKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req keyCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		expiresAt, err := parseCreateExpiresAt(req.ExpiresAt)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		rules := ""
		if req.TokenQuotaRules != nil {
			normalized, err := normalizeKeyRules(*req.TokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			rules = normalized
		}
		if _, err := store.GetUserByID(r.Context(), req.UserID); err != nil {
			if model.IsNotFound(err) {
				httpx.WriteError(w, http.StatusBadRequest, "user_id does not exist")
				return
			}
			writeInternalError(w, err)
			return
		}

		plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		key := &model.APIKey{
			Name:             req.Name,
			UserID:           req.UserID,
			GroupID:          req.GroupID,
			ProxyTokenHash:   hash,
			ProxyTokenPrefix: prefix,
			MaxTokens:        req.MaxTokens,
			TokenQuotaRules:  rules,
			Enabled:          true,
			ExpiresAt:        expiresAt,
		}
		// 绑定校验（恰好一个 primary、模型存在）交由 model 层：参数错误 400，内部错误 500。
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

func handleListKeys(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := store.ListAPIKeys(r.Context())
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

func handleGetKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid key id")
			return
		}
		key, err := store.GetAPIKeyByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "key")
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

func handleUpdateKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid key id")
			return
		}
		before, err := store.GetAPIKeyByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "key")
			return
		}
		var req keyUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 先替换绑定（校验失败不留半更新），再更新标量字段。
		if req.Bindings != nil {
			if err := store.ReplaceBindings(r.Context(), id, dto.ToAPIKeyModels(*req.Bindings)); err != nil {
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
		if req.TokenQuotaRules != nil {
			normalized, err := normalizeKeyRules(*req.TokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["token_quota_rules"] = normalized
		}
		if req.Enabled != nil {
			fields["enabled"] = *req.Enabled
		}
		if len(req.ExpiresAt) > 0 {
			val, clear, err := parseUpdateExpiresAt(req.ExpiresAt)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			if clear {
				fields["expires_at"] = nil
			} else {
				fields["expires_at"] = val
			}
			// 有效期变更 → 过期观测随生命周期重置（续期后再次过期会重新发出事件）。
			fields["expired_at"] = nil
		}
		key, err := store.UpdateAPIKey(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "key")
			return
		}
		resp, err := dto.BuildKeyResponse(r.Context(), store, key)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
		// 仅在「启用 → 禁用」的真实跃迁上发出事件（已是禁用态不重复发）。
		if before.Enabled && !key.Enabled {
			emitKeyDisabled(key, keyDisabledReasonDisabled)
		}
	}
}

func handleDeleteKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid key id")
			return
		}
		key, err := store.GetAPIKeyByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "key")
			return
		}
		if err := store.DeleteAPIKey(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		emitKeyDisabled(key, keyDisabledReasonDeleted)
	}
}

func handleRotateKey(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid key id")
			return
		}
		key, err := store.GetAPIKeyByID(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "key")
			return
		}
		plain, displayPrefix, err := store.RotateAPIKey(r.Context(), id)
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

// emitKeyDisabled 发出 key.disabled 事件（reason 为 disabled/deleted）。
// 埋点只调 event.Emit，未装配投递器时为 no-op，不影响管理面响应。
func emitKeyDisabled(k *model.APIKey, reason string) {
	event.Emit(event.TypeKeyDisabled, map[string]any{
		"key_id":  k.ID,
		"name":    k.Name,
		"user_id": k.UserID,
		"reason":  reason,
	})
}

// parseCreateExpiresAt 解析创建请求的 expires_at（可选，RFC3339；空串视为未设置）。
func parseCreateExpiresAt(raw *string) (*time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*raw))
	if err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC3339 timestamp")
	}
	utc := t.UTC()
	return &utc, nil
}

// parseUpdateExpiresAt 解析更新请求的 expires_at：显式 null = 清除；RFC3339 = 设置；其余非法。
func parseUpdateExpiresAt(raw json.RawMessage) (val *time.Time, clear bool, err error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var s string
	if e := json.Unmarshal(raw, &s); e != nil {
		return nil, false, fmt.Errorf("expires_at must be an RFC3339 timestamp or null")
	}
	t, e := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if e != nil {
		return nil, false, fmt.Errorf("expires_at must be an RFC3339 timestamp or null")
	}
	utc := t.UTC()
	return &utc, false, nil
}

// normalizeKeyRules 规范化密钥级配额文本：
//   - 空串（或仅空白）= 清除/未配置，原样返回空串（检查时不限制）；
//   - 其余非空值必须是合法 JSON 数组，并经配额规则校验与规范化后返回。
func normalizeKeyRules(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return normalizeRulesJSON("token_quota_rules", raw)
}
