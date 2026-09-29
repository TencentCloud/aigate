# Deployment

[English](deploy.md) | [简体中文](deploy.zh-CN.md)

This document covers how to run Tencent Cloud AIGate, the full configuration
reference, secret encryption, the Docker image, the upstream SSRF policy, and
day-to-day operations. For a project overview see the [README](../README.md).

The gateway is a single binary with no frontend. It needs a database
(SQLite for local use, MySQL for production) and network access to the upstream
model endpoints it forwards to (and, if webhooks are configured, to the
receivers it delivers to).

## Local: SQLite

SQLite is intended for local development and evaluation. It is single-writer
and cannot be deployed as multiple replicas.

```bash
make run
```

`make run` builds `bin/aigate` and starts it with a SQLite file:

```bash
bin/aigate -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token
```

On SQLite the gateway creates its tables automatically at startup, so there is
no schema step. `make build` produces the binary on its own, and `make test`
runs the unit tests. Run `bin/aigate -version` to print `Tencent Cloud AIGate 0.1.0`
without touching the database.

## Production: MySQL

Use MySQL for anything beyond local evaluation. Schema management is explicit:
the gateway does **not** auto-migrate MySQL, so import `sql/init.sql` first.

```bash
# 1) Create the database.
mysql -h 127.0.0.1 -P 3306 -u root \
  -e "CREATE DATABASE aigate DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 2) Import the schema.
mysql -h 127.0.0.1 -P 3306 -u root aigate < sql/init.sql
```

`sql/init.sql` creates every table (`users`, `ai_models`, `api_keys`,
`api_key_models`, `llm_usage_logs`, `daily_usage_summaries`, `groups`,
`settings`, `credentials`, `webhooks`, `webhook_deliveries`) and seeds the
singleton `settings` row used for global defaults.

Then start the gateway against the DSN. The DSN follows the
`go-sql-driver/mysql` format and must keep `parseTime=True`:

```bash
bin/aigate \
  -db-type mysql \
  -db-dsn 'user:password@tcp(127.0.0.1:3306)/aigate?charset=utf8mb4&parseTime=True&loc=Local' \
  -timezone Asia/Shanghai \
  -init-admin-token '<bootstrap-admin-token>' \
  -master-key '<64-hex-or-base64-key>'
```

The DSN is `user:password@tcp(host:port)/database?params`. Keep `parseTime=True`
so timestamp columns scan correctly, and set `charset=utf8mb4`.

Schema changes must be kept in sync between the GORM models and `sql/init.sql`
(the single source of truth for a fresh install): edit `sql/init.sql` directly.
If an already-deployed instance needs an in-place upgrade, also add an
incremental `sql/YYYYMMDD-<slug>.sql` (year-first date prefix).

## Configuration reference

Every option is available as both a command-line flag and a `AIGATE_*`
environment variable. Precedence is: **flag > environment variable > default**.
Configuration is validated at startup; an invalid value (unknown `db-type`,
bad `timezone`, `usage-buffer < 1`, `webhook-buffer < 1`, a negative
`webhook-delivery-retention-days`, a `quota-exceeded-status` other than 429 or
403, empty `addr`/`db-dsn`, or a malformed master key) makes the process exit
rather than fall back silently.

| Flag | Environment variable | Default | Validation / notes |
|------|----------------------|---------|--------------------|
| `-addr` | `AIGATE_ADDR` | `:8080` | Listen address. Must not be empty. |
| `-db-type` | `AIGATE_DB_TYPE` | `sqlite` | `sqlite` or `mysql`. |
| `-db-dsn` | `AIGATE_DB_DSN` | `aigate.db` | SQLite file path or MySQL DSN. Must not be empty. |
| `-timezone` | `AIGATE_TIMEZONE` | `Asia/Shanghai` | IANA name; used for quota windows and daily aggregation labels. Must load. |
| `-usage-buffer` | `AIGATE_USAGE_BUFFER` | `256` | Capacity of the async usage-write queue. Must be >= 1. |
| `-webhook-buffer` | `AIGATE_WEBHOOK_BUFFER` | `256` | Capacity of the webhook event queue. Must be >= 1; an invalid value is rejected at startup rather than silently clamped. |
| `-webhook-delivery-retention-days` | `AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS` | `7` | Days to retain `webhook_deliveries` rows before they are pruned (`0` = keep forever). The dispatcher prunes at startup and then hourly; must be >= 0. |
| `-quota-exceeded-status` | `AIGATE_QUOTA_EXCEEDED_STATUS` | `429` | `429` or `403` only. Status code used when a quota check rejects a request. |
| `-init-admin-token` | `AIGATE_INIT_ADMIN_TOKEN` | (empty) | Bootstrap admin token. Empty means "generate and print once". |
| `-allow-private-upstreams` | `AIGATE_ALLOW_PRIVATE_UPSTREAMS` | `false` | When true, relaxes SSRF strict mode to allow private/loopback upstreams. |
| `-master-key` | `AIGATE_MASTER_KEY` | (empty) | AES-256-GCM master key: 64 hex characters or base64 decoding to 32 bytes. Empty disables encryption (plaintext storage plus a startup warning). See "Secret encryption". |
| `-migrate-encrypt` | — | `false` | One-off start-up switch: encrypt existing plaintext secret rows and exit without serving. Requires `-master-key`. See "Secret encryption". |

