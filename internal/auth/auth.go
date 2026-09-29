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

// Package auth 提供管理面与用户面的 Bearer token 鉴权中间件：
// 校验访问 token（只存哈希）、按角色放行，并把当前用户注入请求 context 供 handler 读取。
//
// 鉴权语义：
//   - token 缺失/非法/用户不存在/用户被禁用 → 401；
//   - 角色不在允许集合内 → 403；
//   - admin 与 user 角色的用户面访问均放行（RequireUser），管理面仅 admin（RequireAdmin）。
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// userContextKey 是 context 中当前登录用户的私有键类型（仅本包可构造）。
type userContextKey struct{}

const bearerScheme = "Bearer "

// errUnauthenticated 表示凭据缺失/非法/用户不存在（→ 401）；
// 其它错误（如数据库不可用）按后端故障处理（→ 503），避免与「凭据无效」混淆。
var errUnauthenticated = errors.New("unauthenticated")

// RequireAdmin 校验管理面访问：token 缺失/非法/用户不存在/用户被禁用 → 401，角色非 admin → 403。
func RequireAdmin(store *model.Store, next http.Handler) http.Handler {
	return requireRole(store, next, model.RoleAdmin)
}

// RequireUser 校验用户面访问：token 缺失/非法/用户不存在/用户被禁用 → 401；admin 与 user 角色均可通过。
func RequireUser(store *model.Store, next http.Handler) http.Handler {
	return requireRole(store, next, model.RoleAdmin, model.RoleUser)
}

// UserFromContext 取出经鉴权中间件注入的当前用户。
func UserFromContext(ctx context.Context) (*model.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(*model.User)
	return u, ok
}

// requireRole 是两中间件的公共实现：鉴权通过后将用户注入 context 再交给 next。
func requireRole(store *model.Store, next http.Handler, roles ...string) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := authenticate(store, r)
		if err != nil {
			if errors.Is(err, errUnauthenticated) {
				httpx.WriteError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
			// 后端点不可用：以 503 明示（日志留细节），避免被误读为「凭据无效」。
			slog.Error("auth backend error", "error", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, "authentication backend unavailable")
			return
		}
		if !u.Enabled {
			httpx.WriteError(w, http.StatusUnauthorized, "user is disabled")
			return
		}
		if _, ok := allowed[u.Role]; !ok {
			httpx.WriteError(w, http.StatusForbidden, "insufficient role")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, u)))
	})
}

// authenticate 解析 Bearer token 并按其哈希查库：
// 凭据缺失/用户不存在 → errUnauthenticated；查询故障 → 原样返回（调用方按 503 处理）。
func authenticate(store *model.Store, r *http.Request) (*model.User, error) {
	token, ok := bearerToken(r)
	if !ok {
		return nil, errUnauthenticated
	}
	u, err := store.GetUserByTokenHash(r.Context(), model.HashToken(token))
	if err != nil {
		if model.IsNotFound(err) {
			return nil, errUnauthenticated
		}
		return nil, err
	}
	return u, nil
}

// bearerToken 提取 `Authorization: Bearer <token>` 中的 token。
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= len(bearerScheme) || !strings.EqualFold(h[:len(bearerScheme)], bearerScheme) {
		return "", false
	}
	token := strings.TrimSpace(h[len(bearerScheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
