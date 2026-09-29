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
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 用户角色。
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User 网关用户：管理员（role=admin）与普通用户（role=user）。
// 认证长期为 Bearer token（纯 API 服务，无登录页）：token 只存哈希。
type User struct {
	ID             uint      `gorm:"primaryKey"`
	Username       string    `gorm:"size:128;not null;default:''"`
	Role           string    `gorm:"size:16;not null;default:'user'"`
	GroupID        uint      `gorm:"not null;default:0;index"`
	APITokenHash   string    `gorm:"column:api_token_hash;size:64;not null;default:'';uniqueIndex:uk_users_api_token_hash"`
	APITokenPrefix string    `gorm:"column:api_token_prefix;size:32;not null;default:''"`
	Enabled        bool      `gorm:"not null;default:true"`
	CreatedAt      time.Time `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

// CreateUser 创建用户（调用方负责生成 token 哈希与前缀）。
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	if u.Role == "" {
		u.Role = RoleUser
	}
	if err := s.db.WithContext(ctx).Create(u).Error; err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUserByID 按 ID 查询用户。
func (s *Store) GetUserByID(ctx context.Context, id uint) (*User, error) {
	var u User
	if err := s.db.WithContext(ctx).First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByTokenHash 按 token 哈希查询用户（鉴权入口）。
func (s *Store) GetUserByTokenHash(ctx context.Context, hash string) (*User, error) {
	var u User
	if err := s.db.WithContext(ctx).Where("api_token_hash = ?", hash).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers 列出全部用户。
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&users).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// CountUsersByRole 统计指定角色的用户数。
func (s *Store) CountUsersByRole(ctx context.Context, role string) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&User{}).Where("role = ?", role).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CountEnabledUsersByRole 统计指定角色且处于启用状态的用户数（「最后一个管理员」保护用）。
func (s *Store) CountEnabledUsersByRole(ctx context.Context, role string) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&User{}).
		Where("role = ? AND enabled = ?", role, true).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count enabled users: %w", err)
	}
	return n, nil
}

// UpdateUser 按字段更新用户并返回更新后的实体。
func (s *Store) UpdateUser(ctx context.Context, id uint, fields map[string]any) (*User, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update user: %w", err)
		}
	}
	return s.GetUserByID(ctx, id)
}

// DeleteUser 删除用户，并级联删除其名下的数据面密钥（含绑定）。
// 历史用量日志保留（api_key_id 作为历史引用）。
func (s *Store) DeleteUser(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var keyIDs []uint
		if err := tx.Model(&APIKey{}).Where("user_id = ?", id).Pluck("id", &keyIDs).Error; err != nil {
			return fmt.Errorf("list user keys: %w", err)
		}
		if len(keyIDs) > 0 {
			if err := tx.Where("api_key_id IN ?", keyIDs).Delete(&APIKeyModel{}).Error; err != nil {
				return fmt.Errorf("delete key bindings: %w", err)
			}
			if err := tx.Where("id IN ?", keyIDs).Delete(&APIKey{}).Error; err != nil {
				return fmt.Errorf("delete keys: %w", err)
			}
		}
		if err := tx.Delete(&User{}, id).Error; err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		return nil
	})
}

// RotateUserToken 换发用户访问 token，返回新明文与展示前缀（旧 token 立即失效）。
func (s *Store) RotateUserToken(ctx context.Context, id uint) (plain, displayPrefix string, err error) {
	plain, hash, display, err := GenerateToken(TokenPrefixAccess)
	if err != nil {
		return "", "", err
	}
	if err := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", id).
		Updates(map[string]any{"api_token_hash": hash, "api_token_prefix": display}).Error; err != nil {
		return "", "", fmt.Errorf("rotate user token: %w", err)
	}
	return plain, display, nil
}
