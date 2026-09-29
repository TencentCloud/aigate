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

package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"aigate/internal/crypto"
)

// 投递记录状态。
const (
	// DeliveryStatusPending 排队中 / 重试中（非终态）。
	DeliveryStatusPending = "pending"
	// DeliveryStatusSuccess 投递成功（收到 2xx）。
	DeliveryStatusSuccess = "success"
	// DeliveryStatusFailed 重试耗尽（终态）。
	DeliveryStatusFailed = "failed"
)

// emptyEventTypes 是「订阅全部事件」的落库表示。
const emptyEventTypes = "[]"

// Webhook 是事件订阅：事件触发时按 url 投递，secret 用于 HMAC 签名。
// EventTypes 为 JSON 字符串数组（"[]" 表示订阅全部）；secret 落库为 enc:v1: 密文
// （未配置主密钥时明文），读侧一律掩码，明文不落响应。
type Webhook struct {
	ID         uint      `gorm:"primaryKey"`
	Name       string    `gorm:"size:128;not null;default:''"`
	URL        string    `gorm:"size:512;not null;default:''"`
	Secret     string    `gorm:"size:512;not null;default:''"`
	EventTypes string    `gorm:"type:text;not null"`
	Enabled    bool      `gorm:"not null;default:true"`
	CreatedAt  time.Time `gorm:"not null"`
	UpdatedAt  time.Time `gorm:"not null"`
}

// WebhookDelivery 是一次事件投递的记录：每次尝试更新 attempts/last_status/last_error，
// 成功或耗尽后落终态 status（success/failed），重试期间保持 pending。
type WebhookDelivery struct {
	ID         uint      `gorm:"primaryKey"`
	WebhookID  uint      `gorm:"not null;index"`
	EventType  string    `gorm:"size:64;not null;default:''"`
	Payload    string    `gorm:"type:text;not null"`
	Status     string    `gorm:"size:16;not null;default:'pending'"`
	Attempts   int       `gorm:"not null;default:0"`
	LastStatus int       `gorm:"not null;default:0"`
	LastError  string    `gorm:"size:512;not null;default:''"`
	CreatedAt  time.Time `gorm:"not null;index"`
	UpdatedAt  time.Time `gorm:"not null"`
}

// GetEventTypes 解析订阅的事件类型列表；空串或 "[]" 返回空切片（表示订阅全部）。
// JSON 非法时返回错误（调用方按不可投递处理，不静默放行）。
func (w *Webhook) GetEventTypes() ([]string, error) {
	raw := strings.TrimSpace(w.EventTypes)
	if raw == "" {
		return nil, nil
	}
	var types []string
	if err := json.Unmarshal([]byte(raw), &types); err != nil {
		return nil, fmt.Errorf("parse webhook %d event_types: %w", w.ID, err)
	}
	return types, nil
}

// Subscribes 判断该订阅是否接收指定事件类型：enabled 且（event_types 为空 或 含该类型）。
// event_types 解析失败时返回 false（订阅配置损坏时不投递）。
func (w *Webhook) Subscribes(eventType string) bool {
	if !w.Enabled {
		return false
	}
	types, err := w.GetEventTypes()
	if err != nil {
		return false
	}
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if t == eventType {
			return true
		}
	}
	return false
}

// CreateWebhook 创建订阅；event_types 为空时落库为 "[]"（订阅全部）。
// secret 加密后落库；传入实体的 Secret 保持明文（调用方展示/回显用）。
func (s *Store) CreateWebhook(ctx context.Context, w *Webhook) error {
	if strings.TrimSpace(w.EventTypes) == "" {
		w.EventTypes = emptyEventTypes
	}
	row := *w
	sealed, err := sealIfPresent(row.Secret)
	if err != nil {
		return fmt.Errorf("seal webhook secret: %w", err)
	}
	row.Secret = sealed
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	w.ID = row.ID
	w.CreatedAt = row.CreatedAt
	w.UpdatedAt = row.UpdatedAt
	return nil
}

