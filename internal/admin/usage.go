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
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/queryparams"
)

// usageLogRow 是用量明细的对外表示（与用户面共用同一结构）。
type usageLogRow = queryparams.UsageLogRow

// handleUsageSummary 聚合查询：按 group_by（date 默认 | model | key | user | group）细分。
func handleUsageSummary(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
		if groupBy == "" {
			groupBy = "date"
		}
		if !queryparams.ValidGroupBy(groupBy) {
			httpx.WriteError(w, http.StatusBadRequest, "group_by must be one of date, model, key, user, group")
			return
		}
		rows, err := store.QueryUsageSummary(r.Context(), f, groupBy)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if rows == nil {
			rows = []model.UsageSummaryRow{}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"group_by": groupBy, "rows": rows})
	}
}

// handleUsageLogs 明细查询：按时间倒序分页。
func handleUsageLogs(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		page, pageSize, err := queryparams.ParsePagination(r)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		logs, total, err := store.ListUsageLogs(r.Context(), f, page, pageSize)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		rows := make([]usageLogRow, 0, len(logs))
		for i := range logs {
			rows = append(rows, queryparams.ToUsageLogRow(&logs[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"logs":      rows,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		})
	}
}

// usageExportBatch 导出时的分批大小（keyset 游标；不整表载入内存）。
const usageExportBatch = 5000

// handleUsageExport 实现 `GET /admin/usage/export`：流式导出用量明细。
// format=csv|json（缺省 csv）；过滤参数与 /admin/usage/logs 完全同源。
//
// 说明：响应头在导出开始前写出；若中途数据库读取失败，只能中断流并记录日志
// （HTTP 状态已发送，无法再改写为错误码）——导出内容以流终止为失败信号。
func handleUsageExport(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		format := strings.TrimSpace(r.URL.Query().Get("format"))
		if format == "" {
			format = "csv"
		}
		switch format {
		case "csv":
			exportUsageCSV(w, r, store, f)
		case "json":
			exportUsageJSON(w, r, store, f)
		default:
			httpx.WriteError(w, http.StatusBadRequest, "format must be csv or json")
		}
	}
}

// usageExportCSVColumns 与明细接口（UsageLogRow）字段一一对应。
var usageExportCSVColumns = []string{
	"id", "api_key_id", "user_id", "group_id", "ai_model_id", "model", "provider",
	"prompt_tokens", "completion_tokens", "total_tokens",
	"prompt_cache_read_tokens", "prompt_cache_write_tokens",
	"status_code", "latency", "created_at",
}

func exportUsageCSV(w http.ResponseWriter, r *http.Request, store *model.Store, f model.UsageFilter) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-export.csv"`)
	cw := csv.NewWriter(w)
	if err := cw.Write(usageExportCSVColumns); err != nil {
		slog.Error("usage export: write csv header failed", "error", err)
		return
	}
	err := store.ExportUsageLogs(r.Context(), f, usageExportBatch, func(rows []model.LLMUsageLog) error {
		for i := range rows {
			row := &rows[i]
			record := []string{
				strconv.FormatUint(uint64(row.ID), 10),
				strconv.FormatUint(uint64(row.APIKeyID), 10),
				strconv.FormatUint(uint64(row.UserID), 10),
				strconv.FormatUint(uint64(row.GroupID), 10),
				strconv.FormatUint(uint64(row.AIModelID), 10),
				row.Model, row.Provider,
				strconv.Itoa(row.PromptTokens), strconv.Itoa(row.CompletionTokens), strconv.Itoa(row.TotalTokens),
				strconv.Itoa(row.PromptCacheReadTokens), strconv.Itoa(row.PromptCacheWriteTokens),
				strconv.Itoa(row.StatusCode), strconv.Itoa(row.Latency),
				row.CreatedAt.UTC().Format(time.RFC3339),
			}
			if err := cw.Write(record); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	})
	if err != nil {
		slog.Error("usage export: csv stream aborted", "error", err)
		return
	}
	cw.Flush()
}

func exportUsageJSON(w http.ResponseWriter, r *http.Request, store *model.Store, f model.UsageFilter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-export.json"`)
	if _, err := io.WriteString(w, `{"logs":[`); err != nil {
		return
	}
	first := true
	err := store.ExportUsageLogs(r.Context(), f, usageExportBatch, func(rows []model.LLMUsageLog) error {
		for i := range rows {
			payload, err := json.Marshal(queryparams.ToUsageLogRow(&rows[i]))
			if err != nil {
				return err
			}
			if !first {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			first = false
			if _, err := w.Write(payload); err != nil {
				return err
			}
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		slog.Error("usage export: json stream aborted", "error", err)
		return
	}
	_, _ = io.WriteString(w, `]}`)
}
