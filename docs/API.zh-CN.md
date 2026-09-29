# API 参考

[English](API.md) | [简体中文](API.zh-CN.md)

Tencent Cloud AIGate 是自托管的 LLM API 网关：**纯 API 服务**（无前端界面）。对外提供四个层面——数据面、管理面、用户面，以及一个免鉴权的健康检查。

下文所有示例使用基础地址 `http://127.0.0.1:8080`。

## 1. 概览

### 层面与认证方式

| 层面 | 路径前缀 | 认证方式 | 凭据 |
|------|----------|----------|------|
| 数据面 | `/v1/*` | `Authorization: Bearer <token>`；`/v1/messages` 另接受 `x-api-key` | 数据面密钥（`sk-…`） |
| 管理面 | `/admin/*` | `Authorization: Bearer <token>` | `role=admin` 用户的访问 token（`gk-…`） |
| 用户面 | `/api/user/*` | `Authorization: Bearer <token>` | 任意启用用户的访问 token（`gk-…`） |
| 健康检查 | `/healthz` | 无 | — |

`Authorization` 头的值必须以 `Bearer ` 开头（管理面对 scheme 大小写不敏感；数据面要求精确的 `Bearer ` 前缀）。token 两端的空白会被去除。不接受来自查询串或其他头的 token。`/v1/messages` 上若同时携带 `x-api-key` 与 `Authorization`，以 `x-api-key` 为准。

用户面上，`user` 角色 token 被强制作用于本人数据；`admin` 角色 token 可用 `?user_id=` 指定其他用户。

### 错误响应格式

各平面的所有错误响应形状一致，`code` 等于 HTTP 状态码：

```json
{
  "error": {
    "message": "invalid API key",
    "type": "error",
    "code": 401
  }
}
```

唯一例外是数据面的 `/v1/messages`，它使用 Anthropic 错误信封（见该端点）。

### 管理员 token 引导

启动时网关确保至少存在一个 `role=admin` 用户：

| 情形 | 行为 |
|------|------|
| 已存在任一管理员 | 不创建任何用户。若指定了 `-init-admin-token`，该参数被忽略并输出一条日志。 |
| 无管理员，且指定 `-init-admin-token <TOKEN>` | 以该 token 创建用户 `admin`。 |
| 无管理员，且未指定 token | 自动生成一个 token，并**向 stdout 打印一次**：`initial admin token (shown once): gk-…` |

### token 与密钥安全语义

| 规则 | 说明 |
|------|------|
| 存储 | 只持久化 SHA-256 哈希与展示前缀（形如 `sk-abcd1234…wxyz`），明文永不落库。 |
| 一次性展示 | 明文仅在创建与 rotate 两个时机返回一次，任何列表/详情接口都不会回显。 |
| 管理面 `api_key` | 上游凭据（`ai_models.api_key`）在任何响应中都不出现。 |
| 凭据密钥 | 凭据中的密钥值在每次读取时都以掩码返回（见 §3.2），明文绝不回显。 |
| webhook secret | webhook 的 secret 只写不读回：读侧恒返回掩码（见 §3.8）。 |
| rotate | rotate 会替换存储的 token 哈希与前缀，旧凭据立即失效。 |
| 回显控制 | 除创建/rotate 外，响应中的 `token` 字段经 `omitempty` 省略。 |

### 请求体限制

- 所有 JSON 请求体上限为 **1 MiB**。超限**直接拒绝、不做截断**：数据面返回 `413`，管理面与用户面返回 `400`，文案均为 `request body too large`。
- 写接口要求 JSON 对象请求体；空请求体返回 `400`：数据面文案为 `invalid json body`，管理面与用户面附带解析器原因（如 `invalid json body: EOF`）。

## 2. 数据面

### POST /v1/chat/completions

把 OpenAI 风格的对话补全请求转发给密钥绑定命中的上游模型。流式与非流式均支持；对
`anthropic-messages` 模型，请求与响应会与 Anthropic Messages API 互相转换。

**认证：** `Authorization: Bearer sk-…`

**请求字段**

请求体是一个 OpenAI Chat Completions 对象。网关只读取并改写下列字段，其余字段原样转发。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | 否 | 目标模型。存在且非空时必须命中该密钥绑定的模型（见解析规则）；缺失或为空串时使用绑定的 `primary`。 |
| `stream` | bool | 否 | 为 `true` 时走 SSE 流式。 |
| `max_tokens` | integer | 否 | 当密钥 `max_tokens` 上限 `> 0` 时被截断至该上限。 |
| `max_completion_tokens` | integer | 否 | 同样受上限截断。 |
| 其他任意字段 | — | 否 | 原样转发。 |

**网关改写（转发前）**

| 改写 | 规则 |
|------|------|
| 模型名替换 | body 中的 `model` 被替换为解析命中的上游 `model_id`（保留原始大小写）。客户端原值绝不原样转发。 |
| `max_tokens` 上限 | 仅在密钥 `max_tokens > 0` 时生效。若存在 `max_tokens` 与 `max_completion_tokens` 则一并封顶；若**两者都不存在**，则按上限注入 `max_tokens`。上限 `<= 0` 时 body 不做改动。 |
| 参数归一化 | 对 OpenAI 类型上游，GPT-5+ 与 o 系列模型使用 `max_completion_tokens` 而非 `max_tokens`（其余模型相反）。 |
| 流式 | `stream: true` 时，OpenAI 类型上游还会收到 `stream: true` 与 `stream_options.include_usage: true`。 |
| 自定义请求头 | 见下文「自定义请求头透传」。 |

**模型解析规则**

| 客户端 `model` | 密钥绑定 | 结果 |
|----------------|----------|------|
| 存在，命中绑定的**启用**模型 | — | 用该模型的上游 `model_id` 转发。匹配大小写不敏感，且 `/`、`:` 与 `-` 等价；`provider/model_id` 前缀（如 `qcloudlkeap/deepseek-v3.2`）可精确指定不同来源的同名模型；前缀同样接受保存绑定时生成的拼接形态 `{provider}-{model_id}`（如 `hatchery-glm-4-plus/glm-4-plus`），按相同规则匹配。 |
| 存在但未命中 | — | `400 requested model is not bound to this key`，**不回退** primary。 |
| 缺失或空串 | 有 `role=primary` 绑定 | 使用 primary 模型转发。 |
| 缺失或空串 | 无 `primary`（或无启用绑定） | `400 no available model for this key`。 |

只有模型 `enabled=true` 的绑定参与解析。

同一套解析、配额与 `max_tokens` 逻辑同样作用于 `/v1/messages`。

