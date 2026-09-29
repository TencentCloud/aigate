<p align="center">
  <img src="assets/logo.svg" alt="Tencent Cloud AIGate" width="360">
</p>

<h1 align="center">Route, control, and account for every model call — in one binary</h1>

<p align="center">
  <a href="README.md">English</a> | <a href="README.zh-CN.md">简体中文</a>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License: Apache-2.0"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.24%2B-00ADD8.svg" alt="Go 1.24+"></a>
  <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/version-v0.1.0-informational.svg" alt="v0.1.0"></a>
</p>

**Tencent Cloud AIGate is a self-hosted, enterprise-grade LLM API gateway — a pure API service with no web UI.**
It forwards both OpenAI-format and Anthropic-format requests to either an
OpenAI-compatible endpoint or the Anthropic Messages API, records per-request
usage, enforces token quotas, and emits signed webhook events.

**Origin.** AIGate is incubated by [ClawPro](https://cloud.tencent.com/document/product/213/129156)
— Tencent Cloud's one-stop enterprise AI agent management platform. ClawPro
rolls secure, governed AI assistants out to an entire workforce in minutes — no
code required — with centralized member permissions, model and knowledge-base
configuration, one-click messenger channels, and real-time cost monitoring.

Building that for enterprises is what shaped this gateway: AIGate is ClawPro's
model-access layer, so enterprise requirements — multi-tenant quotas, per-key
model bindings, complete usage accounting, hardened outbound access — are its
defaults, not add-ons. Extracted from a production-verified LLM proxy, its
behaviour reflects request patterns and failure modes that have already run in
production. Read the full story and design principles:
[docs/why-aigate.md](docs/why-aigate.md).

**Status:** v0.1.0 — see [CHANGELOG.md](CHANGELOG.md) for the release history
and [docs/ROADMAP.md](docs/ROADMAP.md) for what is coming next.

<p align="center">
  <img src="assets/architecture.svg" alt="Clients send OpenAI- or Anthropic-format requests with an sk- key; the gateway handles auth, model routing, quota, usage accounting, webhook events and SSRF-hardened egress; upstreams are an OpenAI-compatible endpoint or the Anthropic Messages API" width="100%">
</p>

## Why AIGate

- **Keep your SDK, keep your upstream** — clients speak OpenAI or Anthropic, and
  upstreams can be either protocol. The gateway translates when they differ and
  passes through natively when they match.
- **Control without a platform team** — per-key model bindings, three-level
  quotas, usage accounting, and event webhooks are all built in; there is no
  proxy fleet or sidecar to operate.
- **Run it anywhere** — a single static binary or a non-root container; SQLite
  for a laptop, MySQL for production.

## Features

- **Dual-format data plane** — `POST /v1/chat/completions`, `POST /v1/messages`
  (Anthropic Messages ingress), and `GET /v1/models`, authenticated with `sk-`
  keys that are stored only as SHA-256 hashes and never in plaintext.
- **Anthropic ingress with passthrough fidelity** — `x-api-key` (preferred) or
  `Authorization: Bearer`; an `anthropic-messages` upstream receives the request
  natively (so `cache_control` and other Anthropic-only fields survive), while
  an OpenAI-compatible upstream gets full request/response/SSE translation.
- **Dual upstream adapters** — one request path reaches either an
  OpenAI-compatible endpoint or an Anthropic Messages endpoint.
- **Per-key routing rules** — case-insensitive model resolution (an optional
  `provider/model_id` prefix pins the source when models share a name) with
  primary-model fallback, forced replacement of the upstream model ID,
  `max_tokens` cap enforcement, and opt-in custom-header passthrough.
- **Quota and usage accounting** — four-level token quotas (global → model →
  user → key) with `429`/`Retry-After` rejection; group-level overrides for the
  global tier; per-request logs plus daily aggregates (including cache
  read/write tokens), written asynchronously so forwarding is never blocked;
  CSV/JSON usage export and per-key usage trends.
- **Credential vault** — upstream credentials are managed separately from
  models and referenced by `credential_id`; secrets are masked on read and
  encrypted at rest with AES-256-GCM when a master key is configured.
- **Legacy import** — `POST /admin/import` ingests existing groups, users,
  models, and keys in one idempotent, dry-run-capable call.
- **Connectivity testing** — `POST /admin/models/{id}/connectivity` probes an
  upstream with a minimal request and returns a structured error classification.
- **Self-service keys** — users mint and rotate their own keys under
  `/api/user/keys`, bounded by a per-user limit and a model scope derived from
  their existing keys; a single-key usage trend endpoint is included.
- **Bring your own key (BYOK)** — mark a model `user_key_required` and it
  stores no shared credential: each key binding carries its own upstream key
  (encrypted at rest, never echoed), resolved per request and isolated per
  binding.
- **Key lifecycle** — optional per-key expiry (`expires_at`, RFC 3339; an
  explicit `null` clears it) with `expired_at` recording the first rejection:
  an expired key is rejected exactly like an invalid credential and emits
  `key.expired` once per validity window. Deleting a model promotes the affected
  keys' smallest-`sort_order` remaining binding to primary.
- **Webhook event egress** — `request.completed`, `quota.exceeded`,
  `key.disabled`, and `key.expired` are delivered asynchronously with HMAC-SHA256
  signatures, retries, and queryable delivery records (older rows are pruned
  after `-webhook-delivery-retention-days`, default 7 days).
- **Admin API** — models, credentials, keys, users, groups, settings, webhooks,
  import, and usage queries under `/admin/*`; Bearer-only, no frontend.
- **Hardened outbound path** — every upstream request (and every webhook
  delivery) passes through SSRF protection: URL validation, dial-time
  re-resolution, and redirect checks.
- **Single binary and container** — static build; a multi-stage `Dockerfile`
  runs as a non-root user with a health check.

## Quick start (SQLite)

Requires Go 1.24+.

```bash
make run
```

`make run` builds `bin/aigate` and starts it over a local SQLite file. The
equivalent explicit invocation is:

```bash
go run ./cmd/aigate -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token
```

With `-init-admin-token`, the first boot creates an admin with exactly that
token. Boots after the first one are idempotent: an existing admin makes the
flag a no-op. Omit the flag instead and the gateway generates an admin token
and prints it once on stdout.

`-master-key` / `AIGATE_MASTER_KEY` is **optional**. When it is not set, secrets
such as `ai_models.api_key` and credential values are stored in plaintext and the
gateway logs a startup warning; set it to encrypt them at rest (see
[docs/deploy.md](docs/deploy.md)).

### Minimal end-to-end example

The IDs `1` (model) and `2` (user) below are the ones this flow creates on a
fresh database; substitute whatever your own calls return.

```bash
ADMIN=dev-admin-token
BASE=http://127.0.0.1:8080

# 1) Register an upstream model. `model_type` picks the adapter:
#    - openai-completions: url is the OpenAI-compatible base URL; the gateway
#      appends only /chat/completions (e.g. OpenAI ".../v1", Zhipu ".../api/paas/v4")
#    - anthropic-messages: url is the host (the gateway appends /v1/messages)
curl -sS "$BASE/admin/models" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{
        "provider": "zhipu",
        "model_id": "glm-5.1",
        "model_name": "GLM-5.1",
        "url": "https://open.bigmodel.cn/api/paas/v4",
        "api_key": "<upstream-api-key>",
        "model_type": "openai-completions"
      }'

# 2) Create the user that owns the data-plane key.
curl -sS "$BASE/admin/users" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"username": "demo"}'

# 3) Issue a key bound to the model. The sk- token is returned exactly once.
curl -sS "$BASE/admin/keys" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name": "demo-key", "user_id": 2,
       "bindings": [{"ai_model_id": 1, "role": "primary"}]}'

# 4) Forward a request through the gateway using the sk- token. The same token
#    also works on the Anthropic-compatible ingress via x-api-key.
curl -sS "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer sk-..." -H 'Content-Type: application/json' \
  -d '{"model": "GLM-5.1", "messages": [{"role": "user", "content": "hello"}]}'

curl -sS "$BASE/v1/messages" \
  -H "x-api-key: sk-..." -H 'Content-Type: application/json' \
  -d '{"model": "GLM-5.1", "max_tokens": 64,
       "messages": [{"role": "user", "content": "hello"}]}'
```

The request `model` is matched case-insensitively against the key's bound
`model_id`; when several sources share the same model name, pin the source
with a `provider/model_id` prefix (e.g. `qcloudlkeap/deepseek-v3.2`) — the
provider segment may also be the concatenated form written when a binding is
saved (`{provider}-{model_id}`, e.g. `hatchery-glm-4-plus/glm-4-plus`). Omit
`model` entirely to fall back to the bound primary model. Drop
`"stream": true` into the body to get an SSE response. `GET /v1/models` lists
the key's enabled models, and `GET /healthz` is an unauthenticated liveness
probe.

## Production (MySQL)

SQLite is for local use only (single-writer, no multi-replica deployment).
Production uses MySQL; create the database, import `sql/init.sql`, and point
`-db-dsn` at it. Full instructions, the complete configuration reference, the
credential-encryption runbook, the Docker notes, and the upstream SSRF policy are
in [docs/deploy.md](docs/deploy.md).

## Configuration

Every option is available as both a flag and a `AIGATE_*` environment
variable; a flag takes precedence over the environment variable, which takes
precedence over the default.

| Flag | Environment variable | Default | Purpose |
|------|----------------------|---------|---------|
| `-addr` | `AIGATE_ADDR` | `:8080` | Listen address |
| `-db-type` | `AIGATE_DB_TYPE` | `sqlite` | `sqlite` or `mysql` |
| `-db-dsn` | `AIGATE_DB_DSN` | `aigate.db` | SQLite file path or MySQL DSN |
| `-timezone` | `AIGATE_TIMEZONE` | `Asia/Shanghai` | IANA timezone for quota windows and daily aggregates |
| `-usage-buffer` | `AIGATE_USAGE_BUFFER` | `256` | Capacity of the async usage-write queue (must be >= 1) |
| `-webhook-buffer` | `AIGATE_WEBHOOK_BUFFER` | `256` | Capacity of the webhook event queue (must be >= 1) |
| `-webhook-delivery-retention-days` | `AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS` | `7` | Days to retain `webhook_deliveries` rows (`0` = keep forever; must be >= 0) |
| `-quota-exceeded-status` | `AIGATE_QUOTA_EXCEEDED_STATUS` | `429` | Status code for quota rejection (`429` or `403`) |
| `-init-admin-token` | `AIGATE_INIT_ADMIN_TOKEN` | (empty) | Bootstrap admin token; generated and printed once when empty |
| `-allow-private-upstreams` | `AIGATE_ALLOW_PRIVATE_UPSTREAMS` | `false` | Allow private/loopback upstreams (relaxes SSRF strict mode) |
| `-master-key` | `AIGATE_MASTER_KEY` | (empty) | 64-hex or base64 32-byte key for AES-256-GCM; empty = plaintext + warning |
| `-migrate-encrypt` | — | `false` | One-off: encrypt existing plaintext secret rows, then exit (requires a master key) |

Invalid configuration is rejected at startup rather than silently defaulted.
`-version` prints `Tencent Cloud AIGate 0.1.0` and exits. The authoritative table lives in
[docs/deploy.md](docs/deploy.md).

## Documentation

| Topic | Location |
|-------|----------|
| Background and design principles | [docs/why-aigate.md](docs/why-aigate.md) |
| API reference — endpoints, payloads, examples | [docs/API.md](docs/API.md) |
| Deployment, configuration, encryption, Docker | [docs/deploy.md](docs/deploy.md) |
| OpenAPI 3.0 document | [docs/openapi.yaml](docs/openapi.yaml) |
| Changelog | [CHANGELOG.md](CHANGELOG.md) |

## Contributing

Feedback and merge requests are welcome — open an issue or a merge request on
the repository.

## License

Apache-2.0 — see [LICENSE](LICENSE).
