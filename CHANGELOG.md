# Changelog

本项目遵循 [Semantic Versioning](https://semver.org/)，格式参考
[Keep a Changelog](https://keepachangelog.com/)。

## [Unreleased]

### Changed

- **v2 整洁架构重写切换**（破坏性,部署需按 doc/ARCHITECTURE-V2.md §8 执行）:
  - 二进制入口改为 `./cmd/ir-agent-server`（Makefile/CI 已同步）;旧 `ir/` 包与
    根 `main.go` 删除,全部实现迁至 `internal/`（domain/app/infra/api 四层,
    `internal/archguard` 机械守护依赖方向）;
  - 管理 API 重组为 `/api/admin/v2`（八模块 IA:岗位/智能体/团队/运行日志/
    审计日志/账号管理/设置/总览 + 执行后端 providers 热更新）;
  - 新增表 `job_positions`（岗位;避开共享库中金融应用既有的 `positions`
    持仓表命名冲突）、`agent_positions`、`teams`、`team_runs`、`channel_events`,
    `agent_runs` 增列 team_run_id/team_step_id/position_code/attempts/usage_json
    （只增不改,旧行兼容）;
  - `PLATFORM__ENABLED=true` 成为硬性要求（旧「嵌入占位 Processor」模式随
    legacy 栈退役）;
  - 进程内 Agent Registry（2s 轮询）退役:分发按 slug 直读数据库,无收敛窗口;
  - 线协议冻结不变:`/ir/push`、`/agent-api/v1`、`/agent/{slug}` 动态入口、
    A2A v1.0 的请求/响应/签名/幂等语义与 v1 逐字节兼容;
  - 修复旧栈潜在缺陷:Provider `vault_ids` 以 null 落库后,下次启动整份
    Provider 快照加载失败（null 现语义为清空）。

- 契约守护测试 `TestContractsMatchRustSource`（跨仓比对 Rust 源码,因 Rust 仓
  2026-11-02 的 `4bc459d` 未回移而失效）替换为仓内 golden 快照
  `TestContractsGolden`（`internal/domain/position/testdata/contracts/`）。
  Go 侧自此为契约权威;有意修改契约需 `-update` 重新生成快照并在本文件记录,
  流程见同目录 `README.md`。**待定**:Rust `4bc459d` 为
  PORTFOLIO_MANAGER 新增的「订单条数硬约束」规则是否回移到 Go 契约。

- **产品由 MoTA 更名为 Intelligent Router**，短前缀统一为 `ir`。全量改名（破坏性）：
  - Go 包 `mota` → `ir`，module 与二进制 `mota-agent-server` → `ir-agent-server`；
  - 环境变量前缀 `MOTA__` → `IR__`，配置文件选择器 `MOTA_ENV` → `IR_ENV`，
    TOML 配置段 `[mota]` → `[ir]`；
  - Push 默认路径 `/mota/push` → `/ir/push`（可用 `IR__PUSH_PATH` 覆盖）；
  - Push 请求头 `X-MoTA-Signature` / `X-MoTA-Run-ID` / `X-MoTA-Idempotency-Key` →
    `X-IR-Signature` / `X-IR-Run-ID` / `X-IR-Idempotency-Key`；callback 侧删除
    `X-MoTA-Run-Id` / `X-MoTA-Idempotency-Key` 旧别名，仅接受 `X-Agent-Run-Id` /
    `X-Idempotency-Key`；
  - 管理台 Cookie `mota_admin_session` → `ir_admin_session`（登录态失效一次）；
  - Pi 扩展工具名 `mota_finance_snapshot` / `mota_market_history` →
    `ir_finance_snapshot` / `ir_market_history`，扩展文件更名为
    `extensions/pi/ir-finance-data.ts`，环境变量 `MOTA_FINANCE_USER_AGENT` →
    `IR_FINANCE_USER_AGENT`，UA 与 usage schema 字面量同步更名为 `IR-*` /
    `ir-bound-usage-v2`；API problem 类型 `urn:mota:*` → `urn:ir:*`。
  - **切换须知（与上游平台 agent-center-go lockstep）：**平台侧需同步改发 `X-IR-*`
    头并指向 `/ir/push`；部署环境 `.env` 键名与 TOML 段名需同步重命名（值不变）；
    远端 `/opt/mota-agent-server-go` 与 systemd unit 需改名（Makefile 已指向
    `/opt/ir-agent-server-go`）；Pi 扩展与 skills 需随服务端重发。数据库表名与
    schema 无变化。

### Fixed

- callback 对 nil payload、nil receipt 和异常后台路径进行防御，避免异步 goroutine panic；
- callback 单次请求与回执读取限制为 30 秒、正文限制为 64 KiB，并把 HTTP 408 纳入
  408/429/5xx 有界重试；
- 动态 Agent 只有在 callback `accepted=true` 时完成；明确拒绝记录
  `callback_rejected`，确定投递失败记录 `callback_delivery_failed`，无可信回执记录
  `outcome_uncertain` 并以 503 幂等回放；
- Push 异步后台执行开始后，callback 失败、无可信回执、`accepted=false`、panic 或 usage
  抑制都保守保持 202 重放，避免自动重复可能已经发生的副作用；
- Provider 执行改为每个任务捕获不可变配置快照，热更新不再等待旧任务结束，也不与首次
  执行器构造产生数据竞争；
- Provider 保存使用数据库 revision/CAS 合并并发部分更新；共享配置组事务化保存，多实例
  通过全局 revision 轮询收敛；
- Agent CRUD 增加事务 revision、`If-Match` 与多实例注册表刷新，避免并发更新回滚密钥，
  并让删除、停用和密钥轮换传播到所有副本；
- Provider、Agent 与账号变更和对应审计日志改为同一 PostgreSQL 事务，审计失败会回滚
  业务变更；
- 本地 CLI 响应父 context 并终止整个进程组，stdio 双流清理共享总截止时间；托管 SSE
  与 Pull/heartbeat 回执均改为有界读取；
- 部署使用哈希暂存、远端互斥、原子替换、就绪轮询、运行进程哈希验证和失败回滚；
- Pi 部署要求精确包版本，并在替换服务二进制前完成身份与运行时校验。

### Security

- Provider、Agent 与账号写操作限制为 `admin`，普通用户保持只读并只能查看本人审计；
- 执行器与 callback 的带凭据 HTTP 客户端禁止跨 origin 跟随重定向；
- 启动拒绝 `.env.example` 已知占位符和启用 Provider 的 `replace-me` 密钥；
- bootstrap 明文管理员密码统一限制为 12–512 字节；
- A2A 执行失败只返回稳定公开文案，不泄露内部异常；
- legacy callback token 必须配置唯一 `callback_token_origin`，跨 origin 目标在发送前拒绝；
- 登录退避按账号与真实来源隔离，并只信任显式 CIDR 中反向代理提供的转发来源；
- Provider URL 强制 HTTPS（回环开发例外），普通用户不再收到基础设施配置明细；
- 升级所有可修复的已知漏洞依赖；唯一无修复且不可达的 OpenPGP 公告使用有期限精确例外。

### Migration

- 已配置 `MOTA__CALLBACK_TOKEN` 的部署必须在升级前补充
  `MOTA__CALLBACK_TOKEN_ORIGIN=https://受信平台主机`；未配置 token 的新平台无需设置。
- 旧库升级前必须检查并人工归并重复 `push_idempotency.run_id`；具体归档和恢复流程见
  `doc/DEPLOYMENT.md`。

### Added

- Provider API 的 `revision` 字段、可选 `If-Match` 和
  `412 stale_provider_revision` 并发冲突响应；
- hwhkit-go 固定源码 bootstrap、race、部署脚本模拟与 deploy-check 入口；
- GitHub CI、PostgreSQL 15/16/17 矩阵、GitHub Actions 更新配置与开源维护模板；
- 部署、维护边界和发布就绪文档；
- `MIT OR Apache-2.0` 双许可证、贡献指南、安全策略和行为准则。

### Changed

- README 快速开始现在明确要求 PostgreSQL、完整 Push 信封和幂等请求头；
- 生产协议与通信文档改为描述 Go 实现，不再包含 Rust/cargo 命令；
- `.env.example` 不再提供可被误用为真实凭据的可运行固定值；
- Go 语言基线升级到 1.26，并固定 CI/模块建议工具链为 1.26.8；
- 历史 v1.1 迁移记录明确标记为归档，不再作为当前测试或完成状态证明。

### Known limitations

- 动态 Agent callback 尚无持久化 outbox；202 后进程退出可能需要按原 run ID 对账；
- callback 尚无完整私网 IP/DNS allowlist 防护；
- 本地 CLI 的应用级路径与环境限制不是 OS 沙箱；
- 执行并发限制按实例计算，不是分布式配额。
- Push 终态幂等行需长期保留以阻止无时间戳签名重放，尚未实现紧凑 tombstone/自动归档。
