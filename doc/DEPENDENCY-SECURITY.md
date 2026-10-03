# Dependency security

依赖安全门禁使用固定镜像的 OSV-Scanner，并显式启用 Go 调用路径分析。发现可修复漏洞时应
升级到兼容的最低安全版本；不能用包级或生态级全局忽略绕过门禁。

Go 模块暂不交给 Dependabot：`go.mod` 通过 sibling `replace` 引用固定 hwhkit 源码，
Dependabot 的隔离检出不会执行 `make bootstrap-hwhkit`，会产生不可执行的更新任务。
GitHub Actions 仍由 Dependabot 管理；Go 依赖由每周安全 CI 告警后在完整 bootstrap 环境中
人工更新。hwhkit 发布正式 module 版本或移入仓库后，应恢复 gomod Dependabot。

## 临时例外

| OSV ID | 精确范围 | 判定与证据 | 复核期限 |
|---|---|---|---|
| `GO-2026-5932` | `golang.org/x/crypto/openpgp` 及其子包 | 公告影响所有 OpenPGP 版本且没有修复版本；`go list -deps ./...` 和 OSV Go 调用路径分析均确认当前服务及固定 hwhkit 依赖图不导入这些包 | 2026-12-31 |

该例外只按公告 ID 生效，不忽略整个 `golang.org/x/crypto` 模块。出现以下任一情况必须立即
删除或重新评估例外，不能等待期限：

- 服务或固定 hwhkit 源码开始导入 `golang.org/x/crypto/openpgp`；
- 公告新增可用修复版本或替代迁移路径；
- Go、hwhkit 或任一传递依赖升级后，调用路径分析结论发生变化；
- `ignoreUntil` 到期。

## 复核命令

先验证依赖图中没有 OpenPGP 包：

```bash
go list -deps ./... | grep '^golang.org/x/crypto/openpgp\(/\|$\)'
```

正常结果为空。CI 使用以下等价扫描参数作为阻断门禁：

```text
osv-scanner scan source \
  --config ./osv-scanner.toml \
  --call-analysis=go \
  --include-git-root \
  -r ./
```

复核人员还应准备一个只含注释的普通 TOML 文件作为空配置（不要使用 `/dev/null`），临时
绕过例外并显示未调用路径，确认只剩已记录项目，而没有被调用路径分析隐藏的新问题：

```text
osv-scanner scan source \
  --config /absolute/path/to/empty-osv-scanner.toml \
  --call-analysis=go \
  --all-vulns \
  --include-git-root \
  -r ./
```

该诊断命令当前只应报告未调用且无修复版本的 `GO-2026-5932`。若出现其他项目，必须升级、
移除依赖或新增一条独立、有期限且有调用路径证据的例外。
