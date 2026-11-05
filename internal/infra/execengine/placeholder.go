package execengine

// PlaceholderProcessor 是占位处理器（自 ir/processor.go 迁入，语义逐字保留）：
// 不做真实分析，只记录任务并返回结构化占位结果。

import (
	"context"
	"fmt"
	"strings"
)

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