`-init-admin-token` is consulted only when no admin exists yet. If an admin is
already present the flag is ignored and a log line records that. The token is
stored as a hash; the plaintext is never persisted.

`-version` (or `--version`) prints `Tencent Cloud AIGate 0.1.0` and exits before any
configuration or database work; it takes no other flags.

## Secret encryption

Four columns hold secrets: `ai_models.api_key`,
`api_key_models.api_key` (the per-binding upstream key), `credentials.secrets_json`,
and `webhooks.secret`. All are covered by the same policy.

| State | Behaviour |
|-------|-----------|
| No master key configured | Values are stored as plaintext. The gateway logs a warning at startup: `AIGATE_MASTER_KEY is not configured: credential secrets and upstream api keys will be stored in plaintext...`. |
| Master key configured | New writes are encrypted with AES-256-GCM; the stored value is `enc:v1:<base64(nonce‖ciphertext)>`. Reads and forwards transparently decrypt to the plaintext for the upstream. |
| Malformed master key | The process refuses to start and reports why. |

**Master key format.** It must decode to exactly 32 bytes, given as either 64
hex characters or a base64 value (standard or URL-safe, padded or unpadded).
Generate one, for example, with:

```bash
openssl rand -hex 32
```

**"Configured means new writes are encrypted."** Enabling `-master-key` changes
how *new and updated* values are written; it does **not** rewrite rows already
stored in plaintext. Forwarding keeps working during the transition because the
reader treats any value without the `enc:v1:` prefix as plaintext (and decrypts
values that have it). To encrypt the existing rows in place, run the one-off
migration:

```bash
# Encrypt existing plaintext secret rows, then exit (no server is started).
bin/aigate -db-type mysql \
  -db-dsn '<DSN>' \
  -master-key '<key>' \
  -migrate-encrypt
```

The migration prints a single summary line:

```text
encryption migration finished: migrated=<n> skipped=<n> failed=<n>
```

- `migrated` — plaintext rows that were encrypted in this run.
- `skipped` — rows that were already `enc:v1:` ciphertext (or empty).
- `failed` — rows that could not be encrypted; the run does not abort on the
  first failure, so review the log for these.

The migration is **idempotent**: re-running it skips already-encrypted rows and
does not double-encrypt. It requires a master key; run without one it exits with
`-migrate-encrypt requires a master key`.

> **Back up the master key and keep it safe.** There is no key-escrow or
> recovery path: if the master key is lost, every encrypted value becomes
> unreadable and upstream calls fail. Store it in your secret manager, not in
> the repository or the database. Rotating the key is not supported by the
> current build — re-encrypting to a new key would require a dedicated
> migration.

## Docker

The repository ships a multi-stage `Dockerfile`: it builds a static binary
(CGO disabled; SQLite uses a pure-Go driver) and copies it into a minimal
`alpine` runtime that runs as a non-root user (UID/GID 65532).

```bash
docker build -t aigate .
```

Run it with SQLite (the default). The default DSN is `/data/aigate.db`, and the
image pre-creates `/data` as a writable volume for the non-root user, so mount a
host directory there to persist data:

```bash
docker run -d --name aigate \
  -p 8080:8080 \
  -v "$PWD/gateway-data:/data" \
  -e AIGATE_INIT_ADMIN_TOKEN='<bootstrap-admin-token>' \
  -e AIGATE_MASTER_KEY='<64-hex-or-base64-key>' \
  aigate
```

Notes:

- **Port** — the image exposes `8080`; publish it as needed (`-p 8080:8080`).
- **Data directory** — SQLite defaults to the container working directory path
  `/data/aigate.db`. Mount a volume at `/data` to keep the database across
  restarts; without a mount, a container restart loses all data.
- **Master key** — pass `-e AIGATE_MASTER_KEY=…` to enable encryption. Omit it
  to run in plaintext (with a startup warning); see "Secret encryption".
- **Health check** — the image declares a `HEALTHCHECK` that calls
  `GET /healthz` (via `wget`) every 30s, with a 3s timeout and a 5s start
  period. `docker ps` shows `healthy`/`unhealthy`; a healthy response is
  `{"status":"ok"}`.
- **MySQL** — override the command to point at an external database:
  `docker run ... aigate -db-type mysql -db-dsn '<DSN>' -master-key '<key>'`.

## Upstream SSRF policy

All outbound requests — to upstream model endpoints and to webhook receivers —
pass through SSRF protection on three points: URL validation before the request,
address validation at dial time (re-resolving DNS), and validation of every
redirect target.

