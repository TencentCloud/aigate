-- LLM Gateway schema (full table creation for the MySQL production environment)
-- Convention: one-to-one with the GORM models; keep the two in sync (see internal/model/*.go).
-- Migration strategy: this file is the single source of truth for a fresh install; apply schema changes by editing it directly.
-- If an already-deployed instance needs an in-place upgrade, add an incremental sql/YYYYMMDD-<slug>.sql (year-first date prefix, e.g. sql/20261002-add-foo.sql).
-- Note: the SQLite local-development path creates tables via GORM AutoMigrate and does not use this file.

CREATE TABLE IF NOT EXISTS `users` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `username`         VARCHAR(128)    NOT NULL DEFAULT '',
    `role`             VARCHAR(16)     NOT NULL DEFAULT 'user',
    `group_id`         BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `api_token_hash`   VARCHAR(64)     NOT NULL DEFAULT '',
    `api_token_prefix` VARCHAR(32)     NOT NULL DEFAULT '',
    `enabled`          TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`       DATETIME(3)     NOT NULL,
    `updated_at`       DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_users_api_token_hash` (`api_token_hash`),
    KEY `idx_users_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `ai_models` (
    `id`                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `provider`            VARCHAR(64)     NOT NULL DEFAULT '',
    `model_id`            VARCHAR(191)    NOT NULL DEFAULT '',
    `model_name`          VARCHAR(191)    NOT NULL DEFAULT '',
    `url`                 VARCHAR(512)    NOT NULL DEFAULT '',
    `api_key`             VARCHAR(1024)   NOT NULL DEFAULT '',
    `credential_id`       BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `model_type`          VARCHAR(32)     NOT NULL DEFAULT 'openai-completions',
    `custom_http_headers` TEXT            NOT NULL,
    `quota_day`           BIGINT          NOT NULL DEFAULT -1,
    `enabled`             TINYINT(1)      NOT NULL DEFAULT 1,
    -- user_key_required: BYOK mode; no shared key, the per-binding key is used (immutable after creation)
    `user_key_required`   TINYINT(1)      NOT NULL DEFAULT 0,
    `created_at`          DATETIME(3)     NOT NULL,
    `updated_at`          DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_ai_models_credential_id` (`credential_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `api_keys` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(128)    NOT NULL DEFAULT '',
    `user_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `proxy_token_hash`   VARCHAR(64)     NOT NULL DEFAULT '',
    `proxy_token_prefix` VARCHAR(32)     NOT NULL DEFAULT '',
    `max_tokens`   BIGINT          NOT NULL DEFAULT 0,
    -- token_quota_rules: per-key period quota JSON ('' = unlimited); checked as the 4th quota level
    `token_quota_rules` TEXT       NOT NULL,
    `enabled`      TINYINT(1)      NOT NULL DEFAULT 1,
    `expires_at`   DATETIME(3)     NULL DEFAULT NULL,
    `expired_at`   DATETIME(3)     NULL DEFAULT NULL,
    `created_at`   DATETIME(3)     NOT NULL,
    `updated_at`   DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_api_keys_proxy_token_hash` (`proxy_token_hash`),
    KEY `idx_api_keys_user_id` (`user_id`),
    KEY `idx_api_keys_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `api_key_models` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `api_key_id`  BIGINT UNSIGNED NOT NULL,
    `ai_model_id` BIGINT UNSIGNED NOT NULL,
    `role`        VARCHAR(16)     NOT NULL DEFAULT 'fallback',
    `sort_order`  BIGINT          NOT NULL DEFAULT 0,
    -- api_key: per-binding upstream key (BYOK models only); sealed (enc:v1:) when a master key is set
    `api_key`     VARCHAR(1024)   NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_api_key_model` (`api_key_id`, `ai_model_id`),
    KEY `idx_api_key_models_ai_model_id` (`ai_model_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `llm_usage_logs` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `api_key_id`                BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `user_id`                   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`                  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `ai_model_id`               BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `model`                     VARCHAR(191)    NOT NULL DEFAULT '',
    `provider`                  VARCHAR(64)     NOT NULL DEFAULT '',
    `prompt_tokens`             BIGINT          NOT NULL DEFAULT 0,
    `completion_tokens`         BIGINT          NOT NULL DEFAULT 0,
    `total_tokens`              BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_read_tokens`  BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_write_tokens` BIGINT          NOT NULL DEFAULT 0,
    `status_code`               BIGINT          NOT NULL DEFAULT 0,
    `latency`                   BIGINT          NOT NULL DEFAULT 0,
    `created_at`                DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_llm_usage_logs_api_key_id` (`api_key_id`),
    KEY `idx_llm_usage_logs_user_id` (`user_id`),
    KEY `idx_llm_usage_logs_group_id` (`group_id`),
    KEY `idx_llm_usage_logs_created_at` (`created_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `daily_usage_summaries` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `date`                      DATETIME(3)     NOT NULL,
    `user_id`                   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `api_key_id`                BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `ai_model_id`               BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`                  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `prompt_tokens`             BIGINT          NOT NULL DEFAULT 0,
    `completion_tokens`         BIGINT          NOT NULL DEFAULT 0,
    `total_tokens`              BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_read_tokens`  BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_write_tokens` BIGINT          NOT NULL DEFAULT 0,
    `request_count`             BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_daily_usage` (`date`, `user_id`, `api_key_id`, `ai_model_id`),
    KEY `idx_daily_usage_summaries_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `user_groups` (
    `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`              VARCHAR(128)    NOT NULL DEFAULT '',
    `token_quota_rules` TEXT            NOT NULL,
    -- global_token_quota_rules: group-level global quota override ('' = fall back to site rules)
    `global_token_quota_rules` TEXT     NOT NULL,
    `created_at`        DATETIME(3)     NOT NULL,
    `updated_at`        DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `settings` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `default_token_quota_rules` TEXT            NOT NULL,
    `global_token_quota_rules`  TEXT            NOT NULL,
    `global_quota_scope`        VARCHAR(16)     NOT NULL DEFAULT 'site',
    `user_key_limit`            INT             NOT NULL DEFAULT 5,
    `updated_at`                DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

INSERT IGNORE INTO `settings` (`id`, `default_token_quota_rules`, `global_token_quota_rules`, `global_quota_scope`, `user_key_limit`, `updated_at`)
VALUES (1, '[]', '[]', 'site', 5, NOW(3));

-- Upstream credentials: a set of secret items, referenced by ai_models.credential_id and resolved at forward time.
-- secrets_json is encrypted per the master-key policy (AES-256-GCM, ciphertext prefix enc:v1:); plaintext JSON when no master key is configured.
CREATE TABLE IF NOT EXISTS `credentials` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(128)    NOT NULL DEFAULT '',
    `description`  VARCHAR(1024)   NOT NULL DEFAULT '',
    `secrets_json` TEXT            NOT NULL,
    `enabled`      TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`   DATETIME(3)     NOT NULL,
    `updated_at`   DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Webhook event subscriptions: event_types is a JSON string array ("[]" means subscribe to all); secret is write-only and never read back.
CREATE TABLE IF NOT EXISTS `webhooks` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`        VARCHAR(128)    NOT NULL DEFAULT '',
    `url`         VARCHAR(512)    NOT NULL DEFAULT '',
    `secret`      VARCHAR(512)    NOT NULL DEFAULT '',
    `event_types` TEXT            NOT NULL,
    `enabled`     TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`  DATETIME(3)     NOT NULL,
    `updated_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Webhook delivery records: each attempt updates attempts/last_status/last_error; terminal states are success/failed.
CREATE TABLE IF NOT EXISTS `webhook_deliveries` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `webhook_id`  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `event_type`  VARCHAR(64)     NOT NULL DEFAULT '',
    `payload`     TEXT            NOT NULL,
    `status`      VARCHAR(16)     NOT NULL DEFAULT 'pending',
    `attempts`    INT             NOT NULL DEFAULT 0,
    `last_status` INT             NOT NULL DEFAULT 0,
    `last_error`  VARCHAR(512)    NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)     NOT NULL,
    `updated_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_webhook_deliveries_webhook_id` (`webhook_id`),
    KEY `idx_webhook_deliveries_created_at` (`created_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
