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

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// modelResponse 是模型的对外表示：绝不包含 api_key。
type modelResponse struct {
	ID                uint              `json:"id"`
	Provider          string            `json:"provider"`
	ModelID           string            `json:"model_id"`
	ModelName         string            `json:"model_name"`
	URL               string            `json:"url"`
	CredentialID      uint              `json:"credential_id"`
	ModelType         string            `json:"model_type"`
	CustomHTTPHeaders map[string]string `json:"custom_http_headers"`
	QuotaDay          int               `json:"quota_day"`
	Enabled           bool              `json:"enabled"`
	// UserKeyRequired 用户自备 Key 模式：不保存共享密钥，转发密钥只取密钥绑定行；创建后不可变更。
	UserKeyRequired bool      `json:"user_key_required"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// modelCreateRequest 是创建请求；quota_day / enabled 缺省时分别取 -1 / true。
type modelCreateRequest struct {
	Provider          string           `json:"provider"`
	ModelID           string           `json:"model_id"`
	ModelName         string           `json:"model_name"`
	URL               string           `json:"url"`
	APIKey            string           `json:"api_key"`
	CredentialID      *uint            `json:"credential_id"`
	ModelType         string           `json:"model_type"`
	CustomHTTPHeaders *json.RawMessage `json:"custom_http_headers"`
	QuotaDay          *int             `json:"quota_day"`
	Enabled           *bool            `json:"enabled"`
	UserKeyRequired   *bool            `json:"user_key_required"`
}

// modelUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type modelUpdateRequest struct {
	Provider          *string          `json:"provider"`
	ModelID           *string          `json:"model_id"`
	ModelName         *string          `json:"model_name"`
	URL               *string          `json:"url"`
	APIKey            *string          `json:"api_key"`
	CredentialID      *uint            `json:"credential_id"`
	ModelType         *string          `json:"model_type"`
	CustomHTTPHeaders *json.RawMessage `json:"custom_http_headers"`
	QuotaDay          *int             `json:"quota_day"`
	Enabled           *bool            `json:"enabled"`
	UserKeyRequired   *bool            `json:"user_key_required"`
}

func handleCreateModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req modelCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if msg := validateModelCreate(&req); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		headers, _, err := resolveCustomHeaders(req.CustomHTTPHeaders)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			ok, err := credentialExists(r.Context(), store, *req.CredentialID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if !ok {
				httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("credential %d not found", *req.CredentialID))
				return
			}
		}
		// 自备 Key 模式标记（入参校验见 validateModelCreate）。
		userKeyRequired := req.UserKeyRequired != nil && *req.UserKeyRequired

		m := &model.AIModel{
			Provider:          req.Provider,
			ModelID:           req.ModelID,
			ModelName:         req.ModelName,
			URL:               req.URL,
			APIKey:            req.APIKey,
			ModelType:         req.ModelType,
			CustomHTTPHeaders: headers,
			QuotaDay:          -1,
			Enabled:           true,
		}
		if req.CredentialID != nil {
			m.CredentialID = *req.CredentialID
		}
		if req.QuotaDay != nil {
			m.QuotaDay = *req.QuotaDay
		}
		if req.Enabled != nil {
			m.Enabled = *req.Enabled
		}
		m.UserKeyRequired = userKeyRequired
		if err := store.CreateAIModel(r.Context(), m); err != nil {
			writeInternalError(w, err)
			return
		}
		// 带 default 标签的零值字段在 INSERT 时会被 GORM 跳过（enabled=false / quota_day=0），
		// 请求显式给出这些零值时补一次定向更新，避免落成库默认值。
		if patch := modelZeroValuePatch(req); len(patch) > 0 {
			updated, err := store.UpdateAIModel(r.Context(), m.ID, patch)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			m = updated
		}
		httpx.WriteJSON(w, http.StatusCreated, toModelResponse(m))
	}
}

func handleListModels(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListAIModels(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]modelResponse, 0, len(list))
		for i := range list {
			out = append(out, toModelResponse(&list[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": out})
	}
}

func handleGetModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		m, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toModelResponse(m))
	}
}

func handleUpdateModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		var req modelUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		before, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		if msg := validateModelUpdate(&req, before); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}

		fields, msg := modelUpdateFields(&req)
		if msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			ok, err := credentialExists(r.Context(), store, *req.CredentialID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if !ok {
				httpx.WriteError(
					w,
					http.StatusBadRequest,
					fmt.Sprintf("credential %d not found", *req.CredentialID),
				)
				return
			}
		}
		headers, present, err := resolveCustomHeaders(req.CustomHTTPHeaders)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if present {
			fields["custom_http_headers"] = headers
		}

		m, err := store.UpdateAIModel(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toModelResponse(m))
	}
}

func handleDeleteModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		if _, err := store.GetAIModel(r.Context(), id); err != nil {
			writeLookupError(w, err, "model")
			return
		}
		if err := store.DeleteAIModel(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toModelResponse 将模型实体转为对外表示（丢弃 api_key）。
func toModelResponse(m *model.AIModel) modelResponse {
	return modelResponse{
		ID:                m.ID,
		Provider:          m.Provider,
		ModelID:           m.ModelID,
		ModelName:         m.ModelName,
		URL:               m.URL,
		CredentialID:      m.CredentialID,
		ModelType:         m.ModelType,
		CustomHTTPHeaders: m.GetCustomHTTPHeaders(),
		QuotaDay:          m.QuotaDay,
		Enabled:           m.Enabled,
		UserKeyRequired:   m.UserKeyRequired,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}

// validateModelCreate 校验模型创建请求的纯字段约束，返回错误消息（空串表示通过）。
// 依赖存储的校验（凭据存在性等）留在 handler 内执行。
func validateModelCreate(req *modelCreateRequest) string {
	if strings.TrimSpace(req.ModelID) == "" {
		return "model_id is required"
	}
	if strings.TrimSpace(req.URL) == "" {
		return "url is required"
	}
	if !validModelType(req.ModelType) {
		return "model_type must be openai-completions or anthropic-messages"
	}
	// 自备 Key 模式：不保存共享密钥（照搬 hatchery：UserKeyRequired=true 时 api_key 必须为空）。
	if req.UserKeyRequired != nil && *req.UserKeyRequired {
		if strings.TrimSpace(req.APIKey) != "" {
			return "user_key_required models must not carry a shared api_key"
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			return "user_key_required models must not reference a credential"
		}
	}
	return ""
}

// validateModelUpdate 校验模型更新请求中与既有状态相关的纯约束，返回错误消息（空串表示通过）。
func validateModelUpdate(req *modelUpdateRequest, before *model.AIModel) string {
	// 自备 Key 模式创建后不可变更（照搬 hatchery「创建时确定，编辑时不可转换」）。
	if req.UserKeyRequired != nil && *req.UserKeyRequired != before.UserKeyRequired {
		return "user_key_required cannot be changed after creation"
	}
	// 自备 Key 模型不保存共享密钥：拒绝携带 api_key 或凭据引用。
	if before.UserKeyRequired {
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			return "user_key_required models must not carry a shared api_key"
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			return "user_key_required models must not reference a credential"
		}
	}
	return ""
}

// modelUpdateFields 汇总局部更新的字段集；入场校验（空值、类型）失败时返回错误消息。
func modelUpdateFields(req *modelUpdateRequest) (map[string]any, string) {
	fields := make(map[string]any)
	if req.Provider != nil {
		fields["provider"] = *req.Provider
	}
	if req.ModelID != nil {
		if strings.TrimSpace(*req.ModelID) == "" {
			return nil, "model_id must not be empty"
		}
		fields["model_id"] = *req.ModelID
	}
	if req.ModelName != nil {
		fields["model_name"] = *req.ModelName
	}
	if req.URL != nil {
		if strings.TrimSpace(*req.URL) == "" {
			return nil, "url must not be empty"
		}
		fields["url"] = *req.URL
	}
	if req.ModelType != nil {
		if !validModelType(*req.ModelType) {
			return nil, "model_type must be openai-completions or anthropic-messages"
		}
		fields["model_type"] = *req.ModelType
	}
	if req.APIKey != nil && *req.APIKey != "" {
		fields["api_key"] = *req.APIKey
	}
	if req.CredentialID != nil {
		fields["credential_id"] = *req.CredentialID
	}
	if req.QuotaDay != nil {
		fields["quota_day"] = *req.QuotaDay
	}
	if req.Enabled != nil {
		fields["enabled"] = *req.Enabled
	}
	return fields, ""
}

// validModelType 校验模型接口类型取值。
func validModelType(t string) bool {
	return t == model.ModelTypeOpenAI || t == model.ModelTypeAnthropic
}

// modelZeroValuePatch 返回创建后需要补写的零值字段（见 handleCreateModel 中说明）。
func modelZeroValuePatch(req modelCreateRequest) map[string]any {
	patch := make(map[string]any)
	if req.Enabled != nil && !*req.Enabled {
		patch["enabled"] = false
	}
	if req.QuotaDay != nil && *req.QuotaDay == 0 {
		patch["quota_day"] = 0
	}
	return patch
}

// resolveCustomHeaders 归一化 custom_http_headers：
// 返回值 (存储用 JSON 字符串, 请求中是否出现, 错误)。未出现时返回 ("", false, nil)。
func resolveCustomHeaders(raw *json.RawMessage) (string, bool, error) {
	if raw == nil {
		return "", false, nil
	}
	if strings.TrimSpace(string(*raw)) == "null" {
		return "", true, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(*raw, &headers); err != nil {
		return "", false, fmt.Errorf("custom_http_headers must be a JSON object of string values")
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return "", false, fmt.Errorf("custom_http_headers cannot be encoded")
	}
	return string(encoded), true, nil
}

// writeLookupError 统一处理「查找/更新目标」错误：不存在 → 404，其余 → 500。
func writeLookupError(w http.ResponseWriter, err error, name string) {
	if model.IsNotFound(err) {
		httpx.WriteError(w, http.StatusNotFound, name+" not found")
		return
	}
	writeInternalError(w, err)
}
