package mota

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ToTransportPayload
// ---------------------------------------------------------------------------

// PayloadResult 为对象时原样透传。
func TestToTransportPayloadUsesPayloadResultWhenPresent(t *testing.T) {
	out := &ProcessedOutput{
		Assessments:   []any{"ignored"},
		Summary:       "human readable",
		Extensions:    []any{},
		PayloadResult: map[string]any{"gate": "PASS", "checks": []any{}},
	}
	payload := out.ToTransportPayload(nil)
	result, ok := payload.PayloadResult.(map[string]any)
	if !ok {
		t.Fatalf("payload_result should be an object, got %T", payload.PayloadResult)
	}
	if result["gate"] != "PASS" {
		t.Errorf("gate = %v, want PASS", result["gate"])
	}
	if _, present := result["assessments"]; present {
		t.Error("an explicit payload_result must not be merged with assessments")
	}
	if payload.ContractVersion != "V2" {
		t.Errorf("contract_version = %q, want V2", payload.ContractVersion)
	}
	if payload.PayloadInfo != "human readable" {
		t.Errorf("payload_info = %q, want the summary text", payload.PayloadInfo)
	}
}

// PayloadResult 缺失或不是对象时,用 assessments/summary/extensions 合成。
func TestToTransportPayloadSynthesizesFromLegacyFields(t *testing.T) {
	for name, payloadResult := range map[string]any{
		"nil":    nil,
		"string": "not an object",
		"array":  []any{1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			out := &ProcessedOutput{
				Assessments:   []any{map[string]any{"symbol": "AAPL.US"}},
				Summary:       "summary text",
				Extensions:    []any{},
				PayloadResult: payloadResult,
			}
			result, ok := out.ToTransportPayload(nil).PayloadResult.(map[string]any)
			if !ok {
				t.Fatalf("payload_result should be an object")
			}
			for _, key := range []string{"assessments", "summary", "extensions"} {
				if _, present := result[key]; !present {
					t.Errorf("synthesized payload_result missing %q: %v", key, result)
				}
			}
			if _, present := result["evidenceRefs"]; present {
				t.Error("evidenceRefs must be omitted when empty")
			}
		})
	}
}

// evidence_refs 非空时才写入 evidenceRefs。
func TestToTransportPayloadIncludesEvidenceRefsWhenPresent(t *testing.T) {
	out := &ProcessedOutput{
		Assessments:  []any{},
		Summary:      "s",
		Extensions:   []any{},
		EvidenceRefs: []any{map[string]any{"sourceType": "TASK_INPUT", "sourceId": "task"}},
	}
	result, ok := out.ToTransportPayload(nil).PayloadResult.(map[string]any)
	if !ok {
		t.Fatal("payload_result should be an object")
	}
	refs, ok := result["evidenceRefs"].([]any)
	if !ok || len(refs) != 1 {
		t.Errorf("evidenceRefs = %v, want one entry", result["evidenceRefs"])
	}
}

// 信封必须总是携带 usage(RULE-BND-001):有真实计量用真实值,没有则兜底零值。
func TestToTransportPayloadAlwaysReportsUsage(t *testing.T) {
	usage := FinalReportedUsage(10, nil, 4, nil, 1)
	out := &ProcessedOutput{Assessments: []any{}, Summary: "s", Extensions: []any{}, Usage: usage}

	if payload := out.ToTransportPayload(nil); payload.Usage != usage {
		t.Error("a reported usage must be forwarded verbatim")
	}
	// 没有可靠计量时兜底最小合规 FINAL usage(平台要求 totalTokens > 0),
	// 而不是省略字段(后端会拒收)或零值(判 RUNTIME_USAGE_INVALID)。
	out.Usage = nil
	payload := out.ToTransportPayload(nil)
	if payload.Usage == nil {
		t.Fatal("usage must never be omitted: backend rejects envelopes without it")
	}
	if payload.Usage.PromptTokens != 1 || payload.Usage.CompletionTokens != 1 ||
		payload.Usage.TotalTokens != 2 || payload.Usage.InvocationCount != 1 ||
		payload.Usage.Status != "FINAL" || payload.Usage.Estimated {
		t.Errorf("fallback usage must be minimal FINAL usage (1/1/2), got %+v", payload.Usage)
	}
	// 便捷入口同样必须带 usage。
	out.Usage = usage
	if out.ToPayload().Usage == nil {
		t.Error("ToPayload must include usage")
	}
	language := "zh-CN"
	if out.ToPayloadWithLanguage(&language).Usage == nil {
		t.Error("ToPayloadWithLanguage must include usage")
	}
}

