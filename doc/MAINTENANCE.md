# Maintenance and reliability notes

本文记录维护者需要持续审查的系统边界，不把“测试通过”扩张成没有风险。

## Callback delivery

- callback 每次尝试有 30 秒上限，响应体最多 64 KiB；408、429、5xx 与网络错误有界重试；
- 只有 `code=OK, accepted=true` 是成功；明确拒绝、确定投递失败与无可信回执必须分别
  记录为 `callback_rejected`、`callback_delivery_failed` 和 `outcome_uncertain`；
- callback 当前没有持久化 outbox。202 后进程退出仍可能产生未完成或结果不确定窗口；
- Push 后台执行一旦开始，任何 callback/执行终态都保留 202 幂等状态而不自动重执行；
  人工重跑前必须核对外部副作用；
- Push HMAC 没有时间戳，终态幂等行不得按时间自动删除；需要清理大响应时应先实现并保留
  不可重放的 run ID tombstone，而不是直接删除记录；
- 平台必须按 run ID 去重。运维人员在重跑前应核对平台回执和执行器副作用；
- 若未来引入 outbox，必须同时测试事务提交、lease fencing、重启恢复、密钥轮换、保留期
  和“发送成功但终态提交前崩溃”的至少一次语义。

## Provider and concurrency

- 每次任务持有不可变 Provider 快照，热更新只影响后续任务；
- Provider PUT 使用 revision/CAS，过期的 `If-Match` 返回 412；
- 共享配置组必须在同一数据库事务中保存，不能由旧快照覆盖另一项并发更新；
- 多实例通过 PostgreSQL revision 轮询最终收敛，短时间读到旧快照仍是正常边界；
- 管理台保存的非密钥 Provider 设置以数据库为准；API key 与 MCP Authorization 若由环境
  变量注入，则环境值始终覆盖数据库密文，保证重启时紧急轮换不会被旧快照回滚；
- 每个 Agent 与全局执行的并发信号量仍是单实例内状态；增加副本会提高集群总容量；
- Agent CRUD 在数据库事务中递增注册表 revision；各副本轮询并整体应用一致快照。更新
  使用行锁和可选 `If-Match`，密钥轮换不会被另一副本的旧编辑回滚；

## Security boundaries

- `admin` 才能修改 Provider、Agent 与账号；`user` 仅只读并只能看自己的审计；
- 带凭据的 HTTP 客户端不得跨 origin 跟随重定向；
- callback 目前没有私网地址/DNS rebinding 防护，生产应通过网络层防火墙或透明 egress
  gateway 限制可信 origin；不要依赖应用自动采用可能截获凭据的环境代理；
- 动态 Agent 的 legacy body-only HMAC 默认关闭；只有显式设置迁移开关才启用，调用方
  应迁移到带时间戳、nonce 和完整 canonical 的 v1 签名；
- workspace 规范化、环境变量 allowlist 与进程组清理不是 OS 沙箱。本地 Agent CLI 应在
  独立低权限用户、容器或 VM 中运行；
- 成功结果、请求、审计与数据库备份可能含业务数据。项目尚未提供完整数据分类与自动
  留存策略，部署方必须自行定义访问、脱敏、备份和删除周期。
- 管理台运行/通道列表每页最多 50 条，正文列在 SQL 查询阶段截为 4 KiB 预览（详情 2
  KiB）；不要把列表接口改回批量加载完整大字段。

## Build and dependency model

`go.mod` 暂时通过 `../.hwhkit-go-src` 的 sibling `replace` 使用固定 hwhkit 源码。
`make bootstrap-hwhkit` 必须验证 commit，而不能跟随可移动分支。升级 hwhkit 时应同时：

1. 更新固定引用与 `go.sum`；
2. 审查配置、HTTP middleware、健康检查和退出语义变化；
3. 在 fresh checkout 中执行完整门禁；
4. 在 CHANGELOG 记录兼容影响。

依赖更新使用 Dependabot 辅助，但合并前仍需 `go mod verify`、漏洞扫描和测试。自动更新
不能替代对网络、加密、数据库与 Action 权限的人工审查。OSV 精确例外、调用路径证据与
复核期限见 [依赖安全说明](DEPENDENCY-SECURITY.md)。

## Test evidence

- `make test` 不等于 PostgreSQL 集成测试；数据库门禁必须显式设置
  `TEST_DATABASE_URL`；
- 契约对照测试依赖同级 Rust 源码，standalone checkout 可能 skip；
- 发布报告应记录 commit、dirty 状态、命令、skip、数据库版本和是否使用缓存；
- race、部署脚本模拟、安全扫描、管理 API 权限与真实 callback 回执都是独立证据；
- 本地通过不等于 GitHub Actions、远端部署或渗透测试已完成。

## Repository operations

每次发布前检查默认分支、分支保护、CI 必需检查、私密漏洞报告入口、标签和发布说明。
这些 GitHub 设置不在仓库文件中，不能由本地文档证明已配置。

安全问题按 [SECURITY.md](../SECURITY.md) 私密报告。普通缺陷和功能建议使用 Issue 模板，
行为变化通过 [CHANGELOG.md](../CHANGELOG.md) 记录。
