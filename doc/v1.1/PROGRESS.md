# v1.1 迁移历史:mota-agent-server-rs → mota-agent-server-go

> 记录日期:2026-09-10。目标:把 `mota-agent-server-rs`(Rust,12,418 行)的功能完整复刻到
> `mota-agent-server-go`。
>
> **历史说明：**本文保留当时的迁移过程和一次性验证记录，不再是当前状态的权威来源，
> 也不构成“Go 与 Rust 没有差异”的保证。当前门禁、已知边界与待验证项请查阅
> [`../RELEASE-READINESS.md`](../RELEASE-READINESS.md) 和
> [`../MAINTENANCE.md`](../MAINTENANCE.md)。
>
> **更名说明（2026-11）：**产品已由 MoTA 更名为 **Intelligent Router**（Go 包 `ir`、
> 二进制 `ir-agent-server`）。本文为历史记录，正文中的 `mota-*` 仓库名、`MOTA__*`
> 环境变量与 `MoTA` 品牌字样均指更名前的旧名，不再代表当前代码。

## 0. 总体决策(已定,不再重新讨论)

1. **数据库 PostgreSQL-only**(pgx/v5)。不保留 SQLite 作为 dev 后端 —— 与 rs 语义
   (lease 接管 / `ON CONFLICT DO NOTHING` / 事务内 finalize)不一致。
2. **单 `mota` 包 + 文件按 rs 模块改名**,不镜像 rs 的多包模块树(churn 太大,收益低)。
3. **保留并改造现有 Go 测试**,不用 rs `tests/` 推倒重写(但 rs `tests/` 的场景要在 #13
   补齐等价覆盖)。
4. 契约正文程序化抽取而非手抄,长期用 `TestContractsMatchRustSource` 逐字节比对守护。
5. `executor_process.go` 必须用显式 `os.Pipe`(理由见下方「关键技术决策」)。
6. 环境变量前缀统一 `MOTA__*` / `PLATFORM__*` / `ADMIN__*`(双下划线),移除旧的
   「push_mode=async 必须配 CALLBACK_TOKEN」规则(rs 已放宽)。
7. `AgentStore` 只走 PostgreSQL + `t.Skip` 门禁(`TEST_DATABASE_URL` 未设时跳过);
   `PushIdempotencyStore` 保留 PostgreSQL + 内存双后端(rs 明确「避免单测依赖外部库」)。

## 1. 任务清单与状态

| # | 任务 | 状态 |
|---|---|---|
| 1 | 子进程层 `executor_process.go`(进程组、SIGTERM→2s→SIGKILL、prefix/tail 有界抓取、stderr 排空、审计 JSON) | ✅ 完成 |
| 2 | 本机 CLI 层 `executor_local.go`(Pi/Codex/Reasonix/Claude 四套 argv、skill/tool 选择、env 白名单、模型别名) | ✅ 完成 |
| 3 | 岗位契约(程序化抽取 + `TestContractsMatchRustSource`) | ✅ 完成 |
| 4 | Schema 校验 | ✅ 完成 |
| 5 | 输出归一化(枚举/symbol 标准化与覆盖补全) | ✅ 完成 |
| 6 | Usage 汇总与流式输出解析 | ✅ 完成 |
| 7 | Executor 主体(`executor.go`,本地/网关/进程内三种执行模式) | ✅ 完成 |
| 8 | Dispatcher + `TaskExecutionBackend` | ✅ 完成 |
| 9 | Gateway HTTP + callback 模块 | ✅ 完成 |
| 10 | Admin + Web(argon2 口令) | ✅ 完成 |
| 11 | **MOTA 协议层**(`idempotency.go` / `push.go` / `worker.go` / `Dispatcher()` 访问器) | ✅ 完成 |
| 12 | 装配(main.go/app.go 依赖注入、两个 HTTP 客户端、heartbeat store、路由挂载顺序) | ✅ 完成 |
| 13 | 移植 rs `tests/` 6 个集成测试(含 `protocol_dispatch_test.rs` 的 push/pull 双路径分发) | ✅ 完成 |
| 14 | A2A(Agent2Agent Protocol v1.0)接入,见 §9 | ✅ 完成 |

以下数字仅是 2026-09-10 当时 checkout 的一次性快照，不能代表当前分支：
**1029 个生成子测试 PASS / 0 FAIL / 0 SKIP**，当时记录 `-race`、`go vet`、
`gofmt` 无差异。当前 PostgreSQL 与跨仓契约测试可能因缺少外部条件而跳过，发布说明
必须列出真实 skip 和数据库版本，不能沿用本段数字。

## 2. #11 MOTA 协议层:已完成的部分

### 2.1 `mota/idempotency.go`(新建,约 340 行)

对应 rs `src/mota/idempotency.rs`。

- 常量:`pushLeaseStaleAfterSecs = 15*60`(租约陈旧阈值);
  `pushRecordRetentionSecs = 7*24*60*60`(已完成记录保留期)。
- 三种状态:`processing` / `sync_done` / `async_accepted`。
- `PushDecisionKind`:`PushNew` / `PushInProgress` / `PushSyncCached` /
  `PushAsyncAccepted` / `PushConflict`。
- `Begin` 决策优先级(顺序有意义):
  1. `runId`/请求哈希与既有记录不符 → `Conflict`;
  2. `sync_done` → `SyncCached`(回放响应体);`sync_done` 但 `response_body` 为
     NULL 是不自洽状态,判 `Conflict` 而不是回放空响应;
  3. `async_accepted` → `AsyncAccepted`(回放 202,不带 body);
  4. `processing` 且已陈旧(> 15 分钟)→ 接管,判 `New`;
  5. `processing` 且未陈旧 → `InProgress`;
  6. 无记录 → 插入 `processing`,判 `New`。
- PostgreSQL 路径用事务 + `SELECT ... FOR UPDATE`。**这里发现并修了一个真 bug**:
  行不存在时 `FOR UPDATE` 锁不到任何东西,两个并发 `Begin` 会同时走插入分支,
  后到的那个会撞唯一键约束变成 500(而不是应该的 `InProgress`)。修法:插入
  语句改成 `INSERT ... ON CONFLICT (key) DO NOTHING`,按 `RowsAffected` 判定
  是拿到了 `New` 还是该退回 `InProgress`。`TestPushIdempotencyConcurrentBeginGrantsSingleLease`
  守着这条(12 个并发 goroutine 只允许 1 个 `New`)。
