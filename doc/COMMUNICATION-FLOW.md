# 管理台、MOTA 平台与执行器通信流程

本文描述 Go 服务的主要数据流。示例公网前缀 `/center` 由反向代理剥离；服务内部路由
仍是 `/admin`、`/agent/{slug}` 和 `/api/admin/*`。

## 1. 管理台

```text
浏览器
  GET  /admin
  POST /api/admin/login       -> HttpOnly Session Cookie + CSRF token
  GET  /api/admin/session
  GET  /api/admin/providers   -> Provider 配置与 revision（不含密钥）
  GET  /api/admin/agents      -> 岗位职能体
  GET  /api/admin/runs        -> 最近运行
  GET  /api/admin/audit-logs  -> admin 看全部，user 仅看本人
```

写操作同时要求有效 Session 与 `X-CSRF-Token`。Provider、Agent 与账号变更仅允许
`admin`；普通 `user` 保留只读页面和本人审计权限。权限由服务端执行，不能通过直接调用
API 绕过。

Provider 更新流程：

```text
GET Provider + revision
  -> PUT Provider + If-Match
  -> PostgreSQL CAS 保存加密配置并递增共享 revision
  -> 原子发布新的不可变配置快照
  -> 后续任务使用新快照
```

运行中的任务保留启动时快照。连接同一数据库的其他实例通过版本轮询最终收敛；实例间
不共享内存信号量，因此总并发仍取决于副本数。

## 2. 动态 Agent 同步请求

```text
MOTA 平台
  -> POST /agent/{slug}
  -> 验签 / nonce 防重放 / 幂等认领
  -> agent_runs: running
  -> 获取 Agent 与 Provider 配置快照
  -> 本地 CLI、托管 Agent 或 Gateway 执行
  -> 结果归一化与 usage 校验
  -> 同一事务写运行终态与幂等响应
  <- 200 + V2 结果，或稳定的 4xx/5xx 错误
```

外部别名 `/agent-center-go/{slug}` 的签名 canonical 仍使用 `/agent/{slug}`。

## 3. 动态 Agent 异步请求

请求体有非空 `callbackUrl` 时：

```text
MOTA 平台
  -> POST /agent/{slug}
  -> 验签 / 幂等认领 / agent_runs: running
  <- 202

Go 服务后台任务
  -> 执行并生成 V2 结果
  -> callback v2（最多六次有界尝试）
  -> accepted=true
       -> agent_runs: completed
       -> 保存成功通道事件
     accepted=false
       -> agent_runs: failed, callback_rejected
       -> 保存拒绝通道事件
     确定投递失败
       -> agent_runs: failed, callback_delivery_failed
     已发送但没有可信回执
       -> agent_runs: failed, outcome_uncertain
       -> 保存安全错误摘要
```

这条链路当前由进程内后台任务完成，没有 PostgreSQL callback outbox。202 后到终态写入前
若进程退出，可能留下未完成运行；相同幂等请求可能只重放 202 而不恢复后台任务。应按
原 `runId` 核对平台与外部副作用，再决定人工修复或新的授权请求。不能把网络错误简单
解释成“平台肯定没有收到”。

## 4. Push 与 Pull

Push：

```text
POST /mota/push
  -> 原始 body HMAC
  -> PostgreSQL push_idempotency
  -> 同步 200，或异步 202 + callback
```

`/mota/push` 不写 `agent_runs`；其可恢复状态保存在 Push 幂等表。完整信封、错误码和
callback 规则见 [PUSH-PRODUCTION.md](PUSH-PRODUCTION.md)。

Pull：

```text
claim 长轮询 -> 执行
             -> renew 独立续租
             -> complete（accepted=true 才算采纳）
heartbeat 独立循环
```

## 5. 外部接口

### 入站

| 路径 | 用途 | 认证 |
|---|---|---|
| `/mota/push` | bound-task Push | MOTA HMAC + run ID + 幂等键 |
| `/agent/{slug}` | 动态 Agent | MOTA 兼容签名或 v1 时间戳/nonce 签名 |
| `/agent-center-go/{slug}` | 动态 Agent 公网别名 | canonical 仍为 `/agent/{slug}` |
| `/a2a/*` | A2A v1.0（可选） | 派生 Bearer token |
| `/admin`、`/api/admin/*` | 管理面 | Session + 写操作 CSRF + 角色 |
| `/health`、`/health/ready` | 存活与就绪 | 无；生产可由边缘限制 |
| `/metrics` | Prometheus 指标 | 无；生产应限制访问 |

### 出站

| 目标 | 用途 | 敏感数据 |
|---|---|---|
| MOTA callback | 异步结果 | callback v2 签名、可选旧 token |
| MOTA heartbeat / Pull 队列 | 通道状态与任务 | Bearer bind token |
| LiteLLM / OpenAI / Anthropic | 模型与托管 Agent | Provider API key |
| 本地 Agent CLI | 本机执行 | 受限环境变量与工作区 |

带凭据的生产 HTTP 客户端不得跨 origin 跟随重定向。callback 当前没有 DNS/IP 级 SSRF
阻断，生产环境应在入口或出口代理限制目标 origin。
