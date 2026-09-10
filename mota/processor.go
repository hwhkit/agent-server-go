package mota

// 任务处理器抽象:把 MOTA 任务输入转换为该 agent 的输出 schema。
//
// 默认提供 PlaceholderProcessor 占位实现 —— 只记录任务并按输出 schema 生成结构化
// 占位结果,保证协议链路可跑通。接入真实业务(分析逻辑 / LLM 调用)时实现
// Processor 即可,无需改动 Push/Pull 的协议层。
//
// 对应 Rust 侧 src/processor.rs。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// TaskContext
// ---------------------------------------------------------------------------

// TaskContext 是一次任务执行的上下文(Push 与 Pull 共用)。
type TaskContext struct {
	// RunID Push 时为 X-MoTA-Run-Id;Pull 时为 runId。可能为空。
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
	object, ok := asMapValue(c.RuntimeContext)
	if !ok {
		return false
	}
	value, ok := object["isFinal"].(bool)
	return ok && value
}

// ---------------------------------------------------------------------------
// taskBrief 文本提取
// ---------------------------------------------------------------------------

// briefText 汇总 taskBrief 中用于关键词/数量识别的文本。
// 对象形态只读 userCustomInstructions 与 upstreamRequirements;字符串形态直接用。
func briefText(taskBrief any) string {
	var builder strings.Builder
	if object, ok := asMapValue(taskBrief); ok {
		for _, key := range []string{"userCustomInstructions", "upstreamRequirements"} {
			if value, ok := object[key].(string); ok {
				builder.WriteString(value)
			}
		}
	} else if text, ok := taskBrief.(string); ok {
		builder.WriteString(text)
	}
	return builder.String()
}

// hanNumerals 是中文数字到数值的映射(一 … 十)。
var hanNumerals = []struct {
	glyph rune
	value int
}{
	{'一', 1}, {'二', 2}, {'两', 2}, {'三', 3}, {'四', 4},
	{'五', 5}, {'六', 6}, {'七', 7}, {'八', 8}, {'九', 9}, {'十', 10},
}

// ExtractTargetCount 从 taskBrief 文本解析目标标的数量(如「精选 5 只」「恰好 5 条」
// 「覆盖全部 5 只」)。用于评测断言 EXACT_ITEMS 对齐:全覆盖用例数量 = pool 大小,
// 精选用例数量 < pool 大小。无法识别时返回 (0, false)。
func ExtractTargetCount(taskBrief any) (int, bool) {
	text := briefText(taskBrief)
	if text == "" {
		return 0, false
	}
	runes := []rune(text)
	for i := 0; i < len(runes); {
		if runes[i] >= '0' && runes[i] <= '9' {
			start := i
			for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
				i++
			}
			number, err := parseDecimalRunes(runes[start:i])
			if err != nil {
				// Rust 侧解析失败会直接放弃整段扫描。
				return 0, false
			}
			if value, ok := afterCountUnit(runes, i, number); ok {
				return value, true
			}
			i++
			continue
		}
		if number, ok := hanNumeralAt(runes[i]); ok {
			if value, ok := afterCountUnit(runes, i+1, number); ok {
				return value, true
			}
		}
		i++
	}
	return 0, false
}

func hanNumeralAt(glyph rune) (int, bool) {
	for _, item := range hanNumerals {
		if item.glyph == glyph {
			return item.value, true
		}
	}
	return 0, false
}

func parseDecimalRunes(runes []rune) (int, error) {
	value := 0
	for _, r := range runes {
		digit := int(r - '0')
		if value > (1<<31-1-digit)/10 {
			return 0, fmt.Errorf("numeric overflow")
		}
		value = value*10 + digit
	}
	return value, nil
}

// afterCountUnit 判断数字后是否紧跟计量单位(只/条/个/项/维),是则返回该数量。
// 只接受 1..=100。
func afterCountUnit(runes []rune, index, number int) (int, bool) {
	j := index
	for j < len(runes) && unicode.IsSpace(runes[j]) {
		j++
	}
	if j >= len(runes) {
		return 0, false
	}
	switch runes[j] {
	case '只', '条', '个', '项', '维':
	default:
		return 0, false
	}
	if number < 1 || number > 100 {
		return 0, false
	}
	// 「六维」/「6 个维度」等情况:单位后可再跟 度/数/目。
	k := j + 1
	for k < len(runes) && unicode.IsSpace(runes[k]) {
		k++
	}
	if k < len(runes) && (runes[k] == '度' || runes[k] == '数' || runes[k] == '目') {
		return number, true
	}
	return number, true
}

// failurePathKeywords 是「任务明确要求保守缺省」的关键词。
var failurePathKeywords = []string{
	"不足以支撑",
	"无法完成",
	"保守缺省",
	"无法落地",
	"无法产出",
	"无法执行",
}

