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
)

// meResponse 是当前身份的对外表示：绝不包含 token 哈希等敏感字段。
type meResponse struct {
	ID        uint   `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	GroupID   uint   `json:"group_id"`
	GroupName string `json:"group_name"`
}

func handleMe(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(w, r)
		if !ok {
			return
		}
		resp := meResponse{ID: u.ID, Username: u.Username, Role: u.Role, GroupID: u.GroupID}
		if u.GroupID > 0 {
			g, err := store.GetGroup(r.Context(), u.GroupID)
			switch {
			case err == nil:
				resp.GroupName = g.Name
			case model.IsNotFound(err):
				// 分组已被删除（悬空 group_id）：分组名按空串返回。
			default:
				writeInternalError(w, err)
				return
			}
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}
