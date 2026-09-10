package mota

// A2A(Agent2Agent)Protocol v1.0 数据模型。
// 对应 rs 侧 src/a2a/types.rs。
//
// 规范来源:a2aproject/A2A 的 specification/a2a.proto(v1.0 起 proto 是规范性
// 来源)与 docs/specification.md。序列化规则:
//
//   - JSON 字段名一律 camelCase(§5.5),即使 proto 里是 snake_case。
//   - 枚举按 ProtoJSON 序列化为带类型前缀的 SCREAMING_SNAKE_CASE
//     (TASK_STATE_COMPLETED / ROLE_USER),不是 v0.3 的 completed / user。
//   - oneof 不带 kind 判别字段,靠成员名判别(v1.0 破坏性变更):Part 用
//     text/raw/url/data,StreamResponse 用 task/message/statusUpdate/artifactUpdate。
//   - 时间戳为 UTC ISO 8601、毫秒精度(§5.6.1):YYYY-MM-DDTHH:mm:ss.sssZ。

import (
	"strings"
	"time"
)

// A2AProtocolVersion 是本实现暴露的 A2A 协议版本。v1.0 起 protocolVersion
// 属于单个 AgentInterface,不再是 AgentCard 的顶层字段。
const A2AProtocolVersion = "1.0"

// A2AProtocolBindingJSONRPC 是 JSON-RPC 协议绑定标识
// (AgentInterface.protocolBinding 的核心取值之一)。
const A2AProtocolBindingJSONRPC = "JSONRPC"

// A2AMediaType 是 Agent Card 与 A2A 载荷的媒体类型。
const A2AMediaType = "application/a2a+json"

// A2AErrorDomain 是 A2A 错误的 google.rpc.ErrorInfo.domain。
const A2AErrorDomain = "a2a-protocol.org"

// a2aNowTimestamp 返回当前 UTC 时间,ISO 8601 毫秒精度
// (§5.6.1 要求的 YYYY-MM-DDTHH:mm:ss.sssZ)。
func a2aNowTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// ---------------------------------------------------------------------------
// TaskState / Role
// ---------------------------------------------------------------------------

// A2ATaskState 是任务生命周期状态。JSON 取值带 TASK_STATE_ 前缀。
type A2ATaskState string

const (
	A2ATaskStateUnspecified   A2ATaskState = "TASK_STATE_UNSPECIFIED"
	A2ATaskStateSubmitted     A2ATaskState = "TASK_STATE_SUBMITTED"
	A2ATaskStateWorking       A2ATaskState = "TASK_STATE_WORKING"
	A2ATaskStateCompleted     A2ATaskState = "TASK_STATE_COMPLETED"
	A2ATaskStateFailed        A2ATaskState = "TASK_STATE_FAILED"
	A2ATaskStateCanceled      A2ATaskState = "TASK_STATE_CANCELED"
	A2ATaskStateInputRequired A2ATaskState = "TASK_STATE_INPUT_REQUIRED"
	A2ATaskStateRejected      A2ATaskState = "TASK_STATE_REJECTED"
	A2ATaskStateAuthRequired  A2ATaskState = "TASK_STATE_AUTH_REQUIRED"
)

// IsTerminal 报告是否为终态:不会再有后续更新,流必须关闭(§3.1.2)。
func (s A2ATaskState) IsTerminal() bool {
	switch s {
	case A2ATaskStateCompleted, A2ATaskStateFailed, A2ATaskStateCanceled, A2ATaskStateRejected:
		return true
	default:
		return false
	}
}

// A2ATaskStateFromRunStatus 把 agent_runs.status 映射为 A2A 状态。
// 历史记录只有 running / completed / failed 三种取值。
func A2ATaskStateFromRunStatus(status string) A2ATaskState {
	switch strings.TrimSpace(status) {
	case "submitted":
		return A2ATaskStateSubmitted
	case "running", "working":
		return A2ATaskStateWorking
	case "completed":
		return A2ATaskStateCompleted
	case "failed":
		return A2ATaskStateFailed
	case "canceled", "cancelled":
		return A2ATaskStateCanceled
	case "input_required":
		return A2ATaskStateInputRequired
	case "rejected":
		return A2ATaskStateRejected
	default:
		return A2ATaskStateUnspecified
	}
}

