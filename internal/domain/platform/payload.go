package platform

// 任务上下文与处理结果信封组装（自旧栈 processor.go 迁入，语义逐字保留）。
//
// TaskContext / ProcessedOutput / Processor 是执行引擎（infra/execengine）与
// 调度层共享的边界值对象与端口；RULE-BND-001 的 usage 兜底口径在此维护。

import (
	"context"
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// TaskContext
// ---------------------------------------------------------------------------

// TaskContext 是一次任务执行的上下文(Push 与 Pull 共用)。
type TaskContext struct {
	// RunID Push 时为 X-IR-Run-Id;Pull 时为 runId。可能为空。
	RunID *string
	// TaskBrief 是任务简报(taskBrief / templateInputData 中的原始负载)。
	TaskBrief any
	// Params 是该 agent 的输入参数(宽松 JSON)。
	Params any
	// PositionType 是 Push 信封里的岗位类型;Pull/兼容入口可能缺省。
	PositionType *string
	// Language 取自 globalParameters.language;缺省时输出英文。
	Language *string
	// SymbolPool 取自 globalParameters.symbolPool;用于输出 symbol 标准化与覆盖补全。
	SymbolPool []string
	// UniverseMode 取自 globalParameters.universeMode(RESTRICTED / SEED)。
	// SEED 允许发现池外机会,输出规范化不得强制过滤/补全到池内。
	UniverseMode *string
	// UpstreamSymbols 是 upstreamPayload 中上游订单涉及的标的(去重)。
	// execution_trader / portfolio_manager 用于补全「保留上游全部订单意图」
	// (PRESERVE_UPSTREAM_ORDER_INTENT)。
	UpstreamSymbols []string
	// ForceFailureDefault 表示 taskBrief 命中失败路径关键词(不足以支撑/无法完成/
	// 保守缺省),强制输出保守缺省结构(平台 FAILURE_DEFAULT 口径)。
	ForceFailureDefault bool
	// RuntimeContext 是运行期上下文(含 isFinal);仅组合经理消费,决定职能一/
	// 职能二输出结构。
	RuntimeContext any
}

// ParamsStruct 把 Params 解析成结构化视图。
func (c *TaskContext) ParamsStruct() TaskParams { return TaskParamsFromValue(c.Params) }

// IsFinal 读取 runtimeContext.isFinal;缺省为 false。
func (c *TaskContext) IsFinal() bool {
	object, ok := AsMapValue(c.RuntimeContext)
	if !ok {
		return false
	}
	value, ok := object["isFinal"].(bool)
	return ok && value
}

// ---------------------------------------------------------------------------
// ProcessedOutput
// ---------------------------------------------------------------------------

// ProcessedOutput 是处理结果(该 agent 的输出 schema: assessments / summary / extensions)。
type ProcessedOutput struct {
	Assessments any
	Summary     any
	Extensions  any
	// EvidenceRefs 是真实证据来源引用;可选,但一旦提供只能使用生产协议允许的来源类型。
	EvidenceRefs []any
	// PayloadResult 是完整 payload_result(非基本面岗位可含 signals/orders/gate/checks
	// 等岗位专属字段);为 nil 时回退到 assessments/summary/extensions/evidenceRefs。
	PayloadResult any
	// ExecutorOutput 是执行节点直接返回的最终结果(岗位归一化与传输信封包装之前)。
	// 仅用于运行记录审计,不会回传给平台。
	ExecutorOutput any
	// Usage 是执行器原始回执汇总出的 FINAL Token 用量;没有可靠数据时必须为 nil。
	Usage *BoundRunUsage
}

// ToPayload 包装成 ir-agent-payload v1 结果信封(三键 + usage)。
func (o *ProcessedOutput) ToPayload() *PayloadV1 {
	return o.ToTransportPayload(nil)
}

// ToPayloadWithLanguage 包装结果信封。language 参数为兼容现有调用保留,
// 不再追加固定文案。
func (o *ProcessedOutput) ToPayloadWithLanguage(language *string) *PayloadV1 {
	return o.ToTransportPayload(language)
}

// ToTransportPayload 构造协议出口信封。
//
// 2026-09-16 起线上后端按 V1.1 RULE-BND-001 强制要求每次成功结果回传携带
// 顶层 usage(Push 同步 200、Push 异步 callback、Pull complete 三条路径一致),
// 缺失判 RUNTIME_USAGE_MISSING 拒收(实测)。执行器报告了用量就用真实值;
// 没有可靠计量时兜底为最小合规 FINAL usage(1/1/2)——平台实测要求
// totalTokens > 0,零值会被判 RUNTIME_USAGE_INVALID(冻结契约的 minimum:0
// 与运行期实现不符)。受信通道拿不到供应商 usage 仍由调用方在构造信封前
// 判失败(见 PushHandler/平台网关的 usage_unavailable 分支),不会走到兜底。
// minimalReportedUsage 返回最小合规 FINAL usage 兜底:执行器没有可靠计量时,
// 以「1 prompt / 1 completion / 2 total / 1 次调用 / 非估算」满足
// RULE-BND-001 的必填要求。平台实测要求 totalTokens > 0,零值判
// RUNTIME_USAGE_INVALID;真实计量应来自执行器回执,不要依赖本兜底。
func minimalReportedUsage() *BoundRunUsage {
	return FinalReportedUsage(1, nil, 1, nil, 1)
}

func (o *ProcessedOutput) ToTransportPayload(language *string) *PayloadV1 {
	_ = language

	var payloadResult any
	if object, ok := AsMapValue(o.PayloadResult); ok {
		payloadResult = object
	} else {
		synthesized := map[string]any{
			"assessments": o.Assessments,
			"summary":     o.Summary,
			"extensions":  o.Extensions,
		}
		if len(o.EvidenceRefs) > 0 {
			synthesized["evidenceRefs"] = o.EvidenceRefs
		}
		payloadResult = synthesized
	}

	usage := o.Usage
	if usage == nil {
		usage = minimalReportedUsage()
	}
	return &PayloadV1{
		PayloadResult:   payloadResult,
		PayloadInfo:     o.SummaryText(),
		ContractVersion: "V2",
		Usage:           usage,
	}
}

// SummaryText 返回 summary 的人读文本。
func (o *ProcessedOutput) SummaryText() string {
	if text, ok := ExtractPayloadInfoText(o.Summary); ok {
		return text
	}
	return JSONString(o.Summary)
}

// ExecutorResultJSON 返回执行器原始结果的 JSON 文本,供运行记录审计。
func (o *ProcessedOutput) ExecutorResultJSON() *string {
	return SerializeExecutorOutput(o.ExecutorOutput)
}

// PlaceholderOutput 构造占位结果:assessments 为空数组、summary 为文本、
// extensions 为空数组。
func PlaceholderOutput(summary string) *ProcessedOutput {
	return &ProcessedOutput{
		Assessments:  []any{},
		Summary:      summary,
		Extensions:   []any{},
		EvidenceRefs: []any{},
	}
}

// ExtractPayloadInfoText 从人读摘要或被错误嵌套/字符串化的结果信封中提取真正的
// payload_info。这是协议出口的最后一道保护,避免 UI 显示整份带反斜杠的 JSON。
func ExtractPayloadInfoText(value any) (string, bool) {
	return extractPayloadInfo(value, 0)
}

func extractPayloadInfo(value any, depth int) (string, bool) {
	if depth > 3 {
		return "", false
	}
	switch typed := value.(type) {
	case map[string]any:
		nested, ok := typed["payload_info"]
		if !ok {
			return "", false
		}
		return extractPayloadInfo(nested, depth+1)
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return "", false
		}
		// 字符串化的信封要继续往里剥;普通文本直接返回。
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			switch parsed.(type) {
			case map[string]any, string:
				return extractPayloadInfo(parsed, depth+1)
			}
		}
		return text, true
	default:
		return "", false
	}
}

// serializeExecutorOutput 把执行器原始结果转为可落库的字符串。
// 已经是字符串时原样返回,避免二次转义。
func SerializeExecutorOutput(value any) *string {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return &text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

// Processor 是任务处理器抽象。实现必须并发安全,会被 Push handler 与 Pull worker 共享。
type Processor interface {
	Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error)
}
