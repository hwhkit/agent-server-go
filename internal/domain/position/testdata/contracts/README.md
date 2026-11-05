# 契约 golden 快照

`TestContractsGolden`（`internal/domain/position/contracts_test.go`）把六个预置岗位契约常量与
本目录的 `.txt` 文件逐字节比对。契约文本是模型面对的协议:任何措辞漂移都会改变
输出行为,且不会有其他测试发现——这就是快照存在的唯一目的。

## 修改契约的流程

1. 修改 `internal/domain/position/contracts_text.go` 中的契约常量;
2. 运行 `go test ./internal/domain/position -run TestContractsGolden -update` 重新生成快照;
3. 在 CHANGELOG 记录行为变更及原因。快照更新与代码修改必须出现在同一个提交里,
   让 review 能直接看到改了哪个词。

## 来源与分歧历史

- **2026-09-10**（`eec306c`）:初值取自 `mota-agent-server-rs` 的六个
  `*_CONTRACT` 常量,迁移期由跨仓逐字节比对守护。
- **2026-11-02**:Rust 仓 `4bc459d`（"fix: recover FINAL usage and normalize local
  executor outputs"）为 `PORTFOLIO_MANAGER_CONTRACT` 新增「订单条数硬约束」段与
  「点名条数必须逐次完整作答」句。**该修改未回移到 Go 侧**,跨仓比对自此失效;
  是否回移待定。
- **2026-11-04**（MoTA→Intelligent Router 更名）:跨仓比对替换为本仓 golden
  快照,Go 侧自此为契约权威。其余五个契约与 Rust 初值逐字节一致。