// ToRunStatus 反向映射为 agent_runs.status 存储值。
func (s A2ATaskState) ToRunStatus() string {
	switch s {
	case A2ATaskStateSubmitted:
		return "submitted"
	case A2ATaskStateWorking:
		return "running"
	case A2ATaskStateCompleted:
		return "completed"
	case A2ATaskStateFailed:
		return "failed"
	case A2ATaskStateCanceled:
		return "canceled"
	case A2ATaskStateInputRequired:
		return "input_required"
	case A2ATaskStateRejected:
		return "rejected"
	case A2ATaskStateAuthRequired:
		return "auth_required"
	default:
		return "unknown"
	}
}

// A2ARole 是消息发送方。JSON 取值带 ROLE_ 前缀。
type A2ARole string

const (
	A2ARoleUnspecified A2ARole = "ROLE_UNSPECIFIED"
	A2ARoleUser        A2ARole = "ROLE_USER"
	A2ARoleAgent       A2ARole = "ROLE_AGENT"
)

// ---------------------------------------------------------------------------
// Part / Message
// ---------------------------------------------------------------------------

// A2APart 是统一的内容块。v1.0 取消了 TextPart/FilePart/DataPart 三个类型和
// kind 判别字段,改为一个结构 + oneof content { text | raw | url | data },
// 靠"哪个成员存在"判别。
type A2APart struct {
	Text *string `json:"text,omitempty"`
	// Raw 是内联二进制内容,JSON 里是 base64 字符串。
	Raw *string `json:"raw,omitempty"`
	// URL 是文件引用 URL(取代 v0.3 的 file.fileWithUri)。
	URL *string `json:"url,omitempty"`
	// Data 是结构化 JSON 内容。
	Data any `json:"data,omitempty"`
	// Filename 可选文件名。
	Filename *string `json:"filename,omitempty"`
	// MediaType 取代 v0.3 的 mimeType,且对所有内容类型都可用。
	MediaType *string `json:"mediaType,omitempty"`
	Metadata  any     `json:"metadata,omitempty"`
}

// A2APartText 构造纯文本 part。
func A2APartText(text string) A2APart {
	mediaType := "text/plain"
	return A2APart{Text: &text, MediaType: &mediaType}
}

// A2APartData 构造结构化 JSON part。
func A2APartData(data any) A2APart {
	mediaType := "application/json"
	return A2APart{Data: data, MediaType: &mediaType}
}

// A2APartMarkdown 构造 markdown 文本 part。
func A2APartMarkdown(text string) A2APart {
	mediaType := "text/markdown"
	return A2APart{Text: &text, MediaType: &mediaType}
}

// A2APartContentKind 标识 A2APart 的 oneof 内容类型。
type A2APartContentKind int

const (
	A2APartEmpty A2APartContentKind = iota
	A2APartIsText
	A2APartIsRaw
	A2APartIsURL
	A2APartIsData
)

// Content 按成员存在性判别内容类型。多个成员同时存在时按 proto 字段序取第一个。
func (p *A2APart) Content() (kind A2APartContentKind, value any) {
	switch {
	case p.Text != nil:
		return A2APartIsText, *p.Text
	case p.Raw != nil:
		return A2APartIsRaw, *p.Raw
	case p.URL != nil:
		return A2APartIsURL, *p.URL
	case p.Data != nil:
		return A2APartIsData, p.Data
	default:
		return A2APartEmpty, nil
	}
}

// HasSingleContent 报告是否恰好设置了一个内容成员(协议要求)。
func (p *A2APart) HasSingleContent() bool {
	count := 0
	if p.Text != nil {
		count++
	}
	if p.Raw != nil {
		count++
	}
	if p.URL != nil {
		count++
	}
	if p.Data != nil {
		count++
	}
	return count == 1
}

// A2AMessage 是一次通信单元。MessageID 由创建方生成,服务端可用它做重复消息
// 检测(§3.3.1)。
type A2AMessage struct {
	MessageID        string    `json:"messageId"`
	ContextID        *string   `json:"contextId,omitempty"`
	TaskID           *string   `json:"taskId,omitempty"`
	Role             A2ARole   `json:"role"`
	Parts            []A2APart `json:"parts"`
	Metadata         any       `json:"metadata,omitempty"`
	Extensions       []string  `json:"extensions,omitempty"`
	ReferenceTaskIDs []string  `json:"referenceTaskIds,omitempty"`
}

// A2AAgentMessage 构造一条 agent 侧消息。
func A2AAgentMessage(messageID string, parts []A2APart) A2AMessage {
	return A2AMessage{MessageID: messageID, Role: A2ARoleAgent, Parts: parts}
}

// TextContent 把所有文本内容块拼接成一段纯文本(忽略 data/file 块)。
func (m *A2AMessage) TextContent() string {
	var buffer strings.Builder
	for i := range m.Parts {
		if kind, value := m.Parts[i].Content(); kind == A2APartIsText {
			if buffer.Len() > 0 {
				buffer.WriteByte('\n')
			}
			buffer.WriteString(value.(string))
		}
	}
	return buffer.String()
}

