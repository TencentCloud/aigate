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
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"aigate/internal/crypto"
)

// 绑定角色。
const (
	ModelRolePrimary  = "primary"
	ModelRoleFallback = "fallback"
)

// APIKey 数据面密钥（sk-）。token 只存哈希，明文仅创建/rotate 时返回一次。
type APIKey struct {
	ID               uint       `gorm:"primaryKey"`
	Name             string     `gorm:"size:128;not null;default:''"`
	UserID           uint       `gorm:"not null;default:0;index"`
	GroupID          uint       `gorm:"not null;default:0;index"`
	ProxyTokenHash   string     `gorm:"size:64;not null;default:'';uniqueIndex:uk_api_keys_proxy_token_hash"`
	ProxyTokenPrefix string     `gorm:"column:proxy_token_prefix;size:32;not null;default:''"`
	MaxTokens        int        `gorm:"not null;default:0"` // >0 时截断请求的 max_tokens 上限
	TokenQuotaRules  string     `gorm:"type:text;not null"` // 密钥级周期配额 JSON（空串 = 不限制）
	Enabled          bool       `gorm:"not null;default:true"`
	ExpiresAt        *time.Time `gorm:"default:null"` // 可选有效期（UTC）；NULL = 永不过期
	ExpiredAt        *time.Time `gorm:"default:null"` // 首次因过期被拒绝的观测时间（key.expired 去重用）
	CreatedAt        time.Time  `gorm:"not null"`
	UpdatedAt        time.Time  `gorm:"not null"`
}

// APIKeyModel 密钥与模型的绑定（一个密钥可绑定多个模型，其中一个为 primary）。
type APIKeyModel struct {
	ID        uint   `gorm:"primaryKey"`
	APIKeyID  uint   `gorm:"not null;uniqueIndex:uk_api_key_model"`
	AIModelID uint   `gorm:"not null;uniqueIndex:uk_api_key_model;index:idx_api_key_models_ai_model_id"`
	Role      string `gorm:"size:16;not null;default:'fallback'"`
	SortOrder int    `gorm:"not null;default:0"`
	// APIKey 绑定级上游密钥（仅 user_key_required 模型解析时使用）；落库为 enc:v1: 密文
	// （未配置主密钥时为明文）；列宽为密文膨胀留余量。
	APIKey    string    `gorm:"size:1024;not null;default:''"                                              json:"-"`
	CreatedAt time.Time `gorm:"not null"`
}

// BindingInfo 是绑定关系的展示视图（含模型基础信息）。
type BindingInfo struct {
	AIModelID uint   `json:"ai_model_id"`
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
	Role      string `json:"role"`
	SortOrder int    `json:"sort_order"`
	HasKey    bool   `json:"has_key"` // 绑定级密钥是否已配置（user_key_required 模型需要）
}

// Resolved 是一次数据面请求解析出的目标模型。
type Resolved struct {
	AIModelID         uint
	ModelID           string
	ModelName         string
	Provider          string
	ModelType         string
	URL               string
	APIKey            string
	QuotaDay          int
	CustomHTTPHeaders map[string]string
}

var (
	// ErrNoResolvableModel 表示密钥没有任何可用的主模型。
	ErrNoResolvableModel = errors.New("no resolvable model for this key")
	// ErrModelNotBound 表示请求显式指定了模型，但该模型未绑定到此密钥。
	ErrModelNotBound = errors.New("requested model is not bound to this key")
	// ErrBindingKeyMissing 表示命中的绑定属于 user_key_required 模型，但绑定行未配置上游密钥
	//（照搬 hatchery 语义：不回退模型共享密钥，视为不可解析）。
	ErrBindingKeyMissing = errors.New("binding has no user-provided key")
)

