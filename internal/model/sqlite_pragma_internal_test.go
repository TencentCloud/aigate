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

// 白盒单测：SQLite DSN 的并发 pragma 注入（sqliteDSNWithPragmas）——
// 需要直接执行 PRAGMA 查询验证驱动是否真正应用了 busy_timeout / journal_mode。

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestSQLiteDSNWithPragmas 覆盖 DSN 形态与 pragma 生效性：
// busy_timeout=5000 与 journal_mode=WAL 是并发写入不报 SQLITE_BUSY 的前提。
func TestSQLiteDSNWithPragmas(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/tmp/x.db", "/tmp/x.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"},
		{
			"file:/tmp/x.db?cache=shared",
			"file:/tmp/x.db?cache=shared&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)",
		},
		{"/tmp/x.db?_pragma=busy_timeout(1)", "/tmp/x.db?_pragma=busy_timeout(1)"},
	}
	for _, c := range cases {
		if got := sqliteDSNWithPragmas(c.in); got != c.want {
			t.Errorf("sqliteDSNWithPragmas(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// 真实连接验证：pragma 必须对驱动生效（否则仅在字符串层面正确）。
	dsn := sqliteDSNWithPragmas(filepath.Join(t.TempDir(), "pragma.db"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite with pragmas: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	var busy int
	if err := db.Raw("PRAGMA busy_timeout").Scan(&busy).Error; err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busy)
	}

	var journal string
	if err := db.Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
}

// TestOpen_SQLiteBusyTimeoutApplied 覆盖 Open 路径：通过 Store 打开后 pragma 仍生效。
func TestOpen_SQLiteBusyTimeoutApplied(t *testing.T) {
	store, err := Open(DBTypeSQLite, filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var busy int
	if err := store.db.Raw("PRAGMA busy_timeout").Scan(&busy).Error; err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busy)
	}
}
