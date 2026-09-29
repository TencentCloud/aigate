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

import (
	"strings"
	"testing"

	"aigate/internal/crypto"
	"aigate/internal/model"
)

// testMasterKey 是测试用主密钥（64 位 hex，解码 32 字节）。
const testMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const otherMasterKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// withMasterKey 启用主密钥，并在测试结束时恢复为禁用态（crypto 是包级状态，避免串扰其它用例）。
func withMasterKey(t *testing.T, key string) {
	t.Helper()
	if err := crypto.Configure(key); err != nil {
		t.Fatalf("configure master key: %v", err)
	}
	t.Cleanup(func() { _ = crypto.Configure("") })
}

// mustCreateCredential 入库一条带 header 密钥的凭据。
func mustCreateCredential(t *testing.T, store *model.Store, name, value string) *model.Credential {
	t.Helper()
	c := &model.Credential{Name: name}
	if err := c.SetSecrets(
		[]model.SecretItem{{Key: "Authorization", Value: value, Location: model.SecretLocationHeader}},
	); err != nil {
		t.Fatalf("SetSecrets(%s): %v", name, err)
	}
	if err := store.CreateCredential(testCtx(), c); err != nil {
		t.Fatalf("CreateCredential(%s): %v", name, err)
	}
	return c
}

