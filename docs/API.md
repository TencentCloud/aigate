# API Reference

[English](API.md) | [简体中文](API.zh-CN.md)

Tencent Cloud AIGate is a self-hosted LLM API gateway: a **pure API service** (no web UI). It answers four surfaces — the data plane, the admin plane, the user plane, and an unauthenticated health check.

All examples below use the base URL `http://127.0.0.1:8080`.

## 1. Overview

### Planes and authentication

| Plane | Path prefix | Authentication | Credential |
|-------|-------------|----------------|------------|
| Data plane | `/v1/*` | `Authorization: Bearer <token>`; `/v1/messages` also accepts `x-api-key` | Data-plane key (`sk-…`) |
| Admin plane | `/admin/*` | `Authorization: Bearer <token>` | Access token of a user with `role=admin` (`gk-…`) |
| User plane | `/api/user/*` | `Authorization: Bearer <token>` | Access token of any enabled user (`gk-…`) |
| Health check | `/healthz` | none | — |

The `Authorization` header value must start with `Bearer ` (case-insensitive scheme match on the admin plane; the data plane requires the exact `Bearer ` prefix). The token is trimmed of surrounding whitespace. Tokens are never accepted from query strings or other headers. On `/v1/messages`, an `x-api-key` header takes precedence over `Authorization` when both are present.

On the user plane, a `user`-role token is forced to act on its own data; an `admin`-role token may target another user with `?user_id=`.

### Error response format

Every error response — on all planes — uses a single shape, with `code` equal to the HTTP status code:

```json
{
  "error": {
    "message": "invalid API key",
    "type": "error",
    "code": 401
  }
}
```

The one exception is the data plane's `/v1/messages`, which uses the Anthropic error envelope (see that endpoint).

### Admin token bootstrap

On startup the gateway ensures that at least one `role=admin` user exists:

| Situation | Behaviour |
|-----------|-----------|
| An admin already exists | Nothing is created. If `-init-admin-token` was supplied, it is ignored and a log line is emitted. |
| No admin, `-init-admin-token <TOKEN>` supplied | The user `admin` is created with that token. |
| No admin, no token supplied | A token is generated and **printed once to stdout** as `initial admin token (shown once): gk-…` |

### Token security semantics

| Rule | Detail |
|------|--------|
| Storage | Only the SHA-256 hash and a display prefix (`sk-abcd1234…wxyz`) are persisted. Plaintext is never stored. |
| One-time disclosure | The plaintext is returned exactly once — at creation and at rotation. No list or detail endpoint ever echoes it. |
| Admin `api_key` | The upstream provider credential (`ai_models.api_key`) is never returned by any response. |
| Credential secrets | Credential values are returned masked on every read (see §3.2); the plaintext is never echoed back. |
| Webhook secrets | Webhook secrets are write-only: the read side always returns a mask (see §3.8). |
| Rotation | `rotate` replaces the stored token hash and prefix; the previous credential stops working immediately. |
| Presentation | `token` is omitted from responses outside creation/rotation (`omitempty`). |

### Request body limits

- All JSON request bodies are capped at **1 MiB**. Larger bodies are rejected outright — never truncated: the data plane returns `413`, the admin and user planes return `400`, all with `request body too large`.
- Write endpoints require a JSON object body. An empty body yields `400`: the data plane reports `invalid json body`; the admin and user planes append the parser cause, e.g. `invalid json body: EOF`.

## 2. Data plane

### POST /v1/chat/completions

Forward an OpenAI-style chat completion request to the upstream model bound to the presented key. Streaming and non-streaming are both supported; for `anthropic-messages` models the request/response are translated to and from the Anthropic Messages API.

**Authentication:** `Authorization: Bearer sk-…`

**Request fields**

The body is an OpenAI Chat Completions object. The gateway reads and rewrites only the fields below; everything else is forwarded untouched.

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `model` | string | no | Target model. If present and non-empty it must match a model bound to this key (see resolution rules). If absent or empty, the key's `primary` binding is used. |
| `stream` | bool | no | `true` selects SSE streaming. |
| `max_tokens` | integer | no | Truncated down to the key's `max_tokens` limit when that limit is `> 0`. |
| `max_completion_tokens` | integer | no | Truncated by the same limit. |
| any other field | — | no | Passed through as-is. |

**Gateway transformations (before forwarding)**

| Transformation | Rule |
|----------------|------|
| Model substitution | The body's `model` is replaced with the resolved upstream `model_id` (original casing preserved). The client's value is never forwarded verbatim. |
| `max_tokens` cap | Applies only when the key's `max_tokens > 0`. Both `max_tokens` and `max_completion_tokens` are capped if present. If **neither** is present, `max_tokens` is injected at the limit. If the limit is `<= 0`, the body is left untouched. |
| Parameter normalisation | For OpenAI-type upstreams, GPT-5+ and o-series models receive the parameter under `max_completion_tokens` instead of `max_tokens` (and vice versa for other models). |
| Streaming | For `stream: true`, an OpenAI-type upstream additionally receives `stream: true` and `stream_options.include_usage: true`. |
| Custom headers | See "Custom header passthrough" below. |

**Model resolution rules**

| Client `model` | Key bindings | Result |
|----------------|--------------|--------|
| present, matches a bound **enabled** model | — | Forward using that model's upstream `model_id`. Matching is case-insensitive, and `/` and `:` are treated as equivalent to `-`. A `provider/model_id` prefix (e.g. `qcloudlkeap/deepseek-v3.2`) disambiguates models with the same `model_id` from different providers; the prefix may also be the concatenated form written when a binding is saved (`{provider}-{model_id}`, e.g. `hatchery-glm-4-plus/glm-4-plus`), matched the same way. |
| present, no match | — | `400 requested model is not bound to this key`. There is **no fallback** to primary. |
| absent or empty string | a binding has `role=primary` | Forward using the primary model. |
| absent or empty string | no `primary` (or no enabled binding) | `400 no available model for this key`. |

Only bindings whose model is `enabled=true` participate in resolution.

The same resolution, quota, and `max_tokens` logic applies to `/v1/messages`.

**Custom header passthrough**

A model may declare `custom_http_headers` (a JSON object of string values). Passthrough is all-or-nothing:

| Condition | Behaviour |
|-----------|-----------|
| Every configured header is present on the inbound request with an **exactly equal** value | Those headers are forwarded to the upstream (canonicalised name). |
| Any configured header is missing or has a different value | **No** custom header is forwarded at all, and a warning is logged. |
| Model has no `custom_http_headers` | No custom header handling. |

**Request example**

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

**Response — non-streaming**

