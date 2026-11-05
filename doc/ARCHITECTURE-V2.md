# Intelligent Router v2 架构蓝图（全面重写）

> 2026-11-05 定稿。本文是系统重写的唯一权威设计：分层规则、领域模型、模块映射、
> 迁移与验证策略。实现与本文冲突时，以本文为准；本文修订需在 CHANGELOG 记录。

## 0. 目标与非目标

**目标**

1. 后端按整洁架构（Clean Architecture）重组：依赖只指向内层，领域层零外部依赖；
2. 领域模型演进为 岗位(Position) / 智能体(Agent) / 团队(Team) 三概念分离，
   团队承载静态 DAG 编排；
3. 管理台按八模块 IA 重写：总览 · 岗位 · 智能体 · 团队 · 运行日志 ·
   审计日志(admin) · 账号管理(admin) · 设置(个人/系统)；纵向侧栏，业务/系统分区，
   可折叠图标轨，导航角标；
4. 运行日志支持团队运行 → 步骤运行的父子展开；
5. 审计日志收紧为仅管理员可见（后端强制，不只前端隐藏）。

**非目标**

- 不改对外线协议形状：`POST /ir/push`（X-IR-* 头、bound-task v1 信封）、
  `/agent-api/v1`（claim/renew/complete/heartbeat）、`/agent/{slug}` 动态入口、
  A2A v1.0 全部保持兼容。上游平台与连接器无需感知本次重写；
- 不做嵌套团队（团队步骤只能指向岗位/智能体，不能指向团队）；
- 不做动态编排（LLM 决定控制流）——团队是声明式 DAG。

## 1. 分层与依赖规则

```
cmd/ir-agent-server        装配与入口（唯一允许 import 全部四层的包）
internal/api               接口适配层：HTTP handlers、路由、中间件、DTO
internal/app               应用层：用例编排、事务边界、编排引擎
internal/domain            领域层：实体、值对象、端口(interface)、领域服务
internal/infra             基础设施：PostgreSQL、执行器、HTTP 客户端、crypto、clock
web/                       控制台前端（go:embed）
```

**依赖规则（机械强制，见 §9 守护测试）**

- `domain` 不 import 任何 internal 包与第三方库（std 除外）；
- `app` 只 import `domain`；
- `infra` import `domain`（实现端口），可 import 第三方库（pgx 等）；
- `api` import `app` + `domain`（读实体），不 import `infra`；
- 跨层数据交换用领域实体或 api 层 DTO，禁止 infra 的存储行结构外泄。

**适度抽象原则**：只为「存在多实现」或「测试必须替身」的边界建接口
（仓储、执行器、时钟、出站 HTTP、签名器）；单实现工具（校验、归一化、
模板渲染）直接做包级函数，不造接口。

## 2. 包树