// TestCredential_CRUDAndMasked 覆盖凭据 CRUD、掩码展示与「读回明文」（未配置主密钥路径）。
func TestCredential_CRUDAndMasked(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	c := mustCreateCredential(t, store, "legacy", "sk-1234567890")
	if c.ID == 0 {
		t.Fatal("credential ID not backfilled")
	}
	if !c.Enabled {
		t.Fatal("credential should be enabled by default")
	}

	got, err := store.GetCredentialByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	if got.Name != "legacy" || got.Description != "" {
		t.Fatalf("GetCredentialByID = %+v", got)
	}
	// 未配置主密钥：落库为明文 JSON。
	if crypto.IsSealed(got.SecretsJSON) {
		t.Fatalf("SecretsJSON should not be ciphertext when encryption disabled: %q", got.SecretsJSON)
	}
	secrets, err := got.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Key != "Authorization" || secrets[0].Value != "sk-1234567890" {
		t.Fatalf("GetSecrets = %+v", secrets)
	}
	if v, err := got.FirstHeaderSecretValue(); err != nil || v != "sk-1234567890" {
		t.Fatalf("FirstHeaderSecretValue = (%q, %v)", v, err)
	}
	masked, err := got.GetMaskedSecrets()
	if err != nil {
		t.Fatalf("GetMaskedSecrets: %v", err)
	}
	if len(masked) != 1 || masked[0].Value != "sk*********90" {
		t.Fatalf("GetMaskedSecrets = %+v, want sk*********90", masked)
	}
	if masked[0].Key != "Authorization" || masked[0].Location != model.SecretLocationHeader {
		t.Fatalf("masked response should keep key/location: %+v", masked[0])
	}

	updated, err := store.UpdateCredential(ctx, c.ID, map[string]any{"name": "legacy-2", "enabled": false})
	if err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	if updated.Name != "legacy-2" || updated.Enabled {
		t.Fatalf("UpdateCredential = %+v, want name=legacy-2 enabled=false", updated)
	}
	if updated.SecretsJSON != got.SecretsJSON {
		t.Fatal("secrets must not change when secrets_json omitted")
	}

	second := mustCreateCredential(t, store, "second", "sk-second")
	list, err := store.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 2 || list[0].ID >= list[1].ID {
		t.Fatalf("ListCredentials = %+v, want 2 rows ordered by ID asc", list)
	}

	if err := store.DeleteCredential(ctx, second.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := store.GetCredentialByID(ctx, second.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted credential still readable: %v", err)
	}
	if _, err := store.GetCredentialByID(ctx, 999999); !model.IsNotFound(err) {
		t.Fatalf("GetCredentialByID(unknown) err = %v, want not found", err)
	}
}

// TestMaskSecret 覆盖掩码规则：首尾各 2 字符、长度 ≤4 全掩码。
func TestMaskSecret(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "a", want: "*"},
		{in: "ab", want: "**"},
		{in: "abcd", want: "****"},
		{in: "abcde", want: "ab*de"},
		{in: "sk-1234567890", want: "sk*********90"},
		{in: "密钥值中文测试", want: "密钥***测试"},
	}
	for _, tt := range tests {
		if got := model.MaskSecret(tt.in); got != tt.want {
			t.Errorf("MaskSecret(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCredential_SecretsEncryptedAtRest 覆盖配置主密钥后的落库密文与读回明文。
func TestCredential_SecretsEncryptedAtRest(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	ctx := testCtx()
	withMasterKey(t, testMasterKey)

	c := mustCreateCredential(t, store, "enc", "sk-encrypted-value")

	got, err := store.GetCredentialByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	if !crypto.IsSealed(got.SecretsJSON) {
		t.Fatalf("SecretsJSON not encrypted: %q", got.SecretsJSON)
	}
	if strings.Contains(got.SecretsJSON, "sk-encrypted-value") {
		t.Fatalf("plaintext found in ciphertext: %q", got.SecretsJSON)
	}
	if v, err := got.FirstHeaderSecretValue(); err != nil || v != "sk-encrypted-value" {
		t.Fatalf("FirstHeaderSecretValue = (%q, %v), want decrypted plaintext", v, err)
	}

	// 另一条连接直读原始列，确认落库即密文。
	db := openReadDB(t, dsn)
	var raw model.Credential
	if err := db.First(&raw, c.ID).Error; err != nil {
		t.Fatalf("read raw credential row: %v", err)
	}
	if !strings.HasPrefix(raw.SecretsJSON, crypto.CiphertextPrefix) {
		t.Fatalf("persisted column missing ciphertext prefix: %q", raw.SecretsJSON)
	}

	// 更新密钥后仍为密文、读回新明文。
	sealed, err := model.SealSecrets(
		[]model.SecretItem{{Key: "Authorization", Value: "sk-rotated", Location: "header"}},
	)
	if err != nil {
		t.Fatalf("SealSecrets: %v", err)
	}
	if _, err := store.UpdateCredential(ctx, c.ID, map[string]any{"secrets_json": sealed}); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	after, err := store.GetCredentialByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID(after): %v", err)
	}
	if !crypto.IsSealed(after.SecretsJSON) {
		t.Fatalf("SecretsJSON not encrypted after update: %q", after.SecretsJSON)
	}
	if v, err := after.FirstHeaderSecretValue(); err != nil || v != "sk-rotated" {
		t.Fatalf("FirstHeaderSecretValue after update = (%q, %v)", v, err)
	}
}

// TestCredential_SecretsEdgeCases 覆盖空密钥列表与损坏密文。
func TestCredential_SecretsEdgeCases(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	empty := &model.Credential{Name: "empty"}
	if err := empty.SetSecrets(nil); err != nil {
		t.Fatalf("SetSecrets(nil): %v", err)
	}
	if empty.SecretsJSON != "" {
		t.Fatalf("empty secret list should persist as empty string, got %q", empty.SecretsJSON)
	}
	if err := store.CreateCredential(ctx, empty); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	got, err := store.GetCredentialByID(ctx, empty.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID: %v", err)
	}
	if secrets, err := got.GetSecrets(); err != nil || secrets != nil {
		t.Fatalf("GetSecrets(empty) = (%+v, %v), want (nil, nil)", secrets, err)
	}
	if v, err := got.FirstHeaderSecretValue(); err != nil || v != "" {
		t.Fatalf("FirstHeaderSecretValue(empty) = (%q, %v), want empty string", v, err)
	}

	// 密文损坏：解密报错而非静默返回。
	withMasterKey(t, testMasterKey)
	broken := &model.Credential{Name: "broken", SecretsJSON: crypto.CiphertextPrefix + "AAAA"}
	if err := store.CreateCredential(ctx, broken); err != nil {
		t.Fatalf("CreateCredential(broken): %v", err)
	}
	row, err := store.GetCredentialByID(ctx, broken.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID(broken): %v", err)
	}
	if _, err := row.GetSecrets(); err == nil {
		t.Fatal("corrupted ciphertext: GetSecrets should fail")
	}
	if _, err := row.FirstHeaderSecretValue(); err == nil {
		t.Fatal("corrupted ciphertext: FirstHeaderSecretValue should fail")
	}
	if _, err := row.GetMaskedSecrets(); err == nil {
		t.Fatal("corrupted ciphertext: GetMaskedSecrets should fail")
	}
}

// TestCredential_FirstHeaderSecretValue 覆盖「首条 header」语义与 location 归一化。
func TestCredential_FirstHeaderSecretValue(t *testing.T) {
	tests := []struct {
		name  string
		items []model.SecretItem
		want  string
	}{
		{
			name: "first-header-wins",
			items: []model.SecretItem{
				{Key: "h1", Value: "v1", Location: "header"},
				{Key: "h2", Value: "v2", Location: "header"},
			},
			want: "v1",
		},
		{
			name: "skip-non-header",
			items: []model.SecretItem{
				{Key: "q", Value: "ignored", Location: "query"},
				{Key: "h", Value: "used", Location: "header"},
			},
			want: "used",
		},
		{
			name:  "empty-location-treated-as-header",
			items: []model.SecretItem{{Key: "h", Value: "used", Location: ""}},
			want:  "used",
		},
		{
			name:  "no-header-item",
			items: []model.SecretItem{{Key: "q", Value: "x", Location: "query"}},
			want:  "",
		},
		{
			name: "empty-value-skipped",
			items: []model.SecretItem{
				{Key: "h1", Value: "", Location: "header"},
				{Key: "h2", Value: "v2", Location: "header"},
			},
			want: "v2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &model.Credential{}
			if err := c.SetSecrets(tt.items); err != nil {
				t.Fatalf("SetSecrets: %v", err)
			}
			got, err := c.FirstHeaderSecretValue()
			if err != nil {
				t.Fatalf("FirstHeaderSecretValue: %v", err)
			}
			if got != tt.want {
				t.Fatalf("FirstHeaderSecretValue = %q, want %q", got, tt.want)
			}
		})
	}
}

// 构造一个「模型 + 主密钥绑定 + 引用凭据」的解析场景，返回密钥与模型。
func mustModelWithCredential(
	t *testing.T,
	store *model.Store,
	modelID string,
	credID uint,
) (*model.APIKey, *model.AIModel) {
	t.Helper()
	ctx := testCtx()
	owner := mustCreateUser(t, store, "owner-"+modelID)
	m := newTestAIModel(modelID)
	m.CredentialID = credID
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatalf("CreateAIModel: %v", err)
	}
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(m.ID, model.ModelRolePrimary, 0))
	return key, m
}

