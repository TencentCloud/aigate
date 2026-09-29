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

// Package quota_test 以黑盒方式测试配额引擎（internal/quota）的公开 API。
//
// 夹具说明：每个测试使用独立的临时 SQLite（t.TempDir）+ AutoMigrate，t.Cleanup 关闭连接；
// 业务时区统一固定为 Asia/Shanghai（tzdata 不可用时退化为固定 +08:00 时区），用例结束还原 UTC；
// 所有窗口断言均显式传入 now，保证确定性。
package quota_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/quota"
)

// testCtx 返回测试用上下文。
func testCtx() context.Context { return context.Background() }

// Status.Kind 的取值（与被测包约定一致）。
const (
	kindGlobal = "global"
	kindModel  = "model"
	kindUser   = "user"
	kindKey    = "key"
)

// shanghaiLocation 返回业务时区：优先 Asia/Shanghai，加载失败时退化为固定 +08:00。
func shanghaiLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return loc
}

// useShanghai 把业务时区固定为 Asia/Shanghai，并在用例结束还原为 UTC。
func useShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc := shanghaiLocation(t)
	model.SetLocation(loc)
	t.Cleanup(func() { model.SetLocation(time.UTC) })
	return loc
}

// newTestStore 创建隔离的 SQLite Store（已建表），并把业务时区固定为 Asia/Shanghai。
func newTestStore(t *testing.T) *model.Store {
	t.Helper()
	useShanghai(t)
	store, err := model.Open(model.DBTypeSQLite, filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return store
}

// localTime 按业务时区解析本地时间字符串（"2006-01-02 15:04:05"）。
func localTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, model.BusinessLocation())
	if err != nil {
		t.Fatalf("parse local time %q: %v", s, err)
	}
	return tm
}

// utc 构造一个 UTC 时间点（用于断言窗口返回值）。
func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// i64 返回 int64 指针。
func i64(v int64) *int64 { return &v }

// tptr 返回 time.Time 指针。
func tptr(v time.Time) *time.Time { return &v }

// mustParseRules 解析规则文本为规则数组。
func mustParseRules(t *testing.T, raw string) []quota.Rule {
	t.Helper()
	rules, ok := quota.ParseRules(raw)
	if !ok {
		t.Fatalf("ParseRules(%q) ok = false, want true", raw)
	}
	return rules
}

// assertStatus 断言检查结果完全相等。
func assertStatus(t *testing.T, got, want quota.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("Status = %+v, want %+v", got, want)
	}
}

// assertRules 断言规则集合与期望 JSON 的语义等价（逐字段比较，指针按值比较）。
func assertRules(t *testing.T, got []quota.Rule, wantRaw string) {
	t.Helper()
	want := mustParseRules(t, wantRaw)
	if len(got) != len(want) {
		t.Fatalf("rules len = %d, want %d (%s)", len(got), len(want), wantRaw)
	}
	for i := range want {
		if got[i].Mode != want[i].Mode || got[i].Limit != want[i].Limit || got[i].Refresh != want[i].Refresh {
			t.Fatalf("rule[%d] = %+v, want %+v", i, got[i], want[i])
		}
		if !sameInt64(got[i].Start, want[i].Start) || !sameInt64(got[i].End, want[i].End) {
			t.Fatalf("rule[%d] start/end = %v/%v, want %v/%v", i, got[i].Start, got[i].End, want[i].Start, want[i].End)
		}
	}
}