Two tiers of addresses are denied:

1. **Permanently denied** — rejected regardless of configuration, because
   allowing them has no legitimate use. This covers cloud metadata endpoints
   (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`), link-local
   (`169.254.0.0/16`, `fe80::/10`), unspecified (`0.0.0.0`, `::`),
   multicast/broadcast, `0.0.0.0/8`, `192.0.0.0/24`, `240.0.0.0/4`, and the
   TEST-NET ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`).
2. **Denied in strict mode only** — rejected by default, allowed when
   `-allow-private-upstreams=true`. This covers loopback (`127.0.0.0/8`,
   `::1`), RFC1918 (`10/8`, `172.16/12`, `192.168/16`), CGNAT
   (`100.64.0.0/10`), `198.18.0.0/15`, ULA (`fc00::/7`), and `2001:db8::/32`.

Because the default is strict, an upstream that resolves to a private or
loopback address is rejected with a `target address not allowed` error. Set
the flag when the model endpoint really is local or internal — for example a
local Ollama/vLLM instance, or a self-hosted model service on a private
network:

```bash
bin/aigate ... -allow-private-upstreams=true
# or
AIGATE_ALLOW_PRIVATE_UPSTREAMS=true bin/aigate ...
```

Relaxing the flag only lifts the strict tier; the permanently denied set
(including cloud metadata addresses) stays blocked in every configuration.
The same policy applies to webhook delivery, so a webhook receiver on a private
network also requires the flag.

Environment proxies (`HTTP_PROXY` / `HTTPS_PROXY`) are **not** honored for
upstream requests: with a proxy, the dial-time check would validate the proxy
address instead of the real target, which conflicts with the always-on SSRF
chain. Terminate egress through a transparent proxy or a dedicated NAT instead.

## Operations

### Start-up and graceful shutdown

On `SIGINT`/`SIGTERM` the gateway stops accepting new requests, waits up to
10 seconds for in-flight requests to finish, then drains the usage-write queue
and the webhook event queue (bounded by the same shutdown budget) before exiting. The shutdown log line reports
`usage_dropped` and `webhook_dropped`, the number of entries dropped because the
respective queue was full. Both queues are written asynchronously, so a small
number of entries can be lost under extreme load; the gateway is not the billing
source of truth, and webhook delivery is best-effort with retries.

### Logging

Logs are structured and go to stderr via `log/slog`. Startup logs the listen
address, DB type, timezone, and the SSRF setting (and a warning when no master
key is set); the shutdown logs the dropped usage and webhook counts. Request-path
failures (model resolution, upstream errors, usage write failures) and webhook
delivery failures are logged with the relevant identifiers.

### First-boot admin token

When no admin exists and `-init-admin-token` is empty, the gateway generates a
token and prints it to stdout **once**, prefixed with
`initial admin token (shown once):`. Only the hash is stored, so a lost token
cannot be recovered — issue a new one from the admin API instead. Restarting
the gateway does not recreate or reprint the admin.

### Request limits

Request bodies are limited to 1 MiB. The HTTP server uses a 10-second header
read timeout; upstream calls use a 5-minute client timeout (30 minutes for a
streaming `/v1/messages` request).

### Health check

`GET /healthz` returns `{"status":"ok"}` and requires no authentication; use it
for liveness probes. The Docker image uses it as its `HEALTHCHECK`.

### Webhook delivery

Webhook subscriptions live under `/admin/webhooks`; each delivery is attempted
at most **3 times** (waiting 1s and 5s between attempts) with a 10-second
timeout per attempt, and the outcome is recorded as `success` or `failed`. See
the API reference's "Webhook event delivery" appendix for the envelope, headers,
and signature.

To debug deliveries, use the queryable delivery records:

```bash
# Inspect recent deliveries for a subscription (paginated, newest first).
curl -sS "http://127.0.0.1:8080/admin/webhooks/1/deliveries?page=1&page_size=50" \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

Each record carries `status`, `attempts`, `last_status`, and `last_error`, which
distinguishes a receiver that is down (`network_unreachable`, `last_status` 0)
from one returning an error (`last_status` 5xx). Because delivery is
asynchronous, a receiver that is slow or unavailable cannot affect the data
plane: when the event queue (`-webhook-buffer`) is full, events are dropped and
counted rather than blocking.

> **No retention policy.** Delivery records are not trimmed or expired by the
> gateway; they accumulate indefinitely. There is no built-in TTL or pruning
> job, so plan to archive or delete old `webhook_deliveries` rows yourself.

## Building and testing

```bash
make build      # build bin/aigate
make test       # unit tests
make vet        # go vet
make test-e2e   # end-to-end smoke test (requires a local MySQL)
```

`make test-e2e` resets a dedicated test database, imports `sql/init.sql`,
builds the binary, and drives it with a mock upstream. See `test/e2e/README.md`
for its prerequisites and environment overrides.