// TestResolveModelForKey_CredentialChain 覆盖凭据实时解析、回退与解密失败。
func TestResolveModelForKey_CredentialChain(t *testing.T) {
	t.Run("credential-takes-precedence", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := mustCreateCredential(t, store, "cred", "sk-from-credential")
		key, _ := mustModelWithCredential(t, store, "m-cred", cred.ID)

		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "sk-from-credential" {
			t.Fatalf("APIKey = %q, want credential secret", got.APIKey)
		}
	})

	t.Run("credential-update-takes-effect-immediately", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := mustCreateCredential(t, store, "cred", "sk-old")
		key, _ := mustModelWithCredential(t, store, "m-live", cred.ID)

		sealed, err := model.SealSecrets(
			[]model.SecretItem{{Key: "Authorization", Value: "sk-new", Location: "header"}},
		)
		if err != nil {
			t.Fatalf("SealSecrets: %v", err)
		}
		if _, err := store.UpdateCredential(ctx, cred.ID, map[string]any{"secrets_json": sealed}); err != nil {
			t.Fatalf("UpdateCredential: %v", err)
		}
		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "sk-new" {
			t.Fatalf("APIKey = %q, want live read of the new secret", got.APIKey)
		}
	})

	t.Run("fallback-without-credential", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		key, _ := mustModelWithCredential(t, store, "m-plain", 0)

		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "upstream-secret" {
			t.Fatalf("APIKey = %q, want model's own api_key", got.APIKey)
		}
	})

	t.Run("fallback-disabled-credential", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := mustCreateCredential(t, store, "cred", "sk-disabled")
		key, _ := mustModelWithCredential(t, store, "m-disabled", cred.ID)
		if _, err := store.UpdateCredential(ctx, cred.ID, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("disable credential: %v", err)
		}

		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "upstream-secret" {
			t.Fatalf("APIKey = %q, want fallback to model api_key", got.APIKey)
		}
	})

	t.Run("fallback-missing-credential", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		key, m := mustModelWithCredential(t, store, "m-missing", 0)
		if _, err := store.UpdateAIModel(ctx, m.ID, map[string]any{"credential_id": 999999}); err != nil {
			t.Fatalf("set credential_id: %v", err)
		}

		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "upstream-secret" {
			t.Fatalf("APIKey = %q, want fallback when credential missing", got.APIKey)
		}
	})

	t.Run("fallback-credential-without-header-secret", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := &model.Credential{Name: "no-header"}
		if err := cred.SetSecrets([]model.SecretItem{{Key: "q", Value: "v", Location: "query"}}); err != nil {
			t.Fatalf("SetSecrets: %v", err)
		}
		if err := store.CreateCredential(ctx, cred); err != nil {
			t.Fatalf("CreateCredential: %v", err)
		}
		key, _ := mustModelWithCredential(t, store, "m-noheader", cred.ID)

		got, err := store.ResolveModelForKey(ctx, key, "")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.APIKey != "upstream-secret" {
			t.Fatalf("APIKey = %q, want fallback when no header secret", got.APIKey)
		}
	})

	t.Run("credential-decrypt-failure-errors", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		withMasterKey(t, testMasterKey)
		cred := mustCreateCredential(t, store, "cred", "sk-encrypted")
		key, _ := mustModelWithCredential(t, store, "m-badkey", cred.ID)

		// 更换主密钥：凭据密文不可解，必须显式失败而不是回退。
		if err := crypto.Configure(otherMasterKey); err != nil {
			t.Fatalf("configure other key: %v", err)
		}
		got, err := store.ResolveModelForKey(ctx, key, "")
		if err == nil {
			t.Fatalf("ResolveModelForKey after key change: want error, got success: %+v", got)
		}
		if got != nil {
			t.Fatalf("must not return target model on failure: %+v", got)
		}
	})

	t.Run("model-api-key-decrypt-failure-errors", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		withMasterKey(t, testMasterKey)
		key, _ := mustModelWithCredential(t, store, "m-badkey2", 0)

		if err := crypto.Configure(otherMasterKey); err != nil {
			t.Fatalf("configure other key: %v", err)
		}
		if got, err := store.ResolveModelForKey(ctx, key, ""); err == nil {
			t.Fatalf("model secret decrypt failure: want error, got success: %+v", got)
		}
	})
}

