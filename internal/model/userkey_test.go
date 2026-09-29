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

// mustCreateUserKeyModel 入库一个「用户自备 Key」模型（不保存共享密钥）。
func mustCreateUserKeyModel(t *testing.T, store *model.Store, modelID string) *model.AIModel {
	t.Helper()
	m := newTestAIModel(modelID)
	m.APIKey = "" // 自备 Key 模型不保存共享密钥
	m.UserKeyRequired = true
	if err := store.CreateAIModel(testCtx(), m); err != nil {
		t.Fatalf("create user-key model %q: %v", modelID, err)
	}
	return m
}

func bindingWithKey(aiModelID uint, role string, sortOrder int, key string) model.APIKeyModel {
	b := binding(aiModelID, role, sortOrder)
	b.APIKey = key
	return b
}

// TestResolveModelForKey_UserKey_PerBindingIsolation 照搬 hatchery UT-22：
// 两个密钥绑定同一自备模型，各自持不同绑定密钥，解析结果按绑定隔离、互不串用。
func TestResolveModelForKey_UserKey_PerBindingIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "byok-owner")
	m := mustCreateUserKeyModel(t, store, "gw-model")

	keyA, _ := mustCreateAPIKey(t, store, owner.ID, bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-user-a"))
	keyB, _ := mustCreateAPIKey(t, store, owner.ID, bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-user-b"))

	ra, err := store.ResolveModelForKey(ctx, keyA, "")
	if err != nil {
		t.Fatalf("resolve key A: %v", err)
	}
	rb, err := store.ResolveModelForKey(ctx, keyB, "")
	if err != nil {
		t.Fatalf("resolve key B: %v", err)
	}
	if ra.APIKey != "sk-user-a" {
		t.Errorf("key A resolved APIKey = %q, want sk-user-a", ra.APIKey)
	}
	if rb.APIKey != "sk-user-b" {
		t.Errorf("key B resolved APIKey = %q, want sk-user-b", rb.APIKey)
	}
}

// TestResolveModelForKey_SharedKeyModel_IgnoresBindingKey 照搬 hatchery UT-23：
// 普通模型的绑定行意外携带密钥时，仍只使用模型共享密钥（防用户覆盖）。
func TestResolveModelForKey_SharedKeyModel_IgnoresBindingKey(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "shared-owner")
	m := mustCreateAIModel(t, store, "shared-model") // APIKey = upstream-secret

	key, _ := mustCreateAPIKey(t, store, owner.ID, bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-injected"))

	r, err := store.ResolveModelForKey(ctx, key, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.APIKey != "upstream-secret" {
		t.Errorf("APIKey = %q, want the model shared key (binding key must be ignored)", r.APIKey)
	}
}

// TestResolveModelForKey_UserKey_MissingBindingKey 照搬 hatchery UT-24：
// 自备模型绑定缺密钥时不可解析（不回退共享密钥），未指定模型与指定模型名两条路径都成立。
func TestResolveModelForKey_UserKey_MissingBindingKey(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "missing-owner")
	m := mustCreateUserKeyModel(t, store, "gw-missing")

	// 绑定存在但未携带密钥。
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(m.ID, model.ModelRolePrimary, 0))

	if _, err := store.ResolveModelForKey(ctx, key, ""); err != model.ErrBindingKeyMissing {
		t.Errorf("unnamed resolve err = %v, want model.ErrBindingKeyMissing", err)
	}
	if _, err := store.ResolveModelForKey(ctx, key, "gw-missing"); err != model.ErrBindingKeyMissing {
		t.Errorf("named resolve err = %v, want model.ErrBindingKeyMissing", err)
	}
}

// TestResolveModelForKey_UserKey_NamedMatch：指定模型名命中自备绑定并取绑定密钥。
func TestResolveModelForKey_UserKey_NamedMatch(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "named-owner")
	m := mustCreateUserKeyModel(t, store, "gw/named")

	key, _ := mustCreateAPIKey(t, store, owner.ID, bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-named"))

	r, err := store.ResolveModelForKey(ctx, key, "GW/NAMED") // 大小写不敏感
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.APIKey != "sk-named" {
		t.Errorf("APIKey = %q, want sk-named", r.APIKey)
	}
	if r.QuotaDay != m.QuotaDay {
		t.Errorf("QuotaDay = %d, want %d (自备模型沿用内置配额口径)", r.QuotaDay, m.QuotaDay)
	}
}

// TestBindingKey_SealedAtRest：配置主密钥时绑定密钥以 enc:v1: 密文落库，读取解析为明文。
func TestBindingKey_SealedAtRest(t *testing.T) {
	withMasterKey(t, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	store, dsn := newTestStoreAt(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "sealed-owner")
	m := mustCreateUserKeyModel(t, store, "gw-sealed")

	key, _ := mustCreateAPIKey(t, store, owner.ID, bindingWithKey(m.ID, model.ModelRolePrimary, 0, "sk-plain-secret"))

	// 直读数据库列：必须是密文。
	db := openReadDB(t, dsn)
	var stored string
	if err := db.Model(&model.APIKeyModel{}).Where("api_key_id = ?", key.ID).Pluck("api_key", &stored).Error; err != nil {
		t.Fatalf("read binding column: %v", err)
	}
	if !strings.HasPrefix(stored, crypto.CiphertextPrefix) {
		t.Fatalf("stored binding key = %q, want %q ciphertext", stored, crypto.CiphertextPrefix)
	}
	if strings.Contains(stored, "sk-plain-secret") {
		t.Fatal("stored binding key must not contain the plaintext")
	}

	// 解析路径读到明文。
	r, err := store.ResolveModelForKey(ctx, key, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.APIKey != "sk-plain-secret" {
		t.Errorf("resolved APIKey = %q, want decrypted plaintext", r.APIKey)
	}
}

// TestListKeyModels_SkipsUnresolvableUserKeyBindings 照搬 hatchery UT-26：
// 数据面列表（onlyEnabled）跳过缺密钥的自备绑定；管理视图保留并以 has_key 暴露状态。
func TestListKeyModels_SkipsUnresolvableUserKeyBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	owner := mustCreateUser(t, store, "list-owner")
	mWith := mustCreateUserKeyModel(t, store, "gw-list-with")
	mWithout := mustCreateUserKeyModel(t, store, "gw-list-without")

	key, _ := mustCreateAPIKey(t, store, owner.ID,
		bindingWithKey(mWith.ID, model.ModelRolePrimary, 0, "sk-list"),
		binding(mWithout.ID, model.ModelRoleFallback, 1),
	)

	// 数据面：只返回可解析的绑定。
	infos, err := store.ListKeyModels(ctx, key.ID, true)
	if err != nil {
		t.Fatalf("list (enabled): %v", err)
	}
	if len(infos) != 1 || infos[0].ModelID != "gw-list-with" || !infos[0].HasKey {
		t.Fatalf("enabled list = %+v, want only gw-list-with with has_key=true", infos)
	}

	// 管理视图：保留两条，has_key 反映实际状态。
	infos, err = store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("list (all): %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("all list len = %d, want 2", len(infos))
	}
	byID := map[string]model.BindingInfo{}
	for _, b := range infos {
		byID[b.ModelID] = b
	}
	if !byID["gw-list-with"].HasKey || byID["gw-list-without"].HasKey {
		t.Fatalf("has_key flags = %+v, want with=true without=false", byID)
	}
}
