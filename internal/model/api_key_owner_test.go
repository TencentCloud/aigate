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
	"testing"

	"aigate/internal/crypto"
	"aigate/internal/model"
)

// TestListAndCountAPIKeysByOwner 覆盖按属主过滤与计数：只返回该用户名下的密钥，他人与不存在的属主互不影响。
func TestListAndCountAPIKeysByOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	alice := mustCreateUser(t, store, "alice")
	bob := mustCreateUser(t, store, "bob")
	m1 := mustCreateAIModel(t, store, "m1")

	aliceFirst, _ := mustCreateAPIKey(t, store, alice.ID, binding(m1.ID, model.ModelRolePrimary, 0))
	aliceSecond, _ := mustCreateAPIKey(t, store, alice.ID)
	bobKey, _ := mustCreateAPIKey(t, store, bob.ID, binding(m1.ID, model.ModelRolePrimary, 0))

	t.Run("lists filtered by owner, ordered by ID asc", func(t *testing.T) {
		got, err := store.ListAPIKeysByOwner(ctx, alice.ID)
		if err != nil {
			t.Fatalf("ListAPIKeysByOwner: %v", err)
		}
		if len(got) != 2 || got[0].ID != aliceFirst.ID || got[1].ID != aliceSecond.ID {
			t.Fatalf("alice keys = %+v, want [%d %d]", got, aliceFirst.ID, aliceSecond.ID)
		}
		for i := range got {
			if got[i].UserID != alice.ID {
				t.Fatalf("returned a key not owned by alice: %+v", got[i])
			}
		}

		bobKeys, err := store.ListAPIKeysByOwner(ctx, bob.ID)
		if err != nil {
			t.Fatalf("ListAPIKeysByOwner(bob): %v", err)
		}
		if len(bobKeys) != 1 || bobKeys[0].ID != bobKey.ID {
			t.Fatalf("bob keys = %+v, want only %d", bobKeys, bobKey.ID)
		}
	})

	t.Run("owner without keys returns empty", func(t *testing.T) {
		none, err := store.ListAPIKeysByOwner(ctx, 999999)
		if err != nil {
			t.Fatalf("ListAPIKeysByOwner(unknown): %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("keys of unknown owner = %+v, want empty", none)
		}
	})

	t.Run("counts isolated per owner", func(t *testing.T) {
		cases := []struct {
			owner uint
			want  int64
		}{
			{owner: alice.ID, want: 2},
			{owner: bob.ID, want: 1},
			{owner: 999999, want: 0},
		}
		for _, tc := range cases {
			got, err := store.CountAPIKeysByOwner(ctx, tc.owner)
			if err != nil {
				t.Fatalf("CountAPIKeysByOwner(%d): %v", tc.owner, err)
			}
			if got != tc.want {
				t.Errorf("CountAPIKeysByOwner(%d) = %d, want %d", tc.owner, got, tc.want)
			}
		}
	})

	// 删除后计数与列表同步收敛。
	if err := store.DeleteAPIKey(ctx, aliceSecond.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if n, err := store.CountAPIKeysByOwner(ctx, alice.ID); err != nil || n != 1 {
		t.Fatalf("count after delete = (%d, %v), want 1", n, err)
	}
}

// TestResolveAPIKeyForModel_Branches 覆盖解析的四条分支：// 凭据优先 / 无凭据回退模型 api_key / 禁用凭据回退 / 解密失败报错。
func TestResolveAPIKeyForModel_Branches(t *testing.T) {
	t.Run("credential-takes-precedence", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := mustCreateCredential(t, store, "cred", "sk-from-credential")
		_, m := mustModelWithCredential(t, store, "m-cred", cred.ID)

		got, err := model.ResolveAPIKeyForModel(ctx, store, m)
		if err != nil {
			t.Fatalf("ResolveAPIKeyForModel: %v", err)
		}
		if got != "sk-from-credential" {
			t.Fatalf("resolved key = %q, want credential value", got)
		}
	})

	t.Run("fallback-to-model-api-key", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		_, m := mustModelWithCredential(t, store, "m-plain", 0)

		got, err := model.ResolveAPIKeyForModel(ctx, store, m)
		if err != nil {
			t.Fatalf("ResolveAPIKeyForModel: %v", err)
		}
		if got != "upstream-secret" {
			t.Fatalf("resolved key = %q, want model's own api_key", got)
		}
	})

	t.Run("fallback-disabled-credential", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		cred := mustCreateCredential(t, store, "cred", "sk-disabled")
		_, m := mustModelWithCredential(t, store, "m-disabled", cred.ID)
		if _, err := store.UpdateCredential(ctx, cred.ID, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("disable credential: %v", err)
		}

		got, err := model.ResolveAPIKeyForModel(ctx, store, m)
		if err != nil {
			t.Fatalf("ResolveAPIKeyForModel: %v", err)
		}
		if got != "upstream-secret" {
			t.Fatalf("resolved key = %q, want fallback to model api_key when credential disabled", got)
		}
	})

	t.Run("decrypt-failure-errors", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		withMasterKey(t, testMasterKey)
		cred := mustCreateCredential(t, store, "cred-enc", "sk-encrypted")
		_, withCred := mustModelWithCredential(t, store, "m-bad-cred", cred.ID)
		_, plainModel := mustModelWithCredential(t, store, "m-bad-key", 0)

		// 更换主密钥：凭据与模型 api_key 的密文均不可解，必须显式报错而不是回退。
		if err := crypto.Configure(otherMasterKey); err != nil {
			t.Fatalf("configure other master key: %v", err)
		}
		if got, err := model.ResolveAPIKeyForModel(ctx, store, withCred); err == nil {
			t.Fatalf("credential decrypt failure: want error, got success: %q", got)
		}
		if got, err := model.ResolveAPIKeyForModel(ctx, store, plainModel); err == nil {
			t.Fatalf("model api_key decrypt failure: want error, got success: %q", got)
		}
	})
}

// TestCreateAIModel_EmptyAPIKeyStaysEmpty 覆盖空 api_key 的落库口径：
// 配置主密钥时也保持空串（表示「未自带密钥」），不会被加密成密文；读回仍为空串。
func TestCreateAIModel_EmptyAPIKeyStaysEmpty(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	ctx := testCtx()
	withMasterKey(t, testMasterKey)

	m := &model.AIModel{
		Provider:  "test-provider",
		ModelID:   "empty-key-model",
		URL:       "https://upstream.test/v1",
		ModelType: model.ModelTypeOpenAI,
		QuotaDay:  -1,
		Enabled:   true,
	}
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatalf("CreateAIModel: %v", err)
	}
	if m.APIKey != "" {
		t.Fatalf("in-memory api_key = %q, want empty", m.APIKey)
	}

	db := openReadDB(t, dsn)
	var raw model.AIModel
	if err := db.First(&raw, m.ID).Error; err != nil {
		t.Fatalf("read raw model row: %v", err)
	}
	if raw.APIKey != "" {
		t.Fatalf("persisted api_key = %q, want empty", raw.APIKey)
	}

	got, err := store.GetAIModel(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetAIModel: %v", err)
	}
	if got.APIKey != "" {
		t.Fatalf("read-back api_key = %q, want empty", got.APIKey)
	}
}