**自定义请求头透传**

模型可声明 `custom_http_headers`（字符串值的 JSON 对象）。透传是全有或全无：

| 条件 | 行为 |
|------|------|
| 配置的每个头都在入站请求中**完全相等**地出现 | 这些头被转发到上游（头名做规范化）。 |
| 任一配置头缺失或值不同 | **不转发任何**自定义头，并记录一条告警。 |
| 模型未配置 `custom_http_headers` | 不做任何自定义头处理。 |

**请求示例**

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

**响应 —— 非流式**

上游状态码与响应体**原样透传**（不重新包装）。因此 `200` 的内容就是上游返回的内容，例如：

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

上游错误状态码（如 `400`、`429`）连同上游响应体同样原样透传。

**响应 —— 流式（`stream: true`）**

响应使用 `Content-Type: text/event-stream`，带 `Cache-Control: no-cache` 与
`X-Accel-Buffering: no`，并增量 flush。对 `anthropic-messages` 上游，Anthropic SSE
事件会被转成 OpenAI `chat.completion.chunk` 帧并以 `data: [DONE]` 收尾。若上游在流开始
前返回非 `200`，该状态码与响应体按 JSON 原样透传。用量在流结束后记录。

**错误**

| 状态码 | `message` | 原因 |
|--------|-----------|------|
| 401 | `missing API key` / `invalid API key` | 无 `Authorization` 头（missing）vs 格式错误、未知或已禁用（invalid）。 |
| 400 | `failed to read request body` | 请求体读取失败。 |
| 413 | `request body too large` | 请求体超过 1 MiB；直接拒绝，不截断。 |
| 400 | `invalid json body` | 请求体不是 JSON 对象。 |
| 400 | `requested model is not bound to this key` | 显式 `model` 未命中任何启用绑定。 |
| 400 | `no available model for this key` | 未提供 `model` 且无启用 `primary` 绑定。 |
| 429 / 403 | `Global token quota exceeded` / `Model daily token quota exceeded` / `User token quota exceeded` / `Key token quota exceeded` | 配额校验拒绝请求。状态码取 `-quota-exceeded-status`（默认 `429`），并带 `Retry-After` 头。 |
| 500 | `internal server error` | 模型解析失败（数据库错误）。 |
| 500 | `streaming not supported by server` | `stream: true` 但服务端无法 flush 响应。 |
| 502 | `failed to connect to LLM backend` | 非流式：到上游的 DNS/连接/TLS/超时失败。细节（含上游地址）只记服务端日志，不回显给调用方。 |

### POST /v1/messages

Anthropic Messages 入站。把 Anthropic 格式的请求转发给密钥绑定命中的上游模型：对
Anthropic 上游保留 Anthropic 语义，对 OpenAI 兼容上游做转换。支持流式与非流式、文本、
图片与工具调用。

**认证：** `x-api-key: sk-…`（优先）或 `Authorization: Bearer sk-…`。鉴权失败一律使用
Anthropic 错误信封。

**请求字段**

请求体是一个 Anthropic Messages 对象。网关只校验并改写下列字段。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | **是** | 必须非空。解析规则与 `/v1/chat/completions` 完全一致。 |
| `max_tokens` | integer | **是** | 必须为整数且 `> 0`。当密钥 `max_tokens` 上限 `> 0` 时被截断至该上限。 |
| `stream` | bool | 否 | 为 `true` 时走 Anthropic SSE 流式。 |
| `messages` / `system` / `tools` / `tool_choice` / `thinking` / `stop_sequences` | — | 否 | 见下方两条上游路径。 |
| 其他任意字段 | — | 否 | 直通路径保留；转换路径按可映射范围转发。 |

校验顺序：`model` 缺失 → `400 model is required`；`max_tokens` 缺失或 `<= 0` → `400 max_tokens is required`。

**上游路径**

命中模型的 `model_type` 决定走哪条路径：

| 上游 | 行为 |
|------|------|
| `anthropic-messages` | **原生直通。** 入站 body 原样转发，仅两处修改：`model` 替换为解析命中的上游 ID，`max_tokens` 施加密钥上限。因此 `cache_control` 等 Anthropic 专有字段端到端保留。上游响应（JSON 或 SSE）原样回传。 |
| `openai-completions`（及其它类型） | **转换。** Anthropic 请求转换为 OpenAI 请求（`system` → system 消息、图片 → data URI、`tool_use`/`tool_result` → `tool_calls`/`role=tool`，`tools`/`tool_choice`/`stop_sequences` 映射），OpenAI 响应或 SSE 流再回译为 Anthropic 格式。 |

直通请求头：`x-api-key`（解析出的上游密钥）、`anthropic-version`（入站值，缺省
`2023-06-01`）、`anthropic-beta`（存在时转发），以及命中的 `custom_http_headers`。

上游超时：非流式 5 分钟，流式 30 分钟。

**流式事件序列**

转换路径的 SSE 流是一条合法的 Anthropic 事件序列：

```text
message_start → content_block_start → content_block_delta* → content_block_stop
             → （后续 block）→ message_delta → message_stop
```

直通路径原样转发上游自身的 Anthropic 事件流。两条路径都在流结束后记录用量（输入/cache
计数取自 `message_start.message.usage`，输出取自 `message_delta.usage`）。

**响应 —— 非流式**

`200` 返回 Anthropic Messages 响应对象（直通路径原样，转换路径由 OpenAI 响应回译）。

**错误**

所有错误使用 Anthropic 信封：

```json
{
  "type": "error",
  "error": {"type": "invalid_request_error", "message": "max_tokens is required"}
}
```

| 状态码 | `error.type` | 原因 |
|--------|--------------|------|
| 401 | `authentication_error` | 密钥缺失或无效/已禁用。 |
| 400 | `invalid_request_error` | 请求体不可读、JSON 非法、`model` 缺失，或 `max_tokens` 缺失/为 0；也包括转换失败。 |
| 413 | `invalid_request_error` | 请求体超过 1 MiB；直接拒绝，不截断。 |
| 429 / 403 | `rate_limit_error` | 配额超限（状态码同 `-quota-exceeded-status` 配置）；带 `Retry-After` 头。 |
| 500 | `api_error` | 内部错误，或服务端不支持流式。 |
| 502 | `api_error` | 无法连接上游，或上游响应无法转换。 |

**已知限制**

- 转换路径（OpenAI 兼容上游）对 OpenAI 无对应概念的字段是**有损**的——`cache_control`
  尤其会被丢弃。需要 cache 断点时请使用 `anthropic-messages` 上游。