```
internal/
├── domain/
│   ├── position/    岗位：Code、能力边界、角色契约、输出 schema 引用
│   ├── agent/       智能体：Slug、执行绑定、并发/队列、岗位聘任、密钥
│   ├── team/        团队：DAG 步骤、失败策略、TeamRun 状态机
│   ├── run/         运行：Run、Usage、状态、team_run/step 关联
│   ├── connector/   远端连接器：令牌、worker、租约、队列语义值对象
│   ├── account/     账号、会话、角色
│   ├── audit/       审计事件
│   ├── platform/    平台协议域：bound-task 信封、签名规范、callback 契约
│   └── exec/        执行端口：Executor 接口、TaskInput/TaskOutput/Usage 值对象
├── app/
│   ├── push/        Push 用例（同步/异步、幂等决策）
│   ├── connector/   Pull worker 循环、agent-api 用例（claim/renew/complete/heartbeat）
│   ├── dispatch/    运行调度：队列准入、租约派发、超时、重试（原 processor/dispatcher）
│   ├── teamrun/     编排引擎：TeamRun 提交、DAG 求值、步骤派发、取消传播
│   ├── catalog/     岗位/智能体/团队 CRUD 用例（revision、If-Match、注册表版本）
│   ├── account/     账号、会话、登录退避
│   ├── audit/       审计写入与查询（admin-only 查询策略）
│   ├── dashboard/   总览聚合（健康、队列深度、今日运行、用量汇总）
│   ├── a2a/         A2A 映射用例
│   └── callback/    异步结果回传（有界重试、回执分类）
├── infra/
│   ├── postgres/    全部仓储实现 + 幂等 DDL/seed 迁移
│   ├── execengine/  执行引擎整体：分发矩阵、本机 CLI（pi/codex/opencode/qwen/
│   │                gemini/openhands/claude/deepseek）、托管（OpenAI/Claude）、
│   │                litellm 网关、输出归一化/schema 校验/usage 汇总、占位处理器。
│   │                （实施修订：原计划的 execlocal/execmanaged/execgateway 三包
│   │                合并为 execengine 单包——三个执行器族共享归一化/schema/usage/
│   │                错误机械，拆包需导出数十个内部符号而无行为收益；文件边界保留
│   │                原命名，P4 定义 exec.Executor 适配时如需再拆。）
│   ├── execprocess/ 子进程原语（进程组、有界抓取、审计 JSON）
│   ├── httpx/       出站 HTTP 客户端（禁跨 origin 重定向、限额、超时）
│   ├── crypto/      AES-256-GCM、Argon2id、HMAC 规范化
│   ├── clock/       时钟实现（消费方自声明小接口）
│   └── config/      分层配置加载（TOML → env IR__*）+ origin/回环校验
└── api/
    └── http/
        ├── admin/   /api/admin/* v2 + 控制台页面
        ├── push/    /ir/push
        ├── agentapi//agent-api/v1
        ├── dynamic/ /agent/{slug}、/agent-center-go/{slug}
        ├── a2a/     A2A 路由与 JSON-RPC
        └── middleware/ 安全头、请求 ID、限流、body 限额
web/                 index.html + app.css + app.js + i18n.js（拆分文件，embed.FS）
```

## 3. 领域模型

### 3.1 岗位 Position（工作的边界和能力范畴）

```go
type Position struct {
    Code        string   // 如 FUNDAMENTAL_ANALYST；wire positionType 即此值
    Name        string
    Description string   // 能力范畴说明
    ContractRef string   // 内置角色契约键（golden 快照守护）或自定义契约文本 ID
    OutputSchema json.RawMessage // 可空；输出契约
    Enabled     bool
    Revision    int64
}
```

- 内置六岗位由契约 golden 快照 seed（FUNDAMENTAL_ANALYST、TECHNICAL_ANALYST、
  STRATEGIST、RISK_MANAGER、EXECUTION_TRADER、PORTFOLIO_MANAGER）；
- 岗位是**职责定义**，不含任何执行配置。

### 3.2 智能体 Agent（可以干活的 AI）

```go
type Agent struct {
    Slug          string        // wire 身份：push agentCode / 动态入口 /agent/{slug}
    Name          string
    Binding       ExecutionBinding // ExecutorKind + ExecutionMode（local/gateway/managed/remote-pull）
    Positions     []string      // 聘任的岗位 Code 集合（多对多）
    SystemPrompt  string        // 岗位契约之上的本地化提示（可空）
    Model         *string
    AllowedTools  []string
    PermissionMode string
    TimeoutSecs   uint64
    MaxConcurrency int
    QueueCapacity int
    Enabled       bool
    SigningSecret []byte        // AES-GCM 加密存储，不回显
    Revision      int64
}
```

- **聘任不变量**：Agent 至少聘任一个岗位才可启用；push 的 bound-task 若同时携带
  positionType 与 agentCode，校验该 Agent 聘任了该岗位，否则 422；
- 执行后端配置（二进制路径、API key、MCP）仍是**系统级单例**（原 providers），
  归入 设置→系统 与 智能体模块的「执行后端」子页签；Agent 只引用 kind/mode。

### 3.3 团队 Team（一组智能体 + 编排）

```go
type Team struct {
    ID          string
    Name        string
    Description string
    Steps       []TeamStep   // DAG
    MaxConcurrentRuns int
    Enabled     bool
    Revision    int64
}

type TeamStep struct {
    ID          string          // 团队内唯一
    Target      StepTarget      // 岗位 Code 或 Agent Slug（二选一）
    DependsOn   []string        // 前驱步骤 ID；构成 DAG，提交时校验无环
    InputTemplate json.RawMessage // 引用 {{steps.<id>.output.*}} 的模板
    Retry       RetryPolicy     // MaxAttempts（默认 1）
    OnFailure   FailureMode     // fail_fast(默认) | continue | skip_branch
    TimeoutSecs *uint64         // 覆盖目标 Agent 的墙钟
}
```