The upstream status code and response body are passed through **verbatim** (no re-wrapping). A `200` therefore contains whatever the upstream returned, e.g.:

```json
{
  "id": "chatcmpl-…",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "glm-5.1",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "hi"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
}
```

Upstream error statuses (e.g. `400`, `429`) are also passed through unchanged, with the upstream body.

**Response — streaming (`stream: true`)**

The response uses `Content-Type: text/event-stream` with `Cache-Control: no-cache` and `X-Accel-Buffering: no`, and is flushed incrementally. For an `anthropic-messages` upstream the Anthropic SSE events are converted into OpenAI `chat.completion.chunk` frames and terminated with `data: [DONE]`. If the upstream returns a non-`200` status before the stream starts, that status and body are passed through as JSON. Usage is recorded after the stream ends.

**Errors**

| Status | `message` | Cause |
|--------|-----------|-------|
| 401 | `missing API key` / `invalid API key` | No `Authorization` header (`missing`) vs malformed, unknown, or disabled key (`invalid`). |
| 400 | `failed to read request body` | The request body could not be read. |
| 413 | `request body too large` | The request body exceeds 1 MiB; rejected outright, never truncated. |
| 400 | `invalid json body` | The body is not a JSON object. |
| 400 | `requested model is not bound to this key` | An explicit `model` did not match any enabled binding. |
| 400 | `no available model for this key` | No `model` supplied and no enabled `primary` binding exists. |
| 429 / 403 | `Global token quota exceeded` / `Model daily token quota exceeded` / `User token quota exceeded` / `Key token quota exceeded` | A quota check rejected the request. The status is `-quota-exceeded-status` (default `429`) and a `Retry-After` header is set. |
| 500 | `internal server error` | Failed to resolve the model (database error). |
| 500 | `streaming not supported by server` | `stream: true` but the server cannot flush the response. |
| 502 | `failed to connect to LLM backend` | Non-streaming: DNS/connect/TLS/timeout failure reaching the upstream. Details (including upstream addresses) are logged server-side only. |

### POST /v1/messages

Anthropic Messages ingress. Forward an Anthropic-format request to the upstream model bound to the presented key, preserving Anthropic semantics for Anthropic upstreams and translating for OpenAI-compatible ones. Streaming and non-streaming, text, images, and tool calls are supported.

**Authentication:** `x-api-key: sk-…` (preferred) or `Authorization: Bearer sk-…`. Authentication failures always use the Anthropic error envelope.

**Request fields**

The body is an Anthropic Messages object. The gateway validates and rewrites only the fields below.

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `model` | string | **yes** | Must be non-empty. Resolved against the key's bindings exactly as on `/v1/chat/completions`. |
| `max_tokens` | integer | **yes** | Must be an integer `> 0`. Truncated down to the key's `max_tokens` limit when that limit is `> 0`. |
| `stream` | bool | no | `true` selects Anthropic SSE streaming. |
| `messages` / `system` / `tools` / `tool_choice` / `thinking` / `stop_sequences` | — | no | See the two upstream paths below. |
| any other field | — | no | Preserved in passthrough; otherwise forwarded as far as the conversion allows. |

Validation order: `model` missing → `400 model is required`; `max_tokens` missing or `<= 0` → `400 max_tokens is required`.

**Upstream paths**

The resolved model's `model_type` selects one of two paths:

| Upstream | Behaviour |
|----------|-----------|
| `anthropic-messages` | **Native passthrough.** The inbound body is forwarded unchanged except that `model` is replaced with the resolved upstream ID and `max_tokens` is capped by the key limit. Anthropic-only fields such as `cache_control` are therefore preserved end to end. Upstream responses (JSON or SSE) are returned verbatim. |
| `openai-completions` (and any other type) | **Translation.** The Anthropic request is converted to an OpenAI request (`system` → a system message, images → data URIs, `tool_use`/`tool_result` → `tool_calls`/`role=tool`, `tools`/`tool_choice`/`stop_sequences` mapped), and the OpenAI response or SSE stream is translated back into Anthropic format. |

Passthrough request headers: `x-api-key` (resolved upstream key), `anthropic-version` (inbound value, defaulting to `2023-06-01`), `anthropic-beta` (forwarded when present), plus any matched `custom_http_headers`.

Upstream timeouts: 5 minutes non-streaming, 30 minutes streaming.

**Streaming event sequence**

For the translation path, the SSE stream is a well-formed Anthropic event sequence:

```text
message_start → content_block_start → content_block_delta* → content_block_stop
             → (further blocks) → message_delta → message_stop
```

The passthrough path forwards the upstream's own Anthropic event stream unchanged. Either way, usage is recorded after the stream ends (input/cache counts come from `message_start.message.usage`, output from `message_delta.usage`).

**Response — non-streaming**

A `200` returns the Anthropic Messages response object (verbatim for passthrough; translated from the OpenAI response for the conversion path).

**Errors**

All errors use the Anthropic envelope:

```json
{
  "type": "error",
  "error": {"type": "invalid_request_error", "message": "max_tokens is required"}
}
```

| Status | `error.type` | Cause |
|--------|--------------|-------|
| 401 | `authentication_error` | Missing or invalid/disabled key. |
| 400 | `invalid_request_error` | Unreadable body, invalid JSON, missing `model`, or missing/zero `max_tokens`; also a translation failure. |
| 413 | `invalid_request_error` | The request body exceeds 1 MiB; rejected outright, never truncated. |
| 429 / 403 | `rate_limit_error` | Quota exceeded (same status as configured by `-quota-exceeded-status`); a `Retry-After` header is set. |
| 500 | `api_error` | Internal error, or streaming not supported by the server. |
| 502 | `api_error` | Could not connect to the upstream, or the upstream response could not be translated. |

**Known limitations**

- The translation path (OpenAI-compatible upstream) is **lossy** for fields that have no OpenAI equivalent — `cache_control` in particular is dropped. Use an `anthropic-messages` upstream when cache-control breakpoints matter.
- Unknown request fields are ignored rather than rejected (forward compatible).

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

### GET /v1/models

List the models available to the presented key, in OpenAI list format.

**Authentication:** `Authorization: Bearer sk-…`

**Response**

| Field | Type | Notes |
|-------|------|-------|
| `object` | string | Always `"list"`. |
| `data[].id` | string | Upstream `model_id`, lower-cased. |
| `data[].object` | string | Always `"model"`. |
| `data[].created` | integer | Unix seconds at request time (not a stored timestamp). |
| `data[].owned_by` | string | The model's `provider`. |

Only **enabled** bound models are returned, de-duplicated by lower-cased `model_id`.