// ExtractFailurePath 检测任务简报是否为失败路径(平台要求给保守缺省结构)。
func ExtractFailurePath(taskBrief any) bool {
	_, ok := ExtractFailureReason(taskBrief)
	return ok
}

// ExtractFailureReason 返回失败路径的安全、可展示原因。
// 只回显命中的规则,不回显完整用户输入。
func ExtractFailureReason(taskBrief any) (string, bool) {
	if taskBrief == nil {
		return "", false
	}
	text := briefText(taskBrief)
	for _, keyword := range failurePathKeywords {
		if strings.Contains(text, keyword) {
			return fmt.Sprintf("任务简报命中失败路径规则“%s”，按要求不生成主动决策", keyword), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// globalParameters / upstream 提取
// ---------------------------------------------------------------------------

// ExtractUpstreamSymbols 从上游 payload 提取订单中的标的集合(去重保留顺序)。
//
// 支持两种上游结构:
//  1. upstreamPayload: [{agentInstanceId, payloadResult: {orders: [...]}}]
//  2. upstream(DAG): {edges, nodes: [{agentInstanceId, output: {orders: [...]}}], self}
//
// 只从上游的 orders 提取标的(不收集 target_allocation):
// PRESERVE_UPSTREAM_ORDER_INTENT 只校验 orders;target_allocation 里的 CASH 等是
// 现金/配置占位,不是可成交订单,收集后补全会越出标的池。
func ExtractUpstreamSymbols(upstream any, positionType *string) []string {
	_ = positionType

	var symbols []string
	seen := map[string]bool{}
	collectOrders := func(object map[string]any) {
		orders, ok := object["orders"].([]any)
		if !ok {
			return
		}
		for _, order := range orders {
			entry, ok := asMapValue(order)
			if !ok {
				continue
			}
			raw, ok := entry["symbol"].(string)
			if !ok {
				continue
			}
			if symbol := trimUpper(raw); symbol != "" && !seen[symbol] {
				seen[symbol] = true
				symbols = append(symbols, symbol)
			}
		}
	}

	// 先尝试 upstreamPayload 数组形态。
	if items, ok := upstream.([]any); ok {
		for _, item := range items {
			entry, ok := asMapValue(item)
			if !ok {
				continue
			}
			if result, ok := asMapValue(entry["payloadResult"]); ok {
				collectOrders(result)
			}
		}
	}

	// 再尝试 upstream DAG 结构 (nodes[].output)。
	if len(symbols) == 0 {
		if object, ok := asMapValue(upstream); ok {
			if nodes, ok := object["nodes"].([]any); ok {
				for _, node := range nodes {
					entry, ok := asMapValue(node)
					if !ok {
						continue
					}
					if output, ok := asMapValue(entry["output"]); ok {
						collectOrders(output)
					}
				}
			}
		}
	}
	return symbols
}

// ExtractUniverseMode 从 globalParameters.universeMode 提取投资范围模式;
// 缺失或非法返回 nil(按 RESTRICTED 处理)。
func ExtractUniverseMode(global any) *string {
	object, ok := asMapValue(global)
	if !ok {
		return nil
	}
	raw, ok := object["universeMode"].(string)
	if !ok {
		return nil
	}
	mode := trimUpper(raw)
	if mode == "RESTRICTED" || mode == "SEED" {
		return &mode
	}
	return nil
}

// ExtractSymbolPool 从 globalParameters.symbolPool 提取标的池;缺失/空返回 nil。
func ExtractSymbolPool(global any) []string {
	object, ok := asMapValue(global)
	if !ok {
		return nil
	}
	return asStringSlice(object["symbolPool"])
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
	// 仅用于运行记录审计,不会回传给 MoTA。
	ExecutorOutput any
	// Usage 是执行器原始回执汇总出的 FINAL Token 用量;没有可靠数据时必须为 nil。
	Usage *BoundRunUsage
}

// ToPayload 包装成 mota-agent-payload v1 结果信封(不含 usage)。
func (o *ProcessedOutput) ToPayload() *PayloadV1 {
	return o.ToTransportPayload(nil, false)
}

// ToPayloadWithLanguage 包装结果信封。language 参数为兼容现有调用保留,
// 不再追加固定文案。
func (o *ProcessedOutput) ToPayloadWithLanguage(language *string) *PayloadV1 {
	return o.ToTransportPayload(language, false)
}

// ToTransportPayload 构造协议出口信封。
// 只有已确认受信的 Host 才能设置 includeUsage=true。
func (o *ProcessedOutput) ToTransportPayload(language *string, includeUsage bool) *PayloadV1 {
	_ = language

	var payloadResult any
	if object, ok := asMapValue(o.PayloadResult); ok {
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

	payload := &PayloadV1{
		PayloadResult:   payloadResult,
		PayloadInfo:     o.SummaryText(),
		ContractVersion: "V2",
	}
	if includeUsage {
		payload.Usage = o.Usage
	}
	return payload
}

// SummaryText 返回 summary 的人读文本。
func (o *ProcessedOutput) SummaryText() string {
	if text, ok := ExtractPayloadInfoText(o.Summary); ok {
		return text
	}
	return jsonString(o.Summary)
}

// ExecutorResultJSON 返回执行器原始结果的 JSON 文本,供运行记录审计。
func (o *ProcessedOutput) ExecutorResultJSON() *string {
	return serializeExecutorOutput(o.ExecutorOutput)
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
func serializeExecutorOutput(value any) *string {
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

// ---------------------------------------------------------------------------
// Processor
// ---------------------------------------------------------------------------

// Processor 是任务处理器抽象。实现必须并发安全,会被 Push handler 与 Pull worker 共享。
type Processor interface {
	Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error)
}

// PlaceholderProcessor 是占位处理器:不做真实分析,只记录任务并返回结构化占位结果。
//
// 占位输出:
//   - assessments: 为每个 symbol(若存在)生成一条占位评估;
//   - summary: 说明本次为占位处理、收到哪些输入维度;
//   - extensions: 空数组(契约要求 extensions 为数组,additionalProperties:false)。
type PlaceholderProcessor struct{}

// Process 生成占位结果。
func (PlaceholderProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := task.ParamsStruct()

	// 兼容新旧模板:优先 securities,回退 symbols;评测任务无 templateInputData 时
	// 回退到 globalParameters.symbolPool,并按下发目标数量(精选/全覆盖)对齐。
	symbolsRaw := params.Securities
	if symbolsRaw == nil {
		symbolsRaw = params.Symbols
	}
	symbols := asStringSlice(symbolsRaw)
	if len(symbols) == 0 {
		symbols = task.SymbolPool
	}
	if target, ok := ExtractTargetCount(task.TaskBrief); ok && target >= 1 && target <= len(symbols) {
		symbols = symbols[:target]
	}

	assessments := []any{}
	for _, symbol := range symbols {
		// 岗位契约必填 symbol/rating/fair_value/reasoning;
		// fair_value 必须为数字(null 会 schema 违规)。
		assessments = append(assessments, map[string]any{
			"symbol":     symbol,
			"rating":     "HOLD",
			"fair_value": 0.0,
			"reasoning": fmt.Sprintf(
				"placeholder assessment for %s — replace Processor with real business logic", symbol),
		})
	}

	var dimensions []string
	for _, item := range []struct {
		name  string
		value any
	}{
		{"financial_analysis", params.FinancialAnalysis},
		{"company_announcement", params.CompanyAnnouncement},
		{"consensus_estimate", params.ConsensusEstimate},
		{"research_report", params.ResearchReport},
	} {
		if item.value != nil {
			dimensions = append(dimensions, item.name)
		}
	}
	dimensionText := "none"
	if len(dimensions) > 0 {
		dimensionText = strings.Join(dimensions, ", ")
	}
	runID := "-"
	if task.RunID != nil {
		runID = *task.RunID
	}
	summary := fmt.Sprintf("placeholder result for %d symbol(s) (dimensions: %s) — run_id: %s",
		len(symbols), dimensionText, runID)

	// 占位处理器也按 positionType 给出最小、保守的岗位骨架,避免把基本面
	// assessments 错发给技术面/风控等岗位。真实业务应替换本处理器。
	var payloadResult any
	if task.PositionType != nil {
		switch *task.PositionType {
		case "TECHNICAL_ANALYST":
			payloadResult = map[string]any{"signals": []any{}}
		case "PORTFOLIO_MANAGER":
			payloadResult = map[string]any{
				"decision_type": "NO_ACTION",
				"rationale":     "No action: placeholder processor has no validated portfolio signal.",
			}
		case "RISK_MANAGER":
			payloadResult = map[string]any{"gate": "WARN", "checks": []any{}}
		case "EXECUTION_TRADER":
			payloadResult = map[string]any{"orders": []any{}}
		case "STRATEGIST":
			payloadResult = map[string]any{
				"signals":   []any{},
				"rationale": "No signal: placeholder processor has no validated strategy evidence.",
			}
		case "QUANT_TRADING_AGENT", "SENTINEL":
			payloadResult = map[string]any{
				"assessments": []any{},
				"summary":     summary,
				"extensions":  []any{},
			}
		}
	}

	return &ProcessedOutput{
		Assessments:    assessments,
		Summary:        summary,
		Extensions:     []any{},
		EvidenceRefs:   params.EvidenceRefs(),
		PayloadResult:  payloadResult,
		ExecutorOutput: payloadResult,
	}, nil
}
