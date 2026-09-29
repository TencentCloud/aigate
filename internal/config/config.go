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

// Package config 负责网关的运行配置：命令行 flag 与 AIGATE_* 环境变量（flag 优先，其次环境变量，最后默认值）。
// 配置非法时直接返回错误，由启动流程拒绝启动（不做静默兜底）。
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

const (
	// DBTypeSQLite 本地开发/体验用的嵌入式数据库。
	DBTypeSQLite = "sqlite"
	// DBTypeMySQL 生产部署使用的数据库。
	DBTypeMySQL = "mysql"
)

// Config 网关运行配置。
type Config struct {
	Addr                         string // 监听地址，如 127.0.0.1:8080
	DBType                       string // sqlite | mysql
	DBDSN                        string // SQLite 文件路径；MySQL DSN
	Timezone                     string // IANA 时区名（配额窗口与日聚合的业务口径）
	UsageBuffer                  int    // 用量异步写入队列容量
	WebhookBuffer                int    // webhook 事件投递队列容量
	WebhookDeliveryRetentionDays int    // webhook 投递记录保留天数（0 = 不清理）
	QuotaExceededStatus          int    // 配额超限状态码（M2 使用；默认 429）
	InitAdminToken               string // 初始管理员 token（为空时首次启动自动生成并打印一次）
	AllowPrivateUpstreams        bool   // 是否放行私网/回环上游地址（SSRF 严格模式开关，默认 false）
	MasterKey                    string // 主密钥（64 位 hex 或 base64，解码后 32 字节）；空 = 禁用加密
	MigrateEncrypt               bool   // 启动开关：把存量明文密钥类字段加密后退出（需主密钥）
}

// Load 解析配置。args 为命令行参数（不含程序名）。
func Load(args []string) (*Config, error) {
	usageBufferDefault, err := envInt("AIGATE_USAGE_BUFFER", 256)
	if err != nil {
		return nil, err
	}
	webhookBufferDefault, err := envInt("AIGATE_WEBHOOK_BUFFER", 256)
	if err != nil {
		return nil, err
	}
	webhookRetentionDefault, err := envInt("AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS", 7)
	if err != nil {
		return nil, err
	}
	quotaStatusDefault, err := envInt("AIGATE_QUOTA_EXCEEDED_STATUS", 429)
	if err != nil {
		return nil, err
	}
	allowPrivateDefault, err := envBool("AIGATE_ALLOW_PRIVATE_UPSTREAMS", false)
	if err != nil {
		return nil, err
	}

	fs := flag.NewFlagSet("aigate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := &Config{}
	fs.StringVar(&cfg.Addr, "addr", envOr("AIGATE_ADDR", ":8080"), "listen address")
	fs.StringVar(&cfg.DBType, "db-type", envOr("AIGATE_DB_TYPE", DBTypeSQLite), "database type: sqlite | mysql")
	fs.StringVar(&cfg.DBDSN, "db-dsn", envOr("AIGATE_DB_DSN", "aigate.db"), "SQLite file path or MySQL DSN")
	fs.StringVar(
		&cfg.Timezone,
		"timezone",
		envOr("AIGATE_TIMEZONE", "Asia/Shanghai"),
		"IANA timezone name (quota windows and daily aggregation)",
	)
	fs.IntVar(&cfg.UsageBuffer, "usage-buffer", usageBufferDefault, "capacity of the async usage-write queue")
	fs.IntVar(&cfg.WebhookBuffer, "webhook-buffer", webhookBufferDefault, "capacity of the webhook event queue")
	fs.IntVar(
		&cfg.WebhookDeliveryRetentionDays,
		"webhook-delivery-retention-days",
		webhookRetentionDefault,
		"days to retain webhook delivery records (0 = keep forever)",
	)
	fs.IntVar(
		&cfg.QuotaExceededStatus,
		"quota-exceeded-status",
		quotaStatusDefault,
		"quota-exceeded status code (429 or 403)",
	)
	fs.StringVar(
		&cfg.InitAdminToken,
		"init-admin-token",
		os.Getenv("AIGATE_INIT_ADMIN_TOKEN"),
		"bootstrap admin token (generated and printed once when empty)",
	)
	fs.BoolVar(
		&cfg.AllowPrivateUpstreams,
		"allow-private-upstreams",
		allowPrivateDefault,
		"allow private/loopback upstreams (relaxes SSRF strict mode)",
	)
	fs.StringVar(
		&cfg.MasterKey,
		"master-key",
		os.Getenv("AIGATE_MASTER_KEY"),
		"AES-256-GCM master key (64 hex or base64, 32 bytes decoded; empty = plaintext storage)",
	)
	fs.BoolVar(
		&cfg.MigrateEncrypt,
		"migrate-encrypt",
		false,
		"encrypt existing plaintext secret rows and exit (requires -master-key)",
	)
	if err := fs.Parse(args); err != nil {
		// -h/--help：把用法与 flag 列表打印到 stdout，交给调用方以成功退出。
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fmt.Fprintln(os.Stdout, "Usage: aigate [flags]")
			fs.PrintDefaults()
			return nil, flag.ErrHelp
		}
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Location 返回业务时区。
func (c *Config) Location() (*time.Location, error) {
	return time.LoadLocation(c.Timezone)
}

func (c *Config) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	switch c.DBType {
	case DBTypeSQLite, DBTypeMySQL:
	default:
		return fmt.Errorf("unsupported db-type %q (want sqlite or mysql)", c.DBType)
	}
	if c.DBDSN == "" {
		return fmt.Errorf("db-dsn must not be empty")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	if c.UsageBuffer < 1 {
		return fmt.Errorf("usage-buffer must be >= 1")
	}
	// webhook-buffer 与 usage-buffer 同策略：非法配置直接拒绝启动（不做静默归一），
	// 避免「配置写错但服务照常起来」的隐性偏差。
	if c.WebhookBuffer < 1 {
		return fmt.Errorf("webhook-buffer must be >= 1")
	}
	if c.WebhookDeliveryRetentionDays < 0 {
		return fmt.Errorf("webhook-delivery-retention-days must be >= 0")
	}
	if c.QuotaExceededStatus != 429 && c.QuotaExceededStatus != 403 {
		return fmt.Errorf("quota-exceeded-status must be 429 or 403")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer in environment variable %s: %q", key, v)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean in environment variable %s: %q", key, v)
	}
	return b, nil
}