// TestAIModel_APIKeyEncryptedAtRest 覆盖模型的写入加密 / 读取解密 / 解密失败报错。
func TestAIModel_APIKeyEncryptedAtRest(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	ctx := testCtx()
	withMasterKey(t, testMasterKey)

	m := mustCreateAIModel(t, store, "enc-model")
	if strings.Contains(m.APIKey, "upstream-secret") {
		t.Fatalf("in-memory entity must not keep plaintext: %q", m.APIKey)
	}

	got, err := store.GetAIModel(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetAIModel: %v", err)
	}
	if got.APIKey != "upstream-secret" {
		t.Fatalf("GetAIModel APIKey = %q, want plaintext", got.APIKey)
	}

	db := openReadDB(t, dsn)
	var raw model.AIModel
	if err := db.First(&raw, m.ID).Error; err != nil {
		t.Fatalf("read raw model row: %v", err)
	}
	if !crypto.IsSealed(raw.APIKey) {
		t.Fatalf("persisted api_key not encrypted: %q", raw.APIKey)
	}

	list, err := store.ListAIModels(ctx)
	if err != nil {
		t.Fatalf("ListAIModels: %v", err)
	}
	if len(list) != 1 || list[0].APIKey != "upstream-secret" {
		t.Fatalf("ListAIModels did not decrypt: %+v", list)
	}

	updated, err := store.UpdateAIModel(ctx, m.ID, map[string]any{"api_key": "sk-rotated-model"})
	if err != nil {
		t.Fatalf("UpdateAIModel: %v", err)
	}
	if updated.APIKey != "sk-rotated-model" {
		t.Fatalf("UpdateAIModel APIKey = %q", updated.APIKey)
	}
	if err := db.First(&raw, m.ID).Error; err != nil {
		t.Fatalf("read raw model row: %v", err)
	}
	if !crypto.IsSealed(raw.APIKey) {
		t.Fatalf("persisted api_key not encrypted after update: %q", raw.APIKey)
	}

	// 解密失败必须显式报错（不静默返回密文）。
	if err := crypto.Configure(otherMasterKey); err != nil {
		t.Fatalf("configure other key: %v", err)
	}
	if _, err := store.GetAIModel(ctx, m.ID); err == nil {
		t.Fatal("GetAIModel after key change should fail")
	}
	if _, err := store.ListAIModels(ctx); err == nil {
		t.Fatal("ListAIModels after key change should fail")
	}
}