// sameInt64 按值比较两个可空 int64。
func sameInt64(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// mustCreateUser 入库一个普通用户。
func mustCreateUser(t *testing.T, store *model.Store, name string) *model.User {
	t.Helper()
	_, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	u := &model.User{Username: name, APITokenHash: hash, APITokenPrefix: prefix, Enabled: true}
	if err := store.CreateUser(testCtx(), u); err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
	return u
}

// mustCreateModel 入库一个模型配置。
func mustCreateModel(t *testing.T, store *model.Store, modelID string, quotaDay int) *model.AIModel {
	t.Helper()
	m := &model.AIModel{
		Provider:          "test-provider",
		ModelID:           modelID,
		ModelName:         "name-" + modelID,
		URL:               "https://upstream.test/v1",
		APIKey:            "upstream-secret",
		ModelType:         model.ModelTypeOpenAI,
		CustomHTTPHeaders: "{}",
		QuotaDay:          quotaDay,
		Enabled:           true,
	}
	if err := store.CreateAIModel(testCtx(), m); err != nil {
		t.Fatalf("create ai model %q: %v", modelID, err)
	}
	return m
}

// mustCreateGroup 入库一个分组；rules 为空串表示「未配置」。
func mustCreateGroup(t *testing.T, store *model.Store, name, rules string) *model.Group {
	t.Helper()
	g := &model.Group{Name: name, TokenQuotaRules: rules}
	if err := store.CreateGroup(testCtx(), g); err != nil {
		t.Fatalf("create group %q: %v", name, err)
	}
	return g
}

// mustCreateGroupWithGlobal 入库一个带组级全局规则的分组（globalRules 语义同组级全局覆盖）。
func mustCreateGroupWithGlobal(t *testing.T, store *model.Store, name, userRules, globalRules string) *model.Group {
	t.Helper()
	g := &model.Group{Name: name, TokenQuotaRules: userRules, GlobalTokenQuotaRules: globalRules}
	if err := store.CreateGroup(testCtx(), g); err != nil {
		t.Fatalf("create group %q: %v", name, err)
	}
	return g
}

// mustCreateKey 入库一个数据面密钥；aiModelID>0 时绑定为 primary。
func mustCreateKey(t *testing.T, store *model.Store, ownerID, groupID, aiModelID uint) *model.APIKey {
	t.Helper()
	_, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate api key token: %v", err)
	}
	key := &model.APIKey{
		Name:             "test-key",
		UserID:           ownerID,
		GroupID:          groupID,
		ProxyTokenHash:   hash,
		ProxyTokenPrefix: prefix,
		Enabled:          true,
	}
	var bindings []model.APIKeyModel
	if aiModelID > 0 {
		bindings = append(bindings, model.APIKeyModel{AIModelID: aiModelID, Role: model.ModelRolePrimary})
	}
	if err := store.CreateAPIKey(testCtx(), key, bindings); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return key
}

// mustSetSettings 覆盖全局设置（先确保单例存在）。
func mustSetSettings(t *testing.T, store *model.Store, defaultRules, globalRules, scope string) {
	t.Helper()
	if _, err := store.GetSettings(testCtx()); err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if _, err := store.UpdateSettings(testCtx(), map[string]any{
		"default_token_quota_rules": defaultRules,
		"global_token_quota_rules":  globalRules,
		"global_quota_scope":        scope,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
}

// mustLogKeyUsage 写入一条带密钥/分组维度的请求级用量日志（密钥级与组级配额检查用）。
func mustLogKeyUsage(t *testing.T, store *model.Store, apiKeyID, userID, groupID uint, tokens int, at time.Time) {
	t.Helper()
	entry := &model.LLMUsageLog{
		APIKeyID:    apiKeyID,
		UserID:      userID,
		GroupID:     groupID,
		TotalTokens: tokens,
		CreatedAt:   at.UTC(),
	}
	if err := store.InsertUsageLog(testCtx(), entry); err != nil {
		t.Fatalf("insert key usage log: %v", err)
	}
}

// mustLogUsage 写入一条请求级用量日志（created_at 即窗口判定依据）。
func mustLogUsage(t *testing.T, store *model.Store, userID, groupID, aiModelID uint, tokens int, at time.Time) {
	t.Helper()
	entry := &model.LLMUsageLog{
		UserID:      userID,
		GroupID:     groupID,
		AIModelID:   aiModelID,
		TotalTokens: tokens,
		CreatedAt:   at.UTC(),
	}
	if err := store.InsertUsageLog(testCtx(), entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
}

// mustSumDailyUsage 向业务时区「今天」的日聚合累计用量。
func mustSumDailyUsage(t *testing.T, store *model.Store, userID, aiModelID uint, tokens int) {
	t.Helper()
	entry := &model.LLMUsageLog{UserID: userID, AIModelID: aiModelID, TotalTokens: tokens}
	if err := store.UpsertDailyUsage(testCtx(), entry); err != nil {
		t.Fatalf("upsert daily usage: %v", err)
	}
}