```bash
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer <SK_TOKEN>"
```

```json
{
  "object": "list",
  "data": [
    {"id": "glm-5.1", "object": "model", "created": 1730000000, "owned_by": "zhipu"}
  ]
}
```

**Errors**

| Status | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 500 | `internal server error` |

## 3. Admin plane

All `/admin/*` routes require `Authorization: Bearer <ADMIN_TOKEN>` where the token belongs to a user with `role=admin`. Path `{id}` values must be positive integers; a non-numeric or zero value yields `400 invalid <model|credential|key|user|group|webhook> id`. A missing target yields `404 <model|credential|key|user|group|webhook> not found`.

See "Authentication failures" in the appendix for `401` vs `403` semantics.

### 3.1 Models

Model objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `provider` | string | Free-form label; used as `owned_by` in `/v1/models`. |
| `model_id` | string | Upstream model identifier sent to the provider. |
| `model_name` | string | Display name. |
| `url` | string | Upstream base URL. |
| `credential_id` | integer | Credential referenced for the upstream key; `0` means none (use `api_key`). |
| `model_type` | string | `openai-completions` or `anthropic-messages`. |
| `custom_http_headers` | object \| null | Allowed headers for passthrough (string values). |
| `quota_day` | integer | Model-level daily token quota; `-1` means unlimited. |
| `enabled` | bool | Disabled models are immediately unroutable. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

`api_key` is **never** present in any response.

#### POST /admin/models

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `provider` | string | no | `""` | |
| `model_id` | string | **yes** | — | Must be non-empty after trimming. |
| `model_name` | string | no | `""` | |
| `url` | string | **yes** | — | Must be non-empty after trimming. |
| `api_key` | string | no | `""` | Upstream credential (used when `credential_id` is `0`). |
| `credential_id` | integer | no | `0` | Must reference an existing credential, else `400 credential <id> not found`. |
| `model_type` | string | **yes** | — | Must be `openai-completions` or `anthropic-messages`. |
| `custom_http_headers` | object | no | none | JSON object of string values; `null` stores an empty value. |
| `quota_day` | integer | no | `-1` | |
| `enabled` | bool | no | `true` | |
| `user_key_required` | bool | no | `false` | BYOK mode: the model stores **no** shared key and the per-binding key is used instead. Rejected with `400` when combined with a non-empty `api_key` or a `credential_id > 0`; the flag cannot be changed after creation. |

```bash
curl -X POST http://127.0.0.1:8080/admin/models \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "zhipu",
    "model_id": "glm-5.1",
    "model_name": "GLM-5.1",
    "url": "https://open.bigmodel.cn/api/paas/v4",
    "api_key": "<UPSTREAM_KEY>",
    "model_type": "openai-completions",
    "quota_day": -1,
    "enabled": true
  }'
```

Response `201 Created` — a model object as described above.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; empty `model_id`; empty `url`; invalid `model_type`; malformed `custom_http_headers`; unknown `credential_id`; `user_key_required` combined with a shared `api_key`/credential. |
| 401 / 403 | Authentication / authorisation failure. |
| 500 | Database error. |

#### GET /admin/models

Response `200 OK`:

```json
{"models": [ /* model objects */ ]}
```

The array is `[]` when no models exist.

#### GET /admin/models/{id}

Response `200 OK` — a single model object. `404 model not found` when absent.

#### PUT /admin/models/{id}

Partial update. Only the fields present in the body are changed (absent fields are untouched).

| Field | Type | Notes |
|-------|------|-------|
| `provider` | string | |
| `model_id` | string | Rejected with `400` if empty. |
| `model_name` | string | |
| `url` | string | Rejected with `400` if empty. |
| `api_key` | string | Applied only when non-empty; an empty string is a no-op (the credential cannot be cleared). |
| `credential_id` | integer | `> 0` must reference an existing credential; `0` detaches the reference. |
| `model_type` | string | Must be a valid enum. |
| `custom_http_headers` | object \| null | An object replaces the value; `null` clears it. |
| `quota_day` | integer | |
| `enabled` | bool | |
| `user_key_required` | bool | Immutable: sending a value that differs from the current one is rejected with `400 user_key_required cannot be changed after creation`. For BYOK models, a non-empty `api_key` or `credential_id > 0` is rejected with `400`. |

```bash
curl -X PUT http://127.0.0.1:8080/admin/models/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

Response `200 OK` — the updated model object.

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; empty `model_id`/`url`; invalid `model_type`; malformed `custom_http_headers`; unknown `credential_id`; invalid id. |
| 404 | Model not found. |

#### DELETE /admin/models/{id}

Deletes the model and cleans up every key binding that referenced it.

Response `200 OK`:

```json
{"ok": true}
```

| Status | Condition |
|--------|-----------|
| 200 | Deleted. |
| 400 | Invalid id. |
| 404 | Model not found. |

#### POST /admin/models/{id}/connectivity

Probe an upstream with a minimal request (a single user message and `max_tokens=1`) to check that the address is reachable, the credential is valid, and the model answers.

**The probe result is always returned with HTTP `200`**; success or failure lives in the body's `ok` field:

| Body | Meaning |
|------|---------|
| `{"ok": true, "latency_ms": N}` | Success. `latency_ms` is the round-trip in milliseconds (rounded, and at least `1`). |
| `{"ok": false, "error_kind": "…", "message": "…"}` | Failure. `error_kind` is a closed enum; `message` is human-readable. |

`error_kind` values:

| Value | Cause |
|-------|-------|
| `network_unreachable` | DNS/connect/TLS/timeout, or an SSRF rejection (the default catch-all). |
| `invalid_api_key` | The upstream rejected the credential. |
| `forbidden` | The upstream forbade the request. |
| `rate_limited` | The upstream rate-limited the probe. |
| `upstream_server` | The upstream returned a 5xx. |
| `upstream_client` | The upstream returned another 4xx. |

The outbound request uses the same SSRF protection as the data plane: under the default strict mode a private/loopback upstream is blocked and classified as `network_unreachable`; start with `-allow-private-upstreams` to allow it. The upstream key is resolved live (credential first, then the model's `api_key`); a resolution failure is a server-configuration error and returns `500` rather than a probe result.

A `user_key_required` (BYOK) model has no shared key to probe: the endpoint returns `400 user_key_required model has no shared key to probe; verify it from the data plane with a binding key`.

Only an invalid path id (`400`) and a missing model (`404`) use the standard error envelope.

| Status | Condition |
|--------|-----------|
| 200 | Probe completed (whether `ok` is true or false). |
| 400 | Invalid id; the model is `user_key_required`. |
| 404 | Model not found. |
| 500 | Credential/secret resolution failed. |

### 3.2 Credentials

Upstream credentials group one or more secrets and are referenced by a model's `credential_id`. At forward time the gateway resolves the credential live (no snapshot) and uses its first `location=header` secret; if the credential is missing, disabled, or has no header secret it falls back to the model's own `api_key`.

Credential objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `description` | string | |
| `secrets` | array | Secret items; `value` is always a mask. |
| `enabled` | bool | Disabled credentials fall back to the model's `api_key`. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

Secret item objects:

| Field | Type | Notes |
|-------|------|-------|
| `key` | string | Header name. |
| `value` | string | Masked on read: first 2 + `*`*(n-4) + last 2 for values longer than 4; an all-`*` mask for shorter values. |
| `location` | string | Only `header` is supported; omitted/empty is normalised to `header`. |

#### POST /admin/credentials

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | **yes** | — | Must be non-empty after trimming. |
| `description` | string | no | `""` | |
| `secrets` | array | no | none | Each item needs a non-empty `key`; `location` must be `header`. |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/credentials \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "zhipu-prod",
    "description": "production upstream key",
    "secrets": [{"key": "Authorization", "value": "Bearer <UPSTREAM_KEY>", "location": "header"}]
  }'
```

