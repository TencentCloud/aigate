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
	"net/url"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/queryparams"
)

// webhookResponse 是订阅的对外表示：secret 只写不读回，响应中恒为掩码。
type webhookResponse struct {
	ID         uint      `json:"id"`
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Secret     string    `json:"secret"`
	EventTypes []string  `json:"event_types"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// webhookDeliveryResponse 是投递记录的对外表示。
type webhookDeliveryResponse struct {
	ID         uint      `json:"id"`
	WebhookID  uint      `json:"webhook_id"`
	EventType  string    `json:"event_type"`
	Status     string    `json:"status"`
	Attempts   int       `json:"attempts"`
	LastStatus int       `json:"last_status"`
	LastError  string    `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// webhookCreateRequest 是创建请求；event_types 省略/为空数组表示订阅全部。
type webhookCreateRequest struct {
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Secret     string   `json:"secret"`
	EventTypes []string `json:"event_types"`
	Enabled    *bool    `json:"enabled"`
}

// webhookUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」；
// secret 为空串表示保持不变（不读回的字段不允许用空值清空）。
type webhookUpdateRequest struct {
	Name       *string   `json:"name"`
	URL        *string   `json:"url"`
	Secret     *string   `json:"secret"`
	EventTypes *[]string `json:"event_types"`
	Enabled    *bool     `json:"enabled"`
}

func handleCreateWebhook(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req webhookCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateWebhookURL(req.URL); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateWebhookSecret(req.Secret); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		types, err := normalizeEventTypes(req.EventTypes)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		encoded, err := json.Marshal(types)
		if err != nil {
			writeInternalError(w, err)
			return
		}

		hook := &model.Webhook{
			Name:       req.Name,
			URL:        req.URL,
			Secret:     req.Secret,
			EventTypes: string(encoded),
			Enabled:    true,
		}
		if err := store.CreateWebhook(r.Context(), hook); err != nil {
			writeInternalError(w, err)
			return
		}
		// enabled 带 default 标签，显式 false 在 INSERT 时会被 GORM 跳过，创建后补一次定向更新。
		if req.Enabled != nil && !*req.Enabled {
			updated, err := store.UpdateWebhook(r.Context(), hook.ID, map[string]any{"enabled": false})
			if err != nil {
				writeInternalError(w, err)
				return
			}
			hook = updated
		}
		writeWebhookResponse(w, http.StatusCreated, hook)
	}
}

func handleListWebhooks(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hooks, err := store.ListWebhooks(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]webhookResponse, 0, len(hooks))
		for i := range hooks {
			resp, err := toWebhookResponse(&hooks[i])
			if err != nil {
				writeInternalError(w, err)
				return
			}
			out = append(out, resp)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"webhooks": out})
	}
}

func handleGetWebhook(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid webhook id")
			return
		}
		hook, err := store.GetWebhook(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "webhook")
			return
		}
		writeWebhookResponse(w, http.StatusOK, hook)
	}
}

func handleUpdateWebhook(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid webhook id")
			return
		}
		if _, err := store.GetWebhook(r.Context(), id); err != nil {
			writeLookupError(w, err, "webhook")
			return
		}
		var req webhookUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}

		fields := make(map[string]any)
		if req.Name != nil {
			fields["name"] = *req.Name
		}
		if req.URL != nil {
			if err := validateWebhookURL(*req.URL); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["url"] = *req.URL
		}
		// 空串 = 保持原 secret（该字段不读回，无法回填）。
		if req.Secret != nil && *req.Secret != "" {
			if err := validateWebhookSecret(*req.Secret); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			fields["secret"] = *req.Secret
		}
		if req.EventTypes != nil {
			types, err := normalizeEventTypes(*req.EventTypes)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			encoded, err := json.Marshal(types)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			fields["event_types"] = string(encoded)
		}
		if req.Enabled != nil {
			fields["enabled"] = *req.Enabled
		}

		hook, err := store.UpdateWebhook(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "webhook")
			return
		}
		writeWebhookResponse(w, http.StatusOK, hook)
	}
}

func handleDeleteWebhook(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid webhook id")
			return
		}
		if _, err := store.GetWebhook(r.Context(), id); err != nil {
			writeLookupError(w, err, "webhook")
			return
		}
		if err := store.DeleteWebhook(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleListWebhookDeliveries 投递记录分页查询（默认 1/50，page_size 上限 500），按时间倒序。
func handleListWebhookDeliveries(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid webhook id")
			return
		}
		if _, err := store.GetWebhook(r.Context(), id); err != nil {
			writeLookupError(w, err, "webhook")
			return
		}
		page, pageSize, err := queryparams.ParsePagination(r)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		rows, total, err := store.ListWebhookDeliveries(r.Context(), id, page, pageSize)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]webhookDeliveryResponse, 0, len(rows))
		for i := range rows {
			out = append(out, toWebhookDeliveryResponse(&rows[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"deliveries": out,
			"total":      total,
			"page":       page,
			"page_size":  pageSize,
		})
	}
}

// writeWebhookResponse 写出订阅响应（event_types 解析失败视为内部错误）。
func writeWebhookResponse(w http.ResponseWriter, status int, hook *model.Webhook) {
	resp, err := toWebhookResponse(hook)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	httpx.WriteJSON(w, status, resp)
}

// toWebhookResponse 把订阅实体转为对外表示：secret 掩码，event_types 解析为数组。
func toWebhookResponse(hook *model.Webhook) (webhookResponse, error) {
	types, err := hook.GetEventTypes()
	if err != nil {
		return webhookResponse{}, err
	}
	if types == nil {
		types = []string{}
	}
	return webhookResponse{
		ID:         hook.ID,
		Name:       hook.Name,
		URL:        hook.URL,
		Secret:     model.MaskSecret(hook.Secret),
		EventTypes: types,
		Enabled:    hook.Enabled,
		CreatedAt:  hook.CreatedAt,
		UpdatedAt:  hook.UpdatedAt,
	}, nil
}

// toWebhookDeliveryResponse 把投递记录转为对外表示。
func toWebhookDeliveryResponse(d *model.WebhookDelivery) webhookDeliveryResponse {
	return webhookDeliveryResponse{
		ID:         d.ID,
		WebhookID:  d.WebhookID,
		EventType:  d.EventType,
		Status:     d.Status,
		Attempts:   d.Attempts,
		LastStatus: d.LastStatus,
		LastError:  d.LastError,
		CreatedAt:  d.CreatedAt,
		UpdatedAt:  d.UpdatedAt,
	}
}

// validateWebhookURL 校验订阅地址：必须为可解析的 http/https URL。
func validateWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("url must be a valid http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must be a valid http(s) URL")
	}
	if u.Host == "" {
		return fmt.Errorf("url must be a valid http(s) URL")
	}
	return nil
}

// validateWebhookSecret 校验签名密钥长度：≤256 字节（加密后仍可落在 VARCHAR(512) 列内）。
func validateWebhookSecret(secret string) error {
	if len(secret) > 256 {
		return fmt.Errorf("secret must be at most 256 bytes")
	}
	return nil
}

// normalizeEventTypes 校验并规范化事件类型数组：元素去空白，空串非法。
// 返回空切片表示订阅全部（落库为 "[]"）。
func normalizeEventTypes(types []string) ([]string, error) {
	out := make([]string, 0, len(types))
	for i, t := range types {
		v := strings.TrimSpace(t)
		if v == "" {
			return nil, fmt.Errorf("event_types[%d] must be a non-empty string", i)
		}
		out = append(out, v)
	}
	return out, nil
}