- 未知请求字段被忽略而非拒绝（前向兼容）。

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

以 OpenAI list 格式列出该密钥可用的模型。

**认证：** `Authorization: Bearer sk-…`

**响应**

| 字段 | 类型 | 说明 |
|------|------|------|
| `object` | string | 恒为 `"list"`。 |
| `data[].id` | string | 上游 `model_id`，小写。 |
| `data[].object` | string | 恒为 `"model"`。 |
| `data[].created` | integer | 请求时刻的 Unix 秒（非存储时间戳）。 |
| `data[].owned_by` | string | 模型的 `provider`。 |

只返回**启用**的绑定模型，按小写 `model_id` 去重。

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

**错误**

| 状态码 | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 500 | `internal server error` |

## 3. 管理面

所有 `/admin/*` 路由都要求 `Authorization: Bearer <ADMIN_TOKEN>`，且该 token 属于
`role=admin` 用户。路径 `{id}` 必须为正整数；非数字或 0 返回
`400 invalid <model|credential|key|user|group|webhook> id`。目标不存在返回
`404 <model|credential|key|user|group|webhook> not found`。

`401` 与 `403` 的语义见附录「鉴权失败语义」。

### 3.1 模型

模型对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `provider` | string | 自由标签，在 `/v1/models` 中作为 `owned_by`。 |
| `model_id` | string | 发给上游的模型标识。 |
| `model_name` | string | 展示名。 |
| `url` | string | 上游基础地址。 |
| `credential_id` | integer | 引用的凭据 ID；`0` 表示未引用（使用 `api_key`）。 |
| `model_type` | string | `openai-completions` 或 `anthropic-messages`。 |
| `custom_http_headers` | object \| null | 允许透传的头（字符串值）。 |
| `quota_day` | integer | 模型级日 token 配额；`-1` 表示不限。 |
| `enabled` | bool | 禁用后立即不可路由。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

`api_key` **绝不**出现在任何响应中。

#### POST /admin/models

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `provider` | string | 否 | `""` | |
| `model_id` | string | **是** | — | 去空白后必须非空。 |
| `model_name` | string | 否 | `""` | |
| `url` | string | **是** | — | 去空白后必须非空。 |
| `api_key` | string | 否 | `""` | 上游凭据（`credential_id` 为 `0` 时使用）。 |
| `credential_id` | integer | 否 | `0` | 必须引用存在的凭据，否则 `400 credential <id> not found`。 |
| `model_type` | string | **是** | — | 必须为 `openai-completions` 或 `anthropic-messages`。 |
| `custom_http_headers` | object | 否 | 无 | 字符串值的 JSON 对象；`null` 存为空值。 |
| `quota_day` | integer | 否 | `-1` | |
| `enabled` | bool | 否 | `true` | |
| `user_key_required` | bool | 否 | `false` | 自备 Key（BYOK）模式：模型**不保存**共享密钥，转发密钥取绑定行。与 `api_key`（非空）或 `credential_id > 0` 同时出现返回 `400`；创建后不可变更。 |

```bash
curl -X POST http://127.0.0.1:8080/admin/models \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "zhipu",
    "model_id": "glm-5.1",
    "model_name": "GLM-5.1",
    "url": "https://open.bigmodel.cn/api/paas/v4",
    "api_key": "<上游密钥>",
    "model_type": "openai-completions",
    "quota_day": -1,
    "enabled": true
  }'
```

响应 `201 Created` —— 上述模型对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`model_id` 为空；`url` 为空；`model_type` 非法；`custom_http_headers` 格式错误；`credential_id` 不存在；`user_key_required` 与共享 `api_key`/凭据同时出现。 |
| 401 / 403 | 鉴权 / 授权失败。 |
| 500 | 数据库错误。 |

#### GET /admin/models

响应 `200 OK`：

```json
{"models": [ /* 模型对象 */ ]}
```

无模型时数组为 `[]`。

#### GET /admin/models/{id}

响应 `200 OK` —— 单个模型对象；不存在时 `404 model not found`。

#### PUT /admin/models/{id}

局部更新：只修改请求体中出现的字段（未出现的不动）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `provider` | string | |
| `model_id` | string | 为空返回 `400`。 |
| `model_name` | string | |
| `url` | string | 为空返回 `400`。 |
| `api_key` | string | 仅在非空时应用；空串是无操作（凭据无法被清空）。 |
| `credential_id` | integer | `> 0` 必须引用存在的凭据；`0` 解除引用。 |
| `model_type` | string | 必须是合法枚举。 |
| `custom_http_headers` | object \| null | 对象则替换；`null` 清空。 |
| `quota_day` | integer | |
| `enabled` | bool | |
| `user_key_required` | bool | 不可变更：传值与当前不同返回 `400 user_key_required cannot be changed after creation`。自备 Key 模型携带非空 `api_key` 或 `credential_id > 0` 返回 `400`。 |