Response `201 Created` — a credential object with masked `secrets`.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; empty `name`; a secret with an empty `key` or a `location` other than `header`. |
| 500 | Database error. |

#### GET /admin/credentials

```json
{"credentials": [ /* credential objects, secrets masked */ ]}
```

#### GET /admin/credentials/{id}

Response `200 OK` — a single credential object (secrets masked). `404 credential not found` when absent.

#### PUT /admin/credentials/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | Rejected with `400` if empty. |
| `description` | string | |
| `secrets` | array | When present, **replaces** all secrets. Omit the field to leave them untouched. |
| `enabled` | bool | |

Response `200 OK` — the updated credential object.

#### DELETE /admin/credentials/{id}

Deletes the credential. If any model still references it, the request is rejected with `409` and a message naming the reference count:

```json
{
  "error": {
    "message": "credential is referenced by 2 model(s); detach them before deleting",
    "type": "error",
    "code": 409
  }
}
```

Detach the models (set their `credential_id` to `0` or another credential) first.

| Status | Condition |
|--------|-----------|
| 200 | Deleted (`{"ok": true}`). |
| 404 | Credential not found. |
| 409 | Still referenced by one or more models. |

### 3.3 Keys

Data-plane keys (`sk-…`) owned by a user and optionally bound to a set of models.

Key objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `user_id` | integer | Owning user ID. |
| `group_id` | integer | Optional group, `0` when unset. |
| `proxy_token_prefix` | string | Display prefix, e.g. `sk-abcd1234…wxyz`. |
| `max_tokens` | integer | Request-side cap; `<= 0` means unlimited. |
| `token_quota_rules` | string | Per-key period quota as normalised rule JSON; `""` = no key-level limit. Read-only on the user plane. |
| `enabled` | bool | Disabled keys fail data-plane authentication with `401`. |
| `expires_at` | string / null | Optional expiry (RFC 3339, UTC); `null` = never expires. |
| `expired_at` | string / null | First observed rejection after expiry; `null` = not recorded. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |
| `bindings` | array | Binding views (`[]` when none). |
| `token` | string | **Only** present in the create and rotate responses. |

Binding view objects:

| Field | Type | Notes |
|-------|------|-------|
| `ai_model_id` | integer | Referenced model ID. |
| `model_id` | string | |
| `model_name` | string | |
| `provider` | string | |
| `role` | string | `primary` or `fallback`. |
| `sort_order` | integer | Ordering hint. |
| `has_key` | bool | Whether a per-binding upstream key is configured (required by `user_key_required` models; the value itself is never returned). |

#### POST /admin/keys

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `user_id` | integer | **yes** | — | Must reference an existing user; otherwise `400 user_id does not exist`. |
| `group_id` | integer | no | `0` | |
| `max_tokens` | integer | no | `0` | `<= 0` means no cap. |
| `token_quota_rules` | string | no | `""` | Per-key period quota (rule array JSON, validated and normalised). `""` = no key-level limit; `"[]"` = explicitly unlimited. |
| `expires_at` | string | no | `null` | RFC 3339 timestamp; omitted/`null` = never expires. An unparsable value is rejected with `400 expires_at must be an RFC3339 timestamp`. |
| `bindings` | array | no | none | See below. |

Each binding object:

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `ai_model_id` | integer | **yes** | Must be `> 0` and reference an existing model. |
| `role` | string | **yes** | `primary` or `fallback`. Any other value is rejected. |
| `sort_order` | integer | no | Defaults to `0`. |
| `api_key` | string | no | Per-binding upstream key for `user_key_required` (BYOK) models; ordinary models ignore it. Encrypted at rest, never echoed (only `has_key` is reported). |

When `bindings` is non-empty it must contain **exactly** one `primary`, otherwise the request is rejected with `400 bindings must contain exactly one primary model`.

`api_key` follows the whole-array replacement semantics of `bindings`: replacing the bindings without re-sending `api_key` clears it (`has_key` becomes `false`).

Note: the per-user `settings.user_key_limit` applies to the self-service API (§4.4), **not** to admin-created keys.

```bash
curl -X POST http://127.0.0.1:8080/admin/keys \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "team-a default",
    "user_id": 2,
    "group_id": 0,
    "max_tokens": 8192,
    "bindings": [
      {"ai_model_id": 1, "role": "primary", "sort_order": 0},
      {"ai_model_id": 2, "role": "fallback", "sort_order": 1}
    ]
  }'
```

Response `201 Created` — a key object whose `token` field holds the one-time plaintext (`sk-…`). This is the only time it is returned besides rotation.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; unknown `user_id`; invalid binding role; `ai_model_id` of `0`; unknown `ai_model_id`; not exactly one `primary`; duplicate binding. |
| 401 / 403 | Authentication / authorisation failure. |
| 500 | Database error. |

#### GET /admin/keys

Response `200 OK`:

```json
{"keys": [ /* key objects, without token */ ]}
```

#### GET /admin/keys/{id}

Response `200 OK` — a single key object (no `token`). `404 key not found` when absent.

#### PUT /admin/keys/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `max_tokens` | integer | |
| `token_quota_rules` | string | `""` clears the key-level quota; otherwise a valid rule array (validated and normalised). |
| `enabled` | bool | |
| `expires_at` | string / null | An RFC 3339 value sets a new expiry (and clears `expired_at`); an explicit `null` clears both; anything else is `400`. |
| `bindings` | array | When present, **replaces** all bindings; `[]` clears them. Omit the field to leave bindings untouched. |

