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

package proxy

import (
	"context"
	"log/slog"
	"time"

	"aigate/internal/event"
	"aigate/internal/model"
)

// rejectExpiredKey 判定密钥是否已过期；命中时记录首次观测时间并发出一次 key.expired 事件。
// 返回 true 表示调用方应按「无效凭据」口径拒绝请求（不额外暴露过期状态）。
//
// 去重语义：仅当 expired_at 从 NULL 变为有值的那一次调用发出事件（条件更新保证并发下至多一次）；
// 记录写入失败时按「非首次」处理（宁可不发，不重复刷事件），仅记警告。
func rejectExpiredKey(ctx context.Context, store *model.Store, key *model.APIKey, now time.Time) bool {
	if key.ExpiresAt == nil || !now.After(*key.ExpiresAt) {
		return false
	}
	first, err := store.MarkKeyExpiredIfFirst(ctx, key.ID, now.UTC())
	if err != nil {
		slog.Warn("mark key expired failed", "key_id", key.ID, "error", err)
		return true
	}
	if first {
		event.Emit(event.TypeKeyExpired, map[string]any{
			"key_id":  key.ID,
			"name":    key.Name,
			"user_id": key.UserID,
		})
	}
	return true
}
