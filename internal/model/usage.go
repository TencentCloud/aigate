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
	"gorm.io/gorm/clause"
)

// LLMUsageLog 请求级用量日志（append-only）。
type LLMUsageLog struct {
	ID                     uint      `gorm:"primaryKey"`
	APIKeyID               uint      `gorm:"not null;default:0;index"`
	UserID                 uint      `gorm:"not null;default:0;index"`
	GroupID                uint      `gorm:"not null;default:0;index"`
	AIModelID              uint      `gorm:"not null;default:0"`
	Model                  string    `gorm:"size:191;not null;default:''"`
	Provider               string    `gorm:"size:64;not null;default:''"`
	PromptTokens           int       `gorm:"not null;default:0"`
	CompletionTokens       int       `gorm:"not null;default:0"`
	TotalTokens            int       `gorm:"not null;default:0"`
	PromptCacheReadTokens  int       `gorm:"not null;default:0"`
	PromptCacheWriteTokens int       `gorm:"not null;default:0"`
	StatusCode             int       `gorm:"not null;default:0"`
	Latency                int       `gorm:"not null;default:0"` // 毫秒
	CreatedAt              time.Time `gorm:"not null;index"`
}

// DailyUsageSummary 按（日期，用户，密钥，模型）聚合的日用量，配额实时查询的数据源。
type DailyUsageSummary struct {
	ID                     uint      `gorm:"primaryKey"`
	Date                   time.Time `gorm:"not null;uniqueIndex:uk_daily_usage"`
	UserID                 uint      `gorm:"not null;default:0;uniqueIndex:uk_daily_usage"`
	APIKeyID               uint      `gorm:"not null;default:0;uniqueIndex:uk_daily_usage"`
	AIModelID              uint      `gorm:"not null;default:0;uniqueIndex:uk_daily_usage"`
	GroupID                uint      `gorm:"not null;default:0;index"`
	PromptTokens           int64     `gorm:"not null;default:0"`
	CompletionTokens       int64     `gorm:"not null;default:0"`
	TotalTokens            int64     `gorm:"not null;default:0"`
	PromptCacheReadTokens  int64     `gorm:"not null;default:0"`
	PromptCacheWriteTokens int64     `gorm:"not null;default:0"`
	RequestCount           int64     `gorm:"not null;default:0"`
}

// InsertUsageLog 写入一条请求级用量日志。
func (s *Store) InsertUsageLog(ctx context.Context, entry *LLMUsageLog) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(entry).Error; err != nil {
		return fmt.Errorf("insert usage log: %w", err)
	}
	return nil
}

// UpsertDailyUsage 按日聚合累计用量（total_tokens<=0 时跳过）。
func (s *Store) UpsertDailyUsage(ctx context.Context, entry *LLMUsageLog) error {
	if entry.TotalTokens <= 0 {
		return nil
	}
	row := DailyUsageSummary{
		Date:                   LocalToday(),
		UserID:                 entry.UserID,
		APIKeyID:               entry.APIKeyID,
		AIModelID:              entry.AIModelID,
		GroupID:                entry.GroupID,
		PromptTokens:           int64(entry.PromptTokens),
		CompletionTokens:       int64(entry.CompletionTokens),
		TotalTokens:            int64(entry.TotalTokens),
		PromptCacheReadTokens:  int64(entry.PromptCacheReadTokens),
		PromptCacheWriteTokens: int64(entry.PromptCacheWriteTokens),
		RequestCount:           1,
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "date"}, {Name: "user_id"}, {Name: "api_key_id"}, {Name: "ai_model_id"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"prompt_tokens":             gorm.Expr("prompt_tokens + ?", entry.PromptTokens),
			"completion_tokens":         gorm.Expr("completion_tokens + ?", entry.CompletionTokens),
			"total_tokens":              gorm.Expr("total_tokens + ?", entry.TotalTokens),
			"prompt_cache_read_tokens":  gorm.Expr("prompt_cache_read_tokens + ?", entry.PromptCacheReadTokens),
			"prompt_cache_write_tokens": gorm.Expr("prompt_cache_write_tokens + ?", entry.PromptCacheWriteTokens),
			"request_count":             gorm.Expr("request_count + 1"),
		}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("upsert daily usage: %w", err)
	}
	return nil
}