`user_id` and `group_id` are not updatable.

```bash
curl -X PUT http://127.0.0.1:8080/admin/keys/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false, "bindings": []}'
```

Response `200 OK` — the updated key object (no `token`).

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; invalid binding set; invalid id. |
| 404 | Key not found. |

#### DELETE /admin/keys/{id}

Deletes the key and its bindings. Response `200 OK`:

```json
{"ok": true}
```

#### POST /admin/keys/{id}/rotate

Issues a new `sk-` credential and invalidates the old one immediately. The key's bindings and settings are preserved.

Response `200 OK` — the key object with a fresh one-time `token`.

| Status | Condition |
|--------|-----------|
| 200 | Rotated. |
| 400 | Invalid id. |
| 404 | Key not found. |

### 3.4 Users

Users hold access tokens (`gk-…`) used to authenticate on the admin plane (when `role=admin`) and the user plane.

User objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `role` | string | `admin` or `user`. |
| `group_id` | integer | `0` when unset. |
| `api_token_prefix` | string | Display prefix. |
| `enabled` | bool | Disabled users fail authentication with `401`. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |
| `token` | string | **Only** present in the create and rotate responses. |

#### POST /admin/users

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `role` | string | no | `user` | Must be `admin` or `user`. |
| `group_id` | integer | no | `0` | |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/users \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "role": "user", "group_id": 0}'
```

Response `201 Created` — a user object whose `token` holds the one-time plaintext (`gk-…`).

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; invalid `role`. |
| 500 | Database error. |

#### GET /admin/users

Response `200 OK`:

```json
{"users": [ /* user objects, without token */ ]}
```

There is no single-user `GET /admin/users/{id}` endpoint.

#### PUT /admin/users/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `role` | string | Must be `admin` or `user`. |
| `group_id` | integer | |
| `enabled` | bool | |

Response `200 OK` — the updated user object (no `token`). `404 user not found` when absent.

#### DELETE /admin/users/{id}

Deletes the user and cascades to that user's keys and their bindings. Historical usage logs are retained.

Response `200 OK`:

```json
{"ok": true}
```

#### POST /admin/users/{id}/rotate

Issues a new access token and invalidates the old one immediately.

Response `200 OK` — the user object with a fresh one-time `token`.

### 3.5 Groups

Groups carry a token-quota policy that users can be assigned to (`user.group_id`). During a data-plane quota check, a user's group policy is the first tier of the resolution chain (group policy → global default).

Group objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `token_quota_rules` | string | JSON **text** containing a rule array. `""` means **unconfigured** (fall back to the global default); `"[]"` means explicitly unlimited; otherwise the normalised rule JSON. |
| `global_token_quota_rules` | string | Group-level global-quota override. `""` = unconfigured (fall back to the site rules and `global_quota_scope`); `"[]"` = explicitly unlimited; otherwise the group's own rules, counted **per group**. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

#### POST /admin/user-groups

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `token_quota_rules` | string | no | `""` | Omitted/empty = unconfigured. Otherwise must be a valid JSON array (rules are validated and normalised). |
| `global_token_quota_rules` | string | no | `""` | Optional group-level override of the global quota; same validation. When set (including `"[]"`), the group is checked against its own rules with per-group usage. |

```bash
curl -X POST http://127.0.0.1:8080/admin/user-groups \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}'
```

Response `201 Created` — a group object.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; `token_quota_rules` not a JSON array or failing rule validation. |
| 500 | Database error. |

#### GET /admin/user-groups

```json
{"user_groups": [ /* group objects */ ]}
```

#### GET /admin/user-groups/{id}

Response `200 OK` — a single group object. `404 group not found` when absent.

#### PUT /admin/user-groups/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `token_quota_rules` | string | Same validation as create. |
| `global_token_quota_rules` | string | Same validation as create; `""` clears the override. |

Response `200 OK` — the updated group object.

#### DELETE /admin/user-groups/{id}

Deletes the group itself; it does **not** cascade to users or keys. A user whose `group_id` now dangles falls back to the global default rules.

Response `200 OK`:

```json
{"ok": true}
```

### 3.6 Usage queries

#### GET /admin/usage/summary

Aggregated token usage. Query parameters:

| Parameter | Type | Notes |
|-----------|------|-------|
| `group_by` | string | `date` (default), `model`, `key`, `user`, or `group`. |
| `start` / `end` | string | `YYYY-MM-DD`, interpreted in the business timezone; `end` includes the whole day. |
| `user_id` / `group_id` / `api_key_id` / `ai_model_id` | integer | Optional filters. |

```json
{
  "group_by": "date",
  "rows": [
    {
      "date": "2026-09-01T16:00:00Z",
      "prompt_tokens": 120,
      "completion_tokens": 40,
      "total_tokens": 160,
      "prompt_cache_read_tokens": 0,
      "prompt_cache_write_tokens": 0,
      "request_count": 3
    }
  ]
}
```

The `date` field is present only when `group_by=date`. `rows` is `[]` when there is no data.

#### GET /admin/usage/logs

Per-request usage details, newest first. Accepts the same time-range and id filters as `summary`, plus `page` (default 1) and `page_size` (default 50, capped at 500).

```json
{
  "logs": [
    {
      "id": 1,
      "api_key_id": 3,
      "user_id": 2,
      "group_id": 1,
      "ai_model_id": 1,
      "model": "glm-5.1",
      "provider": "zhipu",
      "prompt_tokens": 120,
      "completion_tokens": 40,
      "total_tokens": 160,
      "prompt_cache_read_tokens": 0,
      "prompt_cache_write_tokens": 0,
      "status_code": 200,
      "latency": 512,
      "created_at": "2026-09-01T16:00:00Z"
    }
  ],
  "total": 42,
  "page": 1,
  "page_size": 50
}
```

Errors on both: `400` for an invalid date, `group_by`, or id filter; `500` on a database error.

#### GET /admin/usage/export

Streams usage details (the same fields as `/admin/usage/logs`) as a downloadable file. Accepts the same time-range and id filters as `logs`; the export is streamed in batches (never loading the whole table into memory).

| Parameter | Type | Notes |
|-----------|------|-------|
| `format` | string | `csv` (default) or `json`. Anything else returns `400 format must be csv or json`. |
| `start` / `end` / `user_id` / `group_id` / `api_key_id` / `ai_model_id` | — | Same semantics as `/admin/usage/logs`. |

- `format=csv` → `text/csv` attachment with a header row (`id,api_key_id,user_id,group_id,ai_model_id,model,provider,prompt_tokens,completion_tokens,total_tokens,prompt_cache_read_tokens,prompt_cache_write_tokens,status_code,latency,created_at`).
- `format=json` → `{"logs": [ … ]}` streamed with the same row objects as `logs`.

```bash
curl -OJ http://127.0.0.1:8080/admin/usage/export?format=csv&start=2026-09-01   -H "Authorization: Bearer <ADMIN_TOKEN>"
```

### 3.7 Settings

Global settings, stored as a single row.

| Field | Type | Notes |
|-------|------|-------|
| `default_token_quota_rules` | string | JSON text containing a rule array. Applied to a user with no group policy. |
| `global_token_quota_rules` | string | JSON text containing a rule array. The global tier. |
| `global_quota_scope` | string | `site` or `group`. How global usage is counted. |
| `user_key_limit` | integer | Maximum self-service keys per user; default `5`; `0` = unlimited (see §4.4). |

#### GET /admin/settings

Returns the current settings, creating the default row on first access. Defaults are `"[]"`, `"[]"`, `site`, `5`.

```json
{
  "default_token_quota_rules": "[]",
  "global_token_quota_rules": "[]",
  "global_quota_scope": "site",
  "user_key_limit": 5
}
```

#### PUT /admin/settings

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `default_token_quota_rules` | string | Must be a valid JSON **array** (an empty string or `null` is rejected). |
| `global_token_quota_rules` | string | Must be a valid JSON array. |
| `global_quota_scope` | string | Must be `site` or `group`. |
| `user_key_limit` | integer | Must be `>= 0`; `0` means unlimited. |

```bash
curl -X PUT http://127.0.0.1:8080/admin/settings \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"global_quota_scope": "group", "user_key_limit": 10}'
```

Response `200 OK` — the full settings object.

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; rules not a JSON array; invalid scope; `user_key_limit < 0`. |
| 500 | Database error. |

### 3.8 Webhooks

Webhook subscriptions receive outbound events. Delivery is asynchronous and signed; see the appendix for the exact semantics.

Webhook objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `url` | string | Receiver URL; must be `http` or `https`. |
| `secret` | string | **Always a mask** (write-only). |
| `event_types` | array | Subscribed types; `[]` means all. |
| `enabled` | bool | Disabled subscriptions receive nothing. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

#### POST /admin/webhooks

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `url` | string | **yes** | — | Must parse as an `http`/`https` URL with a host. |
| `secret` | string | no | `""` | Used to sign deliveries (at most 256 bytes); never read back. |
| `event_types` | array | no | `[]` | Non-empty strings; `[]` (or omitted) subscribes to all. |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/webhooks \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "siem", "url": "https://example.com/hooks/llm",
       "secret": "<HMAC_SECRET>", "event_types": ["quota.exceeded"]}'
```

