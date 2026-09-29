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
	"errors"
	"testing"

	"aigate/internal/model"
)

// 创建密钥并绑定模型：token 可查、绑定含角色与 sort_order、ListKeyModels 按 sort_order 排序。
func TestCreateAPIKey_WithBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	primary := mustCreateAIModel(t, store, "primary-model")
	fallback := mustCreateAIModel(t, store, "fallback-model")

	key, plain := mustCreateAPIKey(t, store, owner.ID,
		binding(fallback.ID, model.ModelRoleFallback, 10),
		binding(primary.ID, model.ModelRolePrimary, 0),
	)

	if key.ID == 0 {
		t.Fatal("key ID should be assigned")
	}
	if key.UserID != owner.ID {
		t.Fatalf("UserID = %d, want %d", key.UserID, owner.ID)
	}
	if !key.Enabled {
		t.Fatal("key should be enabled by default")
	}

	got, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken(plain))
	if err != nil {
		t.Fatalf("GetAPIKeyByTokenHash: %v", err)
	}
	if got.ID != key.ID {
		t.Fatalf("GetAPIKeyByTokenHash id = %d, want %d", got.ID, key.ID)
	}
	if got.ProxyTokenPrefix != key.ProxyTokenPrefix {
		t.Fatalf("ProxyTokenPrefix = %q, want %q", got.ProxyTokenPrefix, key.ProxyTokenPrefix)
	}

	infos, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListKeyModels len = %d, want 2", len(infos))
	}
	if infos[0].AIModelID != primary.ID || infos[0].Role != model.ModelRolePrimary {
		t.Fatalf("infos[0] = %+v, want primary model %d", infos[0], primary.ID)
	}
	if infos[1].AIModelID != fallback.ID || infos[1].Role != model.ModelRoleFallback {
		t.Fatalf("infos[1] = %+v, want fallback model %d", infos[1], fallback.ID)
	}
	if infos[0].SortOrder != 0 || infos[1].SortOrder != 10 {
		t.Fatalf("sort orders = %d/%d, want 0/10", infos[0].SortOrder, infos[1].SortOrder)
	}
	if infos[0].ModelID != primary.ModelID || infos[0].Provider != primary.Provider ||
		infos[0].ModelName != primary.ModelName {
		t.Fatalf("infos[0] = %+v, want model fields of %+v", infos[0], primary)
	}

	if _, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken("sk-unknown")); !model.IsNotFound(err) {
		t.Fatalf("GetAPIKeyByTokenHash(unknown) err = %v, want not found", err)
	}
}

// 绑定校验：两个 primary、无 primary、非法 role、ai_model_id=0、引用不存在的模型均被拒绝，
// 且拒绝发生在事务内 —— 密钥不落库。
func TestCreateAPIKey_RejectsInvalidBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")
	m2 := mustCreateAIModel(t, store, "m2")

	tests := []struct {
		name     string
		bindings []model.APIKeyModel
	}{
		{name: "two-primary", bindings: []model.APIKeyModel{
			binding(m1.ID, model.ModelRolePrimary, 0),
			binding(m2.ID, model.ModelRolePrimary, 1),
		}},
		{name: "no-primary", bindings: []model.APIKeyModel{
			binding(m1.ID, model.ModelRoleFallback, 0),
		}},
		{name: "invalid-role", bindings: []model.APIKeyModel{
			binding(m1.ID, model.ModelRolePrimary, 0),
			binding(m2.ID, "owner", 1),
		}},
		{name: "zero-model-id", bindings: []model.APIKeyModel{
			binding(0, model.ModelRolePrimary, 0),
		}},
		{name: "missing-model", bindings: []model.APIKeyModel{
			binding(999999, model.ModelRolePrimary, 0),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, plain := newAPIKey(t, owner.ID)
			if err := store.CreateAPIKey(ctx, key, tt.bindings); err == nil {
				t.Fatalf("CreateAPIKey(%s) succeeded, want error", tt.name)
			}
			if _, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken(plain)); !model.IsNotFound(err) {
				t.Fatalf("rejected key should not be persisted: err = %v", err)
			}
		})
	}
}

