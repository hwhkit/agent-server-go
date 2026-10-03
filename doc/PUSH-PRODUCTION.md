# MOTA Push production protocol

本文描述 `mota-agent-server-go` 当前实现的 bound-task v1、结果信封 V2 与 callback v2
行为。岗位输出字段仍以 MOTA 创作者中心展示的 schema 为准。

## 1. 请求不变量

| 约束 | 服务行为 |
|---|---|
| 原始字节验签 | 在 JSON 解析前校验 `HMAC-SHA256(signing_secret, raw_body)` |
| 请求头唯一 | run ID、幂等键和签名头必须各出现一次；缺签名返回 401，其余结构错误返回 422 |
| 头体一致 | `X-MoTA-Run-ID` 必须与 `body.runId` 完全一致 |
| 信封版本 | `schemaVersion` 必须为 `1.0` |
| callback 地址 | 必须是绝对 HTTPS URL；HTTP 仅放行 loopback 开发地址 |
| 幂等绑定 | 幂等键同时绑定 run ID 与请求体 SHA-256 |
| 结果信封 | `payload_result`、`payload_info`、`contract_version=V2` 和 `usage` |

服务支持的正式 `positionType`：

```text
FUNDAMENTAL_ANALYST   TECHNICAL_ANALYST   PORTFOLIO_MANAGER   RISK_MANAGER
EXECUTION_TRADER      STRATEGIST          QUANT_TRADING_AGENT SENTINEL
```

探测任务可省略 `positionType`。`globalParameters` 只允许 `language`、`targetMarkets`、
`symbolPool`、`universeMode`、`riskLevel`、`maxPositionSizeUsd`、
`maxDrawdownThreshold`、`stopLossThresholdPct`、`maxInvestmentTargetsUsd`；
`runtimeContext` 只允许 `isFinal`。

## 2. 请求示例

```http
POST /mota/push HTTP/1.1
Content-Type: application/json
X-MoTA-Run-ID: RUN-20261002-001
X-MoTA-Idempotency-Key: delivery-7f9a
X-MoTA-Signature: <lowercase hex HMAC>
```

```json
{
  "schemaVersion": "1.0",
  "runId": "RUN-20261002-001",
  "agentCode": "fundamental-demo",
  "positionType": "FUNDAMENTAL_ANALYST",
  "templateInputData": [
    { "name": "securities", "value": ["AAPL"] }
  ],
  "taskBrief": {
    "userCustomInstructions": "Use a conservative valuation range"
  },
  "globalParameters": {
    "language": "zh-CN",
    "targetMarkets": ["US"],
    "symbolPool": ["AAPL.US"],
    "universeMode": "RESTRICTED"
  },
  "runtimeContext": { "isFinal": false },
  "callbackUrl": "https://api.example.com/callback"
}
```

`Content-Type` 可带 `charset` 等参数。非法 JSON 返回 400；字段和头约束失败返回
422；不支持的媒体类型返回 415。

## 3. 同步与异步

`MOTA__PUSH_MODE=sync` 返回 `200 + JSON`，同步执行预算最多 285 秒。
`MOTA__PUSH_MODE=async` 先返回 `202 + 空 body`，再在当前进程后台执行并投递 callback，
异步执行预算最多 780 秒。

202 表示服务已在**当前进程**接收任务，不代表结果已持久化投递。当前 Go 实现没有
callback outbox；进程在结果投递前退出时，相同 run ID、幂等键与请求体通常只会重放
202，并不会重新启动后台任务。运维人员必须先核对平台与执行器副作用，再决定是否人工
修复状态或使用新的、明确授权的请求。

同步幂等记录的主要状态：

```text
processing → sync_done
```

异步请求被接收后进入 `async_accepted`。后台执行一旦开始，无论 callback 成功、明确
失败、无可信回执、后台 panic、受信执行失败或 usage 抑制，都保留 202 重放；这些情况
都可能已经产生执行器副作用，不能因为投递失败就自动重执行。需要按原 run ID 对账后
由运维人员决定新的授权请求。

## 4. Callback v2

签名覆盖真正发送的同一份 JSON 字节：

```text
timestamp = unix seconds
canonical = b"v2\n" + ascii(timestamp)
          + b"\n" + utf8(runId)
          + b"\n" + exact_body_bytes
digest    = hex_lower(HMAC-SHA256(utf8(signing_secret), canonical))
header    = "t=" + timestamp + ",v2=" + digest
```

```http
POST {callbackUrl}
Content-Type: application/json
X-MoTA-Run-ID: RUN-20261002-001
X-Creator-Signature: t=1790917200,v2=<hex>
```