Response `201 Created` — a webhook object; `secret` is a mask.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; invalid URL; an `event_types` entry that is empty. |
| 500 | Database error. |

#### GET /admin/webhooks

```json
{"webhooks": [ /* webhook objects, secrets masked */ ]}
```

#### GET /admin/webhooks/{id}

Response `200 OK` — a single webhook object (secret masked). `404 webhook not found` when absent.

#### PUT /admin/webhooks/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `url` | string | Must be a valid `http(s)` URL. |
| `secret` | string | **An empty string means "keep the current secret"** — the field cannot be read back, so it also cannot be cleared with an empty value. |
| `event_types` | array | Replaces the subscription set; `[]` subscribes to all. |
| `enabled` | bool | |

Response `200 OK` — the updated webhook object.

#### DELETE /admin/webhooks/{id}

Response `200 OK`:

```json
{"ok": true}
```

#### GET /admin/webhooks/{id}/deliveries

Paginated delivery records for the subscription, newest first (`page` default 1, `page_size` default 50, capped at 500).

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Delivery ID. |
| `webhook_id` | integer | Owning subscription. |
| `event_type` | string | Delivered event type. |
| `status` | string | `pending`, `success`, or `failed`. |
| `attempts` | integer | Attempts made so far. |
| `last_status` | integer | HTTP status of the last attempt (`0` for a network error). |
| `last_error` | string | Error text of the last attempt. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

```json
{
  "deliveries": [
    {"id": 11, "webhook_id": 1, "event_type": "quota.exceeded", "status": "success",
     "attempts": 2, "last_status": 200, "last_error": "",
     "created_at": "2026-09-01T16:00:00Z", "updated_at": "2026-09-01T16:00:06Z"}
  ],
  "total": 1,
  "page": 1,
  "page_size": 50
}
```

### 3.9 Import

Import existing data (groups, users, models, keys) in one call.

**Processing order** is fixed as `user_groups → users → models → keys`: a later entry may reference an entity created earlier in the same request. `dry_run=true` validates and plans without writing.

**Idempotency keys** — an entity that already exists is reported as `skipped`:

| Kind | Idempotency key |
|------|-----------------|
| `groups` | `name` |
| `users` | `name` |
| `models` | `(provider, model_id)` |
| `keys` | `token` (compared after hashing) |

**Request body**

| Field | Type | Notes |
|-------|------|-------|
| `dry_run` | bool | Plan only, no writes. |
| `groups[].name` | string | Required. |
| `user_groups[].token_quota_rules` | string | Optional; validated like `/admin/user-groups`. |
| `users[].name` | string | Required. |
| `users[].role` | string | `admin` or `user`; default `user`. |
| `users[].group` | string | Group **name**; empty = no group. Must resolve to an existing/planned group. |
| `models[].provider` / `model_id` | string | `model_id` required; `(provider, model_id)` is the key. |
| `models[].url` | string | Required. |
| `models[].api_key` | string | Optional. |
| `models[].model_type` | string | `openai-completions` or `anthropic-messages`. |
| `models[].quota_day` / `enabled` | integer / bool | Optional. |
| `keys[].token` | string | **Plaintext**; required. Hashed on import and never stored or echoed. |
| `keys[].user` | string | Owner **name**; required and must resolve. |
| `keys[].group` | string | Group name; empty = none. |
| `keys[].name` / `max_tokens` | string / integer | Optional. |
| `keys[].bindings[]` | array | `{model, role, api_key?}`, where `model` is a **model_id**. Must have exactly one `primary` and no duplicates when non-empty. An optional `api_key` carries the per-binding (BYOK) key; it is encrypted at rest. |

**Response**

