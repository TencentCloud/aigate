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
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// groupResponse 是分组的对外表示。
// TokenQuotaRules 语义：空串 = 未配置（回退全局默认）；"[]" = 显式不限；其余为规范化后的规则 JSON。
type groupResponse struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	TokenQuotaRules string `json:"token_quota_rules"`
	// GlobalTokenQuotaRules 组级全局配额规则：空串 = 未配置（回退站点规则）；"[]" = 显式不限。
	GlobalTokenQuotaRules string    `json:"global_token_quota_rules"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// groupCreateRequest 是创建请求；token_quota_rules 省略或空串表示未配置。
type groupCreateRequest struct {
	Name                  string  `json:"name"`
	TokenQuotaRules       *string `json:"token_quota_rules"`
	GlobalTokenQuotaRules *string `json:"global_token_quota_rules"`
}

// groupUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type groupUpdateRequest struct {
	Name                  *string `json:"name"`
	TokenQuotaRules       *string `json:"token_quota_rules"`
	GlobalTokenQuotaRules *string `json:"global_token_quota_rules"`
}

func handleCreateGroup(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req groupCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		rules := ""
		if req.TokenQuotaRules != nil {
			normalized, err := normalizeGroupRules(*req.TokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			rules = normalized
		}
		globalRules := ""
		if req.GlobalTokenQuotaRules != nil {
			normalized, err := normalizeGroupRules(*req.GlobalTokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			globalRules = normalized
		}
		g := &model.Group{Name: req.Name, TokenQuotaRules: rules, GlobalTokenQuotaRules: globalRules}
		if err := store.CreateGroup(r.Context(), g); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, toGroupResponse(g))
	}
}

func handleListGroups(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		groups, err := store.ListGroups(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]groupResponse, 0, len(groups))
		for i := range groups {
			out = append(out, toGroupResponse(&groups[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"user_groups": out})
	}
}

func handleGetGroup(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := store.GetGroup(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "group")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toGroupResponse(g))
	}
}

func handleUpdateGroup(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid group id")
			return
		}
		if _, err := store.GetGroup(r.Context(), id); err != nil {
			writeLookupError(w, err, "group")
			return
		}
		var req groupUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		fields := make(map[string]any)
		if req.Name != nil {
			fields["name"] = *req.Name
		}
		if req.TokenQuotaRules != nil {
			normalized, err := normalizeGroupRules(*req.TokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["token_quota_rules"] = normalized
		}
		if req.GlobalTokenQuotaRules != nil {
			normalized, err := normalizeGroupRules(*req.GlobalTokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["global_token_quota_rules"] = normalized
		}
		g, err := store.UpdateGroup(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "group")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toGroupResponse(g))
	}
}

// handleDeleteGroup 删除分组本身；不级联用户/密钥（引用该组的 user.group_id 保留为悬空值，
// 读侧按「组不存在」回退全局默认规则）。
func handleDeleteGroup(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid group id")
			return
		}
		if _, err := store.GetGroup(r.Context(), id); err != nil {
			writeLookupError(w, err, "group")
			return
		}
		if err := store.DeleteGroup(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toGroupResponse 将分组实体转为对外表示。
func toGroupResponse(g *model.Group) groupResponse {
	return groupResponse{
		ID:                    g.ID,
		Name:                  g.Name,
		TokenQuotaRules:       g.TokenQuotaRules,
		GlobalTokenQuotaRules: g.GlobalTokenQuotaRules,
		CreatedAt:             g.CreatedAt,
		UpdatedAt:             g.UpdatedAt,
	}
}

// normalizeGroupRules 规范化分组策略文本：
//   - 空串（或仅空白）= 未配置，原样返回空串（读侧回退全局默认）；
//   - 其余非空值必须是合法 JSON 数组，并经配额规则校验与规范化后返回。
func normalizeGroupRules(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return normalizeRulesJSON("token_quota_rules", raw)
}
