# Agent Pull Connector API（远端智能体拉取接入）v1

平台侧托管的 Pull 端点:让部署在任意机器上的远端智能体(Runtime Connector)通过
**纯出站 HTTPS** 认领并执行平台任务。远端不需要固定 IP、公网入口或入站端口,
NAT/容器/家用网络均可接入。

设计原则:操作语义与已验证的 MOTA Pull 协议严格一致(长轮询 claim、租约 renew、
幂等 complete、heartbeat 存活),但载荷是平台原生形态(以 agent slug 为主键),
鉴权用管理台签发的连接器令牌而非 MOTA bind token。

## 0. 总则

| 项 | 约定 |
|---|---|
| Base URL | `{public_base_url}/agent-api/v1` |
| 传输 | HTTPS(生产强制);`Content-Type: application/json; charset=utf-8` |
| 鉴权 | 全部端点要求 `Authorization: Bearer <connector_token>` |
| 令牌 | 管理台"远端智能体"签发,前缀 `act_`,按连接器独立、可吊销,服务端只存哈希;令牌绑定允许认领的 slug 集合(scope) |
| workerId | 连接器自报的稳定标识(2–64 字符,`[a-z0-9-]`);同一令牌下必须唯一 |
| 时间 | 全部 RFC3339 UTC;响应带 `serverTime` 供时钟漂移校正 |
| 体积上限 | claim 响应与 complete 请求体 ≤ 4 MiB;其余请求/响应 ≤ 64 KiB |
| 幂等 | complete 按 runId 幂等;其余端点天然幂等 |

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
  "leaseExpireAt": "2026-11-04T08:00:40Z",
  "leaseSeconds": 90,
  "renewAfterSeconds": 30
}
```

| 字段 | 说明 |
|---|---|
| `runId` | 全局运行 ID;renew/complete/审计共用 |
| `agentSlug` / `agentRevision` | 认领的岗位与其配置版本 |
| `input` | 任务载荷;字段名与 MOTA claim 保持同构,便于复用既有执行器解析 |
| `leaseExpireAt` | 租约绝对到期时刻;此后任务可被重新派发 |
| `renewAfterSeconds` | 服务端建议的续租间隔(= leaseSeconds/3,默认 30) |

其他状态码:

| 码 | 语义 | 连接器动作 |
|---|---|---|
| `204` | 窗口内无任务 | 立即重新 claim |
| `401` | 令牌无效/被吊销 | 停止循环,告警 |
| `403` | 令牌 scope 不含任何可用 slug | 停止循环,检查配置 |
| `409` | workerId 已被另一实例使用(租约保护) | 换 workerId 或等待 |
| `429` | 已达声明并发(capacity) | 按 `Retry-After` 退避 |
| `5xx`/网络错误 | 服务端不可用 | 1s 退避重试 |

## 2. POST /renew — 续租

任务执行期间周期调用(默认 30s 一次)。租约默认 90s,续租成功后重置为完整时长。

```json
POST /agent-api/v1/renew
X-Agent-Run-ID: run_9f41c2
```

无请求体。响应 `200`:

```json
{ "leaseExpireAt": "2026-11-04T08:01:50Z", "leaseSeconds": 90 }
```

| 码 | 语义 | 连接器动作 |
|---|---|---|
| `200` | 续租成功 | 继续处理 |
| `409 {"code":"LEASE_LOST"}` | 租约已过期、任务已被重派 | **立即放弃本地执行**(结果不会被接受),回到 claim |
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
| `status` | 是 | `succeeded` \| `failed` |
| `payload` | succeeded 时必填 | 结果文档,结构与 Push V2 信封的 `payload_result` 同构 |
| `error` | failed 时必填 | `{code, message}`;message 不得包含凭据或供应商原始响应 |
| `usage` | 是 | 计量必须回传;拿不到真实值时回兜底最小值(对齐 RULE-BND-001:`totalTokens > 0`) |
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
遵循(上限 3600s)。连接器超过 3 个心跳间隔未上报即标记离线,容量记账清零,
在途任务等租约自然过期后重派。

## 5. 状态机与重试策略(服务端)

```text
queued ──claim──▶ leased ──complete──▶ completed
  ▲                 │ │
  │   lease 过期     │ └─complete(failed)──▶ failed
  └─────────────────┘
        attempts+1;attempts ≥ 3 时不再重派,终态 failed(记 lease_expired)
```

- 派发顺序:每 slug 一个队列,`SELECT … FOR UPDATE SKIP LOCKED`,无全局锁。
- 并发闸门沿用调度器:入队占 AdmissionSlots(执行+队列容量),claim 出队后
  占用执行槽直到终态;全局 GlobalExecution 并发闸照常生效。
- 任务来源不变:动态 `/agent/{slug}`、MOTA push、A2A message/send 统一进
  dispatcher;`executorKind = remote-pull` 的 Agent 的任务改落入本队列,
  由连接器执行。

## 6. 存储(参考)

PostgreSQL 新表 `agent_pull_tasks`:

| 列 | 说明 |
|---|---|
| `run_id` | PK,关联 runs 审计表 |
| `agent_slug`, `agent_revision` | 认领时快照 |
| `payload` jsonb | claim 响应中的 input |
| `state` | `queued` / `leased` / `completed` / `failed` / `superseded` |
| `lease_owner`, `lease_expires_at` | 当前持有者与到期时刻 |
| `attempts`, `delivered_at`, `completed_at` | 重试计数与时间线 |
| `result` jsonb | 首次接受的 complete 结果 |

`state='queued'` 上建部分索引供 SKIP LOCKED 弹出;`lease_expires_at` 上建
索引供过期回收扫描。

## 7. 配置(环境变量)

| 变量 | 默认 | 说明 |
|---|---|---|
| `AGENT_API__ENABLED` | `false` | 开关;关闭时路由不挂载 |
| `AGENT_API__LEASE_SECONDS` | `90` | 租约时长,≥ 3 × renew 间隔 |
| `AGENT_API__MAX_ATTEMPTS` | `3` | 含首次的总派发次数 |
| `AGENT_API__CLAIM_MAX_WAIT_SECONDS` | `30` | 长轮询窗口上限 |
| `AGENT_API__WORKER_OFFLINE_AFTER_MISSES` | `3` | 心跳缺失多少次判离线 |

## 8. 连接器实现要点(任意语言,~100 行)

1. 两条独立循环:claim(串行认领,一次一个任务)+ heartbeat。
2. 收到任务后:`执行前先续一次租`,处理期间按 `renewAfterSeconds` 续租;
   409 立即放弃本地工作。
3. complete 失败(网络/5xx)按 1s 退避重试——幂等保证重试安全。
4. 时钟:以 `serverTime - 本地时间` 为漂移修正,续租判定以服务端为准,
   本地只需保证"到期前发出 renew"。
5. 超过 `task_timeout`(连接器自配,建议 ≤ lease 的 1/3 处开始密集续租)
   主动 complete(failed) 而不是弃管。
