# 管理台 × MoTA 平台接口通信顺序

> 关联服务:`mota-agent-server-rs`(agent-center:8080)、agent-gateway(caddy)、MoTA 平台(api-ctr.mota.ai)、litellm 网关(litellm.traderwtf.ai)。
> 部署拓扑见 `PUSH-PRODUCTION.md`。

## 1. 打开管理台(`/center/admin#agents-section`)的请求顺序

管理台页面自身不直接与 MoTA 平台通信，全部数据来自 Rust 服务连接的 PostgreSQL（`DATABASE_URL`）。

```
浏览器
 1. GET  https://agent.mota.ai/center/admin
      → Cloudflare → agent-gateway(caddy,剥 /center)→ agent-center:8080 GET /admin
      → 返回 admin.html(无登录态时先显示登录页)
 2. POST /api/admin/login(admin 凭据)→ 返回 session(csrfToken + HttpOnly cookie)
 3. GET  /api/admin/session(校验会话)
 4. GET  /api/admin/agents                       → 智能体列表(state.agents)
 5. GET  /api/admin/runs?limit=50                → 最近运行(state.runs,全智能体)
 6. 前端计算统计卡:智能体总数 / 已启用 / 执行中 / 异常运行
 7. 点击某个智能体 → GET /api/admin/runs?slug={slug} → 该智能体的运行记录(含输入/输出)
```

## 2. 运行记录的产生链路(平台 → 服务 → 平台)

管理台里的每条 run,是 MoTA 平台评测/任务走如下时序落库的:

```
MoTA 平台(api-ctr.mota.ai)
 1. 发起任务(评测 / 连通测试 / 真实业务):
    POST https://agent.mota.ai/center/agent/{slug}
      → Cloudflare → agent-gateway(caddy,剥 /center)
      → agent-center:8080  POST /agent/{slug}
 2. 服务验签:核对 X-MoTA-Signature(= HMAC(mbt_通道密钥, 原始 body))
 3. 幂等检查 + start_run 落库(agent_runs,status=running,存 request_json)
 4. 返回 202(异步)或 200(同步)
 5. 执行器按 Agent.execution_mode 执行:
    local   → 本机 codex / pi / reasonix / claude CLI（统一 deadline 与进程组清理）
    gateway → Anthropic /v1/messages 或 OpenAI /v1/chat/completions
    rust    → 进程内 Processor（不调外部）
 6. 输出规范化:岗位契约键裁剪 / 枚举归一 / symbol 标准化与覆盖补全(与平台岗位 schema 对齐)
 7. finalize_run 落库(completed/failed + result_json + 耗时 + 响应码)
 8. 结果回传平台:
    POST https://api-ctr.mota.ai/mota/creator/publics/bound/callback
      (头:X-MoTA-Run-ID + X-Creator-Signature v2,平台验签)
 9. 平台接受(accepted=true)→ 该 run 计入评测/连通结果
```

## 3. 持续的后台通信(心跳)

```
每 60s(平台回执动态调整):
  POST https://api-ctr.mota.ai/mota/creator/publics/bound/heartbeat
    Authorization: Bearer mbt_*(通道密钥)
    body: {} 或 { selfCheck: { pass, latencyMs, error } }
  → 平台返回 { selfCheckRequired, heartbeatIntervalSeconds, selfCheckIntervalSeconds }
  → selfCheckRequired=true 时,下一次心跳先跑一次真任务自检再上报
  → 连续 15 分钟无有效心跳 → 平台判离线
```

## 4. 完整时序图

```
MoTA平台                agent-gateway            agent-center(8080)            litellm
   │  任务(POST /agent/buffett) │                         │                        │
   ├──────────────────────────►│  /center 剥前缀          │                        │
   │                           ├────────────────────────►│ 验签→start_run→202     │
   │                           │                         ├───────────────────────►│ 执行
   │                           │                         │◄───────────────────────┤ 结果
   │                           │                         │ 规范化→finalize_run    │
   │ 回调(callback)            │                         │                        │
   │◄────────────────────────────────────────────────────┤ POST /publics/bound/   │
   │                           │                         │   callback             │
   │ 心跳(heartbeat)           │                         │                        │
   │◄────────────────────────────────────────────────────┤ POST .../bound/heartbeat│
   │                           │                         │                        │
管理台 ← GET /center/admin → GET /api/admin/agents+runs（读 PostgreSQL，展示以上记录）
```

## 5. 外部接口清单

### 入站(外部 → 服务)

| 接口 | 方法 | 用途 | 认证 |
|---|---|---|---|
| `/agent/{slug}`(公网 `/center/agent/{slug}`) | POST | 智能体执行接口(平台任务主入口) | X-MoTA-Signature / X-Agent-Signature v1 |
| `/mota/push`(可配 `MOTA__PUSH_PATH`) | POST | MOTA Push 接入 | X-MoTA-Signature + X-MoTA-Run-ID |
| `/health`、`/health/ready` | GET | 健康检查 | 无 |
| `/metrics` | GET | Prometheus 指标 | 无 |
| `/version`、`/info` | GET | 版本/信息 | 无 |
| `/admin`、`/api/admin/*` | — | 管理端(登录、智能体、运行记录) | 管理会话 + CSRF |

### 出站(服务 → 外部)

| 目标 | 接口 | 用途 |
|---|---|---|
| MoTA 平台 | `POST /mota/creator/publics/bound/callback` | 异步任务结果回传 |
| MoTA 平台 | `POST /mota/creator/publics/bound/heartbeat` | Push 心跳(已启用) |
| Agent 网关 | `POST /v1/messages` 或 `/v1/chat/completions` | Anthropic/OpenAI 兼容调用 |

## 6. 要点

- **管理台不直接调用 MoTA 平台**；智能体、运行记录和输入输出来自 PostgreSQL。
- **平台 ↔ 服务三个通信点**:入站任务(`/agent/{slug}`)、出站结果回调(`callback`)、出站心跳(`heartbeat`)。
- **执行器双路径**：`local` 派生本机 Agent CLI；`gateway` 走共享 HTTP Client，并支持两种兼容协议。
- **`/mota/push` 通道不落 `agent_runs`**：只写 PostgreSQL `push_idempotency`；动态 `/agent/{slug}` 才写运行记录。
