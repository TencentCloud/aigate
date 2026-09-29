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

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/quota"
)

// 全局配额作用域取值。
const (
	quotaScopeSite  = "site"
	quotaScopeGroup = "group"
)

type settingsResponse struct {
	DefaultTokenQuotaRules string `json:"default_token_quota_rules"`
	GlobalTokenQuotaRules  string `json:"global_token_quota_rules"`
	GlobalQuotaScope       string `json:"global_quota_scope"`
	UserKeyLimit           int    `json:"user_key_limit"`
}

// settingsUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type settingsUpdateRequest struct {
	DefaultTokenQuotaRules *string `json:"default_token_quota_rules"`
	GlobalTokenQuotaRules  *string `json:"global_token_quota_rules"`
	GlobalQuotaScope       *string `json:"global_quota_scope"`
	UserKeyLimit           *int    `json:"user_key_limit"`
}

func handleGetSettings(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := store.GetSettings(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toSettingsResponse(st))
	}
}

func handleUpdateSettings(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req settingsUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		fields := make(map[string]any)
		if req.DefaultTokenQuotaRules != nil {
			normalized, err := normalizeRulesJSON("default_token_quota_rules", *req.DefaultTokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["default_token_quota_rules"] = normalized
		}
		if req.GlobalTokenQuotaRules != nil {
			normalized, err := normalizeRulesJSON("global_token_quota_rules", *req.GlobalTokenQuotaRules)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["global_token_quota_rules"] = normalized
		}
		if req.GlobalQuotaScope != nil {
			scope := *req.GlobalQuotaScope
			if scope != quotaScopeSite && scope != quotaScopeGroup {
				httpx.WriteError(w, http.StatusBadRequest, "global_quota_scope must be site or group")
				return
			}
			fields["global_quota_scope"] = scope
		}
		if req.UserKeyLimit != nil {
			if *req.UserKeyLimit < 0 {
				httpx.WriteError(w, http.StatusBadRequest, "user_key_limit must be >= 0")
				return
			}
			fields["user_key_limit"] = *req.UserKeyLimit
		}

		// 先确保单例设置行存在（model 层的更新不做 upsert，否则首次 PUT 会静默丢弃）。
		if _, err := store.GetSettings(r.Context()); err != nil {
			writeInternalError(w, err)
			return
		}
		st, err := store.UpdateSettings(r.Context(), fields)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toSettingsResponse(st))
	}
}

func toSettingsResponse(st *model.Setting) settingsResponse {
	return settingsResponse{
		DefaultTokenQuotaRules: st.DefaultTokenQuotaRules,
		GlobalTokenQuotaRules:  st.GlobalTokenQuotaRules,
		GlobalQuotaScope:       st.GlobalQuotaScope,
		UserKeyLimit:           st.UserKeyLimit,
	}
}

// normalizeRulesJSON 校验并规范化配额规则文本。
//
// 入参必须是非空的 JSON 数组文本（拒绝空串、null 与非数组），并通过 quota.NormalizeRules
// 完成规则级校验（重复 mode、limit<-1、end<=start、custom start 自动填充）与规范化序列化。
func normalizeRulesJSON(name, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%s must not be empty (use [] for explicit unlimited)", name)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &arr); err != nil || arr == nil {
		return "", fmt.Errorf("%s must be a valid JSON array", name)
	}
	normalized, err := quota.NormalizeRules(trimmed)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return normalized, nil
}
