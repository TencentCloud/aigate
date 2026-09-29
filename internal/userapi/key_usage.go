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

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/queryparams"
)

// keyUsageDefaultDays 未显式提供 start/end 时的默认趋势区间（含今天，共 30 天）。
const keyUsageDefaultDays = 30

// handleUserKeyUsage 实现 `GET /api/user/keys/{id}/usage`：单密钥按日用量趋势。
//
// 权限：不存在 / 非本人（且非 admin 指定用户）统一 404（不泄露他人密钥存在性）；
// 日期口径与既有用量查询一致（业务时区日期标签、含首含尾）；未提供 start/end 时默认最近 30 天。
func handleUserKeyUsage(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _, ok := loadOwnedKey(w, r, store)
		if !ok {
			return
		}
		// user 角色强制 self（路径已校验归属，这里再收紧一层过滤条件）。
		self, _ := currentUser(w, r)
		forced := uint(0)
		if self != nil && self.Role != model.RoleAdmin {
			forced = key.UserID
		}
		f, err := queryparams.ParseUsageFilter(r, forced)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 路径 id 即密钥维度：覆盖 query 参数，杜绝跨密钥查询。
		f.APIKeyID = key.ID
		if f.StartDate == nil && f.EndDate == nil {
			end := model.LocalToday()
			start := end.AddDate(0, 0, -(keyUsageDefaultDays - 1))
			f.StartDate, f.EndDate = &start, &end
		}
		rows, err := store.QueryUsageSummary(r.Context(), f, "date")
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if rows == nil {
			rows = []model.UsageSummaryRow{}
		}
		resp := map[string]any{
			"key_id":   key.ID,
			"group_by": "date",
			"rows":     rows,
		}
		if f.StartDate != nil {
			resp["start"] = f.StartDate.Format("2006-01-02")
		}
		if f.EndDate != nil {
			resp["end"] = f.EndDate.Format("2006-01-02")
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}
