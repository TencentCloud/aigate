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

// Package model_test 以黑盒方式测试数据层（internal/model）的公开 API。
//
// 夹具说明：每个测试用独立的临时 SQLite 文件（t.TempDir），并由 t.Cleanup 关闭连接，
// 测试之间天然隔离。SQLite 为单写模型，测试内不并发写。
package model_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"aigate/internal/model"
)

// testCtx 返回测试用上下文。
func testCtx() context.Context { return context.Background() }

// newTestStore 创建一个隔离的 SQLite Store（已建表，测试结束自动关闭）。
func newTestStore(t *testing.T) *model.Store {
	t.Helper()
	store, _ := newTestStoreAt(t)
	return store
}

// newTestStoreAt 在 newTestStore 的基础上返回 DSN，便于测试另开一条连接直读落库结果
// （数据层未提供用量/设置的行级读取 API，只能从表侧断言）。
func newTestStoreAt(t *testing.T) (*model.Store, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := model.Open(model.DBTypeSQLite, dsn)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dsn
}

// countRows 统计表行数（where 为空时统计全表），用于表侧断言。
func countRows(t *testing.T, db *gorm.DB, dest any, where string, args ...any) int64 {
	t.Helper()
	q := db.Model(dest)
	if where != "" {
		q = q.Where(where, args...)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("count rows (%s): %v", where, err)
	}
	return n
}

// openReadDB 打开指向同一 SQLite 文件的第二条连接，用于直读表数据断言。
func openReadDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open read db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("read db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newTestUser 构造一个带随机访问 token 的用户（未入库），同时返回 token 明文。
func newTestUser(t *testing.T, name string) (*model.User, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	return &model.User{Username: name, APITokenHash: hash, APITokenPrefix: prefix}, plain
}

// mustCreateUser 入库一个默认角色（user）的用户。
func mustCreateUser(t *testing.T, store *model.Store, name string) *model.User {
	t.Helper()
	u, _ := mustCreateUserWithToken(t, store, name)
	return u
}

// mustCreateUserWithToken 入库用户并返回其实体与 token 明文。
func mustCreateUserWithToken(t *testing.T, store *model.Store, name string) (*model.User, string) {
	t.Helper()
	u, plain := newTestUser(t, name)
	if err := store.CreateUser(testCtx(), u); err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
	return u, plain
}

// newTestAIModel 构造一个字段齐全的模型（未入库）。
func newTestAIModel(modelID string) *model.AIModel {
	return &model.AIModel{
		Provider:          "test-provider",
		ModelID:           modelID,
		ModelName:         "name-" + modelID,
		URL:               "https://upstream.test/v1",
		APIKey:            "upstream-secret",
		ModelType:         model.ModelTypeOpenAI,
		CustomHTTPHeaders: `{"X-Test":"1"}`,
		QuotaDay:          -1,
		Enabled:           true,
	}
}

// mustCreateAIModel 入库一个字段齐全的模型。
func mustCreateAIModel(t *testing.T, store *model.Store, modelID string) *model.AIModel {
	t.Helper()
	m := newTestAIModel(modelID)
	if err := store.CreateAIModel(testCtx(), m); err != nil {
		t.Fatalf("create ai model %q: %v", modelID, err)
	}
	return m
}

// binding 构造一条密钥-模型绑定。
func binding(aiModelID uint, role string, sortOrder int) model.APIKeyModel {
	return model.APIKeyModel{AIModelID: aiModelID, Role: role, SortOrder: sortOrder}
}

// newAPIKey 构造一个带随机 token 的密钥（未入库），同时返回 token 明文。
func newAPIKey(t *testing.T, ownerID uint) (*model.APIKey, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate api key token: %v", err)
	}
	return &model.APIKey{Name: "test-key", UserID: ownerID, ProxyTokenHash: hash, ProxyTokenPrefix: prefix}, plain
}

// mustCreateAPIKey 入库密钥（含绑定），返回密钥实体与 token 明文。
func mustCreateAPIKey(
	t *testing.T,
	store *model.Store,
	ownerID uint,
	bindings ...model.APIKeyModel,
) (*model.APIKey, string) {
	t.Helper()
	k, plain := newAPIKey(t, ownerID)
	if err := store.CreateAPIKey(testCtx(), k, bindings); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return k, plain
}