// GetWebhook 按 ID 查询订阅（secret 已解密）。
func (s *Store) GetWebhook(ctx context.Context, id uint) (*Webhook, error) {
	var w Webhook
	if err := s.db.WithContext(ctx).First(&w, id).Error; err != nil {
		return nil, err
	}
	if err := w.decryptSecret(); err != nil {
		return nil, err
	}
	return &w, nil
}

// ListWebhooks 列出全部订阅（按 ID 升序；secret 已解密）。
func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	var hooks []Webhook
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&hooks).Error; err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	for i := range hooks {
		if err := hooks[i].decryptSecret(); err != nil {
			return nil, err
		}
	}
	return hooks, nil
}

// ListEnabledWebhooks 列出已启用的订阅（投递前过滤用；secret 已解密）。
func (s *Store) ListEnabledWebhooks(ctx context.Context) ([]Webhook, error) {
	var hooks []Webhook
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("id ASC").Find(&hooks).Error; err != nil {
		return nil, fmt.Errorf("list enabled webhooks: %w", err)
	}
	for i := range hooks {
		if err := hooks[i].decryptSecret(); err != nil {
			return nil, err
		}
	}
	return hooks, nil
}

// UpdateWebhook 按字段更新订阅并返回更新后的实体（secret 字段加密后落库）。
func (s *Store) UpdateWebhook(ctx context.Context, id uint, fields map[string]any) (*Webhook, error) {
	if raw, ok := fields["secret"].(string); ok {
		sealed, err := sealIfPresent(raw)
		if err != nil {
			return nil, fmt.Errorf("seal webhook secret: %w", err)
		}
		fields["secret"] = sealed
	}
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&Webhook{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update webhook: %w", err)
		}
	}
	return s.GetWebhook(ctx, id)
}

// decryptSecret 就地解密 secret 字段（列内为密文时）。
func (w *Webhook) decryptSecret() error {
	plain, err := crypto.Open(w.Secret)
	if err != nil {
		return fmt.Errorf("decrypt webhook %d secret: %w", w.ID, err)
	}
	w.Secret = plain
	return nil
}

// DeleteWebhook 删除订阅并级联清理其投递记录（同事务）。
func (s *Store) DeleteWebhook(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("webhook_id = ?", id).Delete(&WebhookDelivery{}).Error; err != nil {
			return fmt.Errorf("delete webhook deliveries: %w", err)
		}
		if err := tx.Delete(&Webhook{}, id).Error; err != nil {
			return fmt.Errorf("delete webhook: %w", err)
		}
		return nil
	})
}

// CreateWebhookDelivery 落一条投递记录（投递前调用，ID 即投递 ID）。
func (s *Store) CreateWebhookDelivery(ctx context.Context, d *WebhookDelivery) error {
	if err := s.db.WithContext(ctx).Create(d).Error; err != nil {
		return fmt.Errorf("create webhook delivery: %w", err)
	}
	return nil
}

// UpdateWebhookDelivery 按字段更新投递记录并返回更新后的实体。
func (s *Store) UpdateWebhookDelivery(ctx context.Context, id uint, fields map[string]any) (*WebhookDelivery, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update webhook delivery: %w", err)
		}
	}
	var d WebhookDelivery
	if err := s.db.WithContext(ctx).First(&d, id).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

// PruneWebhookDeliveries 删除创建时间早于 before 的投递记录，返回删除行数。
// 保留期以「天」计（-webhook-delivery-retention-days），远大于单次投递（含重试）的存活时间，
// 因此不会清理到在途记录；历史遗留的 pending 孤儿行同样会被清理。
func (s *Store) PruneWebhookDeliveries(ctx context.Context, before time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Where("created_at < ?", before.UTC()).Delete(&WebhookDelivery{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ListWebhookDeliveries 按创建时间倒序分页返回某订阅的投递记录（含总数）。
func (s *Store) ListWebhookDeliveries(
	ctx context.Context,
	webhookID uint,
	page, pageSize int,
) ([]WebhookDelivery, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	q := s.db.WithContext(ctx).Model(&WebhookDelivery{}).Where("webhook_id = ?", webhookID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count webhook deliveries: %w", err)
	}
	var rows []WebhookDelivery
	err := q.Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error
	if err != nil {
		return nil, 0, fmt.Errorf("list webhook deliveries: %w", err)
	}
	return rows, total, nil
}