// language 不再改写 payload_info(rs 明确不追加固定文案)。
func TestToTransportPayloadDoesNotAppendLanguageBoilerplate(t *testing.T) {
	out := &ProcessedOutput{Assessments: []any{}, Summary: "原始摘要", Extensions: []any{}}
	for _, language := range []*string{nil, strPtr("zh-CN"), strPtr("en-US")} {
		if got := out.ToTransportPayload(language).PayloadInfo; got != "原始摘要" {
			t.Errorf("payload_info = %q, want the untouched summary", got)
		}
	}
}

func strPtr(value string) *string { return &value }

// ---------------------------------------------------------------------------
// ExtractPayloadInfoText
// ---------------------------------------------------------------------------

func TestExtractPayloadInfoText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
		ok    bool
	}{
		{"plain string", "已完成分析。", "已完成分析。", true},
		{"trims whitespace", "  已完成  ", "已完成", true},
		{"empty string", "   ", "", false},
		{"nil", nil, "", false},
		{"number", 42, "", false},
		{"object with payload_info", map[string]any{"payload_info": "摘要"}, "摘要", true},
		{"object without payload_info", map[string]any{"summary": "x"}, "", false},
		{
			"stringified envelope is unwrapped",
			`{"payload_info":"真正的摘要"}`,
			"真正的摘要", true,
		},
		{
			"nested object then string",
			map[string]any{"payload_info": map[string]any{"payload_info": "深层摘要"}},
			"深层摘要", true,
		},
		{
			"json array text is returned verbatim",
			`[1,2,3]`,
			"[1,2,3]", true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractPayloadInfoText(tc.value)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %t), want (%q, %t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// 深度超过 3 层时停止解包,避免恶意构造导致无限递归。
func TestExtractPayloadInfoTextStopsAtDepthLimit(t *testing.T) {
	deep := map[string]any{
		"payload_info": map[string]any{
			"payload_info": map[string]any{
				"payload_info": map[string]any{
					"payload_info": "too deep",
				},
			},
		},
	}
	if _, ok := ExtractPayloadInfoText(deep); ok {
		t.Error("nesting beyond the depth limit must not resolve")
	}
}

// SummaryText:能提取就用提取结果,否则回退 JSON 文本。
func TestSummaryTextFallsBackToJSON(t *testing.T) {
	out := &ProcessedOutput{Summary: map[string]any{"unrelated": "value"}}
	got := out.SummaryText()
	if !strings.Contains(got, "unrelated") {
		t.Errorf("SummaryText should fall back to the JSON text, got %q", got)
	}
	out.Summary = "直接文本"
	if got := out.SummaryText(); got != "直接文本" {
		t.Errorf("SummaryText = %q, want 直接文本", got)
	}
}

// ---------------------------------------------------------------------------
// ExecutorResultJSON
// ---------------------------------------------------------------------------

func TestExecutorResultJSON(t *testing.T) {
	// nil 时不落库。
	if (&ProcessedOutput{}).ExecutorResultJSON() != nil {
		t.Error("a nil executor output must not be serialized")
	}
	// 已经是字符串时原样返回,避免二次转义。
	out := &ProcessedOutput{ExecutorOutput: `{"already":"json"}`}
	got := out.ExecutorResultJSON()
	if got == nil || *got != `{"already":"json"}` {
		t.Errorf("got %v, want the string verbatim", got)
	}
	// 对象被序列化为紧凑 JSON。
	out.ExecutorOutput = map[string]any{"gate": "PASS"}
	got = out.ExecutorResultJSON()
	if got == nil || *got != `{"gate":"PASS"}` {
		t.Errorf("got %v, want {\"gate\":\"PASS\"}", got)
	}
}

// ---------------------------------------------------------------------------
// PlaceholderOutput / TaskContext
// ---------------------------------------------------------------------------

func TestPlaceholderOutputShape(t *testing.T) {
	out := PlaceholderOutput("摘要")
	payload := out.ToTransportPayload(nil)
	if payload.PayloadInfo != "摘要" {
		t.Errorf("payload_info = %q, want 摘要", payload.PayloadInfo)
	}
	result, ok := payload.PayloadResult.(map[string]any)
	if !ok {
		t.Fatal("payload_result should be an object")
	}
	assessments, ok := result["assessments"].([]any)
	if !ok || len(assessments) != 0 {
		t.Errorf("assessments = %v, want an empty array", result["assessments"])
	}
}

func TestTaskContextIsFinal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime any
		want    bool
	}{
		{"nil", nil, false},
		{"empty object", map[string]any{}, false},
		{"isFinal true", map[string]any{"isFinal": true}, true},
		{"isFinal false", map[string]any{"isFinal": false}, false},
		{"isFinal non-bool", map[string]any{"isFinal": "true"}, false},
		{"not an object", "isFinal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &TaskContext{RuntimeContext: tc.runtime}
			if got := task.IsFinal(); got != tc.want {
				t.Errorf("IsFinal() = %t, want %t", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PlaceholderProcessor
// ---------------------------------------------------------------------------

func runPlaceholder(t *testing.T, task *TaskContext) *ProcessedOutput {
	t.Helper()
	out, err := PlaceholderProcessor{}.Process(context.Background(), task)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

// securities 优先于 symbols。
func TestPlaceholderProcessorPrefersSecurities(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		RunID: strPtr("run-1"),
		Params: map[string]any{
			"securities": []any{"AAPL.US", "MSFT.US"},
			"symbols":    []any{"IGNORED.US"},
		},
	})
	assessments, ok := out.Assessments.([]any)
	if !ok || len(assessments) != 2 {
		t.Fatalf("assessments = %v, want 2 entries", out.Assessments)
	}
	first, ok := assessments[0].(map[string]any)
	if !ok {
		t.Fatal("assessment should be an object")
	}
	// 岗位契约必填四字段;fair_value 必须是数字而非 null。
	for _, key := range []string{"symbol", "rating", "fair_value", "reasoning"} {
		if _, present := first[key]; !present {
			t.Errorf("assessment missing %q: %v", key, first)
		}
	}
	if first["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v, want AAPL.US", first["symbol"])
	}
	if first["rating"] != "HOLD" {
		t.Errorf("rating = %v, want HOLD", first["rating"])
	}
	if _, isFloat := first["fair_value"].(float64); !isFloat {
		t.Errorf("fair_value must be numeric, got %T", first["fair_value"])
	}
}

// 无入参标的时回落到 globalParameters.symbolPool。
func TestPlaceholderProcessorFallsBackToSymbolPool(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		RunID:      strPtr("run-1"),
		Params:     map[string]any{},
		SymbolPool: []string{"AAPL.US", "MSFT.US", "TSLA.US"},
	})
	assessments, _ := out.Assessments.([]any)
	if len(assessments) != 3 {
		t.Errorf("len = %d, want 3 from the symbol pool", len(assessments))
	}
}

// taskBrief 指定目标数量时截断到该数量。
func TestPlaceholderProcessorTruncatesToTargetCount(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		RunID:      strPtr("run-1"),
		Params:     map[string]any{},
		SymbolPool: []string{"A.US", "B.US", "C.US", "D.US"},
		TaskBrief:  map[string]any{"userCustomInstructions": "精选 2 只"},
	})
	assessments, _ := out.Assessments.([]any)
	if len(assessments) != 2 {
		t.Errorf("len = %d, want 2", len(assessments))
	}
}

