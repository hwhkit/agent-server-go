# Agent Pull Connector API（远端智能体拉取接入）v1

平台侧托管的 Pull 端点:让部署在任意机器上的远端智能体(Runtime Connector)通过
**纯出站 HTTPS** 认领并执行平台任务。远端不需要固定 IP、公网入口或入站端口,
NAT/容器/家用网络均可接入。

设计原则:操作语义与已验证的主 Pull 协议(worker claim/renew/complete)严格一致(长轮询 claim、租约 renew、
幂等 complete、heartbeat 存活),但载荷是平台原生形态(以 agent slug 为主键),
鉴权用管理台签发的连接器令牌而非 worker bind token。

## 0. 总则

| 项 | 约定 |
|---|---|
| Base URL | `{public_base_url}/agent-api/v1` |
| 传输 | HTTPS(生产强制);`Content-Type: application/json; charset=utf-8` |
| 鉴权 | 全部端点要求 `Authorization: Bearer <connector_token>` |
| 令牌 | 管理台"远端智能体"签发,前缀 `act_`,按连接器独立、可吊销,服务端只存哈希;令牌绑定允许认领的 slug 集合(scope)。轮换有宽限期(§9) |
| workerId | 连接器自报的稳定标识(2–64 字符,`[a-z0-9-]`,建议 `hostname-pid`);同一令牌下必须唯一 |
| 时间 | 全部 RFC3339 UTC;响应带 `serverTime` 供时钟漂移校正 |
| 体积上限 | claim 响应与 complete 请求体 ≤ 4 MiB(解压后);其余请求/响应 ≤ 64 KiB;支持 `Content-Encoding: gzip` |
| 幂等 | complete 按 runId 幂等;其余端点天然幂等 |
| **交付语义** | **at-least-once**:租约过期重派与原执行者迟到的 complete 并存,同一任务可能被执行多次。分析型任务天然安全;**有副作用的连接器(下单、写库)必须自行幂等**(§5.1) |

## 1. POST /claim — 长轮询认领任务

连接器的主动作。服务端在等待窗口内持有请求,有任务立即返回;无任务到窗口耗尽
返回 204。连接器收到 204 后应**立即**重新 claim,不额外 sleep。

请求:

```json
POST /agent-api/v1/claim
{
  "workerId": "gpu-box-03",
  "maxWaitSeconds": 30,
  "inFlight": 1,
  "capacity": 4,
  "preferredSlugs": ["fundamental-analyst", "tech-analyst"]
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `workerId` | 是 | 连接器实例标识 |
| `maxWaitSeconds` | 否 | 长轮询窗口,1–35,默认 30;超时上限与服务端 `claim_timeout` 取小 |
| `inFlight` / `capacity` | 否 | 当前在途/总并发,用于容量记账与 429 判定 |
| `preferredSlugs` | 否 | 只认领这些 slug;缺省为令牌 scope 内全部 |

响应 `200`(认领成功):

```json
{
  "schemaVersion": "agent-api.claim.v1",
  "runId": "run_9f41c2",
  "agentSlug": "fundamental-analyst",
  "agentRevision": 7,
  "executorKind": "remote-pull",
  "input": {
    "taskBrief": "…",
    "params": { },
    "globalParameters": { },
    "upstreamPayload": null,
    "runtimeContext": { }
  },
  "attempts": 1,
  "executionDeadline": "2026-11-04T08:15:00Z",
  "leaseExpireAt": "2026-11-04T08:00:40Z",
  "leaseSeconds": 90,
  "renewAfterSeconds": 30
}
```

| 字段 | 说明 |
|---|---|
| `runId` | 全局运行 ID;renew/complete/审计共用 |
| `agentSlug` / `agentRevision` | 认领的岗位与其配置版本 |
| `input` | 任务载荷;字段名与 worker claim 保持同构,便于复用既有执行器解析 |
| `attempts` | 第几次派发(1 起)。**连接器据此感知重试**,做副作用幂等 |
| `executionDeadline` | 服务端墙钟上限(首次派发时刻 + Agent `timeout_secs`);此后续租必被拒(§6.2) |
| `leaseExpireAt` | 租约绝对到期时刻;此后任务可被重新派发 |
| `renewAfterSeconds` | 服务端建议的续租间隔(= leaseSeconds/3,默认 30) |

其他状态码:

| 码 | 语义 | 连接器动作 |
|---|---|---|
| `204` | 窗口内无任务 | 立即重新 claim |
| `401` | 令牌无效/被吊销 | 停止循环,告警 |
| `403` | 令牌 scope 不含任何可用 slug | 停止循环,检查配置 |
| `409 {"code":"WORKER_LEASE_ACTIVE","runId":…}` | **同 workerId 仍持有活跃租约**(疑似双实例)。仅在有活跃租约时返回;租约结束后同 ID 可正常认领,重启不会被锁死 | 换 workerId,或等租约自然过期 |
| `429` | 已达声明并发(capacity)或全局在途租约上限 | 按 `Retry-After` 退避 |
| `5xx`/网络错误 | 服务端不可用 | 1s 退避重试 |

`204` 响应携带诊断头,便于连接器区分"真空闲"与"配置问题":

```http
204
X-Agent-Api-Reason: empty            # 队列为空(正常)
X-Agent-Api-Reason: no-scope-match   # 令牌 scope 与任何队列不交叉
X-Agent-Api-Reason: rate-limited     # 并发已满(理论上 429 优先,兜底)
```

## 2. POST /renew — 续租

任务执行期间周期调用(默认 30s 一次)。租约默认 90s,续租成功后重置为完整时长。

```json
POST /agent-api/v1/renew
X-Agent-Run-ID: run_9f41c2
```

无请求体。响应 `200`:

```json
{ "abort": false, "leaseExpireAt": "2026-11-04T08:01:50Z", "leaseSeconds": 90 }
```

`abort` 是取消传播通道(§7):`true` 表示平台已请求取消,连接器应立即停止本地
执行并停止续租;可选用 `complete(status="aborted")` 回执,不回执也可以——
租约到期后服务端自行落终态 `canceled`。

| 码 | 语义 | 连接器动作 |
|---|---|---|
| `200` | 续租成功(`abort` 可能为 true) | abort=false 继续;abort=true 停止 |
| `409 {"code":"LEASE_LOST"}` | 租约已过期、任务已被重派 | **立即放弃本地执行**(结果不会被接受),回到 claim |
| `409 {"code":"TASK_DEADLINE_EXCEEDED"}` | 超过 `executionDeadline`(§6.2) | 同上,放弃并回 claim |
| `404` | runId 未知/已终态 | 放弃,回到 claim |

## 3. POST /complete — 回传结果(幂等)

```json
POST /agent-api/v1/complete
X-Agent-Run-ID: run_9f41c2

