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
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/quota"
)

// errInvalidUserID 表示 admin 传入的 `?user_id=` 非法。
var errInvalidUserID = errors.New("invalid user_id")

// quotaResponse 是配额逐条视图的对外表示。
type quotaResponse struct {
	UserID uint              `json:"user_id"`
	Usages []quota.RuleUsage `json:"usages"`
}

func handleQuota(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		self, ok := currentUser(w, r)
		if !ok {
			return
		}
		target, err := resolveTargetUser(r, store, self)
		if err != nil {
			if errors.Is(err, errInvalidUserID) {
				httpx.WriteError(w, http.StatusBadRequest, "invalid user_id")
				return
			}
			writeLookupError(w, err, "user")
			return
		}
		// 视图口径：按目标用户「当前分组」解析规则、统计其「全部」用量（groupID=0）。
		// 注意与数据面的差异：数据面判定按「密钥的分组快照」（key.GroupID）执行——
		// 用户换组后存量密钥仍按原组规则，视图与判定可能短暂不一致，重签/新建密钥后收敛。
		rules, err := quota.ResolveUserRules(r.Context(), store, target.GroupID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		usages, err := quota.UserRuleUsages(r.Context(), store, target.ID, 0, rules, time.Now())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, quotaResponse{UserID: target.ID, Usages: usages})
	}
}

// resolveTargetUser 解析配额查询的目标用户：
//   - user 角色：强制为自己（忽略请求中的 user_id，防止越权查看他人配额）；
//   - admin 角色：可用 `?user_id=` 指定任意用户，缺省为自己。
func resolveTargetUser(r *http.Request, store *model.Store, self *model.User) (*model.User, error) {
	if self.Role != model.RoleAdmin {
		return self, nil
	}
	raw := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if raw == "" {
		return self, nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return nil, errInvalidUserID
	}
	return store.GetUserByID(r.Context(), uint(id))
}
