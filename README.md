# mota-agent-server-go

Go 实现的 MOTA 自托管 Agent 桥接服务。一个进程可同时提供：

- Push：MOTA 调用 `POST /mota/push`，服务同步返回或异步回调结果；
- Pull：worker 通过 `claim → renew → complete` 消费平台队列，并独立上报心跳；
- 动态 Agent：`POST /agent/{slug}` 与 `/agent-center-go/{slug}`；
- 管理台：`GET /admin`，包含 Provider、岗位职能体、运行、账号与审计管理；
- A2A v1.0：默认关闭，启用后暴露 Agent Card 与同步 `SendMessage` / `GetTask`；
  当前不声明尚未实现的流式能力。

HTTP 服务基于
[hwhkit-go](https://github.com/hwhkit/hwhkit-go)，包含健康检查、指标、请求 ID、
panic recovery、请求体限制、超时和优雅退出。

## 当前能力与边界

- PostgreSQL 保存 Agent、Provider、运行、审计、会话、nonce 和幂等状态；
- Agent 与 Provider 密钥使用 AES-256-GCM 加密，管理 API 不回显明文；
- 管理账号支持 `admin` / `user`，密码使用 Argon2id；
- Provider 保存后对后续任务热更新；运行中的任务继续使用启动时快照；
- Provider API 返回 `revision`，PUT 可用 `If-Match` 防止并发覆盖；连接同一数据库的
  多实例通过版本轮询最终收敛；
- Agent 更新同样使用 revision/`If-Match`，CRUD 在事务中递增全局注册表版本；多实例
  会自动收敛停用、删除与签名密钥轮换，运行中的任务继续使用已取得的旧快照；
- Agent 与全局并发限制是**单进程**限制，多副本的总容量需要由外层调度控制；
- 异步 callback 只有收到 `HTTP 200 + code=OK + accepted=true` 才算投递成功；
  明确拒绝、确定投递失败与无法确认的回执分别记录，不把不确定结果当作可安全重试；
- 动态 Agent callback 目前不是持久化 outbox。服务在返回 202 后崩溃仍可能留下需要按
  原 `runId` 核对的结果窗口；相同幂等请求可能持续重放 202，并不会自动恢复后台任务。

更完整的运维边界见 [维护说明](doc/MAINTENANCE.md) 和
[生产协议](doc/PUSH-PRODUCTION.md)。

## 环境要求

- Go 1.26.8 工具链（模块最低语言版本为 1.26）；
- PostgreSQL 15、16 或 17；
- 构建期所需的 hwhkit-go 固定源码；
- 可选的本地 Agent CLI（Codex、Pi、OpenCode、Qwen Code、Gemini CLI、
  OpenHands、Claude Code、DeepSeek Harness）。

当前 hwhkit-go 模块仍通过 `go.mod` 的 sibling `replace` 引用
`../.hwhkit-go-src/*`。首次检出后运行：

```bash
make bootstrap-hwhkit
```

该命令会把固定版本检出到仓库同级的 `.hwhkit-go-src`。手工执行时也必须在
`mota-agent-server-go` 的**父目录**创建该目录，不能克隆到本仓库内部。

## 快速开始

先准备一个专用 PostgreSQL 数据库，再从模板创建本地环境文件：

```bash
cp .env.example .env.dev
```

至少设置：

```bash
MOTA__SIGNING_SECRET='<平台签发的 mbt_ 密钥>'
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/mota_agent'
# 只验证协议桥时关闭管理平台；启用管理平台则按下文补齐管理员凭据。
PLATFORM__ENABLED=false
```

不要把模板中的说明文字当作密钥。推荐生成管理 Session Secret：

```bash
openssl rand -base64 48
```

构建和启动：

```bash
make bootstrap-hwhkit
make build
set -a; . ./.env.dev; set +a
./dist/mota-agent-server
```

验证健康状态：

```bash
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/health/ready
```

最小 Push 请求必须包含 run ID、幂等键和完整 bound-task v1 信封：

```bash
RUN_ID='run-1'
IDEMPOTENCY_KEY='delivery-1'
BODY='{"schemaVersion":"1.0","runId":"run-1","agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST","templateInputData":[],"taskBrief":{},"callbackUrl":"https://platform.example.com/callback"}'
SIG=$(python3 -c 'import hmac,hashlib,os,sys; print(hmac.new(os.environ["MOTA__SIGNING_SECRET"].encode(),sys.argv[1].encode(),hashlib.sha256).hexdigest())' "$BODY")

curl -fsS -X POST http://127.0.0.1:8080/mota/push \
  -H 'Content-Type: application/json' \
  -H "X-MoTA-Run-ID: $RUN_ID" \
  -H "X-MoTA-Idempotency-Key: $IDEMPOTENCY_KEY" \
  -H "X-MoTA-Signature: $SIG" \
  --data-binary "$BODY"
```

## 管理平台

启用管理平台需要显式配置：

```bash
PLATFORM__ENABLED=true
PLATFORM__PUBLIC_BASE_URL='http://127.0.0.1:8080'
ADMIN__USERNAME='admin'
ADMIN__PASSWORD='<独立强密码>'
ADMIN__SESSION_SECRET='<至少 32 字节的稳定随机值>'
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/mota_agent'
```

打开 `http://127.0.0.1:8080/admin`。生产环境还应设置：

- `PLATFORM__PUBLIC_BASE_URL=https://...`；
- `ADMIN__SECURE_COOKIES=true`；
- 独立低权限服务账号与受限的 CLI 工作区；
- 可靠备份 PostgreSQL 和 `ADMIN__SESSION_SECRET`。丢失主密钥将无法解密已保存密钥。

启动时会把 `ADMIN__USERNAME` 与管理员密码幂等迁移为初始管理员。管理员可管理账号、
Provider 和岗位职能体并查看全部审计；普通用户只有只读访问权，且只能查看自己的审计。
停用、删除或修改密码会撤销该账号现有会话，系统会保护最后一个已启用管理员。

### 动态 Agent 签名

v1 canonical 与其他实现一致：

```text
v1\nPOST\n/agent/{slug}\n{unix_timestamp}\n{nonce}\n{run_id}\n{idempotency_key}\n{sha256_hex(body)}
```

外部别名 `/agent-center-go/{slug}` 仍使用 `/agent/{slug}` 参与 canonical 计算。

### Provider 并发更新

Provider 列表中的每项都带 `revision`。管理台会把当前 revision 作为 `If-Match` 发回：

```http
PUT /api/admin/providers/gateway
If-Match: "7"
Content-Type: application/json
X-CSRF-Token: ...
```

过期 revision 返回 `412 stale_provider_revision`，调用方应重新读取后再合并。未发送
`If-Match` 时服务端仍使用数据库 revision 合并有限次数，避免不同 Provider 或共享配置组
并发保存时互相覆盖。Provider 密钥空值表示保留原密钥，不会通过列表 API 回显。

## Push 协议摘要

默认路径为 `/mota/push`，可通过 `MOTA__PUSH_PATH` 修改。

必需请求头：

- `Content-Type: application/json`；
- `X-MoTA-Run-ID`；
- `X-MoTA-Idempotency-Key`；
- `X-MoTA-Signature`。

服务对**原始请求体字节**计算 HMAC-SHA256，再解析 JSON。签名编码默认为小写 hex，
也可设置 `MOTA__SIGNATURE_ENCODING=base64`。

常见状态码：

| 状态 | 含义 |
|---|---|
| 200 | 同步执行成功或同步缓存重放 |
| 202 | 异步任务已由当前进程接收 |
| 400 | JSON 无法解析 |
| 401 | 签名缺失或不匹配 |
| 409 | 幂等冲突或相同请求仍在处理 |
| 415 | Content-Type 不受支持 |
| 422 | 信封、请求头唯一性或字段约束不合法 |
| 502 | 受信通道缺失 FINAL usage 等上游结果问题 |
| 503 | Provider/状态不可用，或 callback 是否已接收无法确认 |
| 504 | 同步执行超时 |

异步 callback 使用签名 v2，每次重试重新签名。网络错误、HTTP 408、429 和 5xx 使用
`1s / 3s / 10s / 30s / 60s` 退避，最多六次。单次请求和回执读取合计限制为 30 秒，
响应体最多读取 64 KiB。详见 [生产协议](doc/PUSH-PRODUCTION.md)。

动态 `/agent/{slug}` 默认只接受覆盖 timestamp、nonce、run ID、幂等键和 body 摘要的
`X-Agent-Signature` v1。旧 `X-MoTA-Signature` 仅在迁移期显式设置
`PLATFORM__ALLOW_LEGACY_BODY_SIGNATURE=true` 后启用；它缺少防重放能力，不应长期使用。

## Pull 协议摘要

设置：

```bash
MOTA__PULL_ENABLED=true
MOTA__QUEUE_URL='https://...'
MOTA__BIND_TOKEN='<平台签发的绑定令牌>'
MOTA__WORKER_ID='worker-1'
```

- `claim` 是长轮询，`MOTA__CLAIM_TIMEOUT_SECS` 不得小于 35；
- 执行期间按 `MOTA__RENEW_INTERVAL_SECS` 续租；
- `complete` 只有 `accepted=true` 才视为平台采纳；
- heartbeat 与 claim 使用独立循环；
- `MOTA__TASK_TIMEOUT_SECS` 限制单任务最长处理时间。

## 配置优先级

配置按以下顺序覆盖：

1. `config/default.toml`；
2. `config/{MOTA_ENV}.toml`（`MOTA_ENV=default` 时跳过）；
3. 环境变量。

空环境变量不会覆盖文件值。完整可用项见 [.env.example](.env.example) 和
[config/default.toml](config/default.toml)。秘密只应由运行环境或秘密管理系统注入，
不要提交 `.env.dev`、生产 TOML、密钥或数据库转储。

## 开发与验证

```bash
make lint                 # gofmt + go vet
make test                 # 非数据库测试
make race                 # race detector
make test-pg              # 需要专用 TEST_DATABASE_URL
make test-deploy-scripts  # 部署脚本模拟回归
make ci                   # 本地综合门禁；PostgreSQL 矩阵由独立任务运行
```

PostgreSQL 测试必须使用可销毁的专用数据库，绝不能指向生产库。缺少
`TEST_DATABASE_URL` 时，数据库测试会跳过，因此发布记录必须明确写出是否真实执行。

当前契约一致性测试还会读取同级 Rust 仓库；standalone 环境缺少该源码时会跳过。
不要把带 skip 的结果描述为完整跨实现验证。

## 部署

```bash
make deploy                  # build + upload + verified restart
# 或分步执行：make deploy-build && make deploy-check && make deploy-upload && make deploy-restart
```

默认构建 Linux ARM64 静态二进制。部署流程会校验 SHA-256、使用唯一暂存文件与远端锁，
原子替换后轮询 `/health/ready` 并核对实际运行进程的二进制哈希；失败时尝试恢复上一版本。
完整变量、回滚边界和 Pi CLI 要求见 [部署手册](doc/DEPLOYMENT.md)。

## 维护与贡献

- [贡献指南](CONTRIBUTING.md)
- [安全策略](SECURITY.md)
- [变更记录](CHANGELOG.md)
- [发布就绪清单](doc/RELEASE-READINESS.md)
- [通信流程](doc/COMMUNICATION-FLOW.md)

本项目按 `MIT OR Apache-2.0` 双许可证发布，参见 [LICENSE](LICENSE)。