{
  "status": "succeeded",
  "payload": {
    "summary": "…",
    "assessments": null,
    "extensions": null,
    "evidenceRefs": null
  },
  "usage": { "inputTokens": 8210, "outputTokens": 1204, "totalTokens": 9414 },
  "metrics": { "durationMs": 41200 }
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `status` | 是 | `succeeded` \| `failed` \| `aborted` |
| `payload` | succeeded 时必填 | 结果文档,结构与 Push V2 信封的 `payload_result` 同构 |
| `error` | failed 时必填 | `{code, message}`;message 不得包含凭据或供应商原始响应 |
| `usage` | 是(除 aborted) | 计量必须回传;拿不到真实值时回兜底最小值(对齐 RULE-BND-001:`totalTokens > 0`) |
| `metrics` | 否 | 连接器侧耗时 |

响应 `200`:

```json
{ "accepted": true, "runStatus": "completed", "runMsg": null }
```

幂等与冲突规则:

- 同一 runId、同一请求体重复 complete → `accepted: true`(幂等重放)。
- 同一 runId、不同请求体 → `409 {"code":"RESULT_CONFLICT"}`,以首次为准。
- 租约过期后到达 → `accepted: false, runStatus: "superseded"`;结果仅归档,
  任务已按重试策略重新派发。
- `aborted` 后到达的其他状态 → `accepted: false, runStatus: "canceled"`。

## 4. POST /heartbeat — 存活与容量上报

独立于 claim 循环,按服务端回执间隔运行(默认 60s)。首次 heartbeat 即完成注册,
capabilities 变化时随时重报。

```json
POST /agent-api/v1/heartbeat
{
  "workerId": "gpu-box-03",
  "inFlight": 1,
  "capacity": 4,
  "capabilities": { "slugs": ["fundamental-analyst"], "version": "1.0.0" }
}
```

响应 `200`:

```json
{
  "heartbeatIntervalSeconds": 60,
  "renewIntervalSeconds": 30,
  "serverTime": "2026-11-04T08:02:10Z",
  "queueDepth": 3
}
```

服务端可通过 `heartbeatIntervalSeconds` 下发新间隔(自适应降频),连接器必须
遵循(上限 3600s)。连接器超过 3 个心跳间隔未上报即标记离线:容量记账清零、
管理台显示离线,在途任务等租约自然过期后重派(不主动抢,§6.3)。

## 5. 双重执行与幂等契约

### 5.1 at-least-once 是契约,不是缺陷

租约过期重派 + 原 worker 网络分区后恢复,必然存在窗口内双执行。平台侧不承诺
exactly-once,做三件事把风险显性化:

1. claim 响应携带 `attempts`,连接器知道自己是第几棒;
2. runs 审计记录**每一次派发**(workerId、派发时刻、结局),双执行可追溯;
3. 副作用防护是连接器责任:下单/写库类动作用业务幂等键(attempts+runId 派生)
   在效果端去重。

### 5.2 响应路由矩阵(各入口如何拿到结果)

远端执行的默认形态是**异步**:任务受理即返回受理凭证,结果落 runs 存储。
同步语义通过**有界等待**提供,任何入口都不无限 hold 连接。

| 入口 | remote-pull Agent 的行为 |
|---|---|
| 动态 `/agent/{slug}`(默认) | `202 Accepted` + `{runId, resultUrl, queueTimeoutSeconds}`;调用方轮询 `GET /agent/{slug}/runs/{runId}`(200 结果 / 202 进行中 / 410 已过期) |
| 动态 `/agent/{slug}`(带 `X-Agent-Wait: <secs>`) | 有界同步 hold:窗口(≤ `AGENT_API__SYNC_HOLD_MAX_SECONDS`,默认 300)内完成 → 就地返回结果信封;超窗 → 降级为与默认相同的 202 响应。**调用方断连不取消任务**,结果照常落库,审计标注 `caller_disconnected` |
| 平台 push | **路由可达 remote-pull Agent 时强制 async**:启动校验要求 `IR__PUSH_MODE=async`(否则启动失败,防呆);运行期兜底——sync 绑定的任务误路由到 remote-pull 时,按有界 hold 等待,超窗返回同步失败信封(沿用现有 502 语义),绝不静默丢弃 |
| A2A `message/send` | 立即返回 `working` 态 task + taskId,调用方 `tasks/get` 轮询(协议标准形态);若调用方在任务上配置了 push notification webhook,终态时平台照常回调(方向是平台→调用方的 webhook,调用方自己保证了可达性) |

## 6. 时间边界:三级时限与状态机

### 6.1 队列 TTL(入队 → 首次派发)

无连接器在线时任务不能无限堆积。`AGENT_API__QUEUE_TTL_SECONDS`(默认 3600)
耗尽 → 终态 `failed(queue_timeout)`,按入口语义回执(callback / 轮询 410)。

### 6.2 执行墙钟(首次派发 → 终态)

`executionDeadline = 首次派发时刻 + Agent 的 timeout_secs`(复用每 Agent
既有超时配置,不新增概念)。到期后:

- renew → `409 TASK_DEADLINE_EXCEEDED`;
- 租约过期重派判定为不可重派(§6.3);
- 任务终态 `failed(task_timeout)`。

没有这条,卡死的连接器可以无限续租,永久占用每 Agent 执行槽。

### 6.3 状态机(含重派判定)

```text
queued ──claim──▶ leased ──complete──▶ completed
  ▲                 │ │ │
  │                 │ │ └─complete(aborted)──▶ canceled
  │   lease 过期    │ └───complete(failed)───▶ failed
  └─────────────────┘
   重派条件(同时满足):
     attempts < AGENT_API__MAX_ATTEMPTS(默认 3)
     AND now() < executionDeadline        ← 墙钟兜底
     AND 无取消请求
   否则终态:failed(lease_expired) / failed(task_timeout) / canceled
```

- 派发顺序:每 slug 一个队列,`SELECT … FOR UPDATE SKIP LOCKED`,无全局锁。
- 入队占 AdmissionSlots(每 Agent 执行+队列容量,原有闸门照常);
- **remote-pull 执行不消耗全局 GlobalExecution 并发闸**(§6.4)。

### 6.4 并发预算:本机与远端分账

全局并发闸保护的对象是**本机资源**(CLI 子进程的 CPU/内存);远端执行两者都
不消耗,不应与本机抢同一个静态预算——否则 16 个全局槽被分钟级远端任务吃满,
本机快速 Agent 全部饿死。因此:

| 预算 | 对象 | 上限 |
|---|---|---|
| GlobalExecution | 本机执行器(原有,不变) | `platform.global_max_concurrency`(默认 16) |
| 每 Agent 执行槽 | 本机 + 远端共用(原有,不变) | Agent `max_concurrency` |
| 全局在途租约 | 仅 remote-pull | `AGENT_API__MAX_OUTSTANDING_LEASES`(默认 64),保护平台侧状态量(任务行、连接、内存)而非远端算力 |

## 7. 取消传播

取消源(三类):管理台删除/终止运行(`DELETE /api/admin/runs/{id}`)、Agent
被停用时的在途策略、队列人工清空。传播路径统一走 **renew 回执的 `abort` 位**,
不新增端点——连接器最迟一个续租间隔(默认 30s)感知:

1. 任一取消源置位 `abort_requested`;
2. 连接器 renew 收到 `abort:true` → 停止本地执行 → 可选 `complete(status="aborted")`;
3. 连接器无响应时,租约到期后服务端直接落 `canceled`(不重派)。

调用方**断连不是取消源**(见 §5.2)——HTTP 调用方断连只影响结果交付方式,
不浪费已付出的远端执行。

## 8. 持久性与重启(设计依据)

队列、租约、重试计数全部落在 PostgreSQL——这是选 PG 做队列的核心理由:

- 平台重启/发版:队列与租约原样存活,长轮询连接断掉只是让连接器重试(1s
  退避),无内存队列恢复问题;
- 租约判定全部以服务端时间为准(连接器只保证"到期前发出 renew"),
  `serverTime` 回执用于连接器校正本地时钟漂移;
- at-least-once 语义在重启前后一致。

## 9. 令牌生命周期

- 签发:管理台"远端智能体"为每个连接器签发 `act_` 令牌,明文仅签发时展示一次,
  服务端存哈希;
- 轮换:签发新令牌可带宽限期——旧令牌在 `AGENT_API__TOKEN_ROTATION_GRACE`
  (默认 24h)内继续有效,宽限期内连接器完成切换;到期自动失效;
- 吊销:立即失效(不宽限),在途任务按租约超时自然回收;
- 可选加固:令牌绑定出口 IP 允许列表(连接器有稳定出网 IP 时启用;NAT 场景
  留空)。

## 10. 存储(参考)

PostgreSQL 新表 `agent_pull_tasks`:

| 列 | 说明 |
|---|---|
| `run_id` | PK,关联 runs 审计表 |
| `agent_slug`, `agent_revision` | 认领时快照 |
| `payload` jsonb | claim 响应中的 input |
| `state` | `queued` / `leased` / `completed` / `failed` / `canceled` / `superseded` |
| `lease_owner`, `lease_expires_at` | 当前持有者与到期时刻 |
| `attempts`, `first_delivered_at`, `execution_deadline`, `expires_at` | 派发计数、墙钟上限、队列 TTL |
| `abort_requested` | 取消位 |
| `delivered_at`, `completed_at` | 时间线 |
| `result` jsonb | 首次接受的 complete 结果 |

`state='queued'` 上建部分索引供 SKIP LOCKED 弹出;`lease_expires_at` 与
`expires_at` 上建索引供过期回收扫描。派发历史(每次 attempts 的 workerId 与
结局)记入 runs 审计事件,支撑 §5.1 的双执行可追溯。

辅助表 `agent_pull_workers`:`worker_id`, `connector_id`, `last_heartbeat_at`,
`capacity`, `in_flight`, `capabilities` jsonb, `online`——管理台连接器状态
卡片(在线/容量/在途/claim 延迟/租约过期计数)的数据源。

## 11. 配置(环境变量)

| 变量 | 默认 | 说明 |
|---|---|---|
| `AGENT_API__ENABLED` | `false` | 开关;关闭时路由不挂载 |
| `AGENT_API__LEASE_SECONDS` | `90` | 租约时长,≥ 3 × renew 间隔 |
| `AGENT_API__MAX_ATTEMPTS` | `3` | 含首次的总派发次数 |
| `AGENT_API__CLAIM_MAX_WAIT_SECONDS` | `30` | 长轮询窗口上限 |
| `AGENT_API__QUEUE_TTL_SECONDS` | `3600` | 入队到首派的排队时限 |
| `AGENT_API__SYNC_HOLD_MAX_SECONDS` | `300` | 同步入口的有界等待上限 |
| `AGENT_API__MAX_OUTSTANDING_LEASES` | `64` | 远端在途租约全局上限 |
| `AGENT_API__WORKER_OFFLINE_AFTER_MISSES` | `3` | 心跳缺失多少次判离线 |
| `AGENT_API__TOKEN_ROTATION_GRACE` | `86400` | 令牌轮换宽限(秒) |

## 12. 连接器实现要点(任意语言,~150 行)

1. 两条独立循环:claim(串行认领,一次一个任务)+ heartbeat。
2. 收到任务后:`执行前先续一次租`,处理期间按 `renewAfterSeconds` 续租;
   409(任一码)立即放弃本地工作;`abort:true` 立即停止。
3. complete 失败(网络/5xx)按 1s 退避重试——幂等保证重试安全;
   `accepted:false` 是终局,不得重试。
4. 时钟:以 `serverTime - 本地时间` 为漂移修正,租约判定以服务端为准,
   本地只需保证"到期前发出 renew"。
5. 副作用幂等:用 `runId + attempts` 派生幂等键,在效果端去重(§5.1)。
6. 超过任务墙钟(`executionDeadline`)主动 `complete(failed)` 而不是弃管。
7. 网络环境:尊重 `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` 环境变量;
   大载荷(> 256 KiB)启用 `Content-Encoding: gzip`。