// TestCountModelCredentialRefs 统计引用凭据的模型数量。
func TestCountModelCredentialRefs(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	cred := mustCreateCredential(t, store, "cred", "sk-x")

	if n, err := store.CountModelCredentialRefs(ctx, cred.ID); err != nil || n != 0 {
		t.Fatalf("CountModelCredentialRefs(no refs) = (%d, %v)", n, err)
	}
	_, m1 := mustModelWithCredential(t, store, "ref-1", cred.ID)
	mustModelWithCredential(t, store, "ref-2", cred.ID)
	// 未引用凭据的模型不计入。
	mustModelWithCredential(t, store, "ref-0", 0)

	n, err := store.CountModelCredentialRefs(ctx, cred.ID)
	if err != nil {
		t.Fatalf("CountModelCredentialRefs: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountModelCredentialRefs = %d, want 2", n)
	}

	// 解除引用后计数归零（删除凭据的前置条件）。
	if _, err := store.UpdateAIModel(ctx, m1.ID, map[string]any{"credential_id": 0}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if n, err := store.CountModelCredentialRefs(ctx, cred.ID); err != nil || n != 1 {
		t.Fatalf("after detaching one ref = (%d, %v), want 1", n, err)
	}
}

// TestReencryptSecrets_MigratesAndIsIdempotent 覆盖一次性重加密迁移：明文行加密、空行跳过、重跑幂等。
func TestReencryptSecrets_MigratesAndIsIdempotent(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	ctx := testCtx()

	// 未配置主密钥时写入的明文行（模型与凭据各含一条空值行）。
	plainModel := mustCreateAIModel(t, store, "plain-model")
	emptyModel := &model.AIModel{
		ModelID:   "empty-key",
		URL:       "https://upstream.test/v1",
		ModelType: model.ModelTypeOpenAI,
		QuotaDay:  -1,
		Enabled:   true,
	}
	if err := store.CreateAIModel(ctx, emptyModel); err != nil {
		t.Fatalf("CreateAIModel(empty): %v", err)
	}
	plainCred := mustCreateCredential(t, store, "plain-cred", "sk-plain-credential")
	emptyCred := &model.Credential{Name: "empty-cred"}
	if err := emptyCred.SetSecrets(nil); err != nil {
		t.Fatalf("SetSecrets(nil): %v", err)
	}
	if err := store.CreateCredential(ctx, emptyCred); err != nil {
		t.Fatalf("CreateCredential(empty): %v", err)
	}

	// 未配置主密钥时拒绝执行。
	if _, _, _, err := model.ReencryptSecrets(ctx, store); err == nil {
		t.Fatal("ReencryptSecrets without master key should fail")
	}

	withMasterKey(t, testMasterKey)
	migrated, skipped, failed, err := model.ReencryptSecrets(ctx, store)
	if err != nil {
		t.Fatalf("ReencryptSecrets: %v", err)
	}
	if migrated != 2 || skipped != 2 || failed != 0 {
		t.Fatalf("first run stats = (migrated=%d skipped=%d failed=%d), want (2,2,0)", migrated, skipped, failed)
	}

	db := openReadDB(t, dsn)
	var rawModel model.AIModel
	if err := db.First(&rawModel, plainModel.ID).Error; err != nil {
		t.Fatalf("read raw model row: %v", err)
	}
	if !strings.HasPrefix(rawModel.APIKey, crypto.CiphertextPrefix) {
		t.Fatalf("api_key not encrypted after migration: %q", rawModel.APIKey)
	}
	var rawCred model.Credential
	if err := db.First(&rawCred, plainCred.ID).Error; err != nil {
		t.Fatalf("read raw credential row: %v", err)
	}
	if !strings.HasPrefix(rawCred.SecretsJSON, crypto.CiphertextPrefix) {
		t.Fatalf("secrets_json not encrypted after migration: %q", rawCred.SecretsJSON)
	}

	// 迁移后读侧仍拿到明文。
	gotModel, err := store.GetAIModel(ctx, plainModel.ID)
	if err != nil {
		t.Fatalf("GetAIModel after migrate: %v", err)
	}
	if gotModel.APIKey != "upstream-secret" {
		t.Fatalf("APIKey after migration = %q, want plaintext", gotModel.APIKey)
	}
	gotCred, err := store.GetCredentialByID(ctx, plainCred.ID)
	if err != nil {
		t.Fatalf("GetCredentialByID after migrate: %v", err)
	}
	if v, err := gotCred.FirstHeaderSecretValue(); err != nil || v != "sk-plain-credential" {
		t.Fatalf("credential value after migration = (%q, %v)", v, err)
	}

	// 重跑：全部跳过，且不产生二次加密。
	before := rawCred.SecretsJSON
	migrated, skipped, failed, err = model.ReencryptSecrets(ctx, store)
	if err != nil {
		t.Fatalf("ReencryptSecrets(second): %v", err)
	}
	if migrated != 0 || skipped != 4 || failed != 0 {
		t.Fatalf("second run stats = (migrated=%d skipped=%d failed=%d), want (0,4,0)", migrated, skipped, failed)
	}
	if err := db.First(&rawCred, plainCred.ID).Error; err != nil {
		t.Fatalf("re-read credential row: %v", err)
	}
	if rawCred.SecretsJSON != before {
		t.Fatalf("second run changed ciphertext: %q -> %q", before, rawCred.SecretsJSON)
	}
}
