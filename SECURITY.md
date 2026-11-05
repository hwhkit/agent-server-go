# Security Policy

## Supported versions

安全修复优先应用于 `main` 和最新稳定 tag。较早版本可能需要先升级。没有 tag 的提交不应
被描述为稳定版本。

## Reporting a vulnerability

如果仓库的 **Security → Report a vulnerability** 可用，请通过该入口私密报告。如果入口
不可用，请创建 **Security contact request** Issue，只请求维护者提供私密联系方式，
不要填写漏洞细节。

不要在公开 Issue、讨论、日志或测试夹具中提交：

- 未修复漏洞的可直接利用步骤；
- API key、签名密钥、Session Secret 或数据库凭据；
- 客户请求、运行结果、数据库转储或其他隐私数据。

私密报告应包含受影响版本/commit、最小复现、影响与前置条件，以及可能的缓解方案。
维护者会先确认收到，再协调修复和披露时间。

## Deployment baseline

- 管理面只通过 HTTPS 暴露，设置 `ADMIN__SECURE_COOKIES=true`；
- 经反向代理暴露时设置 `ADMIN__TRUSTED_PROXY_CIDRS`，且代理必须清除客户端伪造的
  `X-Forwarded-For`；未命中可信 CIDR 时应用会忽略该头；
- 应用内退避按“规范化账号 + 真实来源”隔离，避免单个攻击者锁死所有账号；反向代理仍应
  配置独立的每来源总请求速率限制，阻断轮换用户名的资源消耗；
- 明文 bootstrap 管理密码使用独立的 12–512 字节强密码，生产可直接注入 Argon2id hash；
- `ADMIN__SESSION_SECRET` 使用至少 32 字节稳定随机值，并与管理员密码、IR 密钥分离；
- Agent 与 Provider 密钥只通过秘密管理系统注入并加密落库；
- PostgreSQL、指标接口、本地 CLI 工作区和凭据目录不得直接暴露公网；
- 管理账号、Provider 与 Agent 写 API 仅允许 `admin`，`user` 保持只读且只能查看本人审计；
- OpenHands、Claude Code、DeepSeek Harness 等本地工具必须运行在独立低权限用户、容器
  或 VM 中，并限制挂载与网络出口；应用内路径校验不是 OS 沙箱；
- 带凭据的 Gateway、托管 Agent 与 callback HTTP 客户端不得跨 origin 跟随重定向；
- `.env`、生产 TOML、密钥文件、日志包和数据库备份不得提交版本控制。

## Known boundaries

- callback URL 当前要求 HTTPS（loopback 测试例外），但没有完整的私网 IP、DNS
  rebinding 或目标 allowlist 防护。生产环境应在入口约束平台 callback 地址，并通过
  网络层防火墙或透明 egress gateway 限制可达 origin；携带凭据的客户端不会自动信任
  `HTTP_PROXY` / `HTTPS_PROXY`，避免密钥被未验证代理截获；
- 可选 legacy callback token 会进入目标 URL 查询参数；服务要求
  `IR__CALLBACK_TOKEN_ORIGIN` 精确绑定受信 origin，升级已有 token 配置时必须补齐；
- Push 协议仍按平台契约使用 body-only 平台 HMAC，并以签名 body 内的 run ID 做第二道
  唯一防线；动态 Agent 的 body-only 兼容入口默认关闭，仅迁移期可显式开启，正常调用
  必须使用带 timestamp、nonce、run ID、幂等键和 body hash 的 v1 canonical；
- 动态 Agent callback 没有持久化 outbox。进程在 202 后退出可能需要按原 run ID
  对账，不能自动假定未执行；
- 运行输入、成功结果和审计记录可能含业务敏感数据。项目尚未提供完整的数据分类与
  自动留存策略，部署方需定义最小权限、备份加密与删除周期；
- 单实例的并发限制不是分布式配额，多副本部署需配置外层限流。

安全基线与保留风险还记录在
[doc/MAINTENANCE.md](doc/MAINTENANCE.md) 和
[doc/RELEASE-READINESS.md](doc/RELEASE-READINESS.md)。