```bash
curl -X PUT http://127.0.0.1:8080/admin/models/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

响应 `200 OK` —— 更新后的模型对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；`model_id`/`url` 为空；`model_type` 非法；`custom_http_headers` 格式错误；`credential_id` 不存在；id 非法。 |
| 404 | 模型不存在。 |

#### DELETE /admin/models/{id}

删除模型并清理引用它的所有密钥绑定。

响应 `200 OK`：

```json
{"ok": true}
```

| 状态码 | 条件 |
|--------|------|
| 200 | 已删除。 |
| 400 | id 非法。 |
| 404 | 模型不存在。 |

#### POST /admin/models/{id}/connectivity

以最小请求（单条 user 消息、`max_tokens=1`）探测上游，验证地址可达、凭据有效且模型可调用。

**探测结果一律以 HTTP `200` 返回**，成功/失败体现在 body 的 `ok`：

| Body | 含义 |
|------|------|
| `{"ok": true, "latency_ms": N}` | 成功。`latency_ms` 为往返毫秒数（取整且至少为 `1`）。 |
| `{"ok": false, "error_kind": "…", "message": "…"}` | 失败。`error_kind` 为封闭枚举，`message` 为人类可读原因。 |

`error_kind` 取值：

| 取值 | 原因 |
|------|------|
| `network_unreachable` | DNS/连接/TLS/超时，或被 SSRF 拦截（默认兜底项）。 |
| `invalid_api_key` | 上游拒绝凭据。 |
| `forbidden` | 上游禁止该请求。 |
| `rate_limited` | 上游对探测限流。 |
| `upstream_server` | 上游返回 5xx。 |
| `upstream_client` | 上游返回其它 4xx。 |

出站请求复用数据面一致的 SSRF 防护：默认严格模式下私网/回环上游会被拦截并归类为
`network_unreachable`，需要放行时启动加 `-allow-private-upstreams`。上游密钥实时解析
（优先凭据，其次模型自身 `api_key`）；解析失败属服务端配置问题，按 `500` 返回而非探测结果。

`user_key_required`（自备 Key）模型没有共享密钥可探测：返回 `400 user_key_required model has no shared key to probe; verify it from the data plane with a binding key`。

仅路径 id 非法（`400`）与模型不存在（`404`）走统一错误体。

| 状态码 | 条件 |
|--------|------|
| 200 | 探测完成（无论 `ok` 真假）。 |
| 400 | id 非法；模型为 `user_key_required`。 |
| 404 | 模型不存在。 |
| 500 | 凭据/密钥解析失败。 |

### 3.2 凭据

上游凭据把一条或多条密钥组织在一起，由模型的 `credential_id` 引用。转发时网关实时解析
凭据（不使用快照），取首条 `location=header` 的密钥；凭据不存在、已禁用或无 header
密钥时回退到模型自身的 `api_key`。

凭据对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `description` | string | |
| `secrets` | array | 密钥项；`value` 恒为掩码。 |
| `enabled` | bool | 禁用后回退模型自身 `api_key`。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

密钥项对象：

| 字段 | 类型 | 说明 |
|------|------|------|
| `key` | string | 头名。 |
| `value` | string | 读取时掩码：长度 > 4 时保留首 2 + `*`*(n-4) + 末 2；更短时整体为 `*`。 |
| `location` | string | 仅支持 `header`；省略/空串会归一化为 `header`。 |

#### POST /admin/credentials

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | **是** | — | 去空白后必须非空。 |
| `description` | string | 否 | `""` | |
| `secrets` | array | 否 | 无 | 每项 `key` 必填；`location` 必须为 `header`。 |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/credentials \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "zhipu-prod",
    "description": "生产上游密钥",
    "secrets": [{"key": "Authorization", "value": "Bearer <上游密钥>", "location": "header"}]
  }'
```

响应 `201 Created` —— 带掩码 `secrets` 的凭据对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`name` 为空；某条密钥 `key` 为空或 `location` 非 `header`。 |
| 500 | 数据库错误。 |

#### GET /admin/credentials

```json
{"credentials": [ /* 凭据对象，secrets 已掩码 */ ]}
```

#### GET /admin/credentials/{id}

响应 `200 OK` —— 单个凭据对象（secrets 掩码）；不存在时 `404 credential not found`。

#### PUT /admin/credentials/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 为空返回 `400`。 |
| `description` | string | |
| `secrets` | array | 出现时**整体替换**；不传则该字段不动。 |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的凭据对象。

#### DELETE /admin/credentials/{id}

删除凭据。若仍有模型引用它，请求被拒绝并返回 `409`，message 含引用数量：

```json
{
  "error": {
    "message": "credential is referenced by 2 model(s); detach them before deleting",
    "type": "error",
    "code": 409
  }
}
```

请先解除引用（把模型的 `credential_id` 置 `0` 或改指其它凭据）。

| 状态码 | 条件 |
|--------|------|
| 200 | 已删除（`{"ok": true}`）。 |
| 404 | 凭据不存在。 |
| 409 | 仍被一个或多个模型引用。 |

### 3.3 密钥

数据面密钥（`sk-…`）归属于某个用户，并可绑定一组模型。

密钥对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `user_id` | integer | 属主用户 ID。 |
| `group_id` | integer | 可选分组，未设置时为 `0`。 |
| `proxy_token_prefix` | string | 展示前缀，形如 `sk-abcd1234…wxyz`。 |
| `max_tokens` | integer | 请求侧上限；`<= 0` 表示不限。 |
| `token_quota_rules` | string | 密钥级周期配额（规范化后的规则 JSON）；`""` = 无密钥级限制。用户面只读。 |
| `enabled` | bool | 禁用后数据面鉴权返回 `401`。 |
| `expires_at` | string / null | 可选有效期（RFC 3339，UTC）；`null` = 永不过期。 |
| `expired_at` | string / null | 首次因过期被拒绝的观测时间；`null` = 未发生。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |
| `bindings` | array | 绑定视图（无绑定时为 `[]`）。 |
| `token` | string | **仅**在创建与 rotate 响应中出现。 |

绑定视图对象：

| 字段 | 类型 | 说明 |
|------|------|------|
| `ai_model_id` | integer | 引用的模型 ID。 |
| `model_id` | string | |
| `model_name` | string | |
| `provider` | string | |
| `role` | string | `primary` 或 `fallback`。 |
| `sort_order` | integer | 排序提示。 |
| `has_key` | bool | 是否已配置绑定级上游密钥（`user_key_required` 模型必需；值本身永不返回）。 |

#### POST /admin/keys

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `user_id` | integer | **是** | — | 必须引用存在的用户，否则 `400 user_id does not exist`。 |
| `group_id` | integer | 否 | `0` | |
| `max_tokens` | integer | 否 | `0` | `<= 0` 表示不封顶。 |
| `token_quota_rules` | string | 否 | `""` | 密钥级周期配额（规则数组 JSON，会做校验与规范化）。`""` = 无密钥级限制；`"[]"` = 显式不限。 |
| `expires_at` | string | 否 | `null` | RFC 3339 时间戳；缺省/`null` = 永不过期。无法解析时报 `400 expires_at must be an RFC3339 timestamp`。 |
| `bindings` | array | 否 | 无 | 见下。 |

每个绑定对象：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `ai_model_id` | integer | **是** | 必须 `> 0` 且引用存在的模型。 |
| `role` | string | **是** | `primary` 或 `fallback`；其它取值被拒绝。 |
| `sort_order` | integer | 否 | 默认 `0`。 |
| `api_key` | string | 否 | 绑定级上游密钥，仅 `user_key_required`（自备 Key）模型使用；普通模型忽略。加密落库、永不回显（仅以 `has_key` 反映是否已配置）。 |

`api_key` 随 `bindings` 整体替换：替换绑定但未重新携带 `api_key` 时该绑定的密钥被清空（`has_key` 变为 `false`）。

`bindings` 非空时必须**恰好一个** `primary`，否则返回
`400 bindings must contain exactly one primary model`。

注意：每用户 `settings.user_key_limit` 只作用于自助接口（§4.4），**不**约束管理面创建的密钥。

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

