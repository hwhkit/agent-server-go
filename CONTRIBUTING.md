# Contributing

感谢你参与 `mota-agent-server-go`。提交代码前请阅读 [SECURITY.md](SECURITY.md)；
未修复漏洞不要放进公开 Issue 或 Pull Request。

## Development setup

需要 Go 1.26.8、Git，以及构建期的固定 hwhkit-go 源码：

```bash
make bootstrap-hwhkit
make build
```

PostgreSQL 测试需要专用、可销毁的 PostgreSQL 15、16 或 17 数据库。部署脚本检查还需要
POSIX shell 与 ShellCheck。本地 Agent CLI 不是普通测试的前置条件，测试应使用隔离 fake。

## Validation

按改动范围运行：

```bash
make lint
make test
make race
make test-deploy-scripts
make verify
```

涉及数据库、Provider、账号、callback、幂等或迁移时必须额外运行：

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/mota_test' \
  make test-pg
```

禁止对生产数据库运行测试。缺少 `TEST_DATABASE_URL` 会使部分测试跳过；PR 中应列出所有
未执行或跳过的门禁。`make ci` 是本地综合检查，但 PostgreSQL 版本矩阵仍由独立任务执行。

## Pull requests

1. 一个 PR 聚焦一个问题，说明行为、兼容性、数据迁移与安全影响；
2. 修复必须带能在旧实现上失败的回归测试；
3. 新 Provider 或执行器要覆盖配置脱敏、assignment、超时、进程/HTTP 生命周期；
4. callback 改动要分别测试 accepted、rejected、无可信回执、重试和幂等状态；
5. 并发状态变更要用 race detector 和真实 PostgreSQL 验证，不只测试纯函数；
6. 新配置同步更新 `.env.example`、`config/default.toml`、README 和安全策略；
7. 不提交 API key、签名密钥、数据库密码、Session Secret、客户数据或生产日志；
8. 更新 GitHub Actions 时使用上游仓库的完整 commit SHA，并限制权限。

代码风格遵循 `gofmt`。对外错误返回稳定代码和安全文案，内部 URL、命令行、路径、密钥
和远端原始错误不得直接暴露给调用方。

## Documentation

文档必须区分：

- 当前已实现行为；
- 已验证证据；
- 已知边界或待办。

不要复制其他语言实现的命令，也不要把本地缓存结果、生成子测试数量或带 skip 的运行
描述成当前分支的完整验证。发布证据使用
[doc/RELEASE-READINESS.md](doc/RELEASE-READINESS.md)。

## Release checklist

1. 更新 [CHANGELOG.md](CHANGELOG.md)；
2. 在 fresh checkout 运行 bootstrap、综合门禁和 PostgreSQL 15/16/17 矩阵；
3. 对最终 `go.sum` 执行依赖漏洞扫描；
4. 生成 Linux ARM64 产物并记录 SHA-256；
5. 在 staging 验证数据库就绪、RBAC、Provider 热更新、callback 与回滚；
6. 确认 GitHub 默认分支、branch protection、私密漏洞报告和仓库可见性；
7. 合并后创建不可变的 `vX.Y.Z` tag，不移动已公开 tag。

提交贡献即表示你同意按 `MIT OR Apache-2.0` 双许可证发布贡献。