- 双后端:`ConnectPushIdempotencyStore(ctx, databaseURL)` / 内部复用连接池的
  `NewPushIdempotencyStoreFromPool(pool)` / 单测用的 `NewInMemoryPushIdempotencyStore()`。
- 测试:`mota/idempotency_test.go`,15 组用例 × 内存/PostgreSQL 双后端 = 45 个
  子测试,PostgreSQL 子测试是真连库跑的,不是 skip。覆盖 Begin 五分支、
  CompleteSync 回放、AcceptAsync 回放、陈旧租约接管、未到阈值不接管、已完成
  记录永不被接管、`sync_done` 无 body 判 Conflict、Release 三种归属情形(自己的/
  别人的/已完成的)、租约丢失后提交报错、并发 Begin 只有一个 New。

### 2.2 `mota/push.go`(完全重写,230 → 约 800 行)

对应 rs `src/mota/push.rs`。

- 常量:`syncExecutionBudget = 285s`,`asyncExecutionBudget = 780s`(协议侧硬预算,
  与配置的 `TaskTimeout` 取更严的一个)。
- 8 个 `pushPositionTypes`(FUNDAMENTAL_ANALYST / TECHNICAL_ANALYST /
  PORTFOLIO_MANAGER / RISK_MANAGER / EXECUTION_TRADER / STRATEGIST /
  QUANT_TRADING_AGENT / SENTINEL);4 个 `pushGlobalParameterKeys`
  (language / targetMarkets / symbolPool / universeMode)。
- 主流程顺序:`validateContentType` → `requiredUniqueHeader(RunIDHeader)` →
  `requiredUniqueHeader(IdempotencyKeyHeader)` → `requiredSignatureHeader` →
  `validatePushMetadata(runID, 200)` → `validatePushMetadata(idemKey, 240)` →
  读体 → HMAC 验签(对原始字节,JSON 解析之前)→ JSON 解析 → 必须是对象 →
  `BoundTask` 解码 → `validateTask` → sha256 requestHash →
  `Idempotency.Begin` 五分支路由 → `pushTaskContext` → sync/async 分支执行。
- 错误码:`urn:mota:unsupported-content-type`(415)/ `urn:mota:invalid-json`(400)/
  `urn:mota:task-timeout`(504)/ `urn:mota:usage-unavailable`(502);缺签名头 401;
  幂等冲突 409;字段校验统一 422(Validation Failed,带 `fields[]`)。
- `pushTaskContext` 与 Gateway 网关入口的两处刻意差异:`taskBrief` **不回退到
  整个 body**;`params` **无条件**走 `task.ParamsView(raw)` —— Push 的信封是平台
  严格下发的结构,没有「业务字段平铺在顶层」这种旧协议兼容形态。
