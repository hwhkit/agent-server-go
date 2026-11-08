# Intelligent Router 运维手册

面向平台管理员（管理数百~上千智能体）的日常运行手册：扩容旋钮、数据治理、
故障排查、备份与告警。架构背景见 `ARCHITECTURE-V2.md`；本文只讲操作。

## 1. 扩容旋钮（环境变量 / `config/*.toml` [platform] 段）

| 旋钮 | 默认 | 说明 |
|---|---|---|
| `PLATFORM__GLOBAL_MAX_CONCURRENCY` | 16 | 全局执行并发闸。千级 Agent 常态派发建议 32–128；单实例上限 256 |
| `PLATFORM__DB_MAX_CONNS` | 8 | pgxpool 上限。经验公式 ≈ 全局并发 × 副本数 / 4，再留 2–4 条管理余量；上限 256 |
| `PLATFORM__RETENTION_DAYS` | 0（关） | `channel_events`/`audit_log` 保留窗口（天）。合规允许时建议 90；0 只清过期会话 |
| `PLATFORM__MAX_REQUEST_BODY_BYTES` | 2MiB | 请求体上限；大模板场景上调后同步核对回调端容量 |
| `IR__PUSH_MODE` | sync | 大规模建议 `async`（callback 回传），同步长执行占连接 |

多副本：所有副本共库即可——Provider 与注册表变更通过全局 revision 轮询
（2s）自动收敛；`/agent/{slug}` 幂等键按 `(slug, key)` 唯一，跨副本安全。
push 幂等行 `processing` 超过 15 分钟会被新请求接管，10 分钟一次后台清扫。

## 2. 日常运维面

- **执行后端热更新**：管理台「智能体 → 执行后端」保存即热更新（后续任务用
  新配置，运行中任务保持启动时快照）；多副本由 revision 轮询在 ~2s 内收敛。
- **密钥轮换不断流**：改智能体签名密钥后，旧钥仍有 24h 宽限（日志出现
  `verified with previous signing secret` 即调用方尚未迁移）；24h 后仅新钥有效。
- **批量迁移**：管理台「智能体」页导出 JSON（含密文，主密钥一致即可导入）
  / CSV（观察）；导入对话框逐条回执。密文 AAD 绑定 slug——迁移保持 slug。
- **总览与指标**：`/metrics`（Prometheus）暴露 `ir_*` 域指标与 HTTP 计数；
  `/health/ready` 的 checks 数组逐项给出 migrations/agent_registry/queue 状态。
- **失败排查**：运行日志按 `status`/`error_code`/时间范围筛选；团队失败运行
  一键「重试」（同输入新幂等键，键形如 `<原键>/retry/<nonce>` 可追溯）。
- **审计合规**：审计日志仅管理员可见（后端强制）；`/audits/export?limit=10000`
  导出 CSV 留档。

## 3. 故障排查速查

| 症状 | 首查 | 处置 |
|---|---|---|
| `/health/ready` 503 | checks 里 `database`/`migrations` 项 | DB 连接/迁移标记；`/metrics` 的 `ir_db_pool_conns` 看池耗尽 |
| `/health/ready` degraded | `agent_registry` 或 `queue` 项 | 注册表读取或队列表异常；多为连接抖动，持续出现再排查 |
| 智能体全部 401 | 主密钥是否更换 | 主密钥换 = 全部签名密文不可解；恢复密钥或批量重发密钥 |
| 单个 Agent 401 | 该 Agent 是否刚换钥 | 宽限期内=调用方仍用旧钥（正常，等迁移）；过期=更新调用方 |
| push 全部 422 | 校验明细字段 | 见响应 fields；多为 schemaVersion/runId/callbackUrl 缺失 |
| push 幂等 429/冲突 | `ir_queue_tasks` 与重复 run_id | 同 run_id 换键重投是协议冲突，纠正调用方 |
| 团队运行卡 running | `ir_team_runs_active` 与日志 `team run reconcile` | 引擎 15s 对账自动重派/判死；`Recover` 失败再查 DB |
| 提供商状态「未找到可执行文件」 | 执行后端卡片 status | 本机 PATH 无该 CLI；改二进制路径或安装 CLI |

## 4. 备份与恢复

- **必须备份**：`platform_settings`（Provider 密文+revision）、`agents`
  （签名密文）、`positions`/`agent_positions`/`teams`/`team_runs`、
  `agent_runs`（审计依据）。`push_idempotency` 的终态行是重放判据，随库备份。
- **恢复口径**：主密钥（`ADMIN__SESSION_SECRET`）必须与备份时一致，否则全部
  密文不可解。智能体可用「导出 JSON → 导入」跨主密钥环境重放（导出含密文，
  需同主密钥；跨主密钥需明文重录密钥）。
- **演练**：季度做一次 `pg_dump` → 空库恢复 → `/health/ready` degraded 全清 →
  控制台登录抽查。

## 5. 告警建议（基于 /metrics）

| 指标 | 阈值 | 含义 |
|---|---|---|
| `ir_db_up` | <1 持续 1m | 存储不可达 |
| `ir_db_pool_conns{state="acquired"}` | = max 持续 5m | 连接池耗尽（上调 DB_MAX_CONNS 或降并发） |
| `ir_queue_tasks{state="queued"}` | 持续 > 容量×2 | remote-pull 派发积压（连接器掉线或吞吐不足） |
| `ir_runs_today{status="failed"}` | 5 分钟速率突增 | 执行器批量故障（提供商/key） |
| `ir_team_runs_active` | 持续走高不落 | 编排引擎推进受阻（看 reconcile 日志） |
| `ir_http_requests_total{status=~"5.."}` 速率 | >1% | 服务端错误，按 path 标签定位 |
