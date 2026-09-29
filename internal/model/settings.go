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
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Group 用户分组（扁平结构，组树后置）。TokenQuotaRules 为配额规则 JSON（M2 使用）。
type Group struct {
	ID              uint   `gorm:"primaryKey"`
	Name            string `gorm:"size:128;not null;default:''"`
	TokenQuotaRules string `gorm:"type:text;not null"`
	// GlobalTokenQuotaRules 组级全局配额规则（可空语义）：
	//   - 空串/非法 JSON：未配置 → 全局检查回退站点规则（与升级前一致）；
	//   - "[]"：显式不限（跳过全局检查）；
	//   - 规则 JSON：命中时该组固定按组计量（组用量 vs 组规则）。
	GlobalTokenQuotaRules string    `gorm:"type:text;not null"`
	CreatedAt             time.Time `gorm:"not null"`
	UpdatedAt             time.Time `gorm:"not null"`
}

// TableName 对齐上游数据接入命名（hatchery user_groups）。
func (Group) TableName() string { return "user_groups" }

// Setting 全局设置（单行，id=1）。
type Setting struct {
	ID                     uint      `gorm:"primaryKey"`
	DefaultTokenQuotaRules string    `gorm:"type:text;not null"` // 全局默认用户配额规则（M2）
	GlobalTokenQuotaRules  string    `gorm:"type:text;not null"` // 全站/按组全局配额规则（M2）
	GlobalQuotaScope       string    `gorm:"size:16;not null;default:'site'"`
	UserKeyLimit           int       `gorm:"not null;default:5"` // 普通用户可创建的密钥数上限；0 = 不限
	UpdatedAt              time.Time `gorm:"not null"`
}

// 用户密钥数上限默认值（0 表示不限）。
const DefaultUserKeyLimit = 5

// GetSettings 读取全局设置（不存在时以默认值创建；并发首建冲突时回读）。
func (s *Store) GetSettings(ctx context.Context) (*Setting, error) {
	var st Setting
	err := s.db.WithContext(ctx).First(&st, 1).Error
	if err == nil {
		return &st, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("get settings: %w", err)
	}
	st = Setting{
		ID:                     1,
		DefaultTokenQuotaRules: "[]",
		GlobalTokenQuotaRules:  "[]",
		GlobalQuotaScope:       "site",
		UserKeyLimit:           DefaultUserKeyLimit,
	}
	if err := s.db.WithContext(ctx).Create(&st).Error; err != nil {
		// 两个请求同时未命中时会并发首建，其中一个撞主键冲突：回读一次即成功。
		var existing Setting
		if rerr := s.db.WithContext(ctx).First(&existing, 1).Error; rerr == nil {
			return &existing, nil
		}
		return nil, fmt.Errorf("create settings: %w", err)
	}
	return &st, nil
}

// UpdateSettings 更新全局设置。
func (s *Store) UpdateSettings(ctx context.Context, fields map[string]any) (*Setting, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&Setting{}).Where("id = ?", 1).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update settings: %w", err)
		}
	}
	return s.GetSettings(ctx)
}

// CreateGroup 创建分组。TokenQuotaRules 语义：空串 = 未配置（回退全局默认）；"[]" = 显式不限。
func (s *Store) CreateGroup(ctx context.Context, g *Group) error {
	if err := s.db.WithContext(ctx).Create(g).Error; err != nil {
		return fmt.Errorf("create group: %w", err)
	}
	return nil
}

// GetGroup 按 ID 查询分组。
func (s *Store) GetGroup(ctx context.Context, id uint) (*Group, error) {
	var g Group
	if err := s.db.WithContext(ctx).First(&g, id).Error; err != nil {
		return nil, err
	}
	return &g, nil
}

// ListGroups 列出全部分组。
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	var groups []Group
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&groups).Error; err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	return groups, nil
}

// UpdateGroup 按字段更新分组。
func (s *Store) UpdateGroup(ctx context.Context, id uint, fields map[string]any) (*Group, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&Group{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update group: %w", err)
		}
	}
	return s.GetGroup(ctx, id)
}

// DeleteGroup 删除分组。
func (s *Store) DeleteGroup(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&Group{}, id).Error; err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	return nil
}
