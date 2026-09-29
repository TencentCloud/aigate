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

package userapi

import (
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/queryparams"
)

// usageLogRow 是用量明细的对外表示（与管理面共用同一结构）。
type usageLogRow = queryparams.UsageLogRow

// handleUsageSummary 聚合查询：按 group_by（date 默认 | model | key | user | group）细分。
func handleUsageSummary(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := currentUsageFilter(w, r)
		if !ok {
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
		writeUsageSummary(w, r, store, f, groupBy)
	}
}

// handleUsageByModel 与 summary 同源，固定按模型聚合（忽略请求中的 group_by）。
func handleUsageByModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := currentUsageFilter(w, r)
		if !ok {
			return
		}
		writeUsageSummary(w, r, store, f, "model")
	}
}

// handleUsageLogs 明细查询：按时间倒序分页。
func handleUsageLogs(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := currentUsageFilter(w, r)
		if !ok {
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

// currentUsageFilter 取当前用户并构造带行级隔离的用量过滤；失败时已写响应，返回 ok=false。
func currentUsageFilter(w http.ResponseWriter, r *http.Request) (model.UsageFilter, bool) {
	self, ok := currentUser(w, r)
	if !ok {
		return model.UsageFilter{}, false
	}
	f, err := usageFilterForRequest(r, self)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return model.UsageFilter{}, false
	}
	return f, true
}

// writeUsageSummary 执行聚合并写响应（rows 为空时返回空数组而非 null）。
func writeUsageSummary(
	w http.ResponseWriter,
	r *http.Request,
	store *model.Store,
	f model.UsageFilter,
	groupBy string,
) {
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

// usageFilterForRequest 解析用量查询过滤并施加行级隔离：
//   - user 角色：强制 user_id = self，请求中的 user_id 一律忽略；
//   - admin 角色：`?user_id=` 可指定任意用户，缺省为自己。
func usageFilterForRequest(r *http.Request, self *model.User) (model.UsageFilter, error) {
	if self.Role == model.RoleAdmin {
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			return f, err
		}
		if f.UserID == 0 {
			f.UserID = self.ID
		}
		return f, nil
	}
	return queryparams.ParseUsageFilter(r, self.ID)
}