// CreateAPIKey 创建密钥（调用方负责生成 token 哈希与前缀）。
// bindings 非空时必须恰好包含一个 primary，且引用的模型必须存在。
func (s *Store) CreateAPIKey(ctx context.Context, key *APIKey, bindings []APIKeyModel) error {
	if err := validateBindings(bindings); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(key).Error; err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		for i := range bindings {
			if err := ensureAIModelExists(tx, bindings[i].AIModelID); err != nil {
				return err
			}
			sealed, err := sealIfPresent(bindings[i].APIKey)
			if err != nil {
				return fmt.Errorf("seal binding key: %w", err)
			}
			bindings[i].APIKey = sealed
			bindings[i].ID = 0
			bindings[i].APIKeyID = key.ID
			if err := tx.Create(&bindings[i]).Error; err != nil {
				return fmt.Errorf("create key binding: %w", err)
			}
		}
		return nil
	})
}

// GetAPIKeyByID 按 ID 查询密钥。
func (s *Store) GetAPIKeyByID(ctx context.Context, id uint) (*APIKey, error) {
	var k APIKey
	if err := s.db.WithContext(ctx).First(&k, id).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// GetAPIKeyByTokenHash 按 token 哈希查询密钥（数据面鉴权入口）。
func (s *Store) GetAPIKeyByTokenHash(ctx context.Context, hash string) (*APIKey, error) {
	var k APIKey
	if err := s.db.WithContext(ctx).Where("proxy_token_hash = ?", hash).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// MarkKeyExpiredIfFirst 记录密钥「首次因过期被拒绝」的观测时间（UTC）。
// 返回 true 表示本次是首次观测（调用方据此发出一次 key.expired 事件）；
// 条件更新保证并发下至多一次为 true，天然幂等。
func (s *Store) MarkKeyExpiredIfFirst(ctx context.Context, id uint, now time.Time) (bool, error) {
	res := s.db.WithContext(ctx).Model(&APIKey{}).
		Where("id = ? AND expired_at IS NULL", id).
		Update("expired_at", now)
	if res.Error != nil {
		return false, fmt.Errorf("mark key expired: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// ListAPIKeys 列出全部密钥。
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var keys []APIKey
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&keys).Error; err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return keys, nil
}

// ListAPIKeysByOwner 列出某用户名下的全部密钥（按 ID 升序）；无密钥时返回空切片。
func (s *Store) ListAPIKeysByOwner(ctx context.Context, ownerID uint) ([]APIKey, error) {
	var keys []APIKey
	if err := s.db.WithContext(ctx).Where("user_id = ?", ownerID).Order("id ASC").Find(&keys).Error; err != nil {
		return nil, fmt.Errorf("list api keys by owner %d: %w", ownerID, err)
	}
	return keys, nil
}

// CountAPIKeysByOwner 统计某用户名下的密钥数量。
func (s *Store) CountAPIKeysByOwner(ctx context.Context, ownerID uint) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&APIKey{}).Where("user_id = ?", ownerID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count api keys by owner %d: %w", ownerID, err)
	}
	return n, nil
}

// UpdateAPIKey 按字段更新密钥并返回更新后的实体。
func (s *Store) UpdateAPIKey(ctx context.Context, id uint, fields map[string]any) (*APIKey, error) {
	if len(fields) > 0 {
		if err := s.db.WithContext(ctx).Model(&APIKey{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return nil, fmt.Errorf("update api key: %w", err)
		}
	}
	return s.GetAPIKeyByID(ctx, id)
}

// DeleteAPIKey 删除密钥并清理绑定。
func (s *Store) DeleteAPIKey(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("api_key_id = ?", id).Delete(&APIKeyModel{}).Error; err != nil {
			return fmt.Errorf("delete key bindings: %w", err)
		}
		if err := tx.Delete(&APIKey{}, id).Error; err != nil {
			return fmt.Errorf("delete api key: %w", err)
		}
		return nil
	})
}

// RotateAPIKey 换发密钥：生成新 token 并立即失效旧 token。
func (s *Store) RotateAPIKey(ctx context.Context, id uint) (plain, displayPrefix string, err error) {
	plain, hash, display, err := GenerateToken(TokenPrefixKey)
	if err != nil {
		return "", "", err
	}
	if err := s.db.WithContext(ctx).Model(&APIKey{}).Where("id = ?", id).
		Updates(map[string]any{"proxy_token_hash": hash, "proxy_token_prefix": display}).Error; err != nil {
		return "", "", fmt.Errorf("rotate api key: %w", err)
	}
	return plain, display, nil
}

