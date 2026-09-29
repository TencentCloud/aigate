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

package model_test

// 黑盒单测：webhook secret 的落库加密与迁移（与 ai_models.api_key / credentials.secrets_json 同口径）。

import (
	"strings"
	"testing"

	"aigate/internal/crypto"
	"aigate/internal/model"
)

// TestWebhook_SecretEncryptedAtRest 覆盖 webhook secret 的落库加密：
// 配置主密钥后列内为 enc:v1: 密文；读侧（Get / List / ListEnabled）返回明文；
// 更新 secret 同样加密。未配置主密钥时保持明文（防误伤本地体验）。
func TestWebhook_SecretEncryptedAtRest(t *testing.T) {
	withMasterKey(t, testMasterKey)
	store, dsn := newTestStoreAt(t)
	read := openReadDB(t, dsn)
	ctx := testCtx()

	hook := &model.Webhook{
		Name:       "h",
		URL:        "https://example.com/hook",
		Secret:     "s3cret-signing-key",
		EventTypes: "[]",
		Enabled:    true,
	}
	if err := store.CreateWebhook(ctx, hook); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if hook.ID == 0 {
		t.Fatal("webhook ID not backfilled")
	}
	if hook.Secret != "s3cret-signing-key" {
		t.Fatalf("in-memory secret mutated: %q", hook.Secret)
	}

	var raw model.Webhook
	if err := read.First(&raw, hook.ID).Error; err != nil {
		t.Fatalf("read raw webhook row: %v", err)
	}
	if !strings.HasPrefix(raw.Secret, crypto.CiphertextPrefix) {
		t.Fatalf("persisted secret missing ciphertext prefix: %q", raw.Secret)
	}

	got, err := store.GetWebhook(ctx, hook.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.Secret != "s3cret-signing-key" {
		t.Fatalf("GetWebhook secret = %q, want plaintext", got.Secret)
	}

	hooks, err := store.ListEnabledWebhooks(ctx)
	if err != nil {
		t.Fatalf("ListEnabledWebhooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Secret != "s3cret-signing-key" {
		t.Fatalf("ListEnabledWebhooks returned unexpected secret: %+v", hooks)
	}

	// 更新 secret 后仍为密文、读回新明文。
	if _, err := store.UpdateWebhook(ctx, hook.ID, map[string]any{"secret": "rotated-secret"}); err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}
	if err := read.First(&raw, hook.ID).Error; err != nil {
		t.Fatalf("re-read raw webhook row: %v", err)
	}
	if !strings.HasPrefix(raw.Secret, crypto.CiphertextPrefix) {
		t.Fatalf("updated secret not encrypted: %q", raw.Secret)
	}
	after, err := store.GetWebhook(ctx, hook.ID)
	if err != nil {
		t.Fatalf("GetWebhook after update: %v", err)
	}
	if after.Secret != "rotated-secret" {
		t.Fatalf("secret after update = %q, want rotated-secret", after.Secret)
	}
}

// TestReencryptSecrets_CoversWebhookSecret 覆盖迁移覆盖 webhooks.secret：
// 明文行被就地加密、幂等重跑全部 skipped。
func TestReencryptSecrets_CoversWebhookSecret(t *testing.T) {
	// 先在未配置主密钥的状态下写入明文行。
	store, dsn := newTestStoreAt(t)
	read := openReadDB(t, dsn)
	ctx := testCtx()

	hook := &model.Webhook{
		Name:       "legacy",
		URL:        "https://example.com/hook",
		Secret:     "legacy-plain-secret",
		EventTypes: "[]",
		Enabled:    true,
	}
	if err := store.CreateWebhook(ctx, hook); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	var raw model.Webhook
	if err := read.First(&raw, hook.ID).Error; err != nil {
		t.Fatalf("read raw webhook row: %v", err)
	}
	if crypto.IsSealed(raw.Secret) {
		t.Fatalf("without master key the secret should stay plaintext, got %q", raw.Secret)
	}

	withMasterKey(t, testMasterKey)
	migrated, skipped, failed, err := model.ReencryptSecrets(ctx, store)
	if err != nil {
		t.Fatalf("ReencryptSecrets: %v", err)
	}
	if migrated != 1 || skipped != 0 || failed != 0 {
		t.Fatalf("migration stats = migrated=%d skipped=%d failed=%d, want 1/0/0", migrated, skipped, failed)
	}
	if err := read.First(&raw, hook.ID).Error; err != nil {
		t.Fatalf("re-read raw webhook row: %v", err)
	}
	if !strings.HasPrefix(raw.Secret, crypto.CiphertextPrefix) {
		t.Fatalf("secret not encrypted after migration: %q", raw.Secret)
	}

	// 幂等：重跑全部跳过；读侧仍拿到明文。
	migrated, skipped, failed, err = model.ReencryptSecrets(ctx, store)
	if err != nil {
		t.Fatalf("ReencryptSecrets (second run): %v", err)
	}
	if migrated != 0 || failed != 0 || skipped != 1 {
		t.Fatalf("second run stats = migrated=%d skipped=%d failed=%d, want 0/1/0", migrated, skipped, failed)
	}
	got, err := store.GetWebhook(ctx, hook.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.Secret != "legacy-plain-secret" {
		t.Fatalf("secret after migration = %q, want plaintext", got.Secret)
	}
}