响应 `201 Created` —— 密钥对象，其 `token` 字段为一次性明文（`sk-…`）。除 rotate 外这是唯一一次返回。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`user_id` 不存在；绑定角色非法；`ai_model_id` 为 `0`；`ai_model_id` 不存在；`primary` 不是恰好一个；绑定重复。 |
| 401 / 403 | 鉴权 / 授权失败。 |
| 500 | 数据库错误。 |

#### GET /admin/keys

响应 `200 OK`：

```json
{"keys": [ /* 密钥对象，不含 token */ ]}
```

#### GET /admin/keys/{id}

响应 `200 OK` —— 单个密钥对象（无 `token`）；不存在时 `404 key not found`。

#### PUT /admin/keys/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `max_tokens` | integer | |
| `token_quota_rules` | string | `""` 清除密钥级配额；否则必须是合法规则数组（会做校验与规范化）。 |
| `enabled` | bool | |
| `expires_at` | string / null | RFC 3339 值 = 设置新有效期（并清空 `expired_at`）；显式 `null` = 两者都清除；其余非法值为 `400`。 |
| `bindings` | array | 出现时**整体替换**；`[]` 清空。不传则保持原绑定。 |

`user_id` 与 `group_id` 不可更新。

```bash
curl -X PUT http://127.0.0.1:8080/admin/keys/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false, "bindings": []}'
```

响应 `200 OK` —— 更新后的密钥对象（无 `token`）。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；绑定集合非法；id 非法。 |
| 404 | 密钥不存在。 |

#### DELETE /admin/keys/{id}

删除密钥及其绑定。响应 `200 OK`：

```json
{"ok": true}
```

#### POST /admin/keys/{id}/rotate

换发新的 `sk-` 凭据，旧凭据立即失效。密钥的绑定与设置保留。

响应 `200 OK` —— 带一次性 `token` 的密钥对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已 rotate。 |
| 400 | id 非法。 |
| 404 | 密钥不存在。 |

### 3.4 用户

用户持有访问 token（`gk-…`），用于管理面（`role=admin` 时）与用户面鉴权。

用户对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `role` | string | `admin` 或 `user`。 |
| `group_id` | integer | 未设置时为 `0`。 |
| `api_token_prefix` | string | 展示前缀。 |
| `enabled` | bool | 禁用后鉴权返回 `401`。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |
| `token` | string | **仅**在创建与 rotate 响应中出现。 |

#### POST /admin/users

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `role` | string | 否 | `user` | 必须为 `admin` 或 `user`。 |
| `group_id` | integer | 否 | `0` | |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/users \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "role": "user", "group_id": 0}'
```

响应 `201 Created` —— 用户对象，其 `token` 为一次性明文（`gk-…`）。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`role` 非法。 |
| 500 | 数据库错误。 |

#### GET /admin/users

响应 `200 OK`：

```json
{"users": [ /* 用户对象，不含 token */ ]}
```

没有单用户 `GET /admin/users/{id}` 端点。

#### PUT /admin/users/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `role` | string | 必须为 `admin` 或 `user`。 |
| `group_id` | integer | |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的用户对象（无 `token`）；不存在时 `404 user not found`。

#### DELETE /admin/users/{id}

删除用户，并级联删除其密钥及绑定。历史用量日志保留。

响应 `200 OK`：

```json
{"ok": true}
```

#### POST /admin/users/{id}/rotate

换发新的访问 token，旧 token 立即失效。

响应 `200 OK` —— 带一次性 `token` 的用户对象。

### 3.5 分组

分组承载 token 配额策略，用户通过 `user.group_id` 归属。数据面配额判定时，用户所属组的
策略是解析链的第一档（组策略 → 全局默认）。

分组对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `token_quota_rules` | string | 规则数组的 JSON **文本**。`""` 表示**未配置**（回退全局默认）；`"[]"` 表示显式不限；其余为规范化后的规则 JSON。 |
| `global_token_quota_rules` | string | 组级全局配额覆盖。`""` = 未配置（回退站点规则与 `global_quota_scope`）；`"[]"` = 显式不限；否则为组自身规则，且**固定按组计量**。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

#### POST /admin/user-groups

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `token_quota_rules` | string | 否 | `""` | 省略/空串 = 未配置；否则必须是合法 JSON 数组（会做规则校验与规范化）。 |
| `global_token_quota_rules` | string | 否 | `""` | 可选的组级全局配额覆盖；校验同上。配置后（含 `"[]"`）该组按自身规则、以组用量判定。 |

```bash
curl -X POST http://127.0.0.1:8080/admin/user-groups \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}'
```

响应 `201 Created` —— 分组对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`token_quota_rules` 不是 JSON 数组或规则校验失败。 |
| 500 | 数据库错误。 |

#### GET /admin/user-groups

```json
{"user_groups": [ /* 分组对象 */ ]}
```

#### GET /admin/user-groups/{id}

响应 `200 OK` —— 单个分组对象；不存在时 `404 group not found`。

#### PUT /admin/user-groups/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `token_quota_rules` | string | 校验同创建。 |
| `global_token_quota_rules` | string | 校验同创建；`""` 清除组级覆盖。 |

响应 `200 OK` —— 更新后的分组对象。

#### DELETE /admin/user-groups/{id}

只删除分组本身，**不**级联用户/密钥。`group_id` 变为悬空的用户回退全局默认规则。

响应 `200 OK`：

```json
{"ok": true}
```

### 3.6 用量查询

#### GET /admin/usage/summary

聚合 token 用量。查询参数：

| 参数 | 类型 | 说明 |
|------|------|------|
| `group_by` | string | `date`（默认）、`model`、`key`、`user` 或 `group`。 |
| `start` / `end` | string | `YYYY-MM-DD`，按业务时区解释；`end` 含当日整天。 |
| `user_id` / `group_id` / `api_key_id` / `ai_model_id` | integer | 可选过滤。 |

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

`date` 字段仅在 `group_by=date` 时出现。无数据时 `rows` 为 `[]`。

#### GET /admin/usage/logs

逐请求用量明细，按时间倒序。接受与 `summary` 相同的时间区间与 id 过滤，外加 `page`
（默认 1）与 `page_size`（默认 50，上限 500）。

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

两者的错误：日期 / `group_by` / id 过滤非法返回 `400`；数据库错误返回 `500`。

#### GET /admin/usage/export

以文件流导出用量明细（字段与 `/admin/usage/logs` 一致）；过滤参数与 `logs` 同源；导出按批流式写出（不会整表载入内存）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `format` | string | `csv`（默认）或 `json`；其余取值返回 `400 format must be csv or json`。 |
| `start` / `end` / `user_id` / `group_id` / `api_key_id` / `ai_model_id` | — | 语义同 `/admin/usage/logs`。 |

- `format=csv` → `text/csv` 附件，首行为表头（`id,api_key_id,user_id,group_id,ai_model_id,model,provider,prompt_tokens,completion_tokens,total_tokens,prompt_cache_read_tokens,prompt_cache_write_tokens,status_code,latency,created_at`）。
- `format=json` → `{"logs": [ … ]}` 流式输出，行对象与 `logs` 一致。

```bash
curl -OJ "http://127.0.0.1:8080/admin/usage/export?format=csv&start=2026-09-01" \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