**TeamRun 状态机**（全部状态落库，进程崩溃可恢复）：

```
queued → running → completed
             ├────→ failed(step_failed|team_timeout)
             └────→ canceled
步骤态: pending → dispatched → running → succeeded|failed|canceled|skipped
派发条件: 前驱全部 succeeded（或按 OnFailure 语义豁免）
```

- 目标解析：Target 为岗位时，在聘任该岗位且已启用的 Agent 中按现有队列/并发
  机制派发（复用 dispatch，不新造执行路径）；为 Agent 时直派；
- 每个步骤派发产生一个普通 Run，携带 `team_run_id` + `team_step_id`；
- 取消传播：取消 TeamRun → 在途子 Run 走既有 run 级 abort 通道，未派发步骤置
  canceled；步骤失败按 OnFailure 决定团队终态；
- 载荷传递：步骤输出 ≤ 4 MiB 内联进模板上下文；超限存 runs 表按引用拉取
  （模板可用 `{{steps.<id>.outputRef}}`）；
- 幂等：TeamRun 提交带 idempotency key；步骤重试与 at-least-once 语义继承
  Run 层约定（副作用幂等是执行方责任）。

### 3.4 运行 Run

现有 `agent_runs` 扩展两列：`team_run_id`、`team_step_id`（可空）。运行日志页
父行 = TeamRun，展开 = 子 Run。单发运行不变。

### 3.5 其余领域

connector（令牌 act_、worker、租约、队列）、account、audit、platform（信封/
签名/callback 契约）、exec（Executor 端口）语义与现状一致，仅按层归位。
审计查询端口带 `actor` 约束：非 admin 角色调用直接返回 domain 错误
（ErrPermissionDenied），api 层映射 403——权限在领域层收口。

## 4. 数据库迁移策略

**只增不改不删**（现网库直接升级）：

1. 新表：`positions`、`agent_positions`（聘任多对多）、`teams`、`team_runs`、
   `team_steps_state`（TeamRun 步骤态）；
2. `agent_runs` ADD COLUMN `team_run_id TEXT`、`team_step_id TEXT`（可空，带索引）；
3. seed：六个内置岗位（契约键引用）；回填——现有 agents 按 position_type 生成
   聘任行；无 position_type 的 agent 挂 `__custom__` 岗位（seed 时创建）；
4. DDL 幂等（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS），沿用现有 ensureSchema 模式；
5. 旧表全部保留原名（agents、agent_pull_*、admin_users、audit_log…）。

## 5. 协议兼容矩阵

| 端点 | 变化 |
|---|---|
| `POST /ir/push` | 形状不变；内部路由 agentCode→Agent、positionType→聘任校验 |
| `/agent-api/v1/*` | 形状不变；claim 的 input 增加可选 `positionCode` 字段（加性） |
| `/agent/{slug}` 动态入口 | 不变 |
| A2A v1.0 | 不变；Agent Card skills 由聘任岗位生成 |
| `/api/admin/*` | **重组为 v2**（控制台是唯一消费者，同步重写）：positions/agents/teams/runs/audit(403 for non-admin)/users/dashboard/settings/system |
| callback v2 / 签名 | 不变 |

## 6. 控制台 IA（八模块）

```
业务区: 总览 · 岗位 · 智能体 · 团队 · 运行日志
系统区: 审计日志(admin) · 账号管理(admin) · 设置
```

- **总览**：服务健康（版本/DB/实例）、在线连接器与队列深度、今日运行
  成功/失败/进行中、token 用量汇总；数据来自 `GET /api/admin/v2/dashboard`；
- **岗位**：卡片列表（能力边界、契约、聘任它的智能体数）；契约文本只读查看；
- **智能体**：两个子页签——「智能体」（三列紧凑网格，含聘任岗位标签）与
  「执行后端」（原执行职能体：本地 CLI/托管/网关/远端连接器配置）；
  创建智能体时选后端与聘任岗位；远端后端显示在线 worker；
- **团队**：列表 + DAG 编辑器（v1 用步骤表单：目标/依赖/模板/失败策略，
  图形化拖拽后置）；提交试跑、查看 TeamRun 进度；
- **运行日志**：单发运行与团队运行两个视图，团队运行可展开步骤子行；
  筛选（状态/岗位/智能体/团队）；
