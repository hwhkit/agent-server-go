# Release readiness checklist

更新时间：2026-10-02。

本文件是发布证据模板，不是发布批准。除非每项都附有当前 commit 的可复核结果，否则不得
声称“全部测试通过”“已部署”或“没有已知差距”。

## Source identity

- [ ] 记录完整 commit SHA 与 dirty/clean 状态；
- [ ] GitHub 默认分支指向维护中的 `main`；
- [ ] release tag 不可变，版本号和 CHANGELOG 一致；
- [ ] fresh checkout 可通过 `make bootstrap-hwhkit` 获得固定依赖并构建。

## Local gates

```bash
make bootstrap-hwhkit
make lint
make test
make race
make build
make deploy-build
make verify
make test-deploy-scripts
```

- [ ] 命令均针对当前 commit 运行，不使用旧二进制作为证据；
- [ ] 记录全部 skip；跨仓契约测试若 skip，明确标为未验证；
- [ ] 管理台或 API 文案变化经过对应交互/权限回归；
- [ ] 最终 Linux ARM64 产物记录大小、格式和 SHA-256。

## PostgreSQL matrix

针对可销毁的 PostgreSQL 15、16、17 分别运行：

```bash
TEST_DATABASE_URL='postgres://...' make test-pg
```

- [ ] 基线 schema 升级保留已有 Agent、用户、会话、Provider、运行和幂等记录；
- [ ] 同 Provider revision 并发写只有一个成功；不同 Provider/共享组并发更新不丢失；
- [ ] 多实例 Provider 变更最终收敛，读到的 config 与 revision 属于同一快照；
- [ ] 多实例 Agent 创建、停用、删除与密钥轮换最终收敛，陈旧 revision 写入返回 412；
- [ ] callback 接受、拒绝、投递错误与 nil/非法回执产生正确运行终态；
- [ ] 幂等重放不会把 callback 失败显示为成功，也不会无条件重复有副作用执行；
- [ ] Agent/Provider 密钥在数据库中保持密文。

## Callback regression

- [ ] nil payload、nil receipt 和异常执行路径不会 panic；
- [ ] accepted=true 才完成，accepted=false 为 `callback_rejected`；
- [ ] 确定未接收为 `callback_delivery_failed`，无可信回执为 `outcome_uncertain`；
- [ ] 408、429、5xx 使用原 run ID 有界重试；
- [ ] 单次尝试 30 秒、正文 64 KiB 上限生效；
- [ ] redirect 不把签名或 token 带到不同 origin；
- [ ] 日志和通道事件不泄露 token、密钥或未经限制的响应体；
- [ ] 文档明确当前没有持久 callback outbox，不宣称跨进程自动恢复。

## Security and permissions

- [ ] 普通用户对 Provider、Agent 与账号写 API 均返回 403；
- [ ] 普通用户只能读取自己的审计，管理员可读取全部；
- [ ] `.env.example` 的空值和已知占位符不能形成可启动的公开凭据；
- [ ] 管理明文密码限制为 12–512 字节，哈希保持 Argon2id；
- [ ] 带凭据客户端禁止跨 origin redirect；
- [ ] 对最终 `go.sum` 运行 govulncheck 或 OSV，记录工具版本和结果；
- [ ] 确认动态 Agent legacy HMAC 保持关闭；人工复核 callback SSRF、本地 CLI 沙箱和数据留存等保留风险。

## Deployment simulation and staging

- [ ] 模拟覆盖哈希不符、并发部署锁、传输中断、健康超时与成功回滚；
- [ ] `deploy-full` 在替换二进制前完成精确版本 Pi 校验；
- [ ] staging 使用实际 systemd unit、环境文件和数据库执行重启；
- [ ] `/health/ready` 验证 PostgreSQL，运行进程哈希匹配预期产物；
- [ ] staging 验证登录、RBAC、Provider 热更新、同步请求、callback 和审计；
- [ ] 回滚步骤在兼容 schema 上实际演练。

## Remote gates

- [ ] GitHub pull request 与 `main` workflow 实际成功；
- [ ] PostgreSQL 15/16/17 jobs 均成功；
- [ ] dependency/security job 成功且 Action 使用不可变 commit SHA；
- [ ] branch protection 将所需 jobs 设为 required；
- [ ] Security Policy 与私密漏洞报告渠道可访问；
- [ ] 仓库可见性符合发布计划。

## Sign-off

发布说明必须分别列出：已验证、未验证、已知边界、迁移步骤和回滚方法。创建 tag 或部署
生产是单独的授权操作；完成本清单本身不会执行 commit、push、tag 或部署。