// FirstData 返回第一个结构化 data 内容块。A2A 调用方用它直通业务信封。
func (m *A2AMessage) FirstData() (any, bool) {
	for i := range m.Parts {
		if kind, value := m.Parts[i].Content(); kind == A2APartIsData {
			return value, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Artifact / TaskStatus / Task
// ---------------------------------------------------------------------------

// A2AArtifact 是任务产出物。至少包含一个 Part。
type A2AArtifact struct {
	ArtifactID  string    `json:"artifactId"`
	Name        *string   `json:"name,omitempty"`
	Description *string   `json:"description,omitempty"`
	Parts       []A2APart `json:"parts"`
	Metadata    any       `json:"metadata,omitempty"`
	Extensions  []string  `json:"extensions,omitempty"`
}

// A2ATaskStatus 是任务状态容器。
type A2ATaskStatus struct {
	State A2ATaskState `json:"state"`
	// Message 是状态附带的说明消息(失败原因、需要补充的输入等)。
	Message   *A2AMessage `json:"message,omitempty"`
	Timestamp *string     `json:"timestamp,omitempty"`
}

// NewA2ATaskStatus 构造一个带当前时间戳的状态。
func NewA2ATaskStatus(state A2ATaskState) A2ATaskStatus {
	timestamp := a2aNowTimestamp()
	return A2ATaskStatus{State: state, Timestamp: &timestamp}
}

// NewA2ATaskStatusWithMessage 构造一个带说明消息的状态。
func NewA2ATaskStatusWithMessage(state A2ATaskState, message A2AMessage) A2ATaskStatus {
	timestamp := a2aNowTimestamp()
	return A2ATaskStatus{State: state, Message: &message, Timestamp: &timestamp}
}

// A2ATask 是 A2A 的核心执行单元。
type A2ATask struct {
	ID        string        `json:"id"`
	ContextID string        `json:"contextId"`
	Status    A2ATaskStatus `json:"status"`
	Artifacts []A2AArtifact `json:"artifacts,omitempty"`
	History   []A2AMessage  `json:"history,omitempty"`
	Metadata  any           `json:"metadata,omitempty"`
}

// ApplyA2AHistoryLength 按 historyLength 语义裁剪历史(§3.2.4):
// 未设置(nil) = 不限制;0 = 不返回;>0 = 最多返回最近 N 条。
func ApplyA2AHistoryLength(history *[]A2AMessage, historyLength *int) {
	if historyLength == nil {
		return
	}
	limit := *historyLength
	if limit <= 0 {
		*history = nil
		return
	}
	if len(*history) > limit {
		*history = (*history)[len(*history)-limit:]
	}
}

// ---------------------------------------------------------------------------
// 流式事件(暴露类型以便契约完整,当前协议层不产出流式响应)
// ---------------------------------------------------------------------------

// A2ATaskStatusUpdateEvent 是任务状态变更事件。v1.0 删除了 final 字段:
// 终态由流关闭表达。
type A2ATaskStatusUpdateEvent struct {
	TaskID    string        `json:"taskId"`
	ContextID string        `json:"contextId"`
	Status    A2ATaskStatus `json:"status"`
	Metadata  any           `json:"metadata,omitempty"`
}

// A2ATaskArtifactUpdateEvent 是产出物增量事件。
type A2ATaskArtifactUpdateEvent struct {
	TaskID    string      `json:"taskId"`
	ContextID string      `json:"contextId"`
	Artifact  A2AArtifact `json:"artifact"`
	// Append 为 true 时表示内容应追加到同 artifactId 的既有产出物。
	Append bool `json:"append,omitempty"`
	// LastChunk 为 true 时表示这是该产出物的最后一个分片。
	LastChunk bool `json:"lastChunk,omitempty"`
	Metadata  any  `json:"metadata,omitempty"`
}

// A2AStreamResponse 是流式响应包装。oneof,靠成员名判别(v1.0 取消了 kind)。
type A2AStreamResponse struct {
	Task           *A2ATask                    `json:"task,omitempty"`
	Message        *A2AMessage                 `json:"message,omitempty"`
	StatusUpdate   *A2ATaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *A2ATaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

// ---------------------------------------------------------------------------
// 请求 / 响应载荷
// ---------------------------------------------------------------------------

// A2ASendMessageRequest 是 SendMessage / SendStreamingMessage 的 params。
type A2ASendMessageRequest struct {
	// Tenant 是多租户路由标识。本实现用路径分片({base_path}/{slug}),
	// 因此该字段仅作校验。
	Tenant        *string                      `json:"tenant,omitempty"`
	Message       *A2AMessage                  `json:"message,omitempty"`
	Configuration *A2ASendMessageConfiguration `json:"configuration,omitempty"`
	Metadata      any                          `json:"metadata,omitempty"`
}

// A2ASendMessageConfiguration 是发送配置。
type A2ASendMessageConfiguration struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
	// ReturnImmediately true = 立即返回(非阻塞),由调用方自行轮询 GetTask;
	// 缺省 false = 阻塞直到终态或中断态(§3.2.2)。
	ReturnImmediately bool `json:"returnImmediately,omitempty"`
	// TaskPushNotificationConfig 是推送通知订阅。本实现暂未支持,收到时按
	// 能力校验拒绝。
	TaskPushNotificationConfig any `json:"taskPushNotificationConfig,omitempty"`
}

// A2ASendMessageResponse 是 SendMessage 的 result。oneof:要么 task,要么 message。
type A2ASendMessageResponse struct {
	Task    *A2ATask    `json:"task,omitempty"`
	Message *A2AMessage `json:"message,omitempty"`
}

// A2ASendMessageResponseWithTask 构造只携带 task 的响应。
func A2ASendMessageResponseWithTask(task A2ATask) A2ASendMessageResponse {
	return A2ASendMessageResponse{Task: &task}
}

// A2AGetTaskRequest 是 GetTask 的 params。
type A2AGetTaskRequest struct {
	Tenant        *string `json:"tenant,omitempty"`
	ID            string  `json:"id"`
	HistoryLength *int    `json:"historyLength,omitempty"`
}

// A2ACancelTaskRequest 是 CancelTask 的 params。
type A2ACancelTaskRequest struct {
	Tenant   *string `json:"tenant,omitempty"`
	ID       string  `json:"id"`
	Metadata any     `json:"metadata,omitempty"`
}

// A2ASubscribeToTaskRequest 是 SubscribeToTask 的 params。
type A2ASubscribeToTaskRequest struct {
	Tenant *string `json:"tenant,omitempty"`
	ID     string  `json:"id"`
}

// A2AListTasksRequest 是 ListTasks 的 params。分页是游标式(pageToken),
// 不是页码式。
type A2AListTasksRequest struct {
	Tenant               *string       `json:"tenant,omitempty"`
	ContextID            *string       `json:"contextId,omitempty"`
	Status               *A2ATaskState `json:"status,omitempty"`
	PageSize             *int          `json:"pageSize,omitempty"`
	PageToken            *string       `json:"pageToken,omitempty"`
	HistoryLength        *int          `json:"historyLength,omitempty"`
	StatusTimestampAfter *string       `json:"statusTimestampAfter,omitempty"`
	IncludeArtifacts     *bool         `json:"includeArtifacts,omitempty"`
}

// A2AListTasksResponse 是 ListTasks 的 result。
type A2AListTasksResponse struct {
	Tasks         []A2ATask `json:"tasks"`
	NextPageToken string    `json:"nextPageToken"`
	PageSize      int       `json:"pageSize"`
	TotalSize     int       `json:"totalSize"`
}

// ---------------------------------------------------------------------------
// Agent Card
// ---------------------------------------------------------------------------

// A2AAgentInterface 是一个可达接口。v1.0 把 v0.3 的顶层 url +
// preferredTransport + additionalInterfaces + protocolVersion 四个字段合并
// 到这里,并且 SupportedInterfaces 数组**有序**——第一项即首选接口。
type A2AAgentInterface struct {
	URL string `json:"url"`
	// ProtocolBinding 核心取值 JSONRPC / GRPC / HTTP+JSON;自定义绑定应使用 URI。
	ProtocolBinding string `json:"protocolBinding"`
	// ProtocolVersion 是单接口暴露的协议版本,如 "1.0"。
	ProtocolVersion string `json:"protocolVersion"`
	// Tenant 是单端点托管多 Agent 时的路由标识。本实现用路径分片,留空。
	Tenant *string `json:"tenant,omitempty"`
}

// A2AAgentProvider 是服务提供方。注意是 organization 而不是 v0.3 的 name。
type A2AAgentProvider struct {
	URL          string `json:"url"`
	Organization string `json:"organization"`
}

// A2AAgentExtension 是协议扩展声明。
type A2AAgentExtension struct {
	URI         string `json:"uri"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Params      any    `json:"params,omitempty"`
}

// A2AAgentCapabilities 是能力声明。ExtendedAgentCard 取代了 v0.3 顶层的
// supportsAuthenticatedExtendedCard;v0.3 的 stateTransitionHistory 已被删除。
type A2AAgentCapabilities struct {
	Streaming         *bool               `json:"streaming,omitempty"`
	PushNotifications *bool               `json:"pushNotifications,omitempty"`
	ExtendedAgentCard *bool               `json:"extendedAgentCard,omitempty"`
	Extensions        []A2AAgentExtension `json:"extensions,omitempty"`
}

// A2AStringList 是 SecurityRequirement.schemes 的值类型(proto StringList)。
type A2AStringList struct {
	List []string `json:"list,omitempty"`
}

// A2ASecurityRequirement 是安全要求:scheme 名 -> 所需 scope 列表。
type A2ASecurityRequirement struct {
	Schemes map[string]A2AStringList `json:"schemes"`
}

// A2ASecurityRequirementScheme 构造无 scope 要求的单 scheme 声明。
func A2ASecurityRequirementScheme(name string) A2ASecurityRequirement {
	return A2ASecurityRequirement{Schemes: map[string]A2AStringList{name: {}}}
}

// A2AHTTPAuthSecurityScheme 是 HTTP 认证方案(Bearer / Basic 等)。
type A2AHTTPAuthSecurityScheme struct {
	Description string `json:"description,omitempty"`
	// Scheme 是 IANA 注册的 HTTP 认证方案名,如 Bearer。
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// A2AAPIKeySecurityScheme 是 API Key 认证方案。
type A2AAPIKeySecurityScheme struct {
	Description string `json:"description,omitempty"`
	// Location 是 query / header / cookie。
	Location string `json:"in"`
	Name     string `json:"name"`
}

// A2ASecurityScheme 是安全方案。proto 里是 oneof,ProtoJSON 用成员名表达,
// 因此 JSON 形如 {"httpAuthSecurityScheme": {...}},而不是 v0.3 的
// {"type": "http", "scheme": "bearer"}。
type A2ASecurityScheme struct {
	APIKeySecurityScheme   *A2AAPIKeySecurityScheme   `json:"apiKeySecurityScheme,omitempty"`
	HTTPAuthSecurityScheme *A2AHTTPAuthSecurityScheme `json:"httpAuthSecurityScheme,omitempty"`
}

// A2ASecuritySchemeBearer 构造 "Authorization: Bearer <token>" 方案。
func A2ASecuritySchemeBearer(description string) A2ASecurityScheme {
	return A2ASecurityScheme{
		HTTPAuthSecurityScheme: &A2AHTTPAuthSecurityScheme{
			Description: description, Scheme: "Bearer",
		},
	}
}

// A2AAgentCardSignature 是 Agent Card 的 JWS 签名(RFC 7515,签名前用
// RFC 8785 JCS 规范化)。
type A2AAgentCardSignature struct {
	Protected string `json:"protected"`
	Signature string `json:"signature"`
	Header    any    `json:"header,omitempty"`
}

// A2AAgentSkill 是一项能力。v1.0 起 tags 是**必填**且不可为空:skill 级发现
// 依赖它做过滤。
type A2AAgentSkill struct {
	ID                   string                   `json:"id"`
	Name                 string                   `json:"name"`
	Description          string                   `json:"description"`
	Tags                 []string                 `json:"tags"`
	Examples             []string                 `json:"examples,omitempty"`
	InputModes           []string                 `json:"inputModes,omitempty"`
	OutputModes          []string                 `json:"outputModes,omitempty"`
	SecurityRequirements []A2ASecurityRequirement `json:"securityRequirements,omitempty"`
}

// A2AAgentCard 是自描述清单。
type A2AAgentCard struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// SupportedInterfaces 有序,第一项为首选接口。
	SupportedInterfaces []A2AAgentInterface `json:"supportedInterfaces"`
	Provider            *A2AAgentProvider   `json:"provider,omitempty"`
	// Version 是 Agent 自身的版本号,**不是**协议版本。
	Version              string                       `json:"version"`
	DocumentationURL     *string                      `json:"documentationUrl,omitempty"`
	Capabilities         A2AAgentCapabilities         `json:"capabilities"`
	SecuritySchemes      map[string]A2ASecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []A2ASecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                     `json:"defaultInputModes"`
	DefaultOutputModes   []string                     `json:"defaultOutputModes"`
	Skills               []A2AAgentSkill              `json:"skills"`
	Signatures           []A2AAgentCardSignature      `json:"signatures,omitempty"`
	IconURL              *string                      `json:"iconUrl,omitempty"`
}
