package execengine

// 绞杀者迁移期兼容层：执行引擎自 ir/ 包迁出后，其引用的边界类型已归位
// domain（exec/agent/platform）与 infra/config。本文件提供包内旧名别名，
// 引擎正文保持逐字迁移；P6 装配切换后按新名收敛。

import (
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/infra/config"
	"ir-agent-server/internal/infra/crypto"
)

type (
	PlatformConfig            = config.PlatformConfig
	LocalAgentConfig          = config.LocalAgentConfig
	OpenAIAgentsConfig        = config.OpenAIAgentsConfig
	ClaudeManagedAgentsConfig = config.ClaudeManagedAgentsConfig
	GatewayConfig             = config.GatewayConfig
	GatewayProtocol           = config.GatewayProtocol
	AgentConfig               = agent.ResolvedConfig
	ExecutorKind              = exec.Kind
	ExecutionMode             = exec.Mode
	TaskContext               = platform.TaskContext
	ProcessedOutput           = platform.ProcessedOutput
	Processor                 = platform.Processor
	BoundRunUsage             = platform.BoundRunUsage
	PayloadV1                 = platform.PayloadV1
	SignatureEncoding         = platform.SignatureEncoding
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
	ExecutorKindGoose           = exec.KindGoose
	ExecutorKindOpenAIAgent     = exec.KindOpenAIAgent
	ExecutorKindClaudeManaged   = exec.KindClaudeManaged
	ExecutionModeGateway        = exec.ModeGateway
	ExecutionModeLocal          = exec.ModeLocal
	ExecutionModeManaged        = exec.ModeManaged
	ExecutionModeRemotePull     = exec.ModeRemotePull
	SignatureEncodingHex        = platform.EncodingHex
	SignatureEncodingBase64     = platform.EncodingBase64
)

var (
	newExecutionError       = NewExecutionError
	jsonString              = platform.JSONString
	asMapValue              = platform.AsMapValue
	serializeExecutorOutput = platform.SerializeExecutorOutput
	FinalReportedUsage      = platform.FinalReportedUsage
	TaskParamsFromValue     = platform.TaskParamsFromValue
	ParseExecutorKind       = exec.ParseKind
	ParseExecutionMode      = exec.ParseMode
	ParseSignatureEncoding  = platform.ParseEncoding
	// RoleContracts 导出预置岗位表，供旧包 A2A skills 映射在迁移期引用。
	RoleContracts = roleContracts
)

const (
	GatewayProtocolAnthropic = config.GatewayProtocolAnthropic
	GatewayProtocolOpenAI    = config.GatewayProtocolOpenAI
)

var (
	ExtractTargetCount     = platform.ExtractTargetCount
	ExtractFailureReason   = platform.ExtractFailureReason
	ExtractFailurePath     = platform.ExtractFailurePath
	ExtractPayloadInfoText = platform.ExtractPayloadInfoText
	randomToken            = crypto.RandomToken
)

// nowTimestamp 返回定宽 RFC3339 UTC 时间戳（与存储层同口径）。
func nowTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

var trimUpper = platform.TrimUpper

var (
	asJSONObject = platform.AsJSONObject
	asJSONArray  = platform.AsJSONArray
)

var (
	asStringSlice     = platform.AsStringSlice
	PlaceholderOutput = platform.PlaceholderOutput
)
