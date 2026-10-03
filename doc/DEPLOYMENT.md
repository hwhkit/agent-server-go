# Deployment guide

本文描述 Makefile 管理的 Linux ARM64 systemd 部署。执行前先确认目标主机、架构、服务名
和就绪地址，不要把示例默认值直接用于生产。

## Prerequisites

本地需要 Go 1.26.8、Git、SSH/scp 和 hwhkit-go 固定源码。远端需要：

- AArch64 Linux 与 systemd；
- `flock`、`sha256sum`、`mktemp`、`sudo`、`curl`；部署账号需免交互执行目标
  service 的 `systemctl restart`，并可通过 `sudo sha256sum /proc/<pid>/exe` 验证
  跨 UID 运行进程；
- 已存在且权限正确的二进制目录、systemd unit 与环境文件；
- 能从本机访问的 PostgreSQL，以及服务可访问的 loopback `/health/ready`。

先初始化依赖并检查部署脚本：

```bash
make bootstrap-hwhkit
make test-deploy-scripts
```

## Variables

| 变量 | 用途 |
|---|---|
| `REMOTE_HOST` | SSH 目标，例如 `ubuntu@mota-test` |
| `REMOTE_ROOT` | Go 服务独立目录 |
| `REMOTE_BIN` | 远端二进制绝对路径 |
| `REMOTE_SERVICE` | systemd unit 名称 |
| `REMOTE_ENV` | systemd 使用的环境文件 |
| `REMOTE_HEALTH_URL` | 远端进程自身访问的就绪 URL |
| `REMOTE_HEALTH_ATTEMPTS` | 重启后的最大就绪尝试次数 |
| `REMOTE_HEALTH_INTERVAL` | 就绪尝试间隔秒数 |
| `HEALTH_URL` | 运维人员从当前机器检查的公开就绪 URL |
| `EXPECTED_SHA256` | 待运行产物的 SHA-256；通常由构建目标计算 |
| `PI_NPM_PACKAGE` | 可选 Pi CLI 的精确 `name@version`，禁止 `@latest` |

变量包含空白或 shell 元字符时应由脚本拒绝，而不是尝试拼接到远端命令。

`ADMIN__PASSWORD` / `ADMIN__PASSWORD_HASH` 只用于首次创建配置中的 bootstrap 管理员；
数据库里账号已存在时，重启不会覆盖其密码、角色或状态。后续轮换应通过账号管理 API，
避免把环境变量改动误认为已经更新现有账号。

## Safe rollout

```bash
make deploy-build
make deploy-check
make deploy-upload
make deploy-restart
```

`make deploy` 串行执行这三个阶段：

1. 生成精简的 Linux ARM64 静态二进制与本地 SHA-256；
2. 上传到唯一临时文件，关闭上传 writer 后在远端复算哈希；
3. 在部署锁内把验证后的文件发布为 SHA 命名的 staged artifact；
4. 受控重启时备份当前二进制并原子替换；
5. 轮询 systemd、`/health/ready`，并核对 `/proc/<pid>/exe` 与预期 SHA-256；
6. 新版本未就绪时恢复上一版本并再次验证。

上传本身不应替换正在使用的二进制，避免上传与计划重启之间发生自动重启时提前运行未
验收版本。两次部署不能共享同一个可写 `.new` 文件，远端锁必须覆盖替换与重启。

## Pi CLI

Pi 是独立供应链依赖。只有明确需要升级时才运行：

```bash
make deploy-pi PI_NPM_PACKAGE='@earendil-works/pi-coding-agent@X.Y.Z'
```

脚本应在写 `REMOTE_ENV` 前核对 Node.js、npm 安装结果、包名、精确版本和最终 `pi`
命令路径。`make deploy-full` 的顺序是 build → Pi 校验/安装 → upload → restart，Pi
失败不能先替换生产二进制。

## Configuration and database

部署脚本不上传生产密钥。使用 root-only 环境文件或秘密管理系统注入：

- `DATABASE_URL`；
- `MOTA__SIGNING_SECRET`、bind token 与 callback token；使用 callback token 时必须同时
  配置仅含 scheme + host 的 `MOTA__CALLBACK_TOKEN_ORIGIN`；
- `ADMIN__PASSWORD` 或 Argon2id hash；
- 稳定的 `ADMIN__SESSION_SECRET`；
- Provider API key 和 MCP Authorization。

若管理面位于反向代理后，必须把代理直连地址配置到逗号分隔的
`ADMIN__TRUSTED_PROXY_CIDRS`。代理需覆盖客户端传入的 `X-Forwarded-For`；否则不要配置
可信代理，应用将只按 TCP 对端做登录退避隔离。

在升级前备份 PostgreSQL 与加密主密钥。只有数据库备份而没有原 Session Secret，无法
解密 Agent 与 Provider 凭据。

本版本会为 `push_idempotency.run_id` 建唯一索引。升级旧库前先确认没有历史重复；若查询
返回任何行，必须按平台记录人工对账，不能自动删除证据：

```sql
SELECT run_id, COUNT(*)
FROM push_idempotency
GROUP BY run_id
HAVING COUNT(*) > 1;
```

可执行升级流程：

1. 停止所有旧版本实例并完成数据库备份；
2. 把重复组原样归档到带发布日期的只读表，例如
   `CREATE TABLE push_idempotency_dup_20261002 AS ...`；
3. 逐个 `run_id` 对照平台回执、callback 与外部副作用，只选择一条权威记录；
4. 用显式 `WHERE key = ... AND run_id = ...` 删除其余行，重新运行上面的查询确认结果为空；
5. 启动一个新实例并验证唯一索引、幂等回放和 callback 后，再恢复全部副本。

若判断错误，停止服务后可从备份/归档表按原主键恢复，再重新对账。不要使用按时间或
`MIN/MAX(key)` 自动挑选权威记录的脚本，因为旧执行可能已经产生不可逆副作用。

Push 签名没有时间戳，因此终态幂等记录当前不会自动按七天清理。不要直接清表；未来清理
必须先保留不可重放的 run ID tombstone。

## Post-deploy checks

```bash
make status
make health
make logs
```

还应验证：

- `/health/ready` 包含 PostgreSQL 必需检查且为 ready；
- 运行中进程哈希等于本次产物；
- 管理登录、只读用户权限和审计可用；
- 一次带幂等键的同步请求和一次 callback 请求；
- Provider 更新仅影响后续任务，并在多实例间收敛；
- 没有密钥、callback query 或业务正文进入日志。

## Rollback boundary

自动回滚只覆盖“新二进制替换后无法通过服务/就绪/哈希检查”。它不能撤销数据库迁移、
外部 Agent 副作用、已送达 callback 或 Provider 配置变更。涉及不可逆 schema 或协议变化
时必须先准备兼容迁移与人工恢复步骤。
