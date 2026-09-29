# 部署

[English](deploy.md) | [简体中文](deploy.zh-CN.md)

本文档说明如何运行 Tencent Cloud AIGate、完整的配置项参考、密钥加密、Docker 镜像、上游
SSRF 策略以及日常运维要点。项目概览见 [README](../README.zh-CN.md)。

网关是单二进制、无前端。它需要一个数据库（本地用 SQLite，生产用 MySQL），以及到
其转发目标模型端点的网络连通性（如配置了 webhook，还需要到投递接收端的连通性）。

## 本地：SQLite

SQLite 用于本地开发与体验。它是单写数据库，无法以多副本方式部署。

```bash
make run
```

`make run` 会构建 `bin/aigate` 并基于 SQLite 文件启动：

```bash
bin/aigate -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token
```

走 SQLite 时，网关在启动阶段自动建表，无需任何 schema 步骤。`make build` 单独产出
二进制，`make test` 运行单测。执行 `bin/aigate -version` 会在不触碰数据库的情况下
打印 `Tencent Cloud AIGate 0.1.0`。

## 生产：MySQL

本地体验之外的场景一律使用 MySQL。Schema 由外部显式管理：网关**不会**对 MySQL
做自动迁移，因此必须先导入 `sql/init.sql`。

```bash
# 1) 建库
mysql -h 127.0.0.1 -P 3306 -u root \
  -e "CREATE DATABASE aigate DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 2) 导入 schema
mysql -h 127.0.0.1 -P 3306 -u root aigate < sql/init.sql
```

`sql/init.sql` 会创建全部表（`users`、`ai_models`、`api_keys`、`api_key_models`、
`llm_usage_logs`、`daily_usage_summaries`、`groups`、`settings`、`credentials`、
`webhooks`、`webhook_deliveries`），并写入单例 `settings` 行，作为全局默认值的承载。

然后用 DSN 启动网关。DSN 采用 `go-sql-driver/mysql` 格式，且必须保留
`parseTime=True`：

```bash
bin/aigate \
  -db-type mysql \
  -db-dsn 'user:password@tcp(127.0.0.1:3306)/aigate?charset=utf8mb4&parseTime=True&loc=Local' \
  -timezone Asia/Shanghai \
  -init-admin-token '<引导管理员 token>' \
  -master-key '<64 hex 或 base64 主密钥>'
```

DSN 结构为 `user:password@tcp(host:port)/database?params`。请保留 `parseTime=True`
以保证时间列正确扫描，并设置 `charset=utf8mb4`。

schema 变更需在 GORM 模型与 `sql/init.sql` 两处同步（`init.sql` 是全新安装的单一
事实源）：直接修改 `sql/init.sql`；已部署实例如需原地升级，另加
`sql/YYYYMMDD-<slug>.sql` 增量（年份在前的日期前缀）。

## 配置项

每项配置都同时提供命令行 flag 与 `AIGATE_*` 环境变量，优先级为：
**flag > 环境变量 > 默认值**。配置在启动阶段校验；非法取值（未知 `db-type`、非法
`timezone`、`usage-buffer < 1`、`webhook-buffer < 1`、`webhook-delivery-retention-days` 为负、`quota-exceeded-status` 非
429/403、`addr`/`db-dsn` 为空、主密钥格式错误）会让进程直接退出，而不是静默兜底。

| Flag | 环境变量 | 默认值 | 校验 / 说明 |
|------|----------|--------|-------------|
| `-addr` | `AIGATE_ADDR` | `:8080` | 监听地址，不能为空。 |
| `-db-type` | `AIGATE_DB_TYPE` | `sqlite` | 取值 `sqlite` 或 `mysql`。 |
| `-db-dsn` | `AIGATE_DB_DSN` | `aigate.db` | SQLite 文件路径或 MySQL DSN，不能为空。 |
| `-timezone` | `AIGATE_TIMEZONE` | `Asia/Shanghai` | IANA 名称，用于配额窗口与日聚合标签；必须可加载。 |
| `-usage-buffer` | `AIGATE_USAGE_BUFFER` | `256` | 用量异步写入队列容量，须 >= 1。 |
| `-webhook-buffer` | `AIGATE_WEBHOOK_BUFFER` | `256` | webhook 事件投递队列容量，须 >= 1；非法取值在启动阶段直接拒绝，不做静默归一。 |
| `-webhook-delivery-retention-days` | `AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS` | `7` | `webhook_deliveries` 记录的保留天数（`0` = 永久保留）；投递器启动时清理一次、此后每小时清理过期记录；须 >= 0。 |
| `-quota-exceeded-status` | `AIGATE_QUOTA_EXCEEDED_STATUS` | `429` | 仅支持 `429` 或 `403`；配额校验拒绝请求时使用的状态码。 |
| `-init-admin-token` | `AIGATE_INIT_ADMIN_TOKEN` | （空） | 引导管理员 token；为空表示「自动生成并打印一次」。 |
| `-allow-private-upstreams` | `AIGATE_ALLOW_PRIVATE_UPSTREAMS` | `false` | 为 true 时放宽 SSRF 严格模式，放行私网/回环上游。 |
| `-master-key` | `AIGATE_MASTER_KEY` | （空） | AES-256-GCM 主密钥：64 位 hex 或 base64（解码后 32 字节）。为空则禁用加密（明文存储 + 启动告警）。见「密钥加密」。 |
| `-migrate-encrypt` | — | `false` | 一次性启动开关：加密存量明文密钥行后退出（不起服务）。需同时配置 `-master-key`。见「密钥加密」。 |