// ReplaceBindings 整体替换密钥的模型绑定。
func (s *Store) ReplaceBindings(ctx context.Context, keyID uint, bindings []APIKeyModel) error {
	if err := validateBindings(bindings); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("api_key_id = ?", keyID).Delete(&APIKeyModel{}).Error; err != nil {
			return fmt.Errorf("clear key bindings: %w", err)
		}
		for i := range bindings {
			if err := ensureAIModelExists(tx, bindings[i].AIModelID); err != nil {
				return err
			}
			sealed, err := sealIfPresent(bindings[i].APIKey)
			if err != nil {
				return fmt.Errorf("seal binding key: %w", err)
			}
			bindings[i].APIKey = sealed
			bindings[i].ID = 0
			bindings[i].APIKeyID = keyID
			if err := tx.Create(&bindings[i]).Error; err != nil {
				return fmt.Errorf("create key binding: %w", err)
			}
		}
		return nil
	})
}

// ListKeyModels 返回密钥的绑定视图；onlyEnabled=true 时只返回已启用模型的绑定。
func (s *Store) ListKeyModels(ctx context.Context, keyID uint, onlyEnabled bool) ([]BindingInfo, error) {
	q := s.db.WithContext(ctx).Model(&AIModel{}).
		Joins("JOIN api_key_models ON api_key_models.ai_model_id = ai_models.id").
		Where("api_key_models.api_key_id = ?", keyID)
	if onlyEnabled {
		q = q.Where("ai_models.enabled = ?", true).
			// 数据面列表（/v1/models）：跳过 user_key_required 且缺绑定密钥的模型
			//（不可解析，照搬 hatchery「缺 Key 的绑定被跳过」语义）。
			Where("ai_models.user_key_required = ? OR api_key_models.api_key <> ?", false, "")
	}
	var infos []BindingInfo
	err := q.Select("ai_models.id AS ai_model_id, ai_models.model_id AS model_id, ai_models.model_name AS model_name," +
		" ai_models.provider AS provider, api_key_models.role AS role, api_key_models.sort_order AS sort_order," +
		" (api_key_models.api_key <> '') AS has_key").
		Order("api_key_models.sort_order ASC").
		Scan(&infos).
		Error
	if err != nil {
		return nil, fmt.Errorf("list key models: %w", err)
	}
	return infos, nil
}

// ResolveModelForKey 解析请求命中的目标模型：
//   - 请求显式指定模型：按 model_id 大小写不敏感匹配（含 '/'、':' → '-' 的 slug 兼容），
//     未命中返回 ErrModelNotBound（不回退 primary，避免静默路由到非请求模型）；
//   - 未指定模型：回退到绑定的 primary；无 primary 返回 ErrNoResolvableModel。
func (s *Store) ResolveModelForKey(ctx context.Context, key *APIKey, requested string) (*Resolved, error) {
	type bindingRow struct {
		AIModel
		Role          string
		SortOrder     int
		BindingAPIKey string `gorm:"column:binding_api_key"`
	}
	var rows []bindingRow
	err := s.db.WithContext(ctx).Model(&AIModel{}).
		Joins("JOIN api_key_models ON api_key_models.ai_model_id = ai_models.id").
		Where("api_key_models.api_key_id = ? AND ai_models.enabled = ?", key.ID, true).
		Select("ai_models.*, api_key_models.role AS role, api_key_models.sort_order AS sort_order," +
			" api_key_models.api_key AS binding_api_key").
		Order("api_key_models.sort_order ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("resolve key bindings: %w", err)
	}

	req := strings.TrimSpace(requested)
	if req != "" {
		// 第一轮：模型名完整匹配（大小写不敏感 + slug 归一化；兼容含 "/" 的原始模型名）。
		reqLower := strings.ToLower(req)
		reqSlug := normalizeModelKey(req)
		for _, r := range rows {
			if strings.ToLower(r.ModelID) == reqLower || normalizeModelKey(r.ModelID) == reqSlug {
				return s.resolvedFromAIModel(ctx, r.AIModel, r.BindingAPIKey)
			}
		}
		// 第二轮：provider/model_id 前缀匹配（OpenRouter 风格），用于区分"不同来源的同名模型"。
		// 前缀接受模型的 provider 名（如 qcloudlkeap/deepseek-v3.2），或保存绑定时生成的拼接形态
		// {provider}-{model_id}（如 hatchery-glm-4-plus/glm-4-plus，与 hatchery ref 格式一致）。
		// 显式指定了来源而前缀未命中时不做回退：避免路由到与调用方指定不符的来源。
		if prefix, rest, ok := splitModelRef(req); ok {
			restLower := strings.ToLower(rest)
			restSlug := normalizeModelKey(rest)
			for _, r := range rows {
				if modelRefMatchesProvider(r.AIModel, prefix) &&
					(strings.ToLower(r.ModelID) == restLower || normalizeModelKey(r.ModelID) == restSlug) {
					return s.resolvedFromAIModel(ctx, r.AIModel, r.BindingAPIKey)
				}
			}
		}
		return nil, ErrModelNotBound
	}
	for _, r := range rows {
		if r.Role == ModelRolePrimary {
			return s.resolvedFromAIModel(ctx, r.AIModel, r.BindingAPIKey)
		}
	}
	return nil, ErrNoResolvableModel
}

