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

// Package model 提供网关的数据模型与存储访问。
// 单租户设计：进程内持有唯一 *gorm.DB，通过 Store 的方法访问；不引入多租户上下文。
package model

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// DBTypeSQLite 本地开发/体验（SQLite 单写，不可多副本部署）。
	DBTypeSQLite = "sqlite"
	// DBTypeMySQL 生产部署。
	DBTypeMySQL = "mysql"
)

// Store 数据访问入口。
type Store struct {
	db *gorm.DB
}

// Open 按类型打开数据库连接。
func Open(dbType, dsn string) (*Store, error) {
	var dialector gorm.Dialector
	switch dbType {
	case DBTypeSQLite:
		dialector = sqlite.Open(sqliteDSNWithPragmas(dsn))
	case DBTypeMySQL:
		dialector = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported db type %q", dbType)
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: newDBLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(10)
		sqlDB.SetConnMaxLifetime(time.Hour)
	}
	return &Store{db: db}, nil
}

// newDBLogger 构造 GORM 日志器：警告级别、忽略 record-not-found 噪声，并开启
// ParameterizedQueries —— 只打印占位符 SQL、不做参数回填，避免密钥类明文（api_key /
// credential secrets / webhook secret）随失败语句或慢查询进入日志。
func newDBLogger() logger.Interface {
	return logger.New(log.New(os.Stderr, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
	})
}

// sqliteDSNWithPragmas 为 SQLite DSN 追加并发相关 pragma：
// busy_timeout 让写锁冲突时等待而不是立刻报 SQLITE_BUSY（用量异步写入与请求读并发）；
// journal_mode=WAL 提升读写并发。调用方已显式配置 _pragma 时保持不变。
func sqliteDSNWithPragmas(dsn string) string {
	if strings.Contains(dsn, "_pragma=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

// AutoMigrate 建表（仅用于 SQLite 本地开发；MySQL 生产使用 sql/init.sql）。
func (s *Store) AutoMigrate() error {
	return s.db.AutoMigrate(
		&User{},
		&AIModel{},
		&APIKey{},
		&APIKeyModel{},
		&LLMUsageLog{},
		&DailyUsageSummary{},
		&Group{},
		&Setting{},
		&Credential{},
		&Webhook{},
		&WebhookDelivery{},
	)
}

// Ping 检查数据库连通性。
func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// IsNotFound 判断是否为「记录不存在」错误。
func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// ValidationError 表示请求参数/配置层面的校验失败：调用方应映射为 400，
// 而不是把驱动/事务错误的原文当成参数错误回显（那样既误导调用方也泄漏内部细节）。
type ValidationError struct{ msg string }

// Error 实现 error 接口。
func (e *ValidationError) Error() string { return e.msg }

// IsValidationError 判断错误链中是否包含参数校验类错误。
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

// invalidInput 构造参数校验错误。
func invalidInput(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// ---- 业务时区与日期口径 ----
//
// 自然日/月/年窗口与日聚合的日期标签均按业务时区计算；
// 日期标签统一表示为「业务时区当日 00:00 对应的 UTC 时刻」（纯日期标签，便于跨库比较）。

var businessLocation = time.UTC

// SetLocation 设置业务时区（启动时由 main 按配置调用）。
func SetLocation(loc *time.Location) {
	if loc != nil {
		businessLocation = loc
	}
}

// BusinessLocation 返回当前业务时区。
func BusinessLocation() *time.Location {
	return businessLocation
}

func localDate(t time.Time) time.Time {
	y, m, d := t.In(businessLocation).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// LocalToday 返回业务时区「今天」的日期标签。
func LocalToday() time.Time {
	return localDate(time.Now())
}
