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
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"aigate/internal/crypto"
	"aigate/internal/protocol"
)

// 模型接口类型。
const (
	ModelTypeOpenAI    = protocol.TypeOpenAICompletions // OpenAI 兼容端点（URL 含 /v1，转发时拼 /chat/completions）
	ModelTypeAnthropic = protocol.TypeAnthropicMessages // Anthropic Messages（URL 为 host，转发时拼 /v1/messages）
)

// AIModel 上游模型配置。
type AIModel struct {
	ID        uint   `gorm:"primaryKey"`
	Provider  string `gorm:"size:64;not null;default:''"`
	ModelID   string `gorm:"size:191;not null;default:''"` // 透传给上游的原始模型 ID
	ModelName string `gorm:"size:191;not null;default:''"`
	URL       string `gorm:"size:512;not null;default:''"`
	// APIKey 落库为 enc:v1: 密文（未配置主密钥时为明文）；列宽为密文膨胀留余量。
	APIKey            string `gorm:"size:1024;not null;default:''"                 json:"-"`
	CredentialID      uint   `gorm:"not null;default:0;index"` // >0 时引用凭据，转发时实时解析
	ModelType         string `gorm:"size:32;not null;default:'openai-completions'"`
	CustomHTTPHeaders string `gorm:"type:text;not null"` // JSON 对象字符串
	QuotaDay          int    `gorm:"not null;default:-1"`
	Enabled           bool   `gorm:"not null;default:true"`
	// UserKeyRequired 标记「用户自备 Key」模式（照搬 hatchery AIModel.UserKeyRequired）：
	//   - false：使用模型共享密钥（credential 引用或 api_key），绑定行密钥被忽略；
	//   - true：不保存共享密钥，转发密钥只取密钥绑定行（api_key_models.api_key），缺则不可解析。
	// 创建时确定，不允许变更。
	UserKeyRequired bool      `gorm:"not null;default:false"`
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

// GetCustomHTTPHeaders 解析自定义请求头配置。
func (m AIModel) GetCustomHTTPHeaders() map[string]string {
	s := strings.TrimSpace(m.CustomHTTPHeaders)
	if s == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// sealIfPresent 加密非空密钥类字段：空串表示「未配置」，原样落空串。
// 与重加密迁移（ReencryptSecrets 跳过空行）口径一致，避免把空值加密成密文，
// 使「模型是否自带 api_key」在列上可直观判断（读路径保持不变：空串解密仍为空串）。
func sealIfPresent(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	return crypto.Seal(raw)
}

// CreateAIModel 创建模型（APIKey 按主密钥策略加密后落库）。
// 调用后实体上的 APIKey 即落库值：配置主密钥时为密文，未配置时保持明文。
func (s *Store) CreateAIModel(ctx context.Context, m *AIModel) error {
	sealed, err := sealIfPresent(m.APIKey)
	if err != nil {
		return fmt.Errorf("seal ai model api key: %w", err)
	}
	m.APIKey = sealed
	if err := s.db.WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("create ai model: %w", err)
	}
	return nil
}

// GetAIModel 按 ID 查询模型并解密 api_key（解密失败返回错误，不静默降级）。
func (s *Store) GetAIModel(ctx context.Context, id uint) (*AIModel, error) {
	var m AIModel
	if err := s.db.WithContext(ctx).First(&m, id).Error; err != nil {
		return nil, err
	}
	if err := m.decryptAPIKey(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListAIModels 列出全部模型并解密 api_key；任一行的密文无法解密即返回错误。
func (s *Store) ListAIModels(ctx context.Context) ([]AIModel, error) {
	var models []AIModel
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("list ai models: %w", err)
	}
	for i := range models {
		if err := models[i].decryptAPIKey(); err != nil {
			return nil, err
		}
	}
	return models, nil
}

// UpdateAIModel 按字段更新模型并返回更新后的实体；fields 含 api_key 时先加密再落库。
func (s *Store) UpdateAIModel(ctx context.Context, id uint, fields map[string]any) (*AIModel, error) {
	if raw, ok := fields["api_key"].(string); ok {
		sealed, err := sealIfPresent(raw)
		if err != nil {
			return nil, fmt.Errorf("seal ai model api key: %w", err)
		}
		fields["api_key"] = sealed
	}
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&AIModel{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update ai model: %w", err)
		}
	}
	return s.GetAIModel(ctx, id)
}

// DeleteAIModel 删除模型并清理其密钥绑定（单事务）。
// 与来源仓库语义一致：若被删模型是某密钥的 primary 且该密钥仍绑定其他模型，
// 先把剩余绑定中 sort_order 最小（并列取 id 最小）者提升为 primary，再清理绑定。
func (s *Store) DeleteAIModel(ctx context.Context, id uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := promoteFallbackForDeletedModel(tx, id); err != nil {
			return err
		}
		if err := tx.Where("ai_model_id = ?", id).Delete(&APIKeyModel{}).Error; err != nil {
			return fmt.Errorf("delete model bindings: %w", err)
		}
		if err := tx.Delete(&AIModel{}, id).Error; err != nil {
			return fmt.Errorf("delete ai model: %w", err)
		}
		return nil
	})
	return err
}

// promoteFallbackForDeletedModel 为「primary 正是被删模型」的密钥提升新的 primary。
// 无剩余绑定时保持无 primary（数据面按「无可解析模型」拒绝，不制造残留绑定）。
func promoteFallbackForDeletedModel(tx *gorm.DB, aiModelID uint) error {
	var keyIDs []uint
	if err := tx.Model(&APIKeyModel{}).
		Where("ai_model_id = ? AND role = ?", aiModelID, ModelRolePrimary).
		Pluck("DISTINCT api_key_id", &keyIDs).Error; err != nil {
		return fmt.Errorf("scan keys with deleted primary: %w", err)
	}
	for _, keyID := range keyIDs {
		var next APIKeyModel
		err := tx.Where("api_key_id = ? AND ai_model_id != ?", keyID, aiModelID).
			Order("sort_order ASC, id ASC").First(&next).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find promoted binding for key %d: %w", keyID, err)
		}
		if err := tx.Model(&APIKeyModel{}).Where("id = ?", next.ID).
			Update("role", ModelRolePrimary).Error; err != nil {
			return fmt.Errorf("promote binding for key %d: %w", keyID, err)
		}
	}
	return nil
}

// decryptAPIKey 就地解密 api_key 字段（列内为密文时）。
func (m *AIModel) decryptAPIKey() error {
	plain, err := crypto.Open(m.APIKey)
	if err != nil {
		return fmt.Errorf("decrypt ai model %d api key: %w", m.ID, err)
	}
	m.APIKey = plain
	return nil
}