// splitModelRef 拆分「provider/model_id」形式的模型引用（按第一个 "/" 切分）；
// 任一侧为空（或没有 "/"）时返回 ok=false。
func splitModelRef(ref string) (prefix, rest string, ok bool) {
	i := strings.Index(ref, "/")
	if i <= 0 || i >= len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// slugifyModelRef 按上游（openclaw/hatchery providerKey）的 slug 规则归一化：
// 小写、保留 [a-z0-9._-]、其余字符替换为 "-"、合并连续 "-"、去除首尾 "-"。
// 与 hatchery SlugifyModelID 规则保持一致，用于 providerKey 形态的前缀匹配。
func slugifyModelRef(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var buf strings.Builder
	buf.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' {
			buf.WriteRune(r)
		} else {
			buf.WriteByte('-')
		}
	}
	return strings.Trim(dashesRE.ReplaceAllString(buf.String(), "-"), "-")
}

// dashesRE 合并连续 "-"（slug 归一化用）。
var dashesRE = regexp.MustCompile(`-+`)

// modelRefMatchesProvider 判断请求前缀是否指向该模型的来源：
// 接受模型的 provider 名（如 qcloudlkeap），或保存绑定时生成的拼接形态
// （provider 与 model_id 的 slug 拼接，如 hatchery-glm-4-plus）。
func modelRefMatchesProvider(m AIModel, prefix string) bool {
	p := slugifyModelRef(prefix)
	if p == "" {
		return false
	}
	if p == slugifyModelRef(m.Provider) {
		return true
	}
	return p == slugifyModelRef(m.Provider)+"-"+slugifyModelRef(m.ModelID)
}

// resolvedFromAIModel 把命中的模型装配为转发目标，并实时解析上游鉴权密钥。
//
// 密钥来源严格由 AIModel.UserKeyRequired 决定（照搬 hatchery builtinResolvedWithKey 语义）：
//   - true：只取绑定行密钥（实时解密）；绑定缺密钥 → ErrBindingKeyMissing（不回退共享密钥）；
//   - false：只取模型共享密钥（凭据引用或 ai_models.api_key），绑定行密钥被忽略。
func (s *Store) resolvedFromAIModel(ctx context.Context, m AIModel, bindingKey string) (*Resolved, error) {
	var apiKey string
	if m.UserKeyRequired {
		if bindingKey == "" {
			return nil, ErrBindingKeyMissing
		}
		plain, err := crypto.Open(bindingKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt binding key of ai model %d: %w", m.ID, err)
		}
		apiKey = plain
	} else {
		plain, err := ResolveAPIKeyForModel(ctx, s, &m)
		if err != nil {
			return nil, err
		}
		apiKey = plain
	}
	return &Resolved{
		AIModelID:         m.ID,
		ModelID:           m.ModelID,
		ModelName:         m.ModelName,
		Provider:          m.Provider,
		ModelType:         m.ModelType,
		URL:               m.URL,
		APIKey:            apiKey,
		QuotaDay:          m.QuotaDay,
		CustomHTTPHeaders: m.GetCustomHTTPHeaders(),
	}, nil
}

