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

// Command aigate 是 LLM 网关的单二进制入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aigate/internal/admin"
	"aigate/internal/config"
	"aigate/internal/crypto"
	"aigate/internal/event"
	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/proxy"
	"aigate/internal/userapi"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

// hasVersionFlag 判断是否请求打印版本（在其它初始化之前解析，不依赖配置/数据库）。
func hasVersionFlag(args []string) bool {
	for _, a := range args {
		if a == "-version" || a == "--version" {
			return true
		}
	}
	return false
}

func run() error {
	if hasVersionFlag(os.Args[1:]) {
		printVersion()
		return nil
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // 用法已打印到 stdout，正常退出
		}
		return err
	}

	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	model.SetLocation(loc)

	// 主密钥：非法配置直接拒绝启动；未配置时密钥类字段明文存储并输出安全告警。
	if err := crypto.Configure(cfg.MasterKey); err != nil {
		return err
	}
	if !crypto.Enabled() {
		slog.Warn(
			"AIGATE_MASTER_KEY is not configured: credential secrets and upstream api keys will be " +
				"stored in plaintext; set -master-key or AIGATE_MASTER_KEY to enable AES-256-GCM encryption",
		)
	}
	if cfg.MigrateEncrypt && !crypto.Enabled() {
		return errors.New("-migrate-encrypt requires a master key (-master-key or AIGATE_MASTER_KEY)")
	}

	// SSRF 策略：默认严格（拒绝私网/回环上游），按配置放行。
	provider.SetAllowPrivateUpstreams(cfg.AllowPrivateUpstreams)

	store, err := model.Open(cfg.DBType, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if cfg.DBType == model.DBTypeSQLite {
		// SQLite 仅本地开发：自动建表；MySQL 生产使用 sql/init.sql。
		if err := store.AutoMigrate(); err != nil {
			return fmt.Errorf("auto migrate: %w", err)
		}
	}
	if err := store.Ping(context.Background()); err != nil {
		return fmt.Errorf("database not reachable: %w", err)
	}

	// 一次性重加密迁移：不改动其它数据，完成后直接退出（不起服务）。
	if cfg.MigrateEncrypt {
		migrated, skipped, failed, err := model.ReencryptSecrets(context.Background(), store)
		if err != nil {
			return fmt.Errorf("migrate encrypt: %w", err)
		}
		slog.Info("encryption migration finished", "migrated", migrated, "skipped", skipped, "failed", failed)
		fmt.Printf("encryption migration finished: migrated=%d skipped=%d failed=%d\n", migrated, skipped, failed)
		return nil
	}

	if err := bootstrapAdmin(context.Background(), store, cfg.InitAdminToken); err != nil {
		return err
	}

	usage := proxy.NewUsageWriter(store, cfg.UsageBuffer)
	usage.Start()

	// 事件出口：装配进程内默认投递器（埋点方只调 event.Emit，未装配时为 no-op）。
	dispatcher := event.NewDispatcher(store, cfg.WebhookBuffer)
	// 投递记录保留期（0 天 = 不清理）；须在 Start 之前配置才会启用清理循环。
	dispatcher.SetDeliveryRetention(time.Duration(cfg.WebhookDeliveryRetentionDays) * 24 * time.Hour)
	dispatcher.Start()
	event.SetDefault(dispatcher)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	proxy.NewHandler(store, usage, cfg.QuotaExceededStatus).Register(mux)
	admin.Register(mux, store)
	userapi.Register(mux, store)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info(
			"gateway listening",
			"addr",
			cfg.Addr,
			"db_type",
			cfg.DBType,
			"timezone",
			cfg.Timezone,
			"allow_private_upstreams",
			cfg.AllowPrivateUpstreams,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// 优雅停机：停新请求 → 卸载事件投递器 → 排空用量与事件队列（各自受停机预算约束）→ 退出。
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 仍有在途请求（如长流式连接）未结束：超时后放行，由进程退出终止。
		slog.Warn("http server shutdown incomplete", "error", err)
	}
	event.SetDefault(nil)
	usage.Stop(shutdownCtx)
	dispatcher.Stop(shutdownCtx)
	slog.Info("stopped", "usage_dropped", usage.Dropped(), "webhook_dropped", dispatcher.Dropped())
	return nil
}

// bootstrapAdmin 确保存在初始管理员：
//   - 已存在任一 admin：跳过（指定了 -init-admin-token 时仅提示忽略）；
//   - 不存在：使用指定 token 创建，或自动生成并打印一次（明文不落库）。
func bootstrapAdmin(ctx context.Context, store *model.Store, initToken string) error {
	n, err := store.CountUsersByRole(ctx, model.RoleAdmin)
	if err != nil {
		return err
	}
	if n > 0 {
		if initToken != "" {
			slog.Info("admin already exists, -init-admin-token ignored")
		}
		return nil
	}

	token := initToken
	generated := false
	if token == "" {
		t, _, _, err := model.GenerateToken(model.TokenPrefixAccess)
		if err != nil {
			return err
		}
		token = t
		generated = true
	}
	admin := &model.User{
		Username:       "admin",
		Role:           model.RoleAdmin,
		Enabled:        true,
		APITokenHash:   model.HashToken(token),
		APITokenPrefix: model.DisplayTokenPrefix(token),
	}
	if err := store.CreateUser(ctx, admin); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	if generated {
		fmt.Printf("initial admin token (shown once): %s\n", token)
		slog.Info("bootstrap admin created", "token", "generated")
	} else {
		slog.Info("bootstrap admin created", "token", "from -init-admin-token")
	}
	return nil
}
