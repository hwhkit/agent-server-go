# mota-agent-server-go

基于 [hwhkit-go](https://github.com/hwhkit/hwhkit-go)(`RunAndServe` / `Application` 新 API)构建的
MOTA <-> 自托管 agent 桥接服务,与 [`mota-agent-server-rs`](../mota-agent-server-rs) /
[`mota-agent-server-py`](../mota-agent-server-py) 同构,一个服务同时覆盖协议文档中的两种接入方式:

| 接入方式 | 方向 | 本服务的实现 |
|---|---|---|
| **Push** | MOTA 调用你的接口 | `POST <endpoint>` 接收任务,校验 `X-MoTA-Signature`,处理并回传结果 |
| **Pull** | 你的 worker 轮询队列 | `claim`(长轮询)→ 处理 → `complete` 回传;期间 `renew` 续租;`heartbeat` 上报存活 |

hwhkit-go 开箱即用能力: `/health`、`/health/ready`、`/metrics`、`/version`、`/info`、
标准中间件束(request-id、panic-recover → RFC 7807、CORS、gzip/br、body limit、timeout)、
SIGTERM 优雅退出(先广播 `ShutdownToken` 再 drain)。

## 单体多 Agent 管理平台

Go 版以 Rust 实现为行为基准，现已在同一进程内增加：

- `GET /admin` 与 `/api/admin/*` 管理面（HttpOnly Session、CSRF、登录退避）
- `POST /agent/{slug}` 动态 Agent 网关与热更新 Registry
- PostgreSQL Agent 配置、Provider 配置、run、nonce、幂等 lease/缓存与管理 Session
- AES-256-GCM 加密的 per-Agent Signing Secret，管理 API 永不回显明文
- 智能体中心：本地系统与远端托管智能体分组，配置完成后才能分配给岗位
- 本地真实 Harness：Codex、Pi、OpenCode、Qwen Code、Gemini CLI、OpenHands、Claude Code、DeepSeek Harness
- 远端托管运行时：OpenAI Agents API 与 Claude Managed Agents Session/SSE
- Agent/全局双层并发门、有界 admission、运行日志和 Provider 热更新
- 账号管理：数据库多账号登录、`admin/user` 角色、停用/改密踢下线、管理员保护与审计日志
- Rust 同款 HMAC v1 canonical、防重放 5 分钟窗口和兼容 `X-MoTA-Signature`

```bash
export PLATFORM__ENABLED=true
export ADMIN__PASSWORD='<强管理员密码>'
export ADMIN__SESSION_SECRET='<至少 32 字节的稳定随机值>'
export PLATFORM__PUBLIC_BASE_URL='http://127.0.0.1:8080'
export DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/mota_agent'
```

浏览器打开 `http://127.0.0.1:8080/admin`。Provider 密钥与 MCP Authorization 会作为
整份配置使用平台主密钥加密后写入 PostgreSQL，管理 API 只返回“已配置”状态，不回显明文。
配置保存后立即对新任务生效；正在运行的任务继续使用启动时的配置快照。

### 账号与审计

启动时 `ADMIN__USERNAME` 和管理员密码会幂等迁移为数据库中的初始 `admin` 账号；
之后可在“账号管理”页面创建管理员或普通用户。管理员可以管理全部账号并查看全部
审计记录，普通用户只能查看自己的审计。修改密码、停用或删除账号会立即撤销该账号
的现有会话；系统禁止当前管理员降级、停用或删除自己，并保护至少一个已启用管理员。
账号密码使用 Argon2id 存储，用户列表与审计 API 不返回密码哈希。

OpenAI 托管智能体启用示例：

```bash
export OPENAI_AGENTS__ENABLED=true
export OPENAI_AGENTS__API_KEY='<OpenAI API key>'
```

Claude 托管智能体启用示例：

```bash
export CLAUDE_MANAGED__ENABLED=true
export CLAUDE_MANAGED__API_KEY='<Anthropic API key>'
export CLAUDE_MANAGED__AGENT_ID='agent_...'
export CLAUDE_MANAGED__ENVIRONMENT_ID='env_...'
```

v1 canonical 与 Rust/Python 相同：

```text
v1\nPOST\n/agent/{slug}\n{unix_timestamp}\n{nonce}\n{run_id}\n{idempotency_key}\n{sha256_hex(body)}
```

## 环境要求

- **Go >= 1.25**
- hwhkit-go 子模块的 go.mod 使用 workspace 内相对 `replace`(未随 tag 发布),
  因此依赖本地源码(`.hwhkit-go-src/`,由仓库根 sparse-checkout 生成)。
  `git pull` 后如源码缺失,重新执行:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/hwhkit/hwhkit-go .hwhkit-go-src
cd .hwhkit-go-src && git sparse-checkout set buildinfo config core observability hwhkit
```

## 目录结构

```
mota-agent-server-go/
├── go.mod               # require + replace(指向 ../.hwhkit-go-src)
├── main.go              # Application impl + hwhkit.Run + Pull worker + production.Serve
└── mota/
    ├── config.go        # MotaConfig(MOTA_* 环境变量 + 启动校验)
    ├── signature.go     # HMAC-SHA256(hex | base64,常数时间比较)
    ├── processor.go     # Processor 接口 + PlaceholderProcessor 占位实现
    ├── push.go          # Push handler(chi route)
    ├── worker.go        # PullWorker(claim/complete/renew/heartbeat)
	├── platform_*.go     # 多智能体配置、PostgreSQL、智能体中心、管理面与网关
	├── executor_*        # 本地 CLI、网关、OpenAI/Claude 托管执行器
    ├── types.go         # 协议数据结构
    └── *_test.go        # 单元 + 集成测试
```

## 快速开始

```bash
export PATH="$PWD/.tools/go1.23.12/bin:$PATH"
cd mota-agent-server-go
go build -o mota-agent-server .

# 注入 MOTA 平台签发的签名密钥(占位值 change-me 会被启动校验拒绝,防伪造)
export MOTA_SIGNING_SECRET="<平台签发的 signing secret>"
MOTA_SIGNING_SECRET=$MOTA_SIGNING_SECRET ./mota-agent-server
```

验证:

```bash
curl -s http://127.0.0.1:8080/health          # {"status":"healthy"}

BODY='{"taskBrief":"assess AAPL","params":{"symbols":["AAPL"]}}'
SIG=$(python3 -c "import hmac,hashlib,sys,os; print(hmac.new(os.environ['MOTA_SIGNING_SECRET'].encode(), sys.argv[1].encode(), hashlib.sha256).hexdigest())" "$BODY")
curl -s -X POST http://127.0.0.1:8080/mota/push \
  -H 'Content-Type: application/json' \
  -H "X-MoTA-Signature: $SIG" \
  -H 'X-MoTA-Run-Id: run-1' \
  -d "$BODY"
# -> {"assessments":[...],"summary":"...","extensions":{...}}
```

## Push 接入

MOTA 用 HTTPS POST 把任务推送到登记 endpoint,本服务从响应体取结果。

- 请求: `POST <your endpoint>`(默认 `/mota/push`,可用 `MOTA_PUSH_PATH` 改)
  - 头: `X-MoTA-Signature: <hmac-sha256(body, signing secret)>`、`X-MoTA-Run-Id`
  - 体: `{ "taskBrief": …, "params": { symbols, financial_analysis, company_announcement, consensus_estimate, research_report } }`
- 响应(200): `{ "assessments": …, "summary": …, "extensions": … }`
- 签名缺失 / 不匹配一律 `401`(RFC 7807 problem+json);body 非法 JSON 返回 `422`;
  处理器异常返回 `500`(内部细节只记日志,不回显)。

签名编码默认为 **hex 小写**;若平台签发 base64,设 `MOTA_SIGNATURE_ENCODING=base64`。
校验用 `crypto/subtle.ConstantTimeCompare` 常数时间比较。

## Pull 接入

worker 随进程启动(main 里在 `production.Serve` 前 `Start()`,退出时 `Stop()`),
轮询由平台签发的队列地址,`Authorization: Bearer <bind token>` 认证。

- `claim`: 长轮询 `POST {queue}/claim?workerId=<id>`,HTTP 读超时由 `MOTA_CLAIM_TIMEOUT_SECS`
  控制(默认 60s,校验强制 >= 35s);收到 `204` 立即重新 claim。
- `complete`: 处理完 `POST {queue}/complete`(带 `X-MoTA-Run-ID`),回执
  `{ code, accepted, runStatus, runMsg }`;仅 `accepted=true` 视为被采纳。
- `renew`: 任务处理期间每 `MOTA_RENEW_INTERVAL_SECS` 续租一次;收到 `409 BOUND_CLAIM_CONFLICT`
  立即放弃该任务(租约已被抢走)。
- `heartbeat`: 独立 goroutine 按回执 `heartbeatIntervalSeconds`(默认 60s)上报存活;
  心跳与 claim **各跑一条循环**(协议要求)。

启用 Pull: 设 `MOTA_PULL_ENABLED=true` 并配置 `MOTA_QUEUE_URL` / `MOTA_BIND_TOKEN` / `MOTA_WORKER_ID`。

## 配置

全部来自环境变量 `MOTA_*`(hwhkit-go 的 `AppConfig` 是固定结构,不支持自定义段)。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `MOTA_PUSH_ENABLED` | `true` | 是否挂载 Push endpoint |
| `MOTA_PUSH_PATH` | `/mota/push` | Push endpoint 路径 |
| `MOTA_SIGNING_SECRET` | — | HMAC-SHA256 签名密钥;空或占位 `change-me` 拒绝启动 |
| `MOTA_SIGNATURE_ENCODING` | `hex` | `hex` 或 `base64` |
| `MOTA_PULL_ENABLED` | `false` | 是否启动 Pull worker |
| `MOTA_QUEUE_URL` | — | 绑定后由平台签发的队列地址 |
| `MOTA_BIND_TOKEN` | — | 绑定令牌 |
| `MOTA_WORKER_ID` | — | worker 标识 |
| `MOTA_CLAIM_TIMEOUT_SECS` | `60` | claim 读超时,强制 >= 35 |
| `MOTA_HEARTBEAT_INTERVAL_SECS` | `60` | 心跳间隔(回执可覆盖) |
| `MOTA_RENEW_INTERVAL_SECS` | `30` | 续租间隔 |
| `MOTA_TASK_TIMEOUT_SECS` | `600` | 单任务处理超时,超时放弃 |

启动校验: Push 启用但签名密钥为空/占位、Pull 启用但缺队列/令牌/workerId、claim 超时 < 35 均拒绝启动。

## 接入真实业务(替换占位处理器)

`Processor` 是唯一业务扩展点,Push 与 Pull 共用。在 `main.go` 替换:

```go
// mota/processor.go
type Processor interface {
    Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error)
}

// main.go
type MyAnalysisProcessor struct{ /* ... */ }
func (MyAnalysisProcessor) Process(ctx context.Context, task *mota.TaskContext) (*mota.ProcessedOutput, error) {
    // 用 task.Params(symbols / financial_analysis / ...) 做真实分析
    return &mota.ProcessedOutput{Assessments: ..., Summary: "...", Extensions: ...}, nil
}

processor := MyAnalysisProcessor{}   // 替换 PlaceholderProcessor{}
```

## 测试

```bash
go test ./...        # 协议桥 + 智能体中心 + 执行器 + 平台安全/管理/幂等集成
go vet ./...
```

## 生产部署注意

- 签名密钥、bind token 一律通过环境变量注入,不要提交明文到仓库。
- Push endpoint 必须在 MOTA 平台登记,且能被公网 HTTPS 访问。
- 连续 15 分钟无心跳会判离线,部署时确保进程存活(如 systemd `Restart=always`)。
- hwhkit-go 尚未发布可安装的 tag(子模块 go.mod 带本地 replace),部署需携带 `.hwhkit-go-src/` 源码。