// 同一密钥重复绑定同一模型被唯一约束拒绝，事务回滚。
func TestCreateAPIKey_DuplicateBindingRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")

	key, plain := newAPIKey(t, owner.ID)
	err := store.CreateAPIKey(ctx, key, []model.APIKeyModel{
		binding(m1.ID, model.ModelRolePrimary, 0),
		binding(m1.ID, model.ModelRoleFallback, 1),
	})
	if err == nil {
		t.Fatal("duplicate binding on same model should be rejected")
	}
	if _, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken(plain)); !model.IsNotFound(err) {
		t.Fatalf("rejected key should not be persisted: err = %v", err)
	}
}

// 更新密钥 + 换发 token：旧 token 立即失效。
func TestAPIKey_UpdateAndRotate(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")
	key, oldPlain := mustCreateAPIKey(t, store, owner.ID, binding(m1.ID, model.ModelRolePrimary, 0))
	oldHash := model.HashToken(oldPlain)

	updated, err := store.UpdateAPIKey(ctx, key.ID, map[string]any{
		"name":       "renamed-key",
		"max_tokens": 2048,
		"enabled":    false,
	})
	if err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if updated.Name != "renamed-key" || updated.MaxTokens != 2048 || updated.Enabled {
		t.Fatalf("UpdateAPIKey = %+v, want name=renamed-key max_tokens=2048 enabled=false", updated)
	}
	if updated.ProxyTokenHash != oldHash {
		t.Fatal("UpdateAPIKey must not change token hash")
	}
	if _, err := store.GetAPIKeyByTokenHash(ctx, oldHash); err != nil {
		t.Fatalf("old token should still work before rotate: %v", err)
	}

	newPlain, display, err := store.RotateAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	if newPlain == oldPlain {
		t.Fatal("rotated token equals old token")
	}
	if len(display) != 8+len("…")+4 {
		t.Fatalf("display = %q, want first-8 + … + last-4 format", display)
	}

	if _, err := store.GetAPIKeyByTokenHash(ctx, oldHash); !model.IsNotFound(err) {
		t.Fatalf("old token hash should be invalid after rotate: err = %v", err)
	}
	got, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken(newPlain))
	if err != nil {
		t.Fatalf("new token hash lookup: %v", err)
	}
	if got.ID != key.ID {
		t.Fatalf("new token resolved id = %d, want %d", got.ID, key.ID)
	}
	if got.ProxyTokenPrefix != display {
		t.Fatalf("stored ProxyTokenPrefix = %q, want %q", got.ProxyTokenPrefix, display)
	}
	// 换发不影响绑定。
	infos, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels after rotate: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("bindings after rotate = %+v, want 1", infos)
	}
}

// 删除密钥级联清理绑定。
func TestDeleteAPIKey_CascadesBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")
	m2 := mustCreateAIModel(t, store, "m2")
	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(m1.ID, model.ModelRolePrimary, 0),
		binding(m2.ID, model.ModelRoleFallback, 1),
	)

	if err := store.DeleteAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, err := store.GetAPIKeyByID(ctx, key.ID); !model.IsNotFound(err) {
		t.Fatalf("deleted key still readable: err = %v", err)
	}
	infos, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("bindings after key delete = %+v, want none", infos)
	}
	// 模型本身不受影响。
	if _, err := store.GetAIModel(ctx, m1.ID); err != nil {
		t.Fatalf("model should survive key delete: %v", err)
	}
}