// 目标数量超出可用标的时不截断。
func TestPlaceholderProcessorIgnoresOutOfRangeTargetCount(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		RunID:      strPtr("run-1"),
		Params:     map[string]any{},
		SymbolPool: []string{"A.US", "B.US"},
		TaskBrief:  map[string]any{"userCustomInstructions": "精选 9 只"},
	})
	assessments, _ := out.Assessments.([]any)
	if len(assessments) != 2 {
		t.Errorf("len = %d, want 2 (target above the pool size must not extend it)", len(assessments))
	}
}

// summary 汇报收到的输入维度与 run_id。
func TestPlaceholderProcessorSummaryReportsDimensions(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		RunID: strPtr("run-42"),
		Params: map[string]any{
			"securities":         []any{"AAPL.US"},
			"financial_analysis": map[string]any{},
			"research_report":    "doc",
		},
	})
	summary, ok := out.Summary.(string)
	if !ok {
		t.Fatalf("summary should be a string, got %T", out.Summary)
	}
	for _, want := range []string{"1 symbol(s)", "financial_analysis", "research_report", "run-42"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q should mention %q", summary, want)
		}
	}
}

// 无维度时 summary 写 none;无 run_id 时写 "-"。
func TestPlaceholderProcessorSummaryDefaults(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{Params: map[string]any{}})
	summary, _ := out.Summary.(string)
	if !strings.Contains(summary, "dimensions: none") {
		t.Errorf("summary %q should say 'dimensions: none'", summary)
	}
	if !strings.Contains(summary, "run_id: -") {
		t.Errorf("summary %q should fall back to run_id: -", summary)
	}
}