`-init-admin-token` 只在尚不存在管理员时生效。若管理员已存在，该 flag 被忽略并记
一条日志。token 以哈希入库，明文不落库。

`-version`（或 `--version`）会在任何配置/数据库动作之前打印 `Tencent Cloud AIGate 0.1.0`
并退出；它不接受其它 flag。

## 密钥加密

有四列存放密钥：`ai_models.api_key`、`api_key_models.api_key`（绑定级自备密钥）、
`credentials.secrets_json` 与 `webhooks.secret`。四者适用同一策略。

| 状态 | 行为 |
|------|------|
| 未配置主密钥 | 明文存储。启动时输出告警：`AIGATE_MASTER_KEY is not configured: credential secrets and upstream api keys will be stored in plaintext...`。 |
| 已配置主密钥 | 新写入以 AES-256-GCM 加密，落库形态为 `enc:v1:<base64(nonce‖ciphertext)>`；读取与转发时透明解密为明文发给上游。 |
| 主密钥非法 | 进程拒绝启动并给出原因。 |

**主密钥格式。** 必须解码为恰好 32 字节，形态为 64 位 hex 或 base64（标准或 URL-safe、
带或不带填充均可）。可用如下方式生成：

```bash
openssl rand -hex 32
```

**「配置后新写入加密」。** 开启 `-master-key` 只改变*新增与更新*值的写入方式，**不会**
改写已明文落库的存量行。过渡期转发不受影响：读取端把不带 `enc:v1:` 前缀的值一律按
明文处理（带前缀的则解密）。要把存量行就地加密，执行一次性迁移：

```bash
# 加密存量明文密钥行后退出（不会启动服务）
bin/aigate -db-type mysql \
  -db-dsn '<DSN>' \
  -master-key '<主密钥>' \
  -migrate-encrypt
```

迁移会输出一行汇总：

```text
encryption migration finished: migrated=<n> skipped=<n> failed=<n>
```

- `migrated` —— 本次被加密的明文行数。
- `skipped` —— 已是 `enc:v1:` 密文（或为空）而被跳过的行数。
- `failed` —— 加密失败的行数；迁移不会因单行失败而中断，请查看日志。

迁移是**幂等**的：重复执行会跳过已加密行，不会二次加密。它要求已配置主密钥；未配置
时会报错退出：`-migrate-encrypt requires a master key`。

> **务必备份并妥善保管主密钥。** 没有密钥托管或找回路径：主密钥一旦丢失，所有密文都
> 无法解密、上游调用会失败。请放入密钥管理服务，不要写进仓库或数据库。当前版本不支持
> 换钥——换用新主密钥需要专门的迁移。

## Docker

仓库提供多阶段 `Dockerfile`：先构建静态二进制（关闭 CGO；SQLite 使用纯 Go 驱动），
再拷贝到极简的 `alpine` 运行镜像，以非 root 用户（UID/GID 65532）运行。

```bash
docker build -t aigate .
```

用 SQLite（默认）运行。默认 DSN 为 `/data/aigate.db`，镜像已为非 root 用户预建可写的
`/data`，因此把宿主目录挂载到该处以持久化数据：

```bash
docker run -d --name aigate \
  -p 8080:8080 \
  -v "$PWD/gateway-data:/data" \
  -e AIGATE_INIT_ADMIN_TOKEN='<引导管理员 token>' \
  -e AIGATE_MASTER_KEY='<64 hex 或 base64 主密钥>' \
  aigate
```

说明：

- **端口** —— 镜像暴露 `8080`，按需发布（`-p 8080:8080`）。
- **数据目录** —— SQLite 默认落在容器工作目录下的 `/data/aigate.db`。把 volume 挂到
  `/data` 才能跨重启保留数据库；不挂载则容器重启即丢失全部数据。
- **主密钥** —— 传 `-e AIGATE_MASTER_KEY=…` 启用加密；不传则以明文运行（带启动告警），
  见「密钥加密」。
- **健康检查** —— 镜像声明了 `HEALTHCHECK`，每 30s 通过 `wget` 调用 `GET /healthz`，
  超时 3s、启动期 5s。`docker ps` 会显示 `healthy`/`unhealthy`；健康响应为
  `{"status":"ok"}`。
- **MySQL** —— 覆盖启动命令以指向外部数据库：
  `docker run ... aigate -db-type mysql -db-dsn '<DSN>' -master-key '<主密钥>'`。

## 上游 SSRF 策略

所有出站请求 —— 发往上游模型端点以及发往 webhook 接收端 —— 都会经过 SSRF 防护，
覆盖三个点位：请求前的 URL 校验、拨号时的地址校验（会重新解析 DNS）、以及每一个
重定向目标的校验。