- **审计日志**：admin-only（前端隐藏 + 后端 403 双保险）；
- **账号管理**：现状迁移；
- **设置**：「个人」页（主题色/语言，localStorage）+「系统」页（admin 只读
  回显当前生效配置：push 路径、签名编码、A2A、agent-api 参数；改动仍走配置文件，
  避免双写入源）。

侧栏：纵向、分区、可折叠为 56px 图标轨、导航角标（运行失败数/在线连接器数，
接 dashboard 轮询）；移动端保留顶部横向 nav。前端拆分为
`web/index.html + app.css + app.js + i18n.js`（embed.FS 目录嵌入）。

## 7. 旧代码 → 新结构映射

| 现有（ir/） | 去向 |
|---|---|
| config.go、platform_config.go、a2a_config.go、agent_pull_config.go | infra/config |
| platform_model.go | domain/agent、domain/run + api DTO |
| platform_store.go、a2a_store.go、agent_pull_store.go | infra/postgres |
| platform_auth.go、platform_admin.go | app/account、app/audit、api/http/admin |
| push.go、idempotency.go、processor.go、dispatcher*、worker.go | app/push、app/dispatch、app/connector |
| callback.go、platform_gateway.go | app/callback、infra/execgateway |
| executor*.go、executor_process*.go | domain/exec(端口+契约) + infra/execlocal/execmanaged/execgateway/execprocess |
| executor_contracts*.go + golden | domain/position（内置契约 seed 源） |
| agent_pull_api.go | api/http/agentapi + app/connector |
| a2a_*.go | api/http/a2a + app/a2a |
| http_security.go、web/ | api/http/middleware、web/ |
| types.go、reliability_context.go | domain/platform、app/dispatch |

迁移方式：**绞杀者模式**——新树逐包落地并接线，旧 `ir/` 包保持可构建直至
对应模块切换完成，最后整包删除。任何时刻 `go build ./... && go test ./...` 必须绿。

## 8. 实施阶段与验证门禁

