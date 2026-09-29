# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-10-02

First release: a single-binary, self-hostable LLM gateway with a data plane
(OpenAI-compatible + Anthropic Messages), an admin plane, a user-facing API, and
an outbound webhook event channel.

### Added

- **Core proxy** — `POST /v1/chat/completions` (streaming and non-streaming) and
  `GET /v1/models`, authenticated by `sk-` data-plane keys. Case-insensitive
  model resolution with primary fallback, forced replacement of the upstream
  model ID, `max_tokens` cap enforcement, and whitelisted custom-header
  passthrough. OpenAI-compatible and Anthropic Messages upstream adapters share
  a single request path. SSRF protection (URL validation, dial-time
  re-resolution, redirect validation) applies to every outbound request.
- **Anthropic ingress** — `POST /v1/messages` with `x-api-key` or Bearer
  authentication, native passthrough for Anthropic upstreams (`cache_control`
  preserved), and Anthropic → OpenAI request translation with response/SSE
  back-translation for OpenAI-compatible upstreams.
- **Quota engine** — rule-based token quotas over natural day/month/year and
  anchored custom windows with `429`/`Retry-After` rejection (configurable to
  `403`), including fail-closed handling of inactive windows and `limit=-1`.
  Checks run in four tiers — global → model daily → user → key — where the user
  tier resolves through the group policy → global default chain, the global tier
  supports group-level overrides (`groups.global_token_quota_rules`) with
  per-group accounting, and each key can carry its own `token_quota_rules`
  (unset or `[]` = no key-level limit).
- **Usage accounting and queries** — asynchronous per-request `llm_usage_logs`
  plus daily `daily_usage_summaries` (including cache read/write tokens),
  written through a bounded queue without blocking forwarding. Admin and
  user-facing query APIs (summary by date/model/key/user/group, paginated
  details), a streaming CSV/JSON export (`GET /admin/usage/export`), and a
  per-key daily trend endpoint (`GET /api/user/keys/{id}/usage`).
- **Key lifecycle** — optional per-key expiry (`expires_at`, RFC 3339; an
  explicit `null` clears it) with `expired_at` recording the first rejection:
  an expired key is rejected exactly like an invalid credential and emits
  `key.expired` once per validity window. Deleting a model promotes the affected
  keys' smallest-`sort_order` remaining binding to primary.
- **Bring your own key (BYOK)** — mark a model `user_key_required` and it stores
  no shared credential: each key binding carries its own upstream key (encrypted
  at rest, never echoed — only `has_key` is reported), resolved per request and
  isolated per binding; an unresolvable binding is rejected instead of silently
  falling back to the shared key. `/v1/models` skips bindings without a key.
- **Credential vault** — upstream credentials are managed separately from models
  and referenced by `credential_id`; secrets are masked on read and encrypted at
  rest with AES-256-GCM when a master key is configured (binding-level BYOK keys
  included); `-migrate-encrypt` re-seals existing plaintext rows.
- **Legacy import** — `POST /admin/import` ingests existing groups, users,
  models, and keys (with optional per-binding BYOK keys) in one idempotent,
  dry-run-capable call.
- **Connectivity testing** — `POST /admin/models/{id}/connectivity` probes an
  upstream with a minimal request and returns a structured error classification
  (BYOK models are reported as not probeable without a binding key).
- **Self-service keys** — users mint and rotate their own keys under
  `/api/user/keys`, bounded by a per-user limit and a model scope derived from
  their existing keys; per-key quotas are administrator-managed and read-only on
  the user plane.
- **Webhook event egress** — `/admin/webhooks` CRUD with write-only secrets
  (read-side masking), http/https URL validation, and `event_types` filtering.
  Asynchronous delivery with HMAC-SHA256 signatures (`X-Gateway-Signature`),
  10-second per-attempt timeout, up to 3 attempts with 1s/5s retry waits, and
  terminal `success`/`failed` status; delivery records are queryable via
  `GET /admin/webhooks/{id}/deliveries` and pruned after
  `-webhook-delivery-retention-days` (default 7, `0` = keep forever). Events:
  `request.completed`, `quota.exceeded` (`kind`: `global` / `model` / `user` /
  `key`), `key.disabled`, `key.expired`.
- **Admin and user planes** — Bearer-only authentication on `/admin/*` and
  `/api/user/*` (no frontend); users, groups, models, credentials, keys,
  settings, webhooks, import, and usage queries on the admin side; identity,
  quota view, usage queries, and self-service keys with row-level isolation on
  the user side.
- **Deployment** — single binary with SQLite for local evaluation and MySQL for
  production; explicit schema management (`sql/init.sql` as the single source of
  truth; in-place upgrades via `sql/YYYYMMDD-<slug>.sql`); multi-stage
  `Dockerfile`; graceful shutdown draining the usage and webhook queues;
  `GET /healthz`; `-version`.