```json
{
  "dry_run": false,
  "summary": {"created": 4, "skipped": 1, "errors": 1},
  "details": [
    {"kind": "group", "ref": "team-a", "action": "created"},
    {"kind": "key", "ref": "sk-abcd1234…wxyz", "action": "error", "message": "user not found: bob"}
  ]
}
```

- `details[].kind` is `group` / `user` / `model` / `key`.
- `details[].action` is `created` / `skipped` / `error`; `message` is present for errors.
- `details[].ref` carries only non-sensitive identifiers (a name, `provider/model_id`, or a token display prefix) — never the token plaintext.

A single failing entry is recorded and the rest continue; the request itself returns `200`. Imported users do **not** return an access token (one is generated and its plaintext discarded); issue one later with `POST /admin/users/{id}/rotate`.

```bash
curl -X POST http://127.0.0.1:8080/admin/import \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "dry_run": true,
    "user_groups": [{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}],
    "users": [{"username": "bob", "role": "user", "group": "team-a"}],
    "models": [{"provider": "zhipu", "model_id": "glm-5.1", "url": "https://open.bigmodel.cn/api/paas/v4",
                "api_key": "<UPSTREAM_KEY>", "model_type": "openai-completions"}],
    "keys": [{"name": "bob-default", "user": "bob", "group": "team-a", "token": "sk-legacy-…",
              "bindings": [{"model": "glm-5.1", "role": "primary"}]}]
  }'
```

## 4. User plane

Prefix `/api/user/*`. Any enabled user token (`user` or `admin`) may authenticate. A `user`-role token is forced to act on its own data (its `user_id` is ignored); an `admin`-role token may target another user with `?user_id=`.

### 4.1 GET /api/user/me

Returns the caller's identity, including the group name (empty when ungrouped or when the group has been deleted).

```json
{"id": 2, "username": "alice", "role": "user", "group_id": 1, "group_name": "team-a"}
```

### 4.2 GET /api/user/quota

Per-rule view of the target user's effective quota rules (resolved through the group policy → global default chain).

| Field | Type | Notes |
|-------|------|-------|
| `user_id` | integer | The resolved target user. |
| `usages[]` | array | One entry per effective rule. |
| `usages[].rule_index` | integer | Rule index within the resolved set. |
| `usages[].mode` | string | `day`, `month`, `year`, or `custom`. |
| `usages[].limit` | integer | `-1` means the window is computed but usage is unlimited. |
| `usages[].used` | integer | Tokens used in the current window. |
| `usages[].active` | bool | Whether the rule currently applies. |
| `usages[].period_start` / `period_end` | integer \| null | Window bounds as Unix seconds; `null` end means open-ended. |

```json
{
  "user_id": 2,
  "usages": [
    {"rule_index": 0, "mode": "day", "limit": 1000000, "used": 25310,
     "active": true, "period_start": 1758038400, "period_end": 1758124800}
  ]
}
```

### 4.3 Usage queries

`GET /api/user/usage/summary`, `GET /api/user/usage/logs`, and `GET /api/user/usage/by-model` mirror the admin endpoints but are row-level isolated: a `user`-role token always sees its own usage regardless of any `user_id` filter.

| Endpoint | Notes |
|----------|-------|
| `GET /api/user/usage/summary` | Same parameters and response as `/admin/usage/summary` (`group_by`, `start`/`end`, id filters). |
| `GET /api/user/usage/logs` | Same as `/admin/usage/logs`, with `page` / `page_size` pagination (newest first). |
| `GET /api/user/usage/by-model` | Same as `summary` but always grouped by `model`; a `group_by` parameter is ignored. |

### 4.4 Self-service keys

`/api/user/keys` lets a user mint and manage their own data-plane keys. Two guardrails apply:

- **Count limit** — `settings.user_key_limit` (default `5`; `0` = unlimited). Exceeding it returns `400 key limit reached (limit N)`.
- **Model scope** — a new or updated key may only bind models that already appear in **any** of the user's existing keys. Binding a model outside that set returns `400 model not available for this user`; if the user has no bindable models at all, it returns `400 no available models, please contact an administrator`.

Isolation: a key that is missing **or** owned by someone else returns `404 key not found`, so ids cannot be probed.

Methods: `POST /api/user/keys`, `GET /api/user/keys`, `GET /api/user/keys/{id}`, `PUT /api/user/keys/{id}`, `DELETE /api/user/keys/{id}`, `POST /api/user/keys/{id}/rotate`.

The key object is the same shape as the admin key object (§3.3). `token` is returned only by create and rotate.

#### POST /api/user/keys

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `max_tokens` | integer | no | `0` | `<= 0` means no cap. |
| `bindings` | array | no | none | `{ai_model_id, role, sort_order, api_key?}`; exactly one `primary` when non-empty, and every model must be within the user's scope. The optional `api_key` is the user's own upstream key for BYOK models (encrypted at rest, never echoed). |

`token_quota_rules` is **read-only** on the user plane: the field is returned by the key APIs but attempts to set it are ignored (only administrators can configure key-level quotas).

```bash
curl -X POST http://127.0.0.1:8080/api/user/keys \
  -H "Authorization: Bearer <USER_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key", "max_tokens": 4096,
       "bindings": [{"ai_model_id": 1, "role": "primary"}]}'
```

Response `201 Created` — the key object with a one-time `token`.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; key limit reached; a model outside the allowed scope; no available models; an invalid binding set. |
| 404 | The target user does not exist. |

#### GET /api/user/keys

```json
{"keys": [ /* key objects, without token */ ]}
```

#### GET /api/user/keys/{id}

Response `200 OK` — a single key object (no `token`). `404 key not found` when absent or not owned.

#### PUT /api/user/keys/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `max_tokens` | integer | |
| `enabled` | bool | |
| `bindings` | array | When present, replaces all bindings; re-validated against the model scope. |

#### DELETE /api/user/keys/{id}

Response `200 OK`:

```json
{"ok": true}
```

#### POST /api/user/keys/{id}/rotate

Issues a new `sk-` credential and invalidates the old one immediately. Response `200 OK` — the key object with a fresh one-time `token`.

#### GET /api/user/keys/{id}/usage

Per-day usage trend of a single key (the same daily aggregation as `/api/user/usage/summary`). `user`-role tokens may only query their own keys (anything else is `404 key not found`); an `admin` token may query any key with `?user_id=`.

| Parameter | Type | Notes |
|-----------|------|-------|
| `start` / `end` | string | `YYYY-MM-DD` (business timezone, inclusive). When both are omitted the range defaults to the **last 30 days**. |

