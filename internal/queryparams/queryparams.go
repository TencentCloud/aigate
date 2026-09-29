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

// Package queryparams 集中实现列表类接口的 HTTP 查询参数解析与用量明细行视图：
// 管理面（/admin/*）与用户面（/api/user/*）共用同一口径，避免两处实现漂移
// （分页上限、日期解释、group_by 白名单、行级隔离参数等）。
package queryparams

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/model"
)

// 分页参数（用量明细、webhook 投递记录等列表接口共用）。
const (
	// DefaultPageSize 默认分页大小。
	DefaultPageSize = 50
	// MaxPageSize 分页大小上限。
	MaxPageSize = 500
	// MaxPage 页码上限：防止 (page-1)*pageSize 在乘法中溢出为负 OFFSET
	//（负 OFFSET 在 MySQL 是语法错误、SQLite 会被当作 0，两者行为不一致）。
	MaxPage = 1_000_000
)

// UsageLogRow 是用量明细的对外表示（显式 JSON 字段名，不含任何内部/敏感列）。
type UsageLogRow struct {
	ID                     uint      `json:"id"`
	APIKeyID               uint      `json:"api_key_id"`
	UserID                 uint      `json:"user_id"`
	GroupID                uint      `json:"group_id"`
	AIModelID              uint      `json:"ai_model_id"`
	Model                  string    `json:"model"`
	Provider               string    `json:"provider"`
	PromptTokens           int       `json:"prompt_tokens"`
	CompletionTokens       int       `json:"completion_tokens"`
	TotalTokens            int       `json:"total_tokens"`
	PromptCacheReadTokens  int       `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int       `json:"prompt_cache_write_tokens"`
	StatusCode             int       `json:"status_code"`
	Latency                int       `json:"latency"`
	CreatedAt              time.Time `json:"created_at"`
}

// ValidGroupBy 校验聚合维度取值。
func ValidGroupBy(v string) bool {
	switch v {
	case "date", "model", "key", "user", "group":
		return true
	default:
		return false
	}
}

// ParsePagination 解析 page / page_size（默认 1/50，page_size 上限 500，page 上限 MaxPage）。
func ParsePagination(r *http.Request) (int, int, error) {
	q := r.URL.Query()
	page := 1
	if s := strings.TrimSpace(q.Get("page")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > MaxPage {
			return 0, 0, fmt.Errorf("invalid page")
		}
		page = n
	}
	pageSize := DefaultPageSize
	if s := strings.TrimSpace(q.Get("page_size")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, 0, fmt.Errorf("invalid page_size")
		}
		pageSize = n
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize, nil
}

// ParseUsageFilter 解析用量查询过滤参数（start/end 为 YYYY-MM-DD，按业务时区解释，end 含当日）。
// forcedUserID>0 时强制覆盖 user_id 且完全忽略请求中的 user_id（用户面行级隔离用）。
func ParseUsageFilter(r *http.Request, forcedUserID uint) (model.UsageFilter, error) {
	q := r.URL.Query()
	var f model.UsageFilter
	if s := strings.TrimSpace(q.Get("start")); s != "" {
		d, err := model.ParseDateLabel(s)
		if err != nil {
			return f, fmt.Errorf("invalid start date, want YYYY-MM-DD")
		}
		f.StartDate = &d
	}
	if s := strings.TrimSpace(q.Get("end")); s != "" {
		d, err := model.ParseDateLabel(s)
		if err != nil {
			return f, fmt.Errorf("invalid end date, want YYYY-MM-DD")
		}
		f.EndDate = &d
	}
	if forcedUserID > 0 {
		f.UserID = forcedUserID
	} else {
		var err error
		if f.UserID, err = parseUintQuery(q.Get("user_id"), "user_id"); err != nil {
			return f, err
		}
	}
	var err error
	if f.GroupID, err = parseUintQuery(q.Get("group_id"), "group_id"); err != nil {
		return f, err
	}
	if f.APIKeyID, err = parseUintQuery(q.Get("api_key_id"), "api_key_id"); err != nil {
		return f, err
	}
	if f.AIModelID, err = parseUintQuery(q.Get("ai_model_id"), "ai_model_id"); err != nil {
		return f, err
	}
	return f, nil
}

// parseUintQuery 解析非负整型查询参数；空串视为 0（不过滤）。
func parseUintQuery(raw, name string) (uint, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return uint(n), nil
}

// ToUsageLogRow 将用量日志实体转为对外表示。
func ToUsageLogRow(l *model.LLMUsageLog) UsageLogRow {
	return UsageLogRow{
		ID:                     l.ID,
		APIKeyID:               l.APIKeyID,
		UserID:                 l.UserID,
		GroupID:                l.GroupID,
		AIModelID:              l.AIModelID,
		Model:                  l.Model,
		Provider:               l.Provider,
		PromptTokens:           l.PromptTokens,
		CompletionTokens:       l.CompletionTokens,
		TotalTokens:            l.TotalTokens,
		PromptCacheReadTokens:  l.PromptCacheReadTokens,
		PromptCacheWriteTokens: l.PromptCacheWriteTokens,
		StatusCode:             l.StatusCode,
		Latency:                l.Latency,
		CreatedAt:              l.CreatedAt,
	}
}
