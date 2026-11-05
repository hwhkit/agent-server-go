package a2a

// A2A 协议类型/常量归 domain/platform；本文件是包内旧名别名，
// 让迁移过来的用例代码保持原符号形态。

import "ir-agent-server/internal/domain/platform"

type (
	A2AAgentCapabilities        = platform.A2AAgentCapabilities
	A2AAgentCard                = platform.A2AAgentCard
	A2AAgentInterface           = platform.A2AAgentInterface
	A2AAgentProvider            = platform.A2AAgentProvider
	A2AAgentSkill               = platform.A2AAgentSkill
	A2AArtifact                 = platform.A2AArtifact
	A2AGetTaskRequest           = platform.A2AGetTaskRequest
	A2AJSONRPCError             = platform.A2AJSONRPCError
	A2AJSONRPCRequest           = platform.A2AJSONRPCRequest
	A2AMessage                  = platform.A2AMessage
	A2APart                     = platform.A2APart
	A2ASecurityRequirement      = platform.A2ASecurityRequirement
	A2ASecurityScheme           = platform.A2ASecurityScheme
	A2ASendMessageConfiguration = platform.A2ASendMessageConfiguration
	A2ASendMessageRequest       = platform.A2ASendMessageRequest
	A2ATask                     = platform.A2ATask
	A2ATaskState                = platform.A2ATaskState
)

const (
	A2AErrInternal                     = platform.A2AErrInternal
	A2AErrInvalidParams                = platform.A2AErrInvalidParams
	A2AErrMethodNotFound               = platform.A2AErrMethodNotFound
	A2AErrPushNotificationNotSupported = platform.A2AErrPushNotificationNotSupported
	A2AErrTaskNotFound                 = platform.A2AErrTaskNotFound
	A2AErrUnsupportedOperation         = platform.A2AErrUnsupportedOperation
	A2AErrorDomain                     = platform.A2AErrorDomain
	A2APartIsData                      = platform.A2APartIsData
	A2APartIsText                      = platform.A2APartIsText
	A2AProtocolBindingJSONRPC          = platform.A2AProtocolBindingJSONRPC
	A2AProtocolVersion                 = platform.A2AProtocolVersion
	A2ARoleAgent                       = platform.A2ARoleAgent
	A2ARoleUser                        = platform.A2ARoleUser
	A2ATaskStateCompleted              = platform.A2ATaskStateCompleted
	A2ATaskStateFailed                 = platform.A2ATaskStateFailed
	A2ATaskStateRejected               = platform.A2ATaskStateRejected
)

// 用例侧消费的平台值对象与构造器别名。
type (
	TaskContext     = platform.TaskContext
	ProcessedOutput = platform.ProcessedOutput
)

var (
	ApplyA2AHistoryLength          = platform.ApplyA2AHistoryLength
	NewA2ATaskStatus               = platform.NewA2ATaskStatus
	NewA2ATaskStatusWithMessage    = platform.NewA2ATaskStatusWithMessage
	A2ASendMessageResponseWithTask = platform.A2ASendMessageResponseWithTask
	A2APartData                    = platform.A2APartData
	A2APartText                    = platform.A2APartText
	A2APartMarkdown                = platform.A2APartMarkdown
	A2AAgentMessage                = platform.A2AAgentMessage
)

var (
	a2aNowTimestamp              = platform.A2ANowTimestamp
	a2aJSONRPCNull               = platform.A2AJSONRPCNull
	A2ASecuritySchemeBearer      = platform.A2ASecuritySchemeBearer
	A2ASecurityRequirementScheme = platform.A2ASecurityRequirementScheme
	ExtractSymbolPool            = platform.ExtractSymbolPool
	ExtractUniverseMode          = platform.ExtractUniverseMode
	ExtractUpstreamSymbols       = platform.ExtractUpstreamSymbols
	ExtractFailurePath           = platform.ExtractFailurePath
	ExtractFailureReason         = platform.ExtractFailureReason
	ExtractTargetCount           = platform.ExtractTargetCount
)

type BoundTask = platform.BoundTask