```json
{
  "key_id": 3,
  "group_by": "date",
  "start": "2026-09-03",
  "end": "2026-10-02",
  "rows": [
    {"date": "2026-10-02T00:00:00Z", "prompt_tokens": 120, "completion_tokens": 40,
     "total_tokens": 160, "prompt_cache_read_tokens": 0, "prompt_cache_write_tokens": 0,
     "request_count": 1}
  ]
}
```

## 5. Health check

### GET /healthz

No authentication.

Response `200 OK`:

```json
{"status": "ok"}
```

## 6. Appendix

### Status code summary

| Status | Where | Meaning |
|--------|-------|---------|
| 200 | all planes | Success; `DELETE` returns `{"ok": true}`. |
| 201 | admin writes (models, credentials, keys, users, groups, webhooks); user-plane key creation | Resource created. |
| 400 | all planes | Malformed body, invalid path id, or a field/business validation failure. |
| 401 | all planes | Credential missing, malformed, unknown, or disabled. |
| 403 | admin plane | Credential is valid but the user's role is not `admin`. |
| 404 | admin / user planes | Target resource does not exist (or is not owned). |
| 409 | admin plane | Credential is still referenced by a model and cannot be deleted. |
| 429 / 403 | data plane | Quota exceeded; the exact status is `-quota-exceeded-status` (default `429`) and a `Retry-After` header is set. |
| 500 | all planes | Internal error. |
| 502 | data plane | Upstream connection failure or an untranslatable upstream response. |

### Authentication failures: 401 vs 403

| Condition | Status | `message` |
|-----------|--------|-----------|
| Header missing, not `Bearer …`, or token unknown | 401 | `missing or invalid bearer token` |
| Token valid but the user is disabled | 401 | `user is disabled` |
| Token valid, user enabled, role not permitted | 403 | `insufficient role` |

On the data plane, an absent credential yields `401 missing API key`; an unknown key and a disabled key are indistinguishable and both yield `401 invalid API key`. On `/v1/messages`, the same conditions yield `401` with `error.type = authentication_error`.

### Webhook event delivery

**Envelope.** Each delivery `POST`s a JSON body to the subscription URL:

```json
{
  "id": "11",
  "type": "quota.exceeded",
  "created_at": "2026-09-01T16:00:00Z",
  "data": { }
}
```

`id` is the delivery ID (also the row id in `/admin/webhooks/{id}/deliveries`); `created_at` is UTC.

**Headers.**

| Header | Value |
|--------|-------|
| `Content-Type` | `application/json` |
| `X-Gateway-Event` | Event type (e.g. `request.completed`) |
| `X-Gateway-Delivery` | Delivery ID |
| `X-Gateway-Signature` | `sha256=<hex(hmac-sha256(secret, raw_body))>` |

Verify the signature by computing HMAC-SHA256 over the **raw request body** with the subscription secret, hex-encoding it, and comparing it to the value after `sha256=`.

**Delivery semantics.**

- Delivery is asynchronous and serialised by a single consumer; it never blocks the data plane.
- Each attempt has a **10-second timeout**. The delivery is retried on a non-2xx response or a network error.
- **At most 3 attempts**; before the 2nd and 3rd attempts the dispatcher waits **1s** and **5s** respectively.
- The delivery record is updated on every attempt (`attempts`, `last_status`, `last_error`) and ends in a terminal `success` or `failed`.
- The event queue (`-webhook-buffer`, default `256`) is bounded: when it is full, new events are dropped and counted (`webhook_dropped`) rather than blocking.
- Retries reuse the same delivery ID, so a receiver may see the same event more than once. Treat delivery as **at-least-once** and deduplicate by `X-Gateway-Delivery`. After the final attempt fails, the event is not re-sent.

**Event types and `data` fields.**

| `type` | Emitted when | `data` fields |
|--------|--------------|---------------|
| `request.completed` | A data-plane forward (including streaming) finishes. | `api_key_id`, `user_id`, `group_id`, `model`, `provider`, `status_code`, `latency_ms`, `usage` (`prompt_tokens`, `completion_tokens`, `total_tokens`, `prompt_cache_read_tokens`, `prompt_cache_write_tokens`) |
| `quota.exceeded` | A request is rejected by a quota check. | `api_key_id`, `user_id`, `group_id`, `model`, `kind` (`global` / `model` / `user` / `key`), `limit`, `used` |
| `key.disabled` | An admin disables or deletes a key. | `key_id`, `name`, `user_id`, `reason` (`disabled` / `deleted`) |
| `key.expired` | A key is used after its `expires_at` (emitted once per validity window, on the first rejection). | `key_id`, `name`, `user_id` |

Only enabled subscriptions whose `event_types` is empty or contains the event type receive it.

### Secret encryption

The gateway can encrypt secret columns at rest with AES-256-GCM:

| Topic | Detail |
|-------|--------|
| Covered columns | `ai_models.api_key`, `api_key_models.api_key` (per-binding upstream key), `credentials.secrets_json`, `webhooks.secret`. |
| Ciphertext format | `enc:v1:` + base64(nonce ‖ ciphertext); a value with the prefix is treated as ciphertext, anything else as plaintext. |
| Master key | 64 hex characters or base64 decoding to 32 bytes (`-master-key` / `AIGATE_MASTER_KEY`). |
| Not configured | Values are stored in plaintext and a warning is logged at startup; reads still work. |
| Configured | New/updated values are encrypted; existing plaintext rows are left as-is until migrated. |
| Migration | `-migrate-encrypt` encrypts existing plaintext rows in place and exits, printing `migrated`/`skipped`/`failed`; it is idempotent and requires a master key. |

See [docs/deploy.md](deploy.md) for the full runbook, including the warning to back up the master key (a lost key makes every encrypted value unreadable).

### Operational notes

| Topic | Detail |
|-------|--------|
| Upstream URL joining | `openai-completions` forwards to `<url>/chat/completions`; `anthropic-messages` forwards to `<url>/v1/messages`. Trailing slashes on `url` are trimmed. |
| SSRF policy | Outbound requests (upstreams and webhook receivers) are validated by default, which **rejects private/loopback addresses**. Local endpoints require `-allow-private-upstreams` (or `AIGATE_ALLOW_PRIVATE_UPSTREAMS=true`). |
| Body size | 1 MiB per request; larger bodies are rejected (`413` on the data plane, `400` on the admin/user planes) — never truncated. |
| Business timezone | Affects quota windows and usage/date bookkeeping only; it does not change API responses. |
| Usage write path | Usage is written asynchronously; under extreme load a small number of entries can be dropped and the gateway is not the billing source of truth. |
| Delivery retention | `webhook_deliveries` rows are kept indefinitely — there is no TTL, pruning, or table cap. |
