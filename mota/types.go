package mota

// MOTA 协议数据类型。
//
// 结构对应 MOTA 平台接入文档中的请求/响应 schema:
//   - Push: POST <endpoint> 接收 mota-agent-bound-task v1,返回 mota-agent-payload v1
//   - Pull: claim / complete / renew / heartbeat 的请求与回执。
//
// 输入输出中的业务字段类型未在协议中固定(由该 agent 的输入/输出 schema 决定),
// 统一用 any 承载,保证前向兼容。
//
// 对应 Rust 侧 src/mota/types.rs。

import (
	"encoding/json"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Push 接入 —— 旧版
// ---------------------------------------------------------------------------

// PushRequest 是旧版 Push 请求体。仅保留为公开类型兼容,不再被生产 Push 路由接受。
type PushRequest struct {
	TaskBrief any `json:"taskBrief,omitempty"`
	Params    any `json:"params,omitempty"`
}

// ---------------------------------------------------------------------------
// Push 接入 —— mota-agent-bound-task v1(新协议)
// ---------------------------------------------------------------------------

// BoundTask 是平台推送到 pushEndpoint 的任务信封 mota-agent-bound-task v1。
type BoundTask struct {
	SchemaVersion *string `json:"schemaVersion,omitempty"`
	RunID         *string `json:"runId,omitempty"`
	AgentCode     *string `json:"agentCode,omitempty"`
	TemplateID    *string `json:"templateId,omitempty"`
	// PositionType 是本次任务岗位;决定 payload_result 必须满足哪套岗位契约。
	PositionType *string `json:"positionType,omitempty"`
	// TemplateInputData 是结构化入参 [{"name": "...", "value": <任意 JSON>}]。
	TemplateInputData *[]InputEntry `json:"templateInputData,omitempty"`
	// TaskBrief 是任务简述,如 expectedOutputFormat。
	TaskBrief any `json:"taskBrief,omitempty"`
	// GlobalParameters 是全局运行参数;无内容时平台不下发该字段。
	GlobalParameters any `json:"globalParameters,omitempty"`
	// UpstreamPayload 是上游节点的产出;无内容时平台不下发该字段。
	UpstreamPayload any `json:"upstreamPayload,omitempty"`
	// Upstream 是上游 DAG 结构:{ edges, nodes: [{agentInstanceId, output: {orders, ...}}], self }。
	// 平台实际下发字段名为 "upstream"(bound 任务信封)。
	Upstream any `json:"upstream,omitempty"`
	// RuntimeContext 是运行期上下文;当前生产协议只允许 isFinal。
	RuntimeContext any `json:"runtimeContext,omitempty"`
	// CallbackURL 是结果回传地址,每次推送都带;同步形态用不上。
	CallbackURL *string `json:"callbackUrl,omitempty"`

	// ParamsBackcompat 是旧协议 {taskBrief, params} 的 params 兜底(不参与序列化)。
	ParamsBackcompat any `json:"-"`
}

// InputEntry 是 templateInputData 中的一项:{"name": "...", "value": <任意 JSON>}。
type InputEntry struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// ParamsView 把 templateInputData 展平为 { name: value } 对象(任务入参视图)。
//
// 无 templateInputData 时保留与旧协议 {taskBrief, params} 的兼容:直接读原始
// params 字段;都没有则返回空对象。
func (t *BoundTask) ParamsView(rawBody map[string]any) any {
	if t.TemplateInputData != nil {
		params := make(map[string]any, len(*t.TemplateInputData))
		for _, entry := range *t.TemplateInputData {
			params[entry.Name] = entry.Value
		}
		return params
	}
	if t.ParamsBackcompat != nil {
		return t.ParamsBackcompat
	}
	if params, ok := rawBody["params"]; ok {
		return params
	}
	return map[string]any{}
}

// Language 返回输出语言。生产协议缺省为英文。
func (t *BoundTask) Language() *string {
	global, ok := t.GlobalParameters.(map[string]any)
	if !ok {
		return nil
	}
	language, ok := global["language"].(string)
	if !ok {
		return nil
	}
	return &language
}

// ---------------------------------------------------------------------------
// TaskParams
// ---------------------------------------------------------------------------

// TaskParams 是该 agent 的输入 schema 字段(params 内的键)。字段类型由 agent 的
// 输入 schema 决定,这里用 any 宽松承载。
type TaskParams struct {
	Symbols any
	// Securities 是新协议模板常用字段(如 single_security_technical_diagnosis)。
	Securities          any
	FinancialAnalysis   any
	CompanyAnnouncement any
	ConsensusEstimate   any
	ResearchReport      any
	// Extra 是未显式映射的其它入参(如 chart_period)。
	Extra map[string]any
}

// TaskParamsFromValue 从任意 params JSON 中宽松解析;非对象时退回全空结构。
func TaskParamsFromValue(value any) TaskParams {
	params := TaskParams{Extra: map[string]any{}}
	object, ok := value.(map[string]any)
	if !ok {
		return params
	}
	for key, item := range object {
		switch key {
		case "symbols":
			params.Symbols = item
		case "securities":
			params.Securities = item
		case "financial_analysis":
			params.FinancialAnalysis = item
		case "company_announcement":
			params.CompanyAnnouncement = item
		case "consensus_estimate":
			params.ConsensusEstimate = item
		case "research_report":
			params.ResearchReport = item
		default:
			params.Extra[key] = item
		}
	}
	return params
}

// EvidenceRefs 始终返回空列表。
//
// 普通模板入参不等于证据,不能伪装成 MCP_TOOL / UPSTREAM / DOC。调用方只有拿到
// 真实来源标识时才应填 evidenceRefs,因此默认不生成。
func (p TaskParams) EvidenceRefs() []any { return []any{} }

// ---------------------------------------------------------------------------
// BoundRunUsage
// ---------------------------------------------------------------------------

// BoundRunUsage 是受信 Bound Host 回传的最终 Token 用量。所有计数必须来自
// 供应商/执行器原始回执;不允许用 tokenizer 或字符数估算后伪装成 FINAL。
type BoundRunUsage struct {
	SchemaVersion     string  `json:"schemaVersion"`
	Status            string  `json:"status"`
	PromptTokens      uint64  `json:"promptTokens"`
	CachedInputTokens *uint64 `json:"cachedInputTokens,omitempty"`
	CompletionTokens  uint64  `json:"completionTokens"`
	ReasoningTokens   *uint64 `json:"reasoningTokens,omitempty"`
	TotalTokens       uint64  `json:"totalTokens"`
	InvocationCount   uint64  `json:"invocationCount"`
	Estimated         bool    `json:"estimated"`
}

// FinalReportedUsage 构造一份 FINAL 用量。以下情况返回 nil(拒绝不自洽的数据):
// invocationCount 为 0、缓存输入超过 prompt 总量、prompt+completion 溢出。
func FinalReportedUsage(
	promptTokens uint64,
	cachedInputTokens *uint64,
	completionTokens uint64,
	reasoningTokens *uint64,
	invocationCount uint64,
) *BoundRunUsage {
	if invocationCount == 0 {
		return nil
	}
	if cachedInputTokens != nil && *cachedInputTokens > promptTokens {
		return nil
	}
	if promptTokens > math.MaxUint64-completionTokens {
		return nil
	}
	return &BoundRunUsage{
		SchemaVersion:     "1.0",
		Status:            "FINAL",
		PromptTokens:      promptTokens,
		CachedInputTokens: cachedInputTokens,
		CompletionTokens:  completionTokens,
		ReasoningTokens:   reasoningTokens,
		TotalTokens:       promptTokens + completionTokens,
		InvocationCount:   invocationCount,
		Estimated:         false,
	}
}

// ---------------------------------------------------------------------------
// PayloadV1
// ---------------------------------------------------------------------------

// PayloadV1 是 mota-agent-payload v1 结果信封。普通 Bound 创作者只发送三个业务键;
// 受信 Investor Workflow Host 可额外发送 FINAL usage。
type PayloadV1 struct {
	// PayloadResult 是该岗位约定的结构化结果(字段见创作者中心岗位契约页)。
	PayloadResult any `json:"payload_result"`
	// PayloadInfo 是人读 markdown 摘要。
	PayloadInfo string `json:"payload_info"`
	// ContractVersion 固定 "V2",声明遵循的岗位契约版本(不是制品版本)。
	ContractVersion string `json:"contract_version"`
	// Usage 仅受信 Host 可发送;普通通道必须保持 nil。
	Usage *BoundRunUsage `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Pull 接入
// ---------------------------------------------------------------------------

// ClaimResponse 是 claim 成功(HTTP 200)时的响应体。
type ClaimResponse struct {
	SchemaVersion     *string `json:"schemaVersion,omitempty"`
	RunID             string  `json:"runId"`
	AgentCode         *string `json:"agentCode"`
	TemplateID        *string `json:"templateId"`
	PositionType      *string `json:"positionType,omitempty"`
	TemplateInputData any     `json:"templateInputData"`
	TaskBrief         any     `json:"taskBrief,omitempty"`
	GlobalParameters  any     `json:"globalParameters,omitempty"`
	UpstreamPayload   any     `json:"upstreamPayload,omitempty"`
	RuntimeContext    any     `json:"runtimeContext,omitempty"`
	LeaseExpireAt     *string `json:"leaseExpireAt"`
}

// CompleteRequest 是 complete 请求体(回传结果)。
type CompleteRequest struct {
	PayloadResult CompleteResult `json:"payload_result"`
	PayloadInfo   *string        `json:"payload_info,omitempty"`
}

// CompleteResult 是 complete 中 payload_result 的结构。
type CompleteResult struct {
	Assessments  any `json:"assessments"`
	Summary      any `json:"summary"`
	Extensions   any `json:"extensions"`
	EvidenceRefs any `json:"evidenceRefs,omitempty"`
}

// CompleteResponse 是 complete 回执(HTTP 200):{ code, accepted, runStatus, runMsg }。
// 结果是否算数只看 accepted。
type CompleteResponse struct {
	Code      *string `json:"code"`
	Accepted  bool    `json:"accepted"`
	RunStatus *string `json:"runStatus,omitempty"`
	RunMsg    *string `json:"runMsg,omitempty"`
}

// HeartbeatResponse 是 heartbeat 回执。
type HeartbeatResponse struct {
	QueueDepth               *uint64 `json:"queueDepth,omitempty"`
	SelfCheckRequired        bool    `json:"selfCheckRequired,omitempty"`
	HeartbeatIntervalSeconds *uint64 `json:"heartbeatIntervalSeconds,omitempty"`
	SelfCheckIntervalSeconds *uint64 `json:"selfCheckIntervalSeconds,omitempty"`
}

// ---------------------------------------------------------------------------
// Push 幂等记录
// ---------------------------------------------------------------------------

// IdempotencyRecordKind 是 Push 幂等缓存记录的种类。
type IdempotencyRecordKind int

const (
	// IdempotencyRecordProcessing 已占用,执行尚未结束;用于阻止并发重放触发第二次副作用。
	IdempotencyRecordProcessing IdempotencyRecordKind = iota
	// IdempotencyRecordSyncDone 同步形态已完成,缓存序列化好的响应体(保持字节一致)。
	IdempotencyRecordSyncDone
	// IdempotencyRecordAsyncAccepted 异步形态已受理,缓存占位响应(202 语义,命中直接返回 202)。
	IdempotencyRecordAsyncAccepted
)

// IdempotencyRecord 是幂等缓存中的一条记录。
type IdempotencyRecord struct {
	Kind        IdempotencyRecordKind
	RunID       string
	RequestHash string
	// ResponseBody 仅 IdempotencyRecordSyncDone 时有值。
	ResponseBody string
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// jsonString 把任意值序列化为紧凑 JSON 文本;失败时返回 "null"。
// 对应 Rust 侧 Value::to_string() 的语义。
func jsonString(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

// asMapValue 在值是 JSON 对象时返回它。
func asMapValue(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// asStringSlice 提取 JSON 数组里的字符串项,跳过非字符串。
func asStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var values []string
	for _, item := range items {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

// trimUpper 去空白并转大写,用于标的代码规范化。
func trimUpper(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}
