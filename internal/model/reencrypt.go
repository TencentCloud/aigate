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
	"log/slog"
	"strings"

	"aigate/internal/crypto"
)

// ErrMasterKeyNotConfigured 表示未配置主密钥，重加密迁移无法执行。
var ErrMasterKeyNotConfigured = errors.New("master key is not configured")

// ReencryptSecrets 把 ai_models.api_key、api_key_models.api_key、credentials.secrets_json
// 与 webhooks.secret 中的明文行就地加密。
//
// 幂等：空串与已带 enc:v1: 前缀的行跳过（skipped）；单行失败只计入 failed 并输出告警，
// 不中断整体迁移。仅当顶层读取失败时返回 err。
func ReencryptSecrets(ctx context.Context, store *Store) (migrated, skipped, failed int, err error) {
	if !crypto.Enabled() {
		return 0, 0, 0, ErrMasterKeyNotConfigured
	}

	m, s, f, err := reencryptColumn(ctx, store, "ai_models", "api_key",
		func(t *AIModel) uint { return t.ID },
		func(t *AIModel) string { return t.APIKey })
	migrated, skipped, failed = m, s, f
	if err != nil {
		return migrated, skipped, failed, err
	}

	m, s, f, err = reencryptColumn(ctx, store, "api_key_models", "api_key",
		func(t *APIKeyModel) uint { return t.ID },
		func(t *APIKeyModel) string { return t.APIKey })
	migrated += m
	skipped += s
	failed += f
	if err != nil {
		return migrated, skipped, failed, err
	}

	m, s, f, err = reencryptColumn(ctx, store, "credentials", "secrets_json",
		func(t *Credential) uint { return t.ID },
		func(t *Credential) string { return t.SecretsJSON })
	migrated += m
	skipped += s
	failed += f
	if err != nil {
		return migrated, skipped, failed, err
	}

	m, s, f, err = reencryptColumn(ctx, store, "webhooks", "secret",
		func(t *Webhook) uint { return t.ID },
		func(t *Webhook) string { return t.Secret })
	migrated += m
	skipped += s
	failed += f
	return migrated, skipped, failed, err
}

// reencryptColumn 对单表（table）的单列（column）执行「明文 → 密文」就地迁移：
// 逐行加密，空串/已加密行计入 skipped，单行失败计入 failed 并记告警、不中断；
// 仅扫描失败时返回 err。idOf/valueOf 提取行 ID 与被加密列。
func reencryptColumn[T any](
	ctx context.Context,
	store *Store,
	table, column string,
	idOf func(t *T) uint,
	valueOf func(t *T) string,
) (migrated, skipped, failed int, err error) {
	var rows []T
	if err := store.db.WithContext(ctx).Model(new(T)).Select("id", column).Find(&rows).Error; err != nil {
		return 0, 0, 0, fmt.Errorf("scan %s for encryption migration: %w", table, err)
	}
	for i := range rows {
		row := &rows[i]
		sealed, needed, sealErr := sealPlaintext(valueOf(row))
		if sealErr != nil {
			failed++
			slog.Warn("encryption migration failed", "table", table, "id", idOf(row), "error", sealErr)
			continue
		}
		if !needed {
			skipped++
			continue
		}
		updateErr := store.db.WithContext(ctx).Model(new(T)).
			Where("id = ?", idOf(row)).Update(column, sealed).Error
		if updateErr != nil {
			failed++
			slog.Warn("encryption migration failed", "table", table, "id", idOf(row), "error", updateErr)
			continue
		}
		migrated++
	}
	return migrated, skipped, failed, nil
}

// sealPlaintext 对「非空且未加密」的值做加密；返回值 needed=false 表示该行应跳过。
func sealPlaintext(stored string) (sealed string, needed bool, err error) {
	if strings.TrimSpace(stored) == "" || crypto.IsSealed(stored) {
		return "", false, nil
	}
	sealed, err = crypto.Seal(stored)
	if err != nil {
		return "", false, err
	}
	return sealed, true, nil
}