- `PushHandler` 结构体 + 两个构造入口:
  - `NewPushHandler(cfg, processor)`:内存幂等 + 进程内 `Processor`,给嵌入模式
    和现有测试用;
  - `NewPushHandlerWithBackend(cfg, backend, idempotency, httpClient)`:生产装配
    入口(#12 用),接 `*TaskExecutionBackend` + PostgreSQL 幂等存储。
- `executeWithBudget`:goroutine + `select` 实现「超时即放弃等待」,不等 Processor
  自己响应 ctx 取消(与 rs 的 `tokio::time::timeout` 语义一致)。
- `executionBudget(configured, budget)` 取代了之前的 `minDuration`:
  `configured <= 0` 时退回协议安全预算,而不是直接返回 0(0 意味着每个同步请求
  立刻 504,配置漏填时完全没有诊断价值)。这是本轮修的第一个 bug。
- `conservativeFailurePayload(positionType, language)`:异步非受信通道执行失败/
  超时时的兜底载荷,按 6 种岗位形状给出「不采取行动」的最小结构;
  `RISK_MANAGER` 的 `checks[0].reasoning` 在 rs 里是**硬编码中文**,与
  `language` 参数无关,这是刻意保留的行为(不是 bug)。
- 受信通道(`TrustedUsageReportingEnabled=true`)语义:同步分支拿不到 FINAL
  usage 直接 502;异步分支拿不到则**抑制 callback**,不发送不完整载荷。

### 2.3 测试基础设施的两个坑(排查耗时,记录下来避免重复踩)

1. **HTTP transport 会按 RFC 7230 剥掉 header 首尾空白**。想测试
   `validatePushMetadata` 拒绝「前导空格」的语义,不能通过发 HTTP 请求验证 ——
   请求头到服务端时空格已经被 Go 的 `net/http` 剥掉了。改成直接调用
   `validatePushMetadata(...)` 函数验证。
2. **`unique()` 测试辅助函数在高并发下会撞号**:原实现只用
   `time.Now().UnixNano()`,macOS 上墙钟分辨率常粗于 1ns,50 个并发 goroutine
   足以撞出重复值。撞号在幂等场景下等价于「同一个键配了不同的 runId」,
   服务端返回 409 是**正确行为**,但会让测试断言失败。修法:加一个
   `atomic.Uint64` 单调序号拼进去。`slowProcessor` 这个类型名也和
   `worker_test.go` 里已有的同名类型撞了,新测试里改名 `sleepyProcessor`。

### 2.4 `mota/push_validation_test.go`(新建,约 570 行)

覆盖 rs `validate_task` 的全部字段校验、请求头/元数据校验、504/502、以及
6 岗位保守载荷形状。具体分组:

- schemaVersion 精确匹配 / runId 必填与头体一致 / agentCode 必填 /
  callbackUrl 必填。
- positionType:8 个受支持值逐个跑通;未知值拒绝;整体缺失允许(探测任务)。
- templateInputData:必需字段;名字为空拒绝;重名拒绝;空数组本身合法。
- taskBrief:必须是对象;数组拒绝;整体缺失允许。
- globalParameters:键白名单;4 个字段各自的类型校验(language 是字符串,
  targetMarkets 是字符串数组允许空白项,symbolPool 是非空字符串数组,
  universeMode 只能是 RESTRICTED/SEED);整体缺失允许。
- upstream:DAG 结构(self/nodes/edges)的类型与非空校验。
- upstreamPayload:每项 agentInstanceId 非空、payloadResult 键必须存在
  (值本身允许 null)。
- runtimeContext:只允许 isFinal 键,且必须是布尔。
- callbackUrl:必须绝对 HTTPS;HTTP 只对回环地址放行。
- Content-Type:只接受 application/json(允许带 charset 等参数);其它类型 415。
- 三个请求头(RunID/IdempotencyKey/Signature)重复 → 422;签名缺失 → 401
  (与「重复」区分开,是凭证问题不是构造问题)。
- runId/幂等键长度上限 200/240,边界值与超界值都测。
- 504:构造一个 `sleepyProcessor` 故意超时,断言状态码、错误 type,并验证
  超时后幂等租约被释放(同一个 handler 用同一个键重试能拿到 200,而不是
  永远卡在 409)。
- 502:`TrustedUsageReportingEnabled=true` 但 `PlaceholderProcessor` 从不产出
  `Usage`,天然命中 usage-unavailable 分支。
- 6 岗位保守载荷形状全覆盖 + PORTFOLIO_MANAGER 必须是 NO_ACTION +
  RISK_MANAGER 必须是 WARN 且 reasoning 硬编码中文 + payload_info 按
  language 前缀切换中英文 + 端到端验证异步超时/受信超时两条路径。

跑完这批之后:`go test ./mota/ -count=1 -v` → **928 PASS / 0 FAIL**,
`gofmt`/`go vet` 干净。

## 3. #11 剩余三个子任务:现状与设计判断(历史记录,已按此执行完毕,见 §7.1 实际结果)

### 3.1 已完成的调研(尚未落盘任何代码改动)

对比了 rs `src/mota/worker.rs`(669 行)与 Go 现有 `mota/worker.go`(约 330 行),
差距很大,**这是一次实质性重写,不是小改**:

| 维度 | rs 现状 | Go 现状 | 需要改的地方 |
|---|---|---|---|
| 执行后端 | `TaskExecutionBackend`(Agent/Processor 两态) | bare `Processor` 接口 | `PullWorker`/`RunPushHeartbeat` 都要改成持有 `*TaskExecutionBackend` |
| claim → TaskContext | 补齐 `symbol_pool`/`universe_mode`/`upstream_symbols`/`force_failure_default`/`runtime_context` 等派生字段 | 只填 `RunID`/`TaskBrief`/`Params`,其余字段全部留空 | 要接入 `ExtractSymbolPool`/`ExtractUniverseMode`/`ExtractUpstreamSymbols`/`ExtractFailurePath` |
| complete 回传 | 用 `ProcessedOutput.to_transport_payload(None, trusted_usage_reporting_enabled)`,受信通道无 usage 直接抑制 complete | 用旧的 `CompleteRequest{PayloadResult: CompleteResult{...}}`,不走 `ToTransportPayload`,没有受信 usage 判断 | 要整块换成 `ToTransportPayload`,删掉 `CompleteRequest`/`CompleteResult`(确认过全局只有 worker.go 一处引用,可以安全删) |
| Push 心跳 | 按**每个启用 Agent 各自的通道密钥**发送心跳,依赖 `AgentStore.list_agent_secrets()`,每次心跳/自检结果写 `channel_events`(`RecordChannelEvent`) | 全局单一密钥(`cfg.SigningSecret`)心跳,不落库 | 心跳函数要改签名接收 `*AgentStore`(或允许 nil 表示嵌入模式跳过),按 Agent 循环,每次调用后 `RecordChannelEvent` |
| self-check | `run_self_check(execution, agent_code, timeout)`,真跑一次最短任务验证「收任务→出结果」整条链路 | `runSelfCheck(processor)`,签名与语义基本对应,但没接 `agentCode` 维度(单密钥场景本来就没有 agent 维度) | 改成对每个 Agent 各自跑自检,`timeout` 用 `cfg.task_timeout.min(30s)` |

### 3.2 关键设计判断(已定,继续执行时不要再纠结)

1. **`RunPushHeartbeat` 签名必须改成接收 `*AgentStore`**(可为 nil)。理由:rs 的
   按 Agent 心跳是协议要求的行为(§2.3 必需),不是平台特性的可选装饰;
   Go 现在的单密钥心跳与 rs 语义不一致,属于功能缺口而不是「简化实现」。
   main.go 里 `go mota.RunPushHeartbeat(cfg, processor, built.Shutdown())`
   这一行调用点会跟着改(补一个 store 参数),这个改动算是把 #11 缺的协议
   功能补全,不算提前侵入 #12 的装配范围 —— #12 要做的是「两个 HTTP 客户端
   分工 + 完整依赖注入顺序」,不是这一行参数补全。
2. **`CompleteRequest`/`CompleteResult` 类型直接删除**,全局搜索确认只有
   `worker.go` 一处引用,没有测试或其它文件依赖其 JSON 形状。
3. **claim 阶段的 `params` 展平逻辑**要照抄 rs 的 `claim_params`:
   `templateInputData` 是数组时展平成 `{name: value}`;是对象且有 `params`
   键时取该键;否则整体当 params。现在 Go 版本的展平逻辑更简陋(只处理了
   `obj["params"]` 这一种情况),需要补数组展平分支。
4. **worker_test.go 的 mock 队列**会跟着改:`newMockQueue` 返回的 claim 响应体
   目前是扁平的 `templateInputData: {taskBrief, params}`,这个测试夹具本身
   不需要变(依然是合法输入),但断言 complete body 的部分要跟着新的
   `ToTransportPayload` 输出形状调整(如果字段名变化)。

### 3.3 Dispatcher() 访问器(#3,尚未写)

给 `PlatformState`(`mota/platform.go`)加一个方法,和现有的 `Executor()`
按需构造 + 缓存模式保持一致:

```go
// Dispatcher 返回共享的 AgentDispatcher。首次调用时构造并缓存结果(含失败)。
func (s *PlatformState) Dispatcher() (*AgentDispatcher, error) {
    executor, err := s.Executor()
    if err != nil {
        return nil, err
    }
    s.dispatcherOnce.Do(func() {
        defaultSlug := ""
        if s.Config != nil {
            // TODO(#12): DefaultAgentSlug 目前挂在 mota.Config 而不是
            // PlatformConfig,装配阶段需要决定这个值从哪条配置流入。
        }
        s.dispatcher = NewAgentDispatcher(s.Registry, executor, s.GlobalExecution, defaultSlug)
    })
    return s.dispatcher, nil
}
```

需要在 `PlatformState` 结构体上加 `dispatcherOnce sync.Once` 和
`dispatcher *AgentDispatcher` 两个字段。**注意**:`defaultAgentSlug` 现在定义在
`mota.Config`(MOTA 接入配置)里,不在 `PlatformConfig`(平台配置)里 ——
`Dispatcher()` 访问器本身不需要这个值也能工作(可以留一个
`SetDefaultAgentSlug` 或者让 #12 装配时通过参数传入),写的时候要留意这个
跨配置对象的依赖,不要凭空往 `PlatformConfig` 加字段。

## 4. 关键技术决策(跨任务通用,遇到类似场景直接复用结论)

1. **数据库 PostgreSQL-only**(用户明确选择)。
2. **单 `mota` 包 + 文件按 rs 模块改名**(用户明确选择)。
3. **保留改造现有测试**(用户明确选择),不是以 rs `tests/` 为准重写。
4. **契约正文程序化抽取而非手抄** + 长期守护测试 `TestContractsMatchRustSource`
   逐字节比对。
5. **`executor_process.go` 必须用显式 `os.Pipe`**:`os/exec` 对非 `*os.File` 的
   `Stdout` 会注册拷贝协程,`cmd.Wait()` 会等它们,持有继承管道的孙进程会让
   `Wait()` 永久阻塞 → 假超时。
6. **修正旧 Go 两处协议错误**:`payload_info` 不再追加免责声明;占位处理器
   不再伪造 `evidenceRefs`。
7. **时间戳定宽** `2006-01-02T15:04:05.000000000Z07:00`(偏离 rs 并修 bug:
   TEXT 列 ORDER BY 时无小数会排到有小数之后)。
8. **环境变量前缀 `MOTA_*` → `MOTA__*`**(双下划线);移除旧「push_mode=async
   必须有 CALLBACK_TOKEN」规则。
9. `AgentStore` PG-only + `t.Skip` 门禁;`PushIdempotencyStore` 保留 PG + 内存
   双后端(rs 明确为「避免单元测试依赖外部 PostgreSQL」)。
10. **usage 溢出/不完整即返回 nil**,绝不上报估算值。
11. **`deepCopyJSONValue`**:rs 的 `.cloned()` 是副本,Go map 是引用,不深拷贝
    会污染运行记录里的执行器原文。
12. **内部函数返回 `*ExecutionError`/`*DispatchError` 而非 `error`**,避免
    typed-nil 装 interface 后 `!= nil` 为真。
13. **argon2 PHC 编解码自己实现**(x/crypto 只给 IDKey/Key)。参数必须与 Rust
    `Argon2::default()` 逐项一致:m=19456/t=2/p=1/keyLen=32/salt 16/v=19;
    base64 用 StdEncoding+NoPadding;**校验时用存储哈希里的参数**;argon2d
    显式拒绝。拒绝方案:引入第三方 PHC 库(可能拉不下来)。
14. **Gateway 异步分支显式移交容量许可**(sync.Once + handOff 标志),不在
    协程里 TryAcquire 重占 —— 后者在 MaxConcurrency=1 时拿不到。
15. **`mutationContext(r) = context.WithoutCancel(r.Context())`** 补齐 rs 的
    detached task 语义,防请求取消把「已写库/未应用 Registry」劈开。
16. **push 用 hwhkit `core/apierror`** 输出 RFC 7807 problem+json,匹配 rs 的
    ApiError 形状。拒绝方案:自造 writeProblem(字段名对不上 rs)。
17. **`TaskTimeout <= 0` 应退回安全预算而非立刻超时**(已落盘,`executionBudget`)。
    拒绝方案:只改 `testConfig()`(手工构造 Config 时每个请求都 504,毫无
    诊断价值)。
18. **PostgreSQL 幂等 `Begin` 的插入语句必须 `ON CONFLICT DO NOTHING`**
    (已落盘)。理由见 §2.1。
19. **测试基础设施**:HTTP header 会被 transport 剥空白,不能用来测「拒绝
    带空白的值」这类语义;`unique()` 辅助函数需要原子序号防止高并发撞号。

## 5. 构建与验证环境(每次继续前先确认)

```bash
cd /Users/louis/mota/all-mota-agent-sdk/mota-agent-server-go
export GOROOT=$PWD/.tools/go1.23.12 PATH=$GOROOT/bin:$PATH \
  GOCACHE=$PWD/.gocache GOTMPDIR=$PWD/.gotmp GOPROXY=https://goproxy.cn,direct
export TEST_DATABASE_URL='postgres://mota:mota@127.0.0.1:55440/mota_agent'
```

- `proxy.golang.org` 超时,**必须**用 `goproxy.cn`;`go mod tidy` 需加
  `GOFLAGS=-mod=mod`。
- `gofmt -w .` 会递归进 `.tools/`(vendored Go 工具链源码)→ 只能
  `gofmt -w mota/ main.go`。
- PostgreSQL 测试容器 `mota-agent-pg-test`,端口 **55440**(55432 被无关项目
  `vault-studio-pg` 占用,不要碰)。容器起法(如果需要重建):
  ```bash
  docker run --rm -d --name mota-agent-pg-test -p 55440:5432 \
    -e POSTGRES_USER=mota -e POSTGRES_PASSWORD=mota -e POSTGRES_DB=mota_agent \
    postgres:16-alpine
  ```
- 标准验证顺序:
  ```bash
  gofmt -w mota/ main.go
  go build ./...
  go vet ./...
  go test ./... -count=1
  go test -race ./... -count=1
  go test ./mota/ -count=1 -v 2>&1 | grep -oE '^\s*--- (PASS|FAIL|SKIP)' | sed 's/^ *//' | sort | uniq -c
  ```

### 最近一次验证记录(2026-09-10,#13 全部完成后,#1~13 除 A2A 全部完成)

```
gofmt: 无差异(mota/ main.go)
go build ./...: 通过(含 main 包,额外用 go build -o /tmp/mota-agent-server . 验证过可执行文件真的能链接)
go vet ./...: 通过
go test ./... -count=1: ok
go test ./mota/ -count=1 -v: 954 --- PASS,0 FAIL,0 SKIP
go test -race ./... -count=1: ok(#11 阶段修复了一处真实数据竞争,§7.1;
  #13 阶段修复了一处测试自身的时序竞争 —— 不是 -race 能抓到的数据竞争,
  是「断言跑在异步写库完成之前」的逻辑竞争,见 §7.4)
```

继续推进时以「改动后重新跑一遍统计」为准,不要用本文档里的数字做假设。

## 7. Step A / Step B / Step C 执行记录(#11 收尾 + #12 + #13 全部)

### 7.1 Step A —— #11 剩余三个子任务

**`mota/worker.go` 完全重写**(约 330 → 约 570 行):

- `PullWorker` 改持有 `*TaskExecutionBackend`;`NewWorker(cfg, processor,
  token)` 保留作嵌入/测试便捷构造(内部包一层 `NewProcessorBackend`),新增
  `NewWorkerWithBackend(cfg, execution, token)` 作生产装配入口 —— 与
  `push.go` 的 `NewPushHandler`/`NewPushHandlerWithBackend` 两层设计一致。
- claim 阶段补齐了 `TaskContext` 全部派生字段:`SymbolPool`/`UniverseMode`/
  `UpstreamSymbols`/`ForceFailureDefault`/`RuntimeContext`/`Language`,复用
  `processor.go` 里现成的 `Extract*` 函数。`claimParams` 补了数组展平分支
  (`[{name,value}]` → `{name: value}`),对齐 rs 的 `claim_params`。
- complete 回传改用 `output.ToTransportPayload(nil,
  cfg.TrustedUsageReportingEnabled)`;受信通道且 `Usage == nil` 时抑制
  complete(不发请求)。删除了 `CompleteRequest`/`CompleteResult` 两个旧
  类型(确认过全局只有 worker.go 一处引用)。
- `RunPushHeartbeat` 签名改为 `(cfg, execution, token, store *AgentStore)`。
  `store == nil` 时直接跳过并 warn(嵌入模式无 Registry 可枚举 Agent 通道
  密钥,对齐 rs「heartbeat skipped: platform store unavailable」)。循环体
  按 `store.ListAgentSecrets(ctx)` 遍历每个 Agent,`agentHeartbeatState`
  各自维护心跳间隔/自检标记/上次发送时间(对应 rs 三个 HashMap)。每次
  心跳(含自检)结束都调 `store.RecordChannelEvent(...)` 落 `channel_events`。
- `runAgentSelfCheck` 取代原来裸 `Processor` 的 `runSelfCheck`,签名变成
  `(execution *TaskExecutionBackend, agentSlug string, timeout)`。

**测试改造**:`worker_test.go` 里 `TestRunSelfCheckPassesWithPlaceholderProcessor`
等改名并适配新签名;`TestPushHeartbeatSendsSelfCheckOnDemand` 整个重写成
`TestPushHeartbeatSendsSelfCheckOnDemandPerAgent`,用真实 PostgreSQL 建一个
测试 Agent,断言心跳按该 Agent 自己的密钥发送且落了 `channel_events`;新增
`TestPushHeartbeatSkippedWithoutStore` 覆盖 `store == nil` 的直接返回路径。

**`PlatformState.Dispatcher()` 访问器**:与 `Executor()` 同款懒加载模式
(`sync.Once` 缓存构造结果含失败),新增 `DefaultAgentSlug string` 字段与
`SetDispatcher`(测试注入用,抑制按需构造)。

**发现并修复的一个真实数据竞争**(`-race` 抓到的,不是理论推测):
`push.go` 的 `executeWithBudget` 在 goroutine 里直接闭包引用 `h.Execution`
字段。超时路径会放弃等待这个 goroutine,但**不会杀死它**——它还在跑,还在
读这个字段。如果调用方在这个被放弃的 goroutine 还没退出时又给
`PushHandler.Execution` 赋了新值(`TestPushSyncTimeoutReleasesLeaseForRetry`
这个测试恰好这么干:同一个 handler 换后端重试),就会跟字段写入形成数据
竞争。修法:进入 goroutine 前把 `h.Execution` 捕获成局部变量。

验证:`go test ./mota/ -count=1 -v` → 929 PASS;`go test -race ./...` 首次
跑出上述竞争,修完后干净。

### 7.2 Step B —— #12 装配

**`mota/platform.go`**:新增 `PlatformDependencies` 结构体(对应 rs
`PlatformDependencies`),字段:`ExecutorHTTPClient`/`CallbackHTTPClient`/
`SigningSecret`/`CallbackToken`/`TrustedUsageReportingEnabled`/
`DefaultAgentSlug`。`NewPlatformState(ctx, cfg, processor)` 保留原签名作
嵌入/测试便捷构造,内部转发到新增的 `NewPlatformStateWithDependencies(ctx,
cfg, processor, deps)`(传空 `PlatformDependencies{}`,验证过两者行为
等价,见 `TestNewPlatformStateIsEquivalentToEmptyDependencies`)。原来标
`TODO(#12)` 的三个字段注释已更新为「经 PlatformDependencies 注入」。

**`main.go` 完全重写**(增加了 `App.execution` 字段与 `App.pushHandler`
方法):

- 两个独立 HTTP 客户端,只在平台开启时构造(平台关闭时网关/回调都不存在,
  没必要建):
  - `executorHTTPClient`:自定义 `http.Transport`,`DialContext` 超时 5s、
    `KeepAlive` 60s,`IdleConnTimeout` 90s、`MaxIdleConnsPerHost` 32,
    **不设 `http.Client.Timeout`**(总时限交给 `AgentExecutor` 的单一
    deadline,避免全局超时截断长推理请求,这是 rs 侧注释里明确强调的点)。
  - `callbackHTTPClient`:`DialContext` 超时 5s,`http.Client.Timeout: 30s`,
    给 callback 回传与心跳用。
- `task_execution` 构造顺序完全对齐 rs:默认
  `execution := mota.NewProcessorBackend(processor)`;平台开启时
  `execution = mota.NewAgentBackend(dispatcher)`,`dispatcher` 来自
  `platform.Dispatcher()`。Pull worker(`NewWorkerWithBackend`)与 Push 心跳
  (`RunPushHeartbeat`)现在共用这同一个 `execution` 实例。
- `heartbeatStore`:平台开启时 `= platform.Store`,否则保持 `nil`
  (`RunPushHeartbeat` 内部据此跳过)。
- **Push 的幂等存储**:这是本次装配里除了 rs 清单外额外发现并补上的一块——
  之前 `main.go` 一直用 `mota.NewPushHandler(motaCfg, processor)`,即**内存
  幂等存储 + 嵌入式 Processor**,从未真正装配过 `PushHandlerWithBackend`。
  新增 `App.pushHandler(ctx, cfg)`:平台开启时用
  `NewPushIdempotencyStoreFromPool(ctx, platform.Store.Pool())` 复用同一个
  PostgreSQL 连接池(避免同进程对同一个库开两组连接);平台关闭时用
  `ConnectPushIdempotencyStore(ctx, os.Getenv("DATABASE_URL"))` 独立连接
  (对齐 rs `router_internal` 里 `idempotency` 的构造分支)。Push 使用的
  执行后端也换成了共享的 `execution`(而不是各自 new 一个
  `NewProcessorBackend`),否则 Push 走的 Agent 分发会跟 Pull/心跳走的不是
  同一套准入闸,容量控制形同虚设。
- 路由挂载顺序核对过 rs `src/app.rs` 的 `router_internal`:Push → (平台
  开启时) `/agent/{slug}` → admin。Go 侧现状顺序一致,未改。

**新增测试** `mota/platform_wiring_test.go`:
`TestPlatformDependenciesFlowIntoState`(6 个字段逐一断言注入生效)、
`TestNewPlatformStateIsEquivalentToEmptyDependencies`、
`TestPlatformDispatcherResolvesRegisteredAgent`(真实建一个 Agent,走
`Dispatcher().Execute()` 端到端跑通)、`TestPlatformDispatcherIsCached`
(两次调用必须拿到同一实例,否则三条协议入口的容量闸各自为战)、
`TestPlatformSetDispatcherSuppressesLazyBuild`。

**验证**:`go build -o /tmp/mota-agent-server .` 额外确认了 `package main`
真的能链接成可执行文件(`main.go` 不在 `go test ./mota/...` 覆盖范围内,
必须单独 build 才能验证)。`go test ./... -count=1` 与 `go test -race
./... -count=1` 均干净,`go test ./mota/ -count=1 -v` → 934 PASS。

### 7.3 Step C —— #13 移植 rs `tests/` 集成测试

读完了 rs `tests/` 目录下全部 6 个非 A2A 集成测试文件(`scheduling_test.rs`/
`executor_modes_test.rs`/`postgres_store_test.rs`/`protocol_dispatch_test.rs`/
`push_test.rs`/`worker_test.rs`),逐个测试函数核对 Go 侧是否已有等价覆盖。

**结论:绝大多数场景已经覆盖,且 Go 侧的覆盖方式往往更细粒度**(rs 用「起一个
假 shell 脚本,靠 `case " $* " in ... exit N` 断言 argv」的黑盒方式测本机 CLI
参数拼装;Go 侧在 `executor_local_test.go` 里直接单测 `agentCommand`/`piCommand`
等函数的返回值,断言更精确、不依赖起子进程)。具体核对结果:

- `scheduling_test.rs`(7 个测试:`ExecutorKind`/`ExecutionMode` 能力矩阵、
  `AgentInput` 校验、准入容量语义、Registry 热更新复用/重建信号量)——
  容量与热更新部分在 `platform_registry_test.go` 里覆盖得比 rs 更全;但
  **能力矩阵解析(`ParseExecutorKind`/`ParseExecutionMode`/`SupportsLocalCLI`/
  `SupportsGateway`)与 `AgentInput.Validate` 的拒绝分支在 Go 侧完全没有独立
  单测**——只在需要真实 PostgreSQL 的 `platform_store_test.go` 里被 happy path
  间接跑过一次。补了 `mota/platform_model_test.go`(约 260 行,不需要数据库),
  覆盖:`ExecutorKind`/`ExecutionMode` 解析与能力矩阵、`AgentInput.Validate`
  的十余个拒绝分支(secret 长度、slug 格式、name/description 长度、
  签名编码、执行器/模式不兼容组合、workspace_path 逃逸、permission_mode、
  数值范围、allowed_tools 校验)与 `applyDefaults` 顺序。
- `executor_modes_test.rs`(约 20 个测试:gateway Anthropic/OpenAI 协议、
  内置 Rust processor、保守缺省理由、dispatcher 默认 Agent 回落、gateway
  重试/用量聚合/deadline 不重置/输出大小上限、本机 CLI 四种执行器、Pi
  岗位契约重试、Pi 输出尾部有界抓取、Pi 技能/工具/金融扩展参数拼装)——
  **全部已覆盖**,分布在 `executor_test.go`(网关部分)与
  `executor_local_test.go`(本机 CLI argv 拼装部分)。
- `postgres_store_test.rs`(3 个测试,需要真实 PostgreSQL,rs 侧用
  `#[cfg(feature = "postgres-integration")]` 门禁,Go 侧对应
  `TEST_DATABASE_URL` 门禁)——Agent CRUD/nonce/run 事务往返、Push 幂等冲突
  已在 `platform_store_test.go` 与 `idempotency_test.go` 里覆盖;第三个测试
  `get_run_reads_single_record_and_a2a_task_snapshot` 依赖 A2A 模块
  (`A2aTaskStore`),Go 侧没有 A2A,这部分随 A2A 缺口一起记录在 §7.4,
  不单独处理。
- `protocol_dispatch_test.rs`(1 个测试,**本次任务的重点**)——
  `push_and_pull_both_dispatch_to_local_pi`:验证 Push 与 Pull 两条协议入口
  最终都通过同一个 `AgentDispatcher` 调用同一个本机 Pi Agent。Go 侧原来
  完全没有等价物(Push 与 Pull 各自的测试都只用 `NewProcessorBackend`,
  从未验证过它们共享同一个 `AgentDispatcher` 时的端到端行为)。新建
  `mota/protocol_dispatch_test.go`(`//go:build unix`,对应 rs 的
  `#![cfg(unix)]`):组装一个指向假 Pi 脚本的 `TaskExecutionBackend`,
  分别通过 `NewPushHandlerWithBackend`(同步 200)与
  `NewWorkerWithBackend`(claim→complete)驱动,断言两条路径都返回了
  `AAPL.US` 的评估结果,且假 Pi 脚本的调用计数(marker 文件行数)恰好是
  2 —— 证明两条协议入口真的各自触发了一次真实的本机进程执行,不是
  各自兜底出一份占位结果。
- `push_test.rs`(约 15 个测试)——绝大多数已被 `push_test.go` +
  `push_validation_test.go` 覆盖(受信 usage、payload_info 不受 language
  影响、v3 契约字段、签名校验、无效 JSON/422、跨进程幂等回放/冲突、
  async v2 签名+回执、探测任务、callback 重试、错误脱敏);callback 重试
  额外在 `callback_test.go` 的单元测试里覆盖得更细。
- `worker_test.rs`(约 10 个测试)——claim/complete/heartbeat/renew/超时/
  拒绝/处理器失败等语义已在 `worker_test.go` 里覆盖(且 #11 阶段针对新的
  `TaskExecutionBackend` 语义重写过);**唯一的真实缺口是受信通道的用量
  语义**(complete 必须携带 FINAL usage;拿不到 usage 必须抑制 complete 而
  不是发不完整结果)——rs 侧有
  `trusted_worker_complete_includes_final_usage` 与
  `trusted_worker_suppresses_complete_when_usage_is_missing` 两个测试,
  Go 侧完全没有等价物(#11 阶段重写 complete 逻辑时补了这个行为,但没补
  对应测试)。新增
  `TestWorkerTrustedCompleteIncludesFinalUsage`/
  `TestWorkerTrustedSuppressesCompleteWhenUsageIsMissing`。

**踩到的一个测试时序坑**(不是 `-race` 能抓到的数据竞争,是逻辑竞争):
`TestPushHeartbeatSendsSelfCheckOnDemandPerAgent` 断言「至少 2 条
`channel_events` 记录」时,直接在 `waitFor(... c.count() >= 2)` 之后就
`token.Cancel()` 再查库。但 `RecordChannelEvent` 发生在「HTTP 心跳请求已经
收到响应」**之后**,`c.count() >= 2` 只保证第二次心跳的 HTTP 往返完成,
不保证它的落库也完成了 —— `token.Cancel()` 可能正好在这次写入还没提交时
把它的 `ctx` 取消掉,导致写入报 `context canceled` 而失败,断言就从「至少
2 条」变成「只有 1 条」。这条在 `-race` 单跑时不一定触发(时间窗口太窄),
但连续跑几次就会偶发。修法:把「等 2 条心跳」换成「等 2 条 channel_events
记录」,`token.Cancel()` 挪到确认记录已经落库之后。

### 7.4 已知缺口(历史记录):A2A 接入未实现,已在 §9 补齐

rs 侧有完整的 A2A(Agent-to-Agent)模块,`src/a2a/` 目录下 10 个文件
(`auth.rs`/`card.rs`/`config.rs`/`handler.rs`/`jsonrpc.rs`/`mapping.rs`/
`mod.rs`/`skills.rs`/`store.rs`/`types.rs`),挂载在 `/.well-known/
agent-card.json` 与 `{base_path}/{slug}` 两个路径,依赖平台的 Registry 与
执行器(`AgentDispatcher`)。**Go 侧目前完全没有 A2A 的任何代码**(全仓库
搜索 `a2a`/`A2A` 零匹配)。

这不在 #11~#13 最初拆解的任务范围内(原始清单 1-13 没有提到 A2A),`main.go`
的重写也没有补这一块 —— 按「不擅自扩大范围」的原则,这里**明确记录为缺口
而不是默默实现或默默跳过**。是否要把 A2A 列为 #14 单独任务,需要用户确认;
在得到确认前不会主动开工。

## 8. 后续执行计划(按顺序执行,不要跳步)

### Step A —— #11 剩余三个子任务(已完成,记录见 §7.1)

### Step B —— #12 装配(已完成,记录见 §7.2)

### Step C —— #13 移植 rs `tests/` 集成测试(已完成,记录见 §7.3)

## 9. #14 A2A(Agent2Agent Protocol v1.0)接入(已完成)

§7.4 记录的缺口已经补上。rs `src/a2a/` 10 个文件全部按 1:1 语义移植到 Go
(新增 9 个 `mota/a2a_*.go` 源文件,约 2400 行):

| rs 文件 | Go 文件 | 内容 |
|---|---|---|
| `types.rs` | `a2a_types.go` | TaskState/Role/Part/Message/Artifact/Task/AgentCard 等全部数据模型,camelCase JSON、SCREAMING_SNAKE_CASE 枚举、oneof 靠成员存在性判别 |
| `jsonrpc.rs` | `a2a_jsonrpc.go` | JSON-RPC 2.0 信封 + `A2AErrorKind`(14 种错误码,-32700~-32009)+ ProtoJSON `google.rpc.ErrorInfo` 包装 |
| `auth.rs` | `a2a_auth.go` | Bearer token 派生(`a2a_` + HMAC-SHA256(signing_secret, "a2a-bearer-v1\n{slug}"))+ 定长比较校验 |
| `skills.rs` | `a2a_skills.go` | 6 个预置岗位 -> `A2AAgentSkill` 静态映射表,tags 必填非空 |
| `card.rs` | `a2a_card.go` | Agent Card 构造:`supportedInterfaces` 有序数组、`provider.organization`、`capabilities.extendedAgentCard` |
| `store.rs` | `a2a_store.go` | `A2ATaskStore` 内存 + PostgreSQL 双后端,复用 `agent_runs` 表(`Task.ID = run_id`),不新建任务表 |
| `mapping.rs` | `a2a_mapping.go` | `A2AMessage` <-> `TaskContext`/`ProcessedOutput` 双向映射:DataPart 直通 + TextPart 降级两条通道 |
| `handler.rs` | `a2a_handler.go` | JSON-RPC 方法分派:`SendMessage`/`GetTask` 已实现,`SendStreamingMessage`/`SubscribeToTask`/`CancelTask`/`ListTasks` 声明但返回 `UnsupportedOperation` |
| `config.rs` | `a2a_config.go` | 独立 `[a2a]` 配置段(不挂到 `mota.Config`,理由与 rs 一致:避免打断既有测试的结构体字面量) |
| `mod.rs` | `a2a_router.go` | 路由组装:`/.well-known/agent-card.json` + 别名 `/.well-known/agent.json`、`{base_path}/{slug}/agent-card.json`、`{base_path}/{slug}` |

**装配到 `main.go`**:`App` 新增 `a2aConfig` 字段;`main()` 启动期
`LoadA2AConfig()` 失败即退出(配置错误立即失败而不是等到首个请求);
`BuildRouter` 里平台开启且 `a2aConfig.Enabled` 时挂载 `A2ARouter`,平台关闭
但 A2A 声称开启时只 warn 不启动(A2A 依赖 Registry 与执行器,两者都由平台
提供,与 rs 语义一致)。`config/default.toml` 补了 `[a2a]` 段,默认
`enabled = false`(A2A 能触发本机 CLI 子进程,必须显式开启)。

**Go 侧复用了已有基建**,没有重新发明:`RoleDescriptor` 加了
`RequiredFields` 字段并新增 `ResolveRoleDescriptor` 导出函数(原来只有
`resolveRoleContract` 私有版本);`AgentStore` 加了 `GetRun`(按主键单读,
`ListRuns` 是面向管理台的倒序列表,定位不到具体一条);`randomToken`/
`AgentDispatcher`/`AgentRegistry` 直接复用,一行都没改。

**测试**(全部新增,rs 侧原有测试逐个核对后 1:1 搬运,只做 Go 语言层面的
表达调整,行为断言未删减):

- `a2a_config_test.go` — 7 个(禁用默认值、base_path 尾斜杠/绝对路径校验、
  provider 字段配对、default_card_slug 回退、`config/default.toml` 可加载、
  disabled 时跳过校验)。
- `a2a_auth_test.go` — 5 个(token 不含明文密钥、绑定 slug+secret、大小写
  不敏感、缺失/错误方案报 Missing、错误 token 报 Invalid)。
- `a2a_skills_test.go` — 5 个(6 个预置岗位都有 profile、tags 必填、空
  prompt 回落基本面、自定义 prompt 走 custom skill、description 永不为空)。
- `a2a_card_test.go` — 5 个(interface 携带 url/binding/version、v0.3 字段
  不出现、Bearer scheme 用 oneof 成员名、description 回落、鉴权可关闭时
  不声明安全字段)。
- `a2a_store_test.go` — 6 个(内存后端:运行中报 Working、完成态往返
  artifacts、重复 task id 判冲突、重复 finish 报错、未知任务返回 nil、
  损坏快照优雅降级)。
- `a2a_mapping_test.go` — 10 个(DataPart 直通解析、positionType 岗位回落、
  纯文本降级、文本并入已有 brief 不覆盖、失败路径关键词检测、多文本块拼接、
  非对象 data 降级到文本通道、自定义 prompt 不设 positionType、artifact
  两段式结构、空摘要省略)。
- `a2a_router_test.go` — 11 个(HTTP 层 card 端点:v1.0 interface 结构、
  v0.3 字段缺失、skills 映射、Bearer scheme、两个 well-known 路径、无默认
  Agent 时 404、停用 Agent 404、未知 slug 404、slug 大小写不敏感、自定义
  base_path、Card 反序列化回强类型)。
- `a2a_rpc_test.go` — 24 个(HTTP 层 JSON-RPC 端到端,走真实本机 Pi CLI:
  SendMessage 完整产出、历史回显、DataPart 驱动岗位契约、GetTask 读取/
  historyLength=0/未知 id、执行失败态、停用 Agent 404、未知方法、4 个
  声明未实现方法、JSON 解析错误、jsonrpc 版本错误、message/parts 字段级
  校验 3 项、推送通知拒绝、returnImmediately 拒绝、多轮会话复用 contextId、
  A2A-Version 协商、4 个鉴权场景)。
- `a2a_postgres_test.go` — 1 个(需要 `TEST_DATABASE_URL`:`GetRun` 单读、
  A2A task 生命周期在真实 `agent_runs` 表里的完整往返,对应 rs
  `postgres_store_test.rs::get_run_reads_single_record_and_a2a_task_snapshot`)。

共 **74 个新测试**,全部一次性通过,没有出现需要回头修的实现 bug(A2A 是
在 #1~13 的基础设施——`AgentRegistry`/`AgentDispatcher`/`AgentExecutor`/
`ProcessedOutput`——已经就位之后做的薄协议适配层,复用面很大)。

**验证**(2026-09-10,A2A 完成后):

```
gofmt: 无差异(mota/ main.go)
go build ./...: 通过;go build -o /tmp/... .: 可执行文件真的能链接
go vet ./...: 通过
go test ./... -count=1: ok
go test ./mota/ -count=1 -v: 1029 --- PASS,0 FAIL,0 SKIP(#13 完成时是 954,
  A2A 新增 74 个 + 1 个既有基建测试因加了 GetRun 之类的辅助改动)
go test -race ./... -count=1: ok
```

截至该历史快照，当时列出的 1-14 号迁移项已完成，包括 A2A 模块。之后 Rust 与 Go
实现继续独立演进；callback 崩溃窗口、Provider 并发更新、部署门禁等差异必须按当前
代码重新审查，不能由这份旧快照推断完成。

## 2026-09-16 · usage 改为每次结果回传必填（RULE-BND-001 对齐）

线上后端自 2026-09-15 起对所有 Bound 结果回传（Push 同步 200 / Push 异步
callback / Pull complete）强制顶层 `usage`，缺失判 `RUNTIME_USAGE_MISSING`
拒收（本仓实测：sync 200 三键信封静默拒收、async callback 回执
`accepted:false` + `RUNTIME_USAGE_MISSING`）。旧实现「普通通道不带 usage、
仅受信 Host 携带」与运行期规则冲突，本次对齐：

- `ToTransportPayload` 去掉 `includeUsage` 门控：执行器报告用量用真实值；
  无计量兜底零值 FINAL usage（0/0/0、invocationCount=1、非估算）。
- `TrustedUsageReportingEnabled` 语义收窄为「受信严格模式」：开启后拿不到
  供应商 usage 仍判失败（同步 502 / 异步抑制 callback），不走零值兜底。
- 同步修改 push.go / worker.go / platform_gateway.go 调用点与
  processor_output_test.go / push_test.go 断言（信封 4 键）。
- 同根因的 mota-creator-web 随包 Push/Pull 开发者工具包已于同日修复并推送。

补充（同日第二轮实测）：零值兜底被平台判 `RUNTIME_USAGE_INVALID`——运行期
要求 `totalTokens > 0` 且等于 `promptTokens + completionTokens`（冻结契约
`minimum: 0` 与实现不符）。兜底改为最小合规值 1/1/2（`FinalReportedUsage(1,
nil, 1, nil, 1)`）。