// ---- 窗口用量查询（配额检查用） ----

// SumUserTokensInWindow 统计用户在 [from, to) 的 token 总量；groupID>0 时只统计该分组，0 为全部。
func (s *Store) SumUserTokensInWindow(
	ctx context.Context,
	userID, groupID uint,
	from time.Time,
	to *time.Time,
) (int64, error) {
	q := s.db.WithContext(ctx).Model(&LLMUsageLog{}).Where("user_id = ? AND created_at >= ?", userID, from)
	if to != nil {
		q = q.Where("created_at < ?", *to)
	}
	if groupID > 0 {
		q = q.Where("group_id = ?", groupID)
	}
	var total int64
	if err := q.Select("COALESCE(SUM(total_tokens), 0)").Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum user tokens: %w", err)
	}
	return total, nil
}

// SumKeyTokensInWindow 统计指定密钥在 [from, to) 的 token 总量（密钥级配额检查用）。
func (s *Store) SumKeyTokensInWindow(ctx context.Context, apiKeyID uint, from time.Time, to *time.Time) (int64, error) {
	q := s.db.WithContext(ctx).Model(&LLMUsageLog{}).Where("api_key_id = ? AND created_at >= ?", apiKeyID, from)
	if to != nil {
		q = q.Where("created_at < ?", *to)
	}
	var total int64
	if err := q.Select("COALESCE(SUM(total_tokens), 0)").Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum key tokens: %w", err)
	}
	return total, nil
}

// SumGroupTokensInWindow 统计分组在 [from, to) 的 token 总量。
func (s *Store) SumGroupTokensInWindow(
	ctx context.Context,
	groupID uint,
	from time.Time,
	to *time.Time,
) (int64, error) {
	q := s.db.WithContext(ctx).Model(&LLMUsageLog{}).Where("group_id = ? AND created_at >= ?", groupID, from)
	if to != nil {
		q = q.Where("created_at < ?", *to)
	}
	var total int64
	if err := q.Select("COALESCE(SUM(total_tokens), 0)").Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum group tokens: %w", err)
	}
	return total, nil
}

// SumGlobalTokensInWindow 统计全站在 [from, to) 的 token 总量。
func (s *Store) SumGlobalTokensInWindow(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
	q := s.db.WithContext(ctx).Model(&LLMUsageLog{}).Where("created_at >= ?", from)
	if to != nil {
		q = q.Where("created_at < ?", *to)
	}
	var total int64
	if err := q.Select("COALESCE(SUM(total_tokens), 0)").Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum global tokens: %w", err)
	}
	return total, nil
}

// SumModelDailyTokens 返回模型在业务时区「今天」的 token 总量（走日聚合表）。
func (s *Store) SumModelDailyTokens(ctx context.Context, aiModelID uint) (int64, error) {
	var total int64
	err := s.db.WithContext(ctx).Model(&DailyUsageSummary{}).
		Where("ai_model_id = ? AND date = ?", aiModelID, LocalToday()).
		Select("COALESCE(SUM(total_tokens), 0)").Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("sum model daily tokens: %w", err)
	}
	return total, nil
}

// ---- 用量查询（管理面 / 用户面） ----

// UsageFilter 用量查询过滤条件；日期为业务时区日期标签（含首含尾）。
type UsageFilter struct {
	StartDate *time.Time
	EndDate   *time.Time
	UserID    uint
	GroupID   uint
	APIKeyID  uint
	AIModelID uint
}