被拒绝的地址分两档：

1. **永久拒绝** —— 与配置无关、始终拒绝，因为放行它们不存在合法用途。包含云元数据
   地址（`169.254.169.254`、`100.100.100.200`、`fd00:ec2::254`）、link-local
   （`169.254.0.0/16`、`fe80::/10`）、未指定地址（`0.0.0.0`、`::`）、多播/广播、
   `0.0.0.0/8`、`192.0.0.0/24`、`240.0.0.0/4`，以及 TEST-NET 网段
   （`192.0.2.0/24`、`198.51.100.0/24`、`203.0.113.0/24`）。
2. **仅严格模式拒绝** —— 默认拒绝，置 `-allow-private-upstreams=true` 后放行。
   包含回环（`127.0.0.0/8`、`::1`）、RFC1918（`10/8`、`172.16/12`、`192.168/16`）、
   CGNAT（`100.64.0.0/10`）、`198.18.0.0/15`、ULA（`fc00::/7`）与
   `2001:db8::/32`。

由于默认是严格模式，解析到私网或回环地址的上游会被拒绝，报 `target address not
allowed`。当模型端点确实在本地或内网时（例如本机的 Ollama/vLLM，或内网自建的模型
服务），请开启该 flag：

```bash
bin/aigate ... -allow-private-upstreams=true
# 或
AIGATE_ALLOW_PRIVATE_UPSTREAMS=true bin/aigate ...
```

放宽该 flag 只会解除严格档；永久拒绝集（含云元数据地址）在任何配置下都保持拦截。
同一策略也作用于 webhook 投递——内网接收端同样需要开启该 flag。

上游请求**不使用环境变量代理**（`HTTP_PROXY` / `HTTPS_PROXY`）：一旦走代理，拨号层
校验的将是代理地址而非真实目标，与「SSRF 全链硬化」冲突。需要代理出口时请使用透明
代理或专用 NAT。

## 运维要点

### 启动与优雅停机

收到 `SIGINT`/`SIGTERM` 后，网关先停止接受新请求，最多等待 10 秒让在途请求完成，
随后依次排空用量写入队列与 webhook 事件队列（同样受该 10 秒预算约束，超时即退出）后退出。停机日志会给出 `usage_dropped` 与
`webhook_dropped`，即因各自队列满而丢弃的条数。两个队列都是异步写入，极端负载下可能
丢失少量条目；网关不承担计费真源职责，webhook 投递也是「尽力而为 + 重试」语义。

### 日志

日志为结构化输出，经 `log/slog` 写到 stderr。启动日志包含监听地址、DB 类型、时区与
SSRF 设置（未配置主密钥时含安全告警）；停机日志包含丢弃的用量与 webhook 条数。请求
链路中的失败（模型解析、上游错误、用量写入失败）与 webhook 投递失败都会连同相关标识
一起记录。

### 首次启动的管理员 token

当不存在管理员且 `-init-admin-token` 为空时，网关会生成一个 token 并**只打印一次**
到 stdout，前缀为 `initial admin token (shown once):`。服务端只存哈希，因此 token
丢失无法找回——请改为通过管理面 API 重新签发。重启网关不会重建或重新打印管理员。

### 请求限制

请求体上限为 1 MiB。HTTP 服务使用 10 秒的请求头读取超时；上游调用使用 5 分钟客户端
超时（流式 `/v1/messages` 请求为 30 分钟）。

### 健康检查

`GET /healthz` 返回 `{"status":"ok"}`，免鉴权，可用于存活探针。Docker 镜像即用它作为
`HEALTHCHECK`。

### webhook 投递

webhook 订阅位于 `/admin/webhooks`；每条投递最多尝试 **3 次**（两次尝试之间分别等待
1s 与 5s），单次超时 10 秒，最终记录为 `success` 或 `failed`。信封、请求头与签名口径
见 API 参考的「webhook 事件交付」附录。

排查投递请使用可查询的投递记录：

```bash
# 查看某订阅的近期投递（分页、按时间倒序）
curl -sS "http://127.0.0.1:8080/admin/webhooks/1/deliveries?page=1&page_size=50" \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

每条记录带 `status`、`attempts`、`last_status` 与 `last_error`，可区分「接收端不可达」
（`network_unreachable`、`last_status` 为 0）与「接收端返回错误」（`last_status` 5xx）。
由于投递是异步的，接收端缓慢或不可用不会影响数据面：事件队列（`-webhook-buffer`）满时
事件被丢弃并计数，而非阻塞调用方。

> **暂无保留策略。** 投递记录不会被网关裁剪或过期，会无限累积。当前没有内置 TTL 或
> 清理任务，请自行规划归档或删除旧的 `webhook_deliveries` 行。

## 构建与测试

```bash
make build      # 构建 bin/aigate
make test       # 单测
make vet        # go vet
make test-e2e   # 端到端冒烟（需要本地 MySQL）
```

`make test-e2e` 会重置专用测试库、导入 `sql/init.sql`、构建二进制，并用 mock 上游
驱动它。前置条件与环境变量覆盖见 `test/e2e/README.md`。