| 阶段 | 内容 | 门禁 |
|---|---|---|
| P1 | 脚手架 + domain 全部实体/端口 | build/vet 绿；domain 零依赖守护测试 |
| P2 | infra/postgres + config + crypto + clock | 仓储单测（真库 TEST_DATABASE_URL） |
| P3 | infra/exec* 执行器迁移 | ✅（实施记录：契约文本+golden → domain/position；进程原语 → infra/execprocess；四个配置子系统 → infra/config；执行引擎整体 → infra/execengine；线协议类型/payload 组装/taskBrief 提取 → domain/platform；ExecutorKind/Mode 常量与方法 → domain/exec；AgentConfig → domain/agent.ResolvedConfig。旧包经别名文件桥接，全仓测试绿 + 运行冒烟通过） |
| P4 | app：dispatch/push/connector/callback/a2a | ✅（实施记录：app/dispatch 准入阶梯+远端有界同步等待；app/push 校验矩阵+sync/async+幂等六分支；app/agentapi（原 connector）claim 长轮询/五分支续租/幂等 complete/令牌轮换；app/callback 六次重试+分类；app/a2a Service.Dispatch+CardConfig 切面；app/worker 拉取 Worker+push 形态心跳） |
| P5 | 新域：catalog(岗位/智能体/团队 CRUD) + teamrun 编排引擎 | ✅（实施记录：app/catalog 管理用例（revision+If-Match、内置岗位保护、聘任存在性校验、审计事件）；app/teamrun 引擎（Submit 幂等+并发上限、基于 team.Evaluate 的推进循环、{{team.input}}/{{steps.x.output}} 模板渲染、Cancel 在飞+待派发全传播、孤儿步骤恢复重派与 engine_lost 判死、Recover/Run 对账循环）；run 域补 StatusRunning。审计与业务变更的同事务耦合在 P6 admin API 装配时经 tx 组合器收口） |
| P6 | dashboard 聚合 + api/http 全量 + main 装配切换 | ✅（实施记录：P6a app/dashboard+api 骨架+push/agent-api 壳；P6b app/admin 鉴权+/api/admin/v2 全量端点；P6c-1 动态 /agent/{slug} 全栈（domain/platform gateway 值对象、infra/postgres 幂等租约状态机、app/dynamic、api 壳）；P6c-2 A2A router→api/http/a2a（MountA2A 具体路径注册，与 "/" 兜底共存）、users/channel-events 端点、执行后端 providers 全栈——app/backend 端口 + infra/backend（加密 CAS 组存储/Manager 热更新+2s revision 收敛轮询/admin v2 GET+PUT /providers/{id}），修复旧栈 vault_ids null 重载失败缺陷；P6c-3 cmd/ir-agent-server 组装根（四配置子系统→postgres Open+Seed→cipher→全仓储→execengine→dispatch/push/agentapi/dynamic/a2a/catalog/teamrun/dashboard/admin/backend→worker/reaper/heartbeat/teamrun 对账/provider 刷新循环→hwhkit 框架壳），web/admin.html 过渡载体（P7 重写），删除 ir/ 包+根 main.go，Makefile 构建路径 ./cmd/ir-agent-server + -X main.version。**岗位表定名 `job_positions`**：dev/prod 库与金融应用共享，`positions` 是既有持仓表，v2 岗位表避让改名。进程内 Agent Registry 退役（按 slug 直读 DB）；crypto 旧栈互验改为固化 golden vector（旧栈密文/argon2 哈希常量）。冒烟：真库启动+health/console/login 401/push 422/claim 401/seed 7 岗位全过，20 测试包绿） |
| P7 | web/ 控制台重写（八模块） | ✅（实施记录：web/ 四文件 SPA——index.html 壳、app.css（铜/银/金/钻四主题 CSS 变量，默认金；纵向侧栏业务/系统分组+56px 折叠图标栏+角标）、i18n.js（zh/en 全量对等，web 包测试守护键对等与资源引用）、app.js（hash 路由八模块、{"data"}/{"error"} 信封客户端、CSRF 随写请求、If-Match 带引号、全数据 textContent 渲染无注入面）。模块：总览统计卡、岗位 CRUD（内置删除禁用）、智能体三子页（列表/执行后端三列卡/连接器令牌一次明文）、团队 DAG 编辑器 v1（步骤表单+依赖 chips+Kahn 分层预览+提交运行）、运行日志三子页（智能体运行行展开详情+团队子运行兄弟表、团队运行步骤状态展开+取消、通道事件）、审计日志（admin 强制）、账号管理、设置（主题/语言+系统信息）。cmd 装配 /admin/ 静态服务（embed.FS，未知路径回落 index.html）。后端微调：GET /session 回发 csrfToken（刷新后免重登可写）。浏览器验收：登录→总览→岗位→执行后端→主题切换持久化→DAG 两步链保存→审计/账号/通道事件→侧栏折叠 56px→截图；验收数据已清理。21 测试包绿） |
| P8 | schema seed/回填 + 端到端冒烟 + 完成审计 | ✅（实施记录：真库 seed 7 内置岗位 + system_prompt→岗位回填实测（risk_manager→RISK_MANAGER，显式聘任不被覆盖）；协议矩阵真服务实测——push（未签名 422/字段矩阵 422/签名同步执行失败 500 与旧栈 `apierror.Internal` 逐字节一致/失败释放租约可重试）、动态入口（未签名 401+`HMAC realm="agent"`/v1 canonical 签名 503 executor_unavailable/nonce 重用 401 防重放/run_id 唯一防线 409 与旧栈一致）、agent-api（claim 204+X-Agent-Api-Reason/heartbeat 200 间隔下发/renew 404/complete 404+400 状态机/坏令牌 401/令牌签发 act_ 一次明文）、A2A（well-known 双路径+每 Agent card 200/SendMessage 参数校验 -32602/版本协商 -32009/全链 Task 信封 TASK_STATE_FAILED+history+agent_runs 落库）；团队编排真服务全链（提交 202→step 解析岗位→派发→子运行 failed 带 team_run_id/team_step_id→团队运行 failed）；控制台运行日志父子展开与连接器签发/吊销 UI 实测；冒烟夹具全部清理（0 智能体/0 团队/0 运行）。已知披露：Rust 4bc459d 条数硬约束回移待定（CHANGELOG）；AGENT_API__TOKEN_ROTATION_GRACE 未接线（轮换=立即失效，P4c 既定语义）；控制台保存后子页签回默认（UX 特性）。21 测试包绿） |

## 9. 整洁架构守护测试

`internal/architecture_test.go`：用 `go/parser` 扫描 internal/ 全部 import，
断言 §1 依赖规则；违例即测试失败。契约 golden 快照守护随 position 包迁移。