`MOTA__CALLBACK_TOKEN` 仅用于兼容旧平台；非空时作为 `token` 查询参数发送。配置它时
必须同时设置 `MOTA__CALLBACK_TOKEN_ORIGIN=https://受信主机`，服务只会把 token 发往
完全匹配的 scheme + host。新平台应优先使用空 token 与 v2 签名。

每次尝试重新生成时间戳和签名。单次请求及回执读取合计最多 30 秒，回执正文上限
64 KiB，防止无界等待或内存占用。

### 重试与终态

| callback 结果 | 行为 |
|---|---|
| 网络错误 | 有界退避重试；耗尽后结果不确定 |
| HTTP 408 / 429 / 5xx | 有界退避重试；耗尽后结果不确定 |
| 其他 4xx | 不重试，记录投递失败 |
| 200 + 非法/缺字段回执 | 记录结果不确定 |
| 200 + `code != OK` | 不重试，记录结果不确定 |
| 200 + `code=OK, accepted=false` | 不重试，记录 `callback_rejected` |
| 200 + `code=OK, accepted=true` | 投递成功 |

退避序列为 `1s → 3s → 10s → 30s → 60s`，最多六次发送。动态 Agent 仅在
`accepted=true` 时把运行标为 completed；明确拒绝为 `callback_rejected / 502`，确定
投递失败为 `callback_delivery_failed / 502`，已发送但无法获得可信回执则为
`outcome_uncertain / 503`。通道事件保存状态、尝试次数和安全摘要，不应依赖原始远端
错误正文诊断。

Push 与动态 Agent 的失败重放不同：动态 Agent 会缓存对应 502/503 终态；Push 在后台
执行开始后保留异步 202，要求按原 run ID 人工核对，不自动重执行。

## 5. 结果信封

```json
{
  "payload_result": {
    "assessments": []
  },
  "payload_info": "No reliable assessment was produced.",
  "contract_version": "V2",
  "usage": {
    "schemaVersion": "1.0",
    "inputTokens": 0,
    "outputTokens": 0,
    "totalTokens": 1
  }
}
```

`MOTA__TRUSTED_USAGE_REPORTING_ENABLED=true` 仅适用于已登记的受信 Host。开启后若
执行器没有供应商 FINAL usage，同步请求返回 502，异步请求抑制 callback 并记录失败；
不能用估算值冒充供应商计量。

正常岗位输出会执行键白名单、枚举、数字和 symbol 归一化。无法可靠完成时使用岗位
schema 允许的保守结构，不得伪造行情、财务数据、证据引用或订单。

## 6. PostgreSQL 与幂等

生产 Push 必须设置 `DATABASE_URL`，即使管理平台未启用。幂等状态与管理平台可共用
连接池；单元测试才使用内存实现。

同一幂等键若更换 run ID 或请求体会返回 409。处理中的请求并发重入也返回 409。
PostgreSQL、加密主密钥和业务运行记录必须进入同一恢复计划。

## 7. 生产安全基线

- 通过秘密管理系统注入签名密钥、bind token、数据库凭据和 Session Secret；
- callback 客户端不得跨 origin 跟随重定向；
- callback 当前没有私网 IP/DNS 阻断策略，生产入口应配置平台固定 callback 地址或在
  出口代理实施 allowlist；
- 动态 Agent 的 legacy body-only HMAC 默认关闭；仅迁移期可显式开启，调用方应使用
  带时间戳和 nonce 的 v1 动态 Agent 签名；
- 本地 Agent CLI 必须运行在低权限用户、容器或 VM 中；应用内 workspace 校验不是
  OS 沙箱；
- 日志、运行结果、审计记录和数据库备份可能含业务数据，必须限制访问并定义留存周期。

## 8. 上线顺序

1. 运行 `make bootstrap-hwhkit` 和本地验证；
2. 使用隔离数据库执行 `make test-pg`；
3. 运行 `make deploy-build && make deploy-check`，确认产物、目标、服务名、架构和就绪地址；
4. 注入 `DATABASE_URL`、MOTA 密钥、管理员凭据和生产 URL；
5. 部署后验证 `/health/ready`、运行中二进制哈希与 PostgreSQL 可用性；
6. 从外网验证 HTTPS、请求签名、幂等重放和第一条真实 callback 回执；
7. 观察运行与审计日志后再扩大流量。

Go 项目的本地门禁：

```bash
make lint
make test
make race
TEST_DATABASE_URL='postgres://...' make test-pg
make test-deploy-scripts
```

部署与回滚细节见 [DEPLOYMENT.md](DEPLOYMENT.md)，发布证据见
[RELEASE-READINESS.md](RELEASE-READINESS.md)。