// ReplaceBindings 整体替换（旧绑定清空、新绑定生效），空绑定被拒绝时保留原绑定。
func TestReplaceBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	m1 := mustCreateAIModel(t, store, "m1")
	m2 := mustCreateAIModel(t, store, "m2")
	m3 := mustCreateAIModel(t, store, "m3")
	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(m1.ID, model.ModelRolePrimary, 0),
		binding(m2.ID, model.ModelRoleFallback, 1),
	)

	if err := store.ReplaceBindings(ctx, key.ID, []model.APIKeyModel{
		binding(m3.ID, model.ModelRolePrimary, 5),
	}); err != nil {
		t.Fatalf("ReplaceBindings: %v", err)
	}

	infos, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != 1 || infos[0].AIModelID != m3.ID {
		t.Fatalf("bindings after replace = %+v, want only model %d", infos, m3.ID)
	}

	// 非法绑定：原绑定保持不变（校验在删除前）。
	if err := store.ReplaceBindings(
		ctx, key.ID,
		[]model.APIKeyModel{binding(m1.ID, model.ModelRoleFallback, 0)},
	); err == nil {
		t.Fatal("ReplaceBindings with no primary should be rejected")
	}
	infos, err = store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != 1 || infos[0].AIModelID != m3.ID {
		t.Fatalf("bindings changed by a rejected replace = %+v", infos)
	}
}

// ListKeyModels 的 onlyEnabled 开关：禁用模型的绑定在 onlyEnabled=true 时不出现。
func TestListKeyModels_OnlyEnabled(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	enabled := mustCreateAIModel(t, store, "enabled-model")
	disabled := mustCreateAIModel(t, store, "disabled-model")
	if _, err := store.UpdateAIModel(ctx, disabled.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable model: %v", err)
	}

	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(enabled.ID, model.ModelRolePrimary, 0),
		binding(disabled.ID, model.ModelRoleFallback, 1),
	)

	all, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels(onlyEnabled=false): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("onlyEnabled=false len = %d, want 2", len(all))
	}

	onlyEnabled, err := store.ListKeyModels(ctx, key.ID, true)
	if err != nil {
		t.Fatalf("ListKeyModels(onlyEnabled=true): %v", err)
	}
	if len(onlyEnabled) != 1 || onlyEnabled[0].AIModelID != enabled.ID {
		t.Fatalf("onlyEnabled=true = %+v, want only model %d", onlyEnabled, enabled.ID)
	}
}

