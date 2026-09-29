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

// Package dto 存放管理面（admin）与用户面（userapi）共用的对外数据结构与组装逻辑，
// 保证两侧的 JSON 契约（密钥对象、绑定请求）只有一个来源、不会漂移。
package dto

import (
	"context"
	"time"

	"aigate/internal/model"
)

// KeyResponse 是密钥的对外表示：绝不含明文 token 与 token 哈希。
// Token 仅在创建/rotate 响应中填充一次（其余场景为空串，经 omitempty 省略）。
type KeyResponse struct {
	ID               uint   `json:"id"`
	Name             string `json:"name"`
	UserID           uint   `json:"user_id"`
	GroupID          uint   `json:"group_id"`
	ProxyTokenPrefix string `json:"proxy_token_prefix"`
	MaxTokens        int    `json:"max_tokens"`
	// TokenQuotaRules 密钥级周期配额（空串 = 未配置/不限制）；仅管理面可写，用户面只读。
	TokenQuotaRules string              `json:"token_quota_rules"`
	Enabled         bool                `json:"enabled"`
	ExpiresAt       *time.Time          `json:"expires_at"` // 可选有效期（UTC）；null = 永不过期（仅管理面可写，用户面只读）
	ExpiredAt       *time.Time          `json:"expired_at"` // 首次因过期被拒绝的观测时间；null = 未发生
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Bindings        []model.BindingInfo `json:"bindings"`
	Token           string              `json:"token,omitempty"`
}

// BindingRequest 是绑定关系请求体（恰好一个 primary 由 model 层校验）。
// APIKey 为可选绑定级上游密钥（仅 user_key_required 模型生效；加密落库、不回显）。
type BindingRequest struct {
	AIModelID uint   `json:"ai_model_id"`
	Role      string `json:"role"`
	SortOrder int    `json:"sort_order"`
	APIKey    string `json:"api_key"`
}

// NewKeyResponse 由实体与绑定视图组装响应（列表路径批量取回绑定后复用）；
// 绑定为空时返回空数组（不是 null）。
func NewKeyResponse(k *model.APIKey, bindings []model.BindingInfo) KeyResponse {
	if bindings == nil {
		bindings = []model.BindingInfo{}
	}
	return KeyResponse{
		ID:               k.ID,
		Name:             k.Name,
		UserID:           k.UserID,
		GroupID:          k.GroupID,
		ProxyTokenPrefix: k.ProxyTokenPrefix,
		MaxTokens:        k.MaxTokens,
		TokenQuotaRules:  k.TokenQuotaRules,
		Enabled:          k.Enabled,
		ExpiresAt:        k.ExpiresAt,
		ExpiredAt:        k.ExpiredAt,
		CreatedAt:        k.CreatedAt,
		UpdatedAt:        k.UpdatedAt,
		Bindings:         bindings,
	}
}

// BuildKeyResponse 组装密钥对外表示（含绑定视图；查询失败返回错误）。
func BuildKeyResponse(ctx context.Context, store *model.Store, k *model.APIKey) (KeyResponse, error) {
	bindings, err := store.ListKeyModels(ctx, k.ID, false)
	if err != nil {
		return KeyResponse{}, err
	}
	return NewKeyResponse(k, bindings), nil
}

// ListKeyResponses 批量组装密钥对外表示：一次取回全部绑定视图（避免按密钥逐条查询的 N+1）。
func ListKeyResponses(ctx context.Context, store *model.Store, keys []model.APIKey) ([]KeyResponse, error) {
	ids := make([]uint, 0, len(keys))
	for i := range keys {
		ids = append(ids, keys[i].ID)
	}
	bindingsByKey, err := store.ListKeyModelsByKeyIDs(ctx, ids, false)
	if err != nil {
		return nil, err
	}
	out := make([]KeyResponse, 0, len(keys))
	for i := range keys {
		out = append(out, NewKeyResponse(&keys[i], bindingsByKey[keys[i].ID]))
	}
	return out, nil
}

// ToAPIKeyModels 将请求绑定转为存储层实体。
func ToAPIKeyModels(reqs []BindingRequest) []model.APIKeyModel {
	out := make([]model.APIKeyModel, 0, len(reqs))
	for _, b := range reqs {
		out = append(out, model.APIKeyModel{
			AIModelID: b.AIModelID,
			Role:      b.Role,
			SortOrder: b.SortOrder,
			APIKey:    b.APIKey,
		})
	}
	return out
}