### 3.7 全局设置

全局设置以单行存储。

| 字段 | 类型 | 说明 |
|------|------|------|
| `default_token_quota_rules` | string | 规则数组的 JSON 文本，作用于无组策略的用户。 |
| `global_token_quota_rules` | string | 规则数组的 JSON 文本，即全局档。 |
| `global_quota_scope` | string | `site` 或 `group`，决定全局用量的统计口径。 |
| `user_key_limit` | integer | 每用户自助密钥数量上限，默认 `5`；`0` = 不限（见 §4.4）。 |

#### GET /admin/settings

返回当前设置，首次访问时创建默认行。默认为 `"[]"`、`"[]"`、`site`、`5`。

```json
{
  "default_token_quota_rules": "[]",
  "global_token_quota_rules": "[]",
  "global_quota_scope": "site",
  "user_key_limit": 5
}
```

#### PUT /admin/settings

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `default_token_quota_rules` | string | 必须是合法 JSON **数组**（空串或 `null` 被拒绝）。 |
| `global_token_quota_rules` | string | 必须是合法 JSON 数组。 |
| `global_quota_scope` | string | 必须为 `site` 或 `group`。 |
| `user_key_limit` | integer | 必须 `>= 0`；`0` 表示不限。 |

```bash
curl -X PUT http://127.0.0.1:8080/admin/settings \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"global_quota_scope": "group", "user_key_limit": 10}'
```

响应 `200 OK` —— 完整的设置对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；规则不是 JSON 数组；scope 非法；`user_key_limit < 0`。 |
| 500 | 数据库错误。 |

### 3.8 webhook

webhook 订阅用于接收出站事件。投递是异步且带签名的；确切语义见附录。

webhook 对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `url` | string | 接收端地址；必须为 `http` 或 `https`。 |
| `secret` | string | **恒为掩码**（只写不读回）。 |
| `event_types` | array | 订阅的事件类型；`[]` 表示订阅全部。 |
| `enabled` | bool | 禁用后不接收任何投递。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

#### POST /admin/webhooks

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `url` | string | **是** | — | 必须能解析为带 host 的 `http`/`https` URL。 |
| `secret` | string | 否 | `""` | 用于签名投递（至多 256 字节）；绝不回读。 |
| `event_types` | array | 否 | `[]` | 非空字符串；`[]`（或省略）表示订阅全部。 |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/webhooks \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "siem", "url": "https://example.com/hooks/llm",
       "secret": "<HMAC_SECRET>", "event_types": ["quota.exceeded"]}'
```

响应 `201 Created` —— webhook 对象；`secret` 为掩码。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；URL 非法；`event_types` 含空串。 |
| 500 | 数据库错误。 |

#### GET /admin/webhooks

```json
{"webhooks": [ /* webhook 对象，secret 已掩码 */ ]}
```

#### GET /admin/webhooks/{id}

响应 `200 OK` —— 单个 webhook 对象（secret 掩码）；不存在时 `404 webhook not found`。

#### PUT /admin/webhooks/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `url` | string | 必须是合法 `http(s)` URL。 |
| `secret` | string | **空串表示保持不变**——该字段不可读回，因此也无法用空值清空。 |
| `event_types` | array | 整体替换订阅集合；`[]` 表示订阅全部。 |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的 webhook 对象。

#### DELETE /admin/webhooks/{id}

响应 `200 OK`：

```json
{"ok": true}
```

#### GET /admin/webhooks/{id}/deliveries

该订阅的投递记录，按时间倒序分页（`page` 默认 1，`page_size` 默认 50，上限 500）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 投递 ID。 |
| `webhook_id` | integer | 所属订阅。 |
| `event_type` | string | 投递的事件类型。 |
| `status` | string | `pending`、`success` 或 `failed`。 |
| `attempts` | integer | 已尝试次数。 |
| `last_status` | integer | 最后一次尝试的 HTTP 状态码（网络错误为 `0`）。 |
| `last_error` | string | 最后一次尝试的错误文本。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

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

### 3.9 导入

一次性导入存量数据（分组、用户、模型、密钥）。

**处理顺序**固定为 `user_groups → users → models → keys`：后者可引用同一请求中先前创建的实体。
`dry_run=true` 只做校验与计划，不写库。

**幂等键** —— 已存在的实体记为 `skipped`：

| 种类 | 幂等键 |
|------|--------|
| `groups` | `name` |
| `users` | `name` |
| `models` | `(provider, model_id)` |
| `keys` | `token`（哈希后比较） |

**请求体**

| 字段 | 类型 | 说明 |
|------|------|------|
| `dry_run` | bool | 只做计划，不写库。 |
| `groups[].name` | string | 必填。 |
| `user_groups[].token_quota_rules` | string | 可选；校验同 `/admin/user-groups`。 |
| `users[].name` | string | 必填。 |
| `users[].role` | string | `admin` 或 `user`；默认 `user`。 |
| `users[].group` | string | 分组**名**；空 = 不指定。必须能解析到库中/本次计划的分组。 |
| `models[].provider` / `model_id` | string | `model_id` 必填；`(provider, model_id)` 为幂等键。 |
| `models[].url` | string | 必填。 |
| `models[].api_key` | string | 可选。 |
| `models[].model_type` | string | `openai-completions` 或 `anthropic-messages`。 |
| `models[].quota_day` / `enabled` | integer / bool | 可选。 |
| `keys[].token` | string | **明文**，必填。导入时哈希，明文不落库、不回显。 |
| `keys[].user` | string | 属主**名**，必填且必须可解析。 |
| `keys[].group` | string | 分组名；空 = 不指定。 |
| `keys[].name` / `max_tokens` | string / integer | 可选。 |
| `keys[].bindings[]` | array | `{model, role, api_key?}`，其中 `model` 是**模型 model_id**。非空时必须恰好一个 `primary` 且无重复；可选 `api_key` 为绑定级（自备 Key）密钥，加密落库。 |

**响应**

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

- `details[].kind` 取值 `group` / `user` / `model` / `key`。
- `details[].action` 取值 `created` / `skipped` / `error`；错误时给出 `message`。
- `details[].ref` 只承载非敏感标识（名字、`provider/model_id`、token 展示前缀），绝不含 token 明文。

单条失败会被记录且不中断其余条目；请求本身返回 `200`。导入的用户**不返回**访问 token
（内部生成随机 token 并丢弃明文）；需要时用 `POST /admin/users/{id}/rotate` 换发。

```bash
curl -X POST http://127.0.0.1:8080/admin/import \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "dry_run": true,
    "user_groups": [{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}],
    "users": [{"username": "bob", "role": "user", "group": "team-a"}],
    "models": [{"provider": "zhipu", "model_id": "glm-5.1", "url": "https://open.bigmodel.cn/api/paas/v4",
                "api_key": "<上游密钥>", "model_type": "openai-completions"}],
    "keys": [{"name": "bob-default", "user": "bob", "group": "team-a", "token": "sk-legacy-…",
              "bindings": [{"model": "glm-5.1", "role": "primary"}]}]
  }'