// normalizeModelKey 归一化模型名用于兼容匹配（大小写 + '/'、':' 与 '-' 等价）。
func normalizeModelKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	return s
}

func validateBindings(bindings []APIKeyModel) error {
	if len(bindings) == 0 {
		return nil
	}
	primaries := 0
	seen := make(map[uint]struct{}, len(bindings))
	for _, b := range bindings {
		switch b.Role {
		case ModelRolePrimary:
			primaries++
		case ModelRoleFallback:
		default:
			return invalidInput("invalid binding role %q", b.Role)
		}
		if b.AIModelID == 0 {
			return invalidInput("binding ai_model_id must be > 0")
		}
		if _, dup := seen[b.AIModelID]; dup {
			return invalidInput("duplicate model binding: ai_model_id %d", b.AIModelID)
		}
		seen[b.AIModelID] = struct{}{}
	}
	if primaries != 1 {
		return invalidInput("bindings must contain exactly one primary model")
	}
	return nil
}

func ensureAIModelExists(tx *gorm.DB, id uint) error {
	var n int64
	if err := tx.Model(&AIModel{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return fmt.Errorf("check ai model: %w", err)
	}
	if n == 0 {
		return invalidInput("ai model %d not found", id)
	}
	return nil
}

// ListKeyModelsByKeyIDs 批量返回多个密钥的绑定视图（keyID → 绑定列表，按 sort_order 升序）。
// 用于列表接口一次性取回绑定，避免按密钥逐条查询（N+1）。
func (s *Store) ListKeyModelsByKeyIDs(
	ctx context.Context,
	keyIDs []uint,
	onlyEnabled bool,
) (map[uint][]BindingInfo, error) {
	out := make(map[uint][]BindingInfo, len(keyIDs))
	if len(keyIDs) == 0 {
		return out, nil
	}
	q := s.db.WithContext(ctx).Model(&AIModel{}).
		Joins("JOIN api_key_models ON api_key_models.ai_model_id = ai_models.id").
		Where("api_key_models.api_key_id IN ?", keyIDs)
	if onlyEnabled {
		q = q.Where("ai_models.enabled = ?", true)
	}
	var rows []struct {
		BindingInfo
		APIKeyID uint `gorm:"column:api_key_id"`
	}
	err := q.Select("api_key_models.api_key_id AS api_key_id, ai_models.id AS ai_model_id," +
		" ai_models.model_id AS model_id, ai_models.model_name AS model_name, ai_models.provider AS provider," +
		" api_key_models.role AS role, api_key_models.sort_order AS sort_order, (api_key_models.api_key <> '') AS has_key").
		Order("api_key_models.api_key_id ASC, api_key_models.sort_order ASC").
		Scan(&rows).
		Error
	if err != nil {
		return nil, fmt.Errorf("list key models by key ids: %w", err)
	}
	for _, r := range rows {
		out[r.APIKeyID] = append(out[r.APIKeyID], r.BindingInfo)
	}
	return out, nil
}

// ListModelIDsByOwner 返回某用户名下所有密钥绑定过的模型 ID（去重）。
// 用于「用户可绑定模型范围」一次查询取回（避免按密钥逐个查询）。
func (s *Store) ListModelIDsByOwner(ctx context.Context, ownerID uint) ([]uint, error) {
	var ids []uint
	err := s.db.WithContext(ctx).Model(&APIKeyModel{}).
		Joins("JOIN api_keys ON api_keys.id = api_key_models.api_key_id").
		Where("api_keys.user_id = ?", ownerID).
		Distinct().
		Pluck("api_key_models.ai_model_id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("list model ids by owner %d: %w", ownerID, err)
	}
	return ids, nil
}
