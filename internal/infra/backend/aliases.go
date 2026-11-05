package backend

// 旧栈符号 → 新栈类型的别名层：providers.go / store.go 从 ir/ 逐字迁移，
// 通过别名保持原文可读；P6c-3 删除 ir/ 后本文件是唯一桥。

import (
	appbackend "ir-agent-server/internal/app/backend"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/infra/config"
)

// PlatformConfig 是共享运行配置（infra/config 唯一真身）。
type PlatformConfig = config.PlatformConfig

// ExecutorKind / ExecutionMode 沿用旧栈命名指向 domain/exec。
type (
	ExecutorKind  = exec.Kind
	ExecutionMode = exec.Mode
)

const (
	ExecutorKindRust            = exec.KindRust
	ExecutorKindClaudeCode      = exec.KindClaudeCode
	ExecutorKindCodex           = exec.KindCodex
	ExecutorKindReasonix        = exec.KindReasonix
	ExecutorKindPi              = exec.KindPi
	ExecutorKindOpenCode        = exec.KindOpenCode
	ExecutorKindQwenCode        = exec.KindQwenCode
	ExecutorKindGeminiCLI       = exec.KindGeminiCLI
	ExecutorKindOpenHands       = exec.KindOpenHands
	ExecutorKindDeepseekHarness = exec.KindDeepseekHarness
	ExecutorKindOpenAIAgent     = exec.KindOpenAIAgent
	ExecutorKindClaudeManaged   = exec.KindClaudeManaged

	ExecutionModeLocal   = exec.ModeLocal
	ExecutionModeGateway = exec.ModeGateway
	ExecutionModeManaged = exec.ModeManaged
)

// ProviderView / ProviderAssignment 即应用层端口的视图类型（同构别名）。
type (
	ProviderView       = appbackend.View
	ProviderAssignment = appbackend.Assignment
)

// ErrStaleProviderRevision 是乐观并发冲突哨兵（应用层同名）。
var ErrStaleProviderRevision = appbackend.ErrStaleRevision

// AuditEvent 是审计事件（domain 真身；Detail 为已编码 JSON）。
type AuditEvent = audit.Event

// lookupNonBlankEnv 是环境变量读取 helper（providers.go 局部参数名 config
// 会遮蔽包名，故保留旧栈的裸函数形态）。
var lookupNonBlankEnv = config.LookupNonBlankEnv