```

## 4. 用户面

前缀 `/api/user/*`。任意启用用户 token（`user` 或 `admin`）均可访问。`user` 角色 token
被强制作用于本人数据（其中的 `user_id` 被忽略）；`admin` 角色 token 可用 `?user_id=`
指定其他用户。

### 4.1 GET /api/user/me

返回调用者身份，含分组名（未分组或分组已删除时为空串）。

```json
{"id": 2, "username": "alice", "role": "user", "group_id": 1, "group_name": "team-a"}
```

### 4.2 GET /api/user/quota

目标用户生效规则的逐条视图（经由组策略 → 全局默认链解析）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `user_id` | integer | 解析后的目标用户。 |
| `usages[]` | array | 每条生效规则一项。 |
| `usages[].rule_index` | integer | 规则在解析集合中的序号。 |
| `usages[].mode` | string | `day`、`month`、`year` 或 `custom`。 |
| `usages[].limit` | integer | `-1` 表示参与窗口计算但不限用量。 |
| `usages[].used` | integer | 当前窗口已用 token。 |
| `usages[].active` | bool | 规则当前是否生效。 |
| `usages[].period_start` / `period_end` | integer \| null | 窗口起止（Unix 秒）；`end` 为 `null` 表示无终止。 |

```json
{
  "user_id": 2,
  "usages": [
    {"rule_index": 0, "mode": "day", "limit": 1000000, "used": 25310,
     "active": true, "period_start": 1758038400, "period_end": 1758124800}
  ]
}
```

### 4.3 用量查询

`GET /api/user/usage/summary`、`GET /api/user/usage/logs` 与
`GET /api/user/usage/by-model` 与管理面对应端点同源，但行级隔离：`user` 角色 token
无论带什么 `user_id` 过滤都只能看到自己的用量。

| 端点 | 说明 |
|------|------|
| `GET /api/user/usage/summary` | 参数与响应同 `/admin/usage/summary`（`group_by`、`start`/`end`、id 过滤）。 |
| `GET /api/user/usage/logs` | 同 `/admin/usage/logs`，带 `page` / `page_size` 分页（时间倒序）。 |
| `GET /api/user/usage/by-model` | 同 `summary`，但固定按 `model` 聚合；`group_by` 参数被忽略。 |

### 4.4 自助密钥

`/api/user/keys` 让用户自行签发与管理数据面密钥。两条护栏：

- **数量上限** —— `settings.user_key_limit`（默认 `5`；`0` = 不限）。超限返回
  `400 key limit reached (limit N)`。
- **模型范围** —— 新建或更新的密钥只能绑定「该用户名下**任意**已有密钥出现过的模型」。
  绑定集合外模型返回 `400 model not available for this user`；若用户完全没有可绑定模型，
  返回 `400 no available models, please contact an administrator`。

隔离：密钥不存在**或**不属于本人一律返回 `404 key not found`，避免通过 id 探测。

方法：`POST /api/user/keys`、`GET /api/user/keys`、`GET /api/user/keys/{id}`、
`PUT /api/user/keys/{id}`、`DELETE /api/user/keys/{id}`、
`POST /api/user/keys/{id}/rotate`。

密钥对象形状与管理面一致（§3.3）。`token` 仅在创建与 rotate 时返回。

#### POST /api/user/keys

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `max_tokens` | integer | 否 | `0` | `<= 0` 表示不封顶。 |
| `bindings` | array | 否 | 无 | `{ai_model_id, role, sort_order, api_key?}`；非空时必须恰好一个 `primary`，且每个模型都在用户范围内。可选 `api_key` 为用户自有上游密钥（自备 Key 模型用；加密落库、不回显）。 |

`token_quota_rules`（密钥级配额）在用户面**只读**：接口会返回该字段，但用户面的写入会被忽略（仅管理员可配置密钥级配额）。

```bash
curl -X POST http://127.0.0.1:8080/api/user/keys \
  -H "Authorization: Bearer <USER_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key", "max_tokens": 4096,
       "bindings": [{"ai_model_id": 1, "role": "primary"}]}'
```

响应 `201 Created` —— 带一次性 `token` 的密钥对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；已达数量上限；模型超出允许范围；无可绑定模型；绑定集合非法。 |
| 404 | 目标用户不存在。 |

#### GET /api/user/keys

```json
{"keys": [ /* 密钥对象，不含 token */ ]}
```

#### GET /api/user/keys/{id}

响应 `200 OK` —— 单个密钥对象（无 `token`）；不存在或非本人时 `404 key not found`。

#### PUT /api/user/keys/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `max_tokens` | integer | |
| `enabled` | bool | |
| `bindings` | array | 出现时整体替换绑定，并重新按模型范围校验。 |

#### DELETE /api/user/keys/{id}

响应 `200 OK`：

```json
{"ok": true}
```

#### POST /api/user/keys/{id}/rotate

换发新的 `sk-` 凭据，旧凭据立即失效。响应 `200 OK` —— 带一次性 `token` 的密钥对象。

#### GET /api/user/keys/{id}/usage

单个密钥的按日用量趋势（与 `/api/user/usage/summary` 同一套日聚合）。`user` 角色只能查询自己名下的密钥（其余返回 `404 key not found`）；`admin` 可用 `?user_id=` 查询任意密钥。

| 参数 | 类型 | 说明 |
|------|------|------|
| `start` / `end` | string | `YYYY-MM-DD`（业务时区，含首含尾）；两者都不传时默认**最近 30 天**。 |

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

## 5. 健康检查

### GET /healthz

无需鉴权。

响应 `200 OK`：

```json
{"status": "ok"}
```

## 6. 附录

### 状态码汇总

| 状态码 | 出现位置 | 含义 |
|--------|----------|------|
| 200 | 全部层面 | 成功；`DELETE` 返回 `{"ok": true}`。 |
| 201 | 管理面写接口（models、credentials、keys、users、groups、webhooks）与用户面密钥创建 | 资源已创建。 |
| 400 | 全部层面 | 请求体格式错误、路径 id 非法，或字段/业务校验失败。 |
| 401 | 全部层面 | 凭据缺失、格式错误、不存在或已禁用。 |
| 403 | 管理面 | 凭据有效但用户角色不是 `admin`。 |
| 404 | 管理面 / 用户面 | 目标资源不存在（或不属于本人）。 |
| 409 | 管理面 | 凭据仍被模型引用，无法删除。 |
| 429 / 403 | 数据面 | 配额超限；确切状态码由 `-quota-exceeded-status` 决定（默认 `429`），并带 `Retry-After` 头。 |
| 500 | 全部层面 | 内部错误。 |
| 502 | 数据面 | 上游连接失败，或上游响应无法转换。 |

### 鉴权失败语义：401 vs 403

| 条件 | 状态码 | `message` |
|------|--------|-----------|
| 头缺失、非 `Bearer …`，或 token 不存在 | 401 | `missing or invalid bearer token` |
| token 有效但用户被禁用 | 401 | `user is disabled` |
| token 有效、用户启用，但角色不允许 | 403 | `insufficient role` |

数据面上，未携带凭据返回 `401 missing API key`；「密钥不存在」与「密钥被禁用」不可区分，
均返回 `401 invalid API key`。在 `/v1/messages` 上，同类情形返回 `401` 且
`error.type = authentication_error`。

### webhook 事件交付

**信封。** 每次投递向订阅 URL `POST` 一个 JSON body：

```json
{
  "id": "11",
  "type": "quota.exceeded",
  "created_at": "2026-09-01T16:00:00Z",
  "data": { }
}
```

`id` 即投递 ID（与 `/admin/webhooks/{id}/deliveries` 的行 id 一致）；`created_at` 为 UTC。

**请求头。**

| 头 | 值 |
|----|----|
| `Content-Type` | `application/json` |
| `X-Gateway-Event` | 事件类型（如 `request.completed`） |
| `X-Gateway-Delivery` | 投递 ID |
| `X-Gateway-Signature` | `sha256=<hex(hmac-sha256(secret, 原始 body))>` |

校验签名：用订阅 secret 对**原始请求 body** 计算 HMAC-SHA256 并 hex 编码，与 `sha256=`
之后的值比较。

**投递语义。**

- 投递异步、由单 consumer 串行处理，绝不阻塞数据面。
- 单次尝试超时 **10 秒**；非 2xx 响应或网络错误都会触发重试。
- **至多 3 次尝试**；第 2、3 次尝试前分别等待 **1s** 与 **5s**。
- 每次尝试都更新投递记录（`attempts`、`last_status`、`last_error`），最终落终态
  `success` 或 `failed`。
- 事件队列（`-webhook-buffer`，默认 `256`）有界：队满时新事件被丢弃并计数
  （`webhook_dropped`），而非阻塞调用方。
- 重试复用同一投递 ID，因此接收端可能多次收到同一事件。请按**至少一次**语义处理，并用
  `X-Gateway-Delivery` 去重。最后一次尝试失败后事件不再重发。

**事件类型与 `data` 字段。**

| `type` | 触发时机 | `data` 字段 |
|--------|----------|-------------|
| `request.completed` | 一次数据面转发（含流式）完成。 | `api_key_id`、`user_id`、`group_id`、`model`、`provider`、`status_code`、`latency_ms`、`usage`（`prompt_tokens`、`completion_tokens`、`total_tokens`、`prompt_cache_read_tokens`、`prompt_cache_write_tokens`） |
| `quota.exceeded` | 请求因配额校验被拒绝。 | `api_key_id`、`user_id`、`group_id`、`model`、`kind`（`global` / `model` / `user` / `key`）、`limit`、`used` |
| `key.disabled` | 管理员禁用或删除密钥。 | `key_id`、`name`、`user_id`、`reason`（`disabled` / `deleted`） |
| `key.expired` | 密钥在 `expires_at` 之后被使用（首次拒绝时发出，同一有效期内只发一次）。 | `key_id`、`name`、`user_id` |

只有 `enabled=true` 且 `event_types` 为空或包含该类型的订阅才会收到。

### 密钥加密

网关可用 AES-256-GCM 对密钥类列做静态加密：

| 事项 | 说明 |
|------|------|
| 覆盖列 | `ai_models.api_key`、`api_key_models.api_key`（绑定级自备密钥）、`credentials.secrets_json`、`webhooks.secret`。 |
| 密文格式 | `enc:v1:` + base64(nonce ‖ ciphertext)；带前缀的值按密文处理，其余按明文。 |
| 主密钥 | 64 位 hex 或 base64（解码后 32 字节），经 `-master-key` / `AIGATE_MASTER_KEY` 注入。 |
| 未配置 | 明文存储并在启动时输出告警；读取照常可用。 |
| 已配置 | 新增/更新值加密；存量明文行在迁移前保持原样。 |
| 迁移 | `-migrate-encrypt` 就地加密存量明文行后退出，输出 `migrated`/`skipped`/`failed`；幂等且需主密钥。 |

完整操作手册（含「务必备份主密钥」的告诫——主密钥丢失将使所有密文不可读）见
[docs/deploy.zh-CN.md](deploy.zh-CN.md)。

### 运行须知

| 事项 | 说明 |
|------|------|
| 上游 URL 拼接 | `openai-completions` 转发到 `<url>/chat/completions`；`anthropic-messages` 转发到 `<url>/v1/messages`。`url` 末尾的斜杠会被去除。 |
| SSRF 策略 | 出站请求（上游与 webhook 接收端）默认校验，会**拒绝私网/回环地址**。本地端点需以 `-allow-private-upstreams`（或 `AIGATE_ALLOW_PRIVATE_UPSTREAMS=true`）启动。 |
| 请求体大小 | 每请求 1 MiB；超限直接拒绝（数据面 413、管理面/用户面 400），不截断。 |
| 业务时区 | 仅影响配额窗口与用量/日期记账口径，不改变 API 响应。 |
| 用量写入 | 用量异步写入；极端负载下可能丢失少量条目，网关不承担计费真源职责。 |
| 投递保留 | `webhook_deliveries` 记录无限保留——没有 TTL、裁剪或表容量上限。 |