// UsageSummaryRow 聚合行（按 groupBy 填充对应维度字段）。
type UsageSummaryRow struct {
	Date                   *time.Time `json:"date,omitempty"`
	UserID                 uint       `json:"user_id,omitempty"`
	APIKeyID               uint       `json:"api_key_id,omitempty"`
	AIModelID              uint       `json:"ai_model_id,omitempty"`
	GroupID                uint       `json:"group_id,omitempty"`
	PromptTokens           int64      `json:"prompt_tokens"`
	CompletionTokens       int64      `json:"completion_tokens"`
	TotalTokens            int64      `json:"total_tokens"`
	PromptCacheReadTokens  int64      `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int64      `json:"prompt_cache_write_tokens"`
	RequestCount           int64      `json:"request_count"`
}

const usageSumSelect = "COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, " +
	"COALESCE(SUM(completion_tokens), 0) AS completion_tokens, " +
	"COALESCE(SUM(total_tokens), 0) AS total_tokens, " +
	"COALESCE(SUM(prompt_cache_read_tokens), 0) AS prompt_cache_read_tokens, " +
	"COALESCE(SUM(prompt_cache_write_tokens), 0) AS prompt_cache_write_tokens, " +
	"COALESCE(SUM(request_count), 0) AS request_count"

// QueryUsageSummary 按 groupBy（date | model | key | user）聚合日用量表；groupBy 非法时按 date。
func (s *Store) QueryUsageSummary(ctx context.Context, f UsageFilter, groupBy string) ([]UsageSummaryRow, error) {
	col := "date"
	switch groupBy {
	case "model":
		col = "ai_model_id"
	case "key":
		col = "api_key_id"
	case "user":
		col = "user_id"
	case "group":
		col = "group_id"
	}
	q := s.applyUsageFilter(s.db.WithContext(ctx).Model(&DailyUsageSummary{}), f, "date")
	var rows []UsageSummaryRow
	err := q.Select(col + ", " + usageSumSelect).Group(col).Order(col + " ASC").Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query usage summary: %w", err)
	}
	return rows, nil
}

// ListUsageLogs 分页查询用量明细（按时间倒序）。
func (s *Store) ListUsageLogs(ctx context.Context, f UsageFilter, page, pageSize int) ([]LLMUsageLog, int64, error) {
	base := s.applyUsageFilter(s.db.WithContext(ctx).Model(&LLMUsageLog{}), f, "created_at")
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count usage logs: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	var logs []LLMUsageLog
	err := base.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&logs).Error
	if err != nil {
		return nil, 0, fmt.Errorf("list usage logs: %w", err)
	}
	return logs, total, nil
}

// ExportUsageLogs 以 id 游标分批（正序）遍历符合条件的用量明细，每批回调一次；
// 用于导出流式写出，避免整表载入内存。batch <= 0 时使用默认批大小。
func (s *Store) ExportUsageLogs(ctx context.Context, f UsageFilter, batch int, fn func([]LLMUsageLog) error) error {
	if batch <= 0 {
		batch = 5000
	}
	var lastID uint
	for {
		var rows []LLMUsageLog
		q := s.applyUsageFilter(s.db.WithContext(ctx).Model(&LLMUsageLog{}), f, "created_at").
			Where("id > ?", lastID).Order("id ASC").Limit(batch)
		if err := q.Find(&rows).Error; err != nil {
			return fmt.Errorf("export usage logs: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		if err := fn(rows); err != nil {
			return err
		}
		lastID = rows[len(rows)-1].ID
		if len(rows) < batch {
			return nil
		}
	}
}

// applyUsageFilter 应用过滤条件；日期为业务时区日期标签，EndDate 含当日。
// dateCol 为表中参与日期过滤的列名（日聚合表为 date、日志表为 created_at）。
func (s *Store) applyUsageFilter(q *gorm.DB, f UsageFilter, dateCol string) *gorm.DB {
	if f.StartDate != nil {
		q = q.Where(dateCol+" >= ?", *f.StartDate)
	}
	if f.EndDate != nil {
		q = q.Where(dateCol+" < ?", f.EndDate.AddDate(0, 0, 1))
	}
	if f.UserID > 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.GroupID > 0 {
		q = q.Where("group_id = ?", f.GroupID)
	}
	if f.APIKeyID > 0 {
		q = q.Where("api_key_id = ?", f.APIKeyID)
	}
	if f.AIModelID > 0 {
		q = q.Where("ai_model_id = ?", f.AIModelID)
	}
	return q
}

// ParseDateLabel 把 "2006-01-02" 解析为业务时区的日期标签（业务时区当日 00:00 的 UTC 表示）。
// 空串返回零值时间与错误；与 LocalToday 同口径，可直接用于 UsageFilter 的 StartDate / EndDate。
func ParseDateLabel(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("date must not be empty")
	}
	t, err := time.ParseInLocation("2006-01-02", s, businessLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return localDate(t), nil
}
