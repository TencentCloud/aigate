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
	"log/slog"
	"strings"
	"time"

	"aigate/internal/crypto"
)

// SecretLocationHeader 是密钥项的位置：目前仅支持请求头（转发时按 header 名注入上游请求）。
const SecretLocationHeader = "header"

// SecretItem 是凭据中的单条密钥项。
type SecretItem struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Location string `json:"location"`
}

// Credential 是上游凭据：一组密钥项，被模型引用后在转发时实时解析（禁止快照）。
// SecretsJSON 为落库列：配置主密钥时为 `enc:v1:` 密文，未配置时为明文 JSON；不参与 JSON 序列化。
type Credential struct {
	ID          uint      `gorm:"primaryKey"`
	Name        string    `gorm:"size:128;not null;default:''"`
	Description string    `gorm:"size:1024;not null;default:''"`
	SecretsJSON string    `gorm:"type:text;not null"            json:"-"`
	Enabled     bool      `gorm:"not null;default:true"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

// SealSecrets 把密钥项序列化并按主密钥策略加密，返回可落库的串（未配置主密钥时为明文 JSON）。
// 空列表返回空串（等价于「无密钥」）。
func SealSecrets(items []SecretItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("encode credential secrets: %w", err)
	}
	sealed, err := crypto.Seal(string(raw))
	if err != nil {
		return "", fmt.Errorf("seal credential secrets: %w", err)
	}
	return sealed, nil
}

// GetSecrets 解密并解析密钥项；无密钥时返回 nil。
func (c *Credential) GetSecrets() ([]SecretItem, error) {
	plain, err := crypto.Open(c.SecretsJSON)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential %d secrets: %w", c.ID, err)
	}
	if strings.TrimSpace(plain) == "" {
		return nil, nil
	}
	var items []SecretItem
	if err := json.Unmarshal([]byte(plain), &items); err != nil {
		return nil, fmt.Errorf("parse credential %d secrets: %w", c.ID, err)
	}
	return items, nil
}

// SetSecrets 加密并写入密钥项（内存态，需再经 CreateCredential/UpdateCredential 持久化）。
func (c *Credential) SetSecrets(items []SecretItem) error {
	sealed, err := SealSecrets(items)
	if err != nil {
		return err
	}
	c.SecretsJSON = sealed
	return nil
}

// GetMaskedSecrets 返回掩码后的密钥项（首尾各保留 2 字符，长度 ≤4 全掩码），用于管理面展示。
func (c *Credential) GetMaskedSecrets() ([]SecretItem, error) {
	items, err := c.GetSecrets()
	if err != nil {
		return nil, err
	}
	out := make([]SecretItem, 0, len(items))
	for _, it := range items {
		out = append(out, SecretItem{Key: it.Key, Value: MaskSecret(it.Value), Location: it.Location})
	}
	return out, nil
}

// FirstHeaderSecretValue 返回首条 header 位置密钥的明文；无 header 密钥时返回空串。
// location 为空串按 header 处理（唯一受支持的位置）。
func (c *Credential) FirstHeaderSecretValue() (string, error) {
	items, err := c.GetSecrets()
	if err != nil {
		return "", err
	}
	for _, it := range items {
		switch strings.ToLower(strings.TrimSpace(it.Location)) {
		case "", SecretLocationHeader:
			if it.Value != "" {
				return it.Value, nil
			}
		}
	}
	return "", nil
}

// ResolveAPIKeyForModel 按数据面语义解析模型实际使用的上游密钥（每次调用实时解析，不使用凭据快照）：
//   - CredentialID>0 且凭据存在/启用且含 header 密钥 → 用凭据（实时解密）；
//   - 否则（未引用 / 凭据不存在 / 已禁用 / 无 header 密钥）回退模型自身 api_key（必要时解密）；
//   - 凭据或模型 api_key 解密失败 → 返回错误（由调用方失败请求并输出日志，不静默回退）。
//
// m.APIKey 既可为落库原样（密文或明文），也可为已解密明文：crypto.Open 对非密文直接放行。
func ResolveAPIKeyForModel(ctx context.Context, store *Store, m *AIModel) (string, error) {
	if m.UserKeyRequired {
		// 自备 Key 模型没有共享密钥路径（转发密钥只来自绑定行）；
		// 该调用方语义错误由调用方处理（如连通性探测返回明确 400）。
		return "", ErrBindingKeyMissing
	}
	if m.CredentialID > 0 {
		cred, err := store.GetCredentialByID(ctx, m.CredentialID)
		switch {
		case err == nil && cred.Enabled:
			secret, err := cred.FirstHeaderSecretValue()
			if err != nil {
				slog.Error("resolve api key for model: credential decrypt failed",
					"ai_model_id", m.ID, "credential_id", cred.ID, "error", err)
				return "", fmt.Errorf("decrypt credential %d for ai model %d: %w", cred.ID, m.ID, err)
			}
			if secret != "" {
				return secret, nil
			}
			slog.Warn("credential has no header secret, falling back to model api key",
				"ai_model_id", m.ID, "credential_id", cred.ID)
		case err == nil:
			slog.Warn("credential disabled, falling back to model api key",
				"ai_model_id", m.ID, "credential_id", cred.ID)
		case IsNotFound(err):
			slog.Warn("credential not found, falling back to model api key",
				"ai_model_id", m.ID, "credential_id", m.CredentialID)
		default:
			return "", fmt.Errorf("load credential %d for ai model %d: %w", m.CredentialID, m.ID, err)
		}
	}
	plain, err := crypto.Open(m.APIKey)
	if err != nil {
		slog.Error("resolve api key for model: model api key decrypt failed", "ai_model_id", m.ID, "error", err)
		return "", fmt.Errorf("decrypt api key of ai model %d: %w", m.ID, err)
	}
	return plain, nil
}

// MaskSecret 掩码密钥值：长度 >4 时保留首尾各 2 字符、中间以 * 填充；长度 ≤4 时全部掩码。
func MaskSecret(v string) string {
	r := []rune(v)
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 4 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:2]) + strings.Repeat("*", len(r)-4) + string(r[len(r)-2:])
}

// CreateCredential 创建凭据。
func (s *Store) CreateCredential(ctx context.Context, c *Credential) error {
	if err := s.db.WithContext(ctx).Create(c).Error; err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	return nil
}

// GetCredentialByID 按 ID 查询凭据（SecretsJSON 保持落库原样，需解密时用 GetSecrets）。
func (s *Store) GetCredentialByID(ctx context.Context, id uint) (*Credential, error) {
	var c Credential
	if err := s.db.WithContext(ctx).First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCredentials 列出全部凭据（按 ID 升序）。
func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	var creds []Credential
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&creds).Error; err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return creds, nil
}

// UpdateCredential 按字段更新凭据并返回更新后的实体。
// fields 中的 secrets_json 必须已是密文（调用方经 SealSecrets 生成）。
func (s *Store) UpdateCredential(ctx context.Context, id uint, fields map[string]any) (*Credential, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&Credential{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update credential: %w", err)
		}
	}
	return s.GetCredentialByID(ctx, id)
}

// DeleteCredential 删除凭据（不级联解除模型引用；调用方需先确认无引用）。
func (s *Store) DeleteCredential(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&Credential{}, id).Error; err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	return nil
}

// CountModelCredentialRefs 统计引用该凭据的模型数量（用于删除前的影响面判断）。
func (s *Store) CountModelCredentialRefs(ctx context.Context, credID uint) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&AIModel{}).Where("credential_id = ?", credID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count model credential refs: %w", err)
	}
	return n, nil
}