// ResolveModelForKey 的解析规则。
func TestResolveModelForKey(t *testing.T) {
	t.Run("explicit-match-case-insensitive", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		m := newTestAIModel("MyModel-X")
		if err := store.CreateAIModel(ctx, m); err != nil {
			t.Fatalf("CreateAIModel: %v", err)
		}
		key, _ := mustCreateAPIKey(t, store, owner.ID, binding(m.ID, model.ModelRolePrimary, 0))

		got, err := store.ResolveModelForKey(ctx, key, "mymodel-x")
		if err != nil {
			t.Fatalf("ResolveModelForKey: %v", err)
		}
		if got.AIModelID != m.ID || got.ModelID != "MyModel-X" || got.ModelType != model.ModelTypeOpenAI {
			t.Fatalf("resolved = %+v, want model %+v", got, m)
		}
		if got.URL != m.URL || got.APIKey != m.APIKey || got.QuotaDay != m.QuotaDay {
			t.Fatalf("resolved = %+v, want upstream fields of %+v", got, m)
		}
		if got.CustomHTTPHeaders["X-Test"] != "1" {
			t.Fatalf("resolved headers = %#v, want X-Test=1", got.CustomHTTPHeaders)
		}
	})

	t.Run("slug-compatible", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		m := newTestAIModel("DeepSeek/V3.1")
		if err := store.CreateAIModel(ctx, m); err != nil {
			t.Fatalf("CreateAIModel: %v", err)
		}
		key, _ := mustCreateAPIKey(t, store, owner.ID, binding(m.ID, model.ModelRolePrimary, 0))

		for _, requested := range []string{"deepseek-v3.1", "deepseek/v3.1", "DEEPSEEK:V3.1", "  deepseek-v3.1  "} {
			got, err := store.ResolveModelForKey(ctx, key, requested)
			if err != nil {
				t.Fatalf("requested %q: ResolveModelForKey: %v", requested, err)
			}
			if got.AIModelID != m.ID {
				t.Fatalf("requested %q: resolved id = %d, want %d", requested, got.AIModelID, m.ID)
			}
		}
	})

	t.Run("provider-prefix-disambiguates-shared-model-id", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		zhipu := newTestAIModel("glm-5.1")
		zhipu.Provider = "zhipu"
		if err := store.CreateAIModel(ctx, zhipu); err != nil {
			t.Fatalf("CreateAIModel: %v", err)
		}
		cloud := newTestAIModel("glm-5.1")
		cloud.Provider = "tencent-cloud"
		if err := store.CreateAIModel(ctx, cloud); err != nil {
			t.Fatalf("CreateAIModel: %v", err)
		}
		key, _ := mustCreateAPIKey(t, store, owner.ID,
			binding(zhipu.ID, model.ModelRolePrimary, 0),
			binding(cloud.ID, model.ModelRoleFallback, 1),
		)

		// provider/model_id 前缀精确命中（含 providerKey 形态与大小写/归一化变体）。
		// providerKey 形态与 hatchery SlugifyModelID 规则对齐：点号保留（glm-5.1）。
		cases := []struct {
			requested string
			want      uint
		}{
			{"zhipu/glm-5.1", zhipu.ID},
			{"ZHIPU/GLM-5.1", zhipu.ID},
			{"zhipu-glm-5.1/glm-5.1", zhipu.ID},
			{"tencent-cloud/glm-5.1", cloud.ID},
			{"tencent-cloud-glm-5.1/GLM-5.1", cloud.ID},
		}
		for _, tc := range cases {
			got, err := store.ResolveModelForKey(ctx, key, tc.requested)
			if err != nil {
				t.Fatalf("requested %q: ResolveModelForKey: %v", tc.requested, err)
			}
			if got.AIModelID != tc.want {
				t.Fatalf("requested %q: resolved id = %d, want %d", tc.requested, got.AIModelID, tc.want)
			}
		}

		// 无前缀请求保持既有行为：按绑定顺序（sort_order）取第一个命中。
		got, err := store.ResolveModelForKey(ctx, key, "glm-5.1")
		if err != nil {
			t.Fatalf("bare model id: ResolveModelForKey: %v", err)
		}
		if got.AIModelID != zhipu.ID {
			t.Fatalf("bare model id resolved id = %d, want %d", got.AIModelID, zhipu.ID)
		}

		// 前缀显式指定但未绑定时不回退。
		if _, err := store.ResolveModelForKey(ctx, key, "openai/glm-5.1"); !errors.Is(err, model.ErrModelNotBound) {
			t.Fatalf("unbound provider prefix should not fall back, got err=%v", err)
		}
	})

	t.Run("hatchery-saved-ref-format", func(t *testing.T) {
		// hatchery 保存/下发的模型引用（binding_id / openclaw ref）形如 "{providerKey}/{modelId}"，
		// providerKey = "{provider}-{slug(model_id)}"：预置模型 provider="hatchery"，
		// 自配模型如 "qcloudlkeap"。请求直接携带该拼接形态时须精确命中。
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		preset := newTestAIModel("GLM-4-plus")
		preset.Provider = "hatchery"
		if err := store.CreateAIModel(ctx, preset); err != nil {
			t.Fatalf("create preset model: %v", err)
		}
		lkeap := newTestAIModel("DeepSeek-V3.2")
		lkeap.Provider = "qcloudlkeap"
		if err := store.CreateAIModel(ctx, lkeap); err != nil {
			t.Fatalf("create lkeap model: %v", err)
		}
		slashed := newTestAIModel("ZhiJia/GLM-5.1-Plus")
		slashed.Provider = "hatchery"
		if err := store.CreateAIModel(ctx, slashed); err != nil {
			t.Fatalf("create slashed model: %v", err)
		}

		key, _ := mustCreateAPIKey(t, store, owner.ID,
			binding(preset.ID, model.ModelRolePrimary, 0),
			binding(lkeap.ID, model.ModelRoleFallback, 1),
			binding(slashed.ID, model.ModelRoleFallback, 2),
		)

		cases := []struct {
			requested string
			want      uint
		}{
			{"hatchery-glm-4-plus/glm-4-plus", preset.ID},
			{"hatchery/glm-4-plus", preset.ID},
			{"qcloudlkeap-deepseek-v3.2/deepseek-v3.2", lkeap.ID},
			{"qcloudlkeap/DeepSeek-V3.2", lkeap.ID},
			{"hatchery-zhijia-glm-5.1-plus/zhijia-glm-5.1-plus", slashed.ID},
		}
		for _, tc := range cases {
			got, err := store.ResolveModelForKey(ctx, key, tc.requested)
			if err != nil {
				t.Fatalf("requested %q: ResolveModelForKey: %v", tc.requested, err)
			}
			if got.AIModelID != tc.want {
				t.Fatalf("requested %q: resolved id = %d, want %d", tc.requested, got.AIModelID, tc.want)
			}
		}
	})

	t.Run("explicit-unbound-does-not-fall-back", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		primary := mustCreateAIModel(t, store, "bound-primary")
		other := mustCreateAIModel(t, store, "other-model")
		key, _ := mustCreateAPIKey(t, store, owner.ID, binding(primary.ID, model.ModelRolePrimary, 0))

		got, err := store.ResolveModelForKey(ctx, key, other.ModelID)
		if !errors.Is(err, model.ErrModelNotBound) {
			t.Fatalf("err = %v, want ErrModelNotBound", err)
		}
		if got != nil {
			t.Fatalf("resolved = %+v, want nil (no fallback to primary)", got)
		}
	})

	t.Run("empty-request-falls-back-to-primary", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		primary := mustCreateAIModel(t, store, "p")
		fallback := mustCreateAIModel(t, store, "f")
		// fallback 的 sort_order 更小，primary 仍应胜出。
		key, _ := mustCreateAPIKey(t, store, owner.ID,
			binding(fallback.ID, model.ModelRoleFallback, 0),
			binding(primary.ID, model.ModelRolePrimary, 9),
		)

		for _, requested := range []string{"", "   "} {
			got, err := store.ResolveModelForKey(ctx, key, requested)
			if err != nil {
				t.Fatalf("requested %q: ResolveModelForKey: %v", requested, err)
			}
			if got.AIModelID != primary.ID {
				t.Fatalf("requested %q: resolved id = %d, want primary %d", requested, got.AIModelID, primary.ID)
			}
		}
	})

	t.Run("no-primary-returns-err-no-resolvable-model", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		key, _ := mustCreateAPIKey(t, store, owner.ID) // 无绑定

		got, err := store.ResolveModelForKey(ctx, key, "")
		if !errors.Is(err, model.ErrNoResolvableModel) {
			t.Fatalf("err = %v, want ErrNoResolvableModel", err)
		}
		if got != nil {
			t.Fatalf("resolved = %+v, want nil", got)
		}
	})

	t.Run("disabled-models-not-resolved", func(t *testing.T) {
		store := newTestStore(t)
		ctx := testCtx()
		owner := mustCreateUser(t, store, "owner")

		primary := mustCreateAIModel(t, store, "p")
		fallback := mustCreateAIModel(t, store, "f")
		key, _ := mustCreateAPIKey(t, store, owner.ID,
			binding(primary.ID, model.ModelRolePrimary, 0),
			binding(fallback.ID, model.ModelRoleFallback, 1),
		)
		if _, err := store.UpdateAIModel(ctx, primary.ID, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("disable primary: %v", err)
		}

		// 禁用后无可用 primary。
		if _, err := store.ResolveModelForKey(ctx, key, ""); !errors.Is(err, model.ErrNoResolvableModel) {
			t.Fatalf("empty request with disabled primary: err = %v, want ErrNoResolvableModel", err)
		}
		// 显式请求禁用模型同样视为未绑定。
		if _, err := store.ResolveModelForKey(ctx, key, primary.ModelID); !errors.Is(err, model.ErrModelNotBound) {
			t.Fatalf("explicit disabled model: err = %v, want ErrModelNotBound", err)
		}
		// 启用的 fallback 仍可被显式命中，但不是 primary。
		if _, err := store.ResolveModelForKey(ctx, key, fallback.ModelID); err != nil {
			t.Fatalf("explicit enabled fallback: %v", err)
		}
	})
}