// extensions 必须是数组(契约 additionalProperties:false)。
func TestPlaceholderProcessorExtensionsIsArray(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{Params: map[string]any{}})
	if _, ok := out.Extensions.([]any); !ok {
		t.Errorf("extensions must be an array, got %T", out.Extensions)
	}
}

// 按 positionType 给出最小岗位骨架。
func TestPlaceholderProcessorPositionSkeletons(t *testing.T) {
	for _, tc := range []struct {
		position string
		wantKeys []string
	}{
		{"TECHNICAL_ANALYST", []string{"signals"}},
		{"PORTFOLIO_MANAGER", []string{"decision_type", "rationale"}},
		{"RISK_MANAGER", []string{"gate", "checks"}},
		{"EXECUTION_TRADER", []string{"orders"}},
		{"STRATEGIST", []string{"signals", "rationale"}},
		{"QUANT_TRADING_AGENT", []string{"assessments", "summary", "extensions"}},
		{"SENTINEL", []string{"assessments", "summary", "extensions"}},
	} {
		t.Run(tc.position, func(t *testing.T) {
			position := tc.position
			out := runPlaceholder(t, &TaskContext{
				RunID: strPtr("run-1"), Params: map[string]any{}, PositionType: &position,
			})
			result, ok := out.PayloadResult.(map[string]any)
			if !ok {
				t.Fatalf("payload_result should be an object for %s, got %T", position, out.PayloadResult)
			}
			for _, key := range tc.wantKeys {
				if _, present := result[key]; !present {
					t.Errorf("%s skeleton missing %q: %v", position, key, result)
				}
			}
			// executor_output 与 payload_result 同源,供运行记录审计。
			if out.ExecutorOutput == nil {
				t.Errorf("%s should record an executor output", position)
			}
		})
	}
}

// 风控骨架的 gate 为 WARN(保守),PM 为 NO_ACTION。
func TestPlaceholderProcessorConservativeSkeletonValues(t *testing.T) {
	risk := "RISK_MANAGER"
	out := runPlaceholder(t, &TaskContext{Params: map[string]any{}, PositionType: &risk})
	result, _ := out.PayloadResult.(map[string]any)
	if result["gate"] != "WARN" {
		t.Errorf("risk gate = %v, want WARN", result["gate"])
	}

	pm := "PORTFOLIO_MANAGER"
	out = runPlaceholder(t, &TaskContext{Params: map[string]any{}, PositionType: &pm})
	result, _ = out.PayloadResult.(map[string]any)
	if result["decision_type"] != "NO_ACTION" {
		t.Errorf("decision_type = %v, want NO_ACTION", result["decision_type"])
	}
}

// 未知/缺省岗位不产出 payload_result,由信封合成兜底。
func TestPlaceholderProcessorUnknownPositionHasNoSkeleton(t *testing.T) {
	for _, position := range []*string{nil, strPtr("FUNDAMENTAL_ANALYST"), strPtr("UNKNOWN_ROLE")} {
		out := runPlaceholder(t, &TaskContext{Params: map[string]any{}, PositionType: position})
		if out.PayloadResult != nil {
			t.Errorf("position %v should not produce a skeleton, got %v", position, out.PayloadResult)
		}
		// 信封仍然合法。
		result, ok := out.ToTransportPayload(nil).PayloadResult.(map[string]any)
		if !ok {
			t.Fatal("payload_result should be synthesized")
		}
		if _, present := result["assessments"]; !present {
			t.Errorf("synthesized payload_result missing assessments: %v", result)
		}
	}
}

// 占位处理器不生成 evidenceRefs。
func TestPlaceholderProcessorProducesNoEvidenceRefs(t *testing.T) {
	out := runPlaceholder(t, &TaskContext{
		Params: map[string]any{"research_report": "doc-1", "chart_period": "1D"},
	})
	if len(out.EvidenceRefs) != 0 {
		t.Errorf("EvidenceRefs = %v, want empty", out.EvidenceRefs)
	}
	encoded, err := json.Marshal(out.ToTransportPayload(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "evidenceRefs") {
		t.Errorf("envelope must not contain evidenceRefs: %s", encoded)
	}
}

// ctx 已取消时立即返回错误。
func TestPlaceholderProcessorHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PlaceholderProcessor{}.Process(ctx, &TaskContext{Params: map[string]any{}})
	if err == nil {
		t.Error("a cancelled context must abort processing")
	}
}
