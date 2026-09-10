package mota

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// BoundRunUsage
// ---------------------------------------------------------------------------

func uint64Ptr(value uint64) *uint64 { return &value }

// 对应 rs trusted_usage_serializes_with_contract_field_names。
func TestFinalReportedUsageSerializesWithContractFieldNames(t *testing.T) {
	usage := FinalReportedUsage(120, uint64Ptr(30), 40, uint64Ptr(10), 2)
	if usage == nil {
		t.Fatal("expected a valid usage report")
	}
	payload := &PayloadV1{
		PayloadResult:   map[string]any{},
		PayloadInfo:     "done",
		ContractVersion: "V2",
		Usage:           usage,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := decoded["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %s", encoded)
	}
	for key, want := range map[string]any{
		"schemaVersion":     "1.0",
		"status":            "FINAL",
		"promptTokens":      float64(120),
		"cachedInputTokens": float64(30),
		"completionTokens":  float64(40),
		"reasoningTokens":   float64(10),
		"totalTokens":       float64(160),
		"invocationCount":   float64(2),
		"estimated":         false,
	} {
		if got[key] != want {
			t.Errorf("usage[%q] = %v, want %v", key, got[key], want)
		}
	}
}

// 对应 rs ordinary_payload_omits_usage。
func TestPayloadOmitsUsageWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(&PayloadV1{
		PayloadResult:   map[string]any{},
		PayloadInfo:     "done",
		ContractVersion: "V2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "usage") {
		t.Errorf("payload must omit usage when it is nil: %s", encoded)
	}
}

// 可选计数为 nil 时不出现在 JSON 里。
func TestFinalReportedUsageOmitsNilOptionals(t *testing.T) {
	usage := FinalReportedUsage(10, nil, 4, nil, 1)
	if usage == nil {
		t.Fatal("expected a valid usage report")
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "cachedInputTokens") {
		t.Errorf("cachedInputTokens must be omitted when nil: %s", encoded)
	}
	if strings.Contains(string(encoded), "reasoningTokens") {
		t.Errorf("reasoningTokens must be omitted when nil: %s", encoded)
	}
}

// 对应 rs usage_rejects_inconsistent_subtotals。
func TestFinalReportedUsageRejectsInconsistentSubtotals(t *testing.T) {
	if FinalReportedUsage(10, uint64Ptr(11), 4, nil, 1) != nil {
		t.Error("cached input tokens above the prompt total must be rejected")
	}
	if FinalReportedUsage(10, nil, 4, nil, 0) != nil {
		t.Error("invocation_count of zero must be rejected")
	}
	// 部分供应商独立上报 reasoning,不计入 completion;total 仍为 prompt+completion。
	independent := FinalReportedUsage(10, nil, 4, uint64Ptr(8), 1)
	if independent == nil {
		t.Fatal("independent reasoning tokens must be accepted")
	}
	if independent.TotalTokens != 14 {
		t.Errorf("total_tokens = %d, want 14", independent.TotalTokens)
	}
	if independent.ReasoningTokens == nil || *independent.ReasoningTokens != 8 {
		t.Errorf("reasoning_tokens = %v, want 8", independent.ReasoningTokens)
	}
	// cached == prompt 是合法边界。
	if FinalReportedUsage(10, uint64Ptr(10), 4, nil, 1) == nil {
		t.Error("cached input tokens equal to the prompt total must be accepted")
	}
}

// ---------------------------------------------------------------------------
// BoundTask
// ---------------------------------------------------------------------------

func TestBoundTaskParamsViewFlattensTemplateInputData(t *testing.T) {
	entries := []InputEntry{
		{Name: "securities", Value: []any{"AAPL.US"}},
		{Name: "chart_period", Value: "1D"},
	}
	task := &BoundTask{TemplateInputData: &entries}
	view, ok := task.ParamsView(map[string]any{}).(map[string]any)
	if !ok {
		t.Fatalf("ParamsView should return an object, got %T", task.ParamsView(map[string]any{}))
	}
	if len(view) != 2 {
		t.Errorf("len = %d, want 2", len(view))
	}
	if view["chart_period"] != "1D" {
		t.Errorf("chart_period = %v, want 1D", view["chart_period"])
	}
}

// 空的 templateInputData 产出空对象(而不是回落到 params)。
func TestBoundTaskParamsViewEmptyTemplateInputData(t *testing.T) {
	entries := []InputEntry{}
	task := &BoundTask{TemplateInputData: &entries}
	view, ok := task.ParamsView(map[string]any{"params": map[string]any{"a": 1}}).(map[string]any)
	if !ok {
		t.Fatal("ParamsView should return an object")
	}
	if len(view) != 0 {
		t.Errorf("an explicit empty templateInputData must win over params, got %v", view)
	}
}

// 无 templateInputData 时回落到旧协议的 params。
func TestBoundTaskParamsViewFallsBackToRawParams(t *testing.T) {
	task := &BoundTask{}
	raw := map[string]any{"params": map[string]any{"symbols": []any{"MSFT.US"}}}
	view, ok := task.ParamsView(raw).(map[string]any)
	if !ok {
		t.Fatal("ParamsView should return an object")
	}
	if _, present := view["symbols"]; !present {
		t.Errorf("expected the raw params object, got %v", view)
	}
}

// ParamsBackcompat 优先于 raw body 的 params。
func TestBoundTaskParamsViewPrefersBackcompat(t *testing.T) {
	task := &BoundTask{ParamsBackcompat: map[string]any{"from": "backcompat"}}
	view, ok := task.ParamsView(map[string]any{"params": map[string]any{"from": "raw"}}).(map[string]any)
	if !ok {
		t.Fatal("ParamsView should return an object")
	}
	if view["from"] != "backcompat" {
		t.Errorf("from = %v, want backcompat", view["from"])
	}
}

// 三者都没有时返回空对象。
func TestBoundTaskParamsViewDefaultsToEmptyObject(t *testing.T) {
	view, ok := (&BoundTask{}).ParamsView(map[string]any{}).(map[string]any)
	if !ok || len(view) != 0 {
		t.Errorf("expected an empty object, got %v", view)
	}
}

func TestBoundTaskLanguage(t *testing.T) {
	if (&BoundTask{}).Language() != nil {
		t.Error("missing globalParameters must yield nil")
	}
	if (&BoundTask{GlobalParameters: map[string]any{}}).Language() != nil {
		t.Error("missing language key must yield nil")
	}
	if (&BoundTask{GlobalParameters: map[string]any{"language": 1}}).Language() != nil {
		t.Error("a non-string language must yield nil")
	}
	task := &BoundTask{GlobalParameters: map[string]any{"language": "zh-CN"}}
	if got := task.Language(); got == nil || *got != "zh-CN" {
		t.Errorf("language = %v, want zh-CN", got)
	}
}

// BoundTask 能从平台下发的完整信封反序列化。
func TestBoundTaskUnmarshalsFullEnvelope(t *testing.T) {
	raw := []byte(`{
		"schemaVersion": "mota-agent-bound-task/v1",
		"runId": "run-123",
		"agentCode": "research-one",
		"templateId": "tpl-1",
		"positionType": "TECHNICAL_ANALYST",
		"templateInputData": [{"name": "securities", "value": ["AAPL.US"]}],
		"taskBrief": {"userCustomInstructions": "精选 3 只"},
		"globalParameters": {"language": "zh-CN", "symbolPool": ["AAPL.US"], "universeMode": "SEED"},
		"upstreamPayload": [],
		"upstream": {"nodes": []},
		"runtimeContext": {"isFinal": true},
		"callbackUrl": "https://platform.example.com/cb"
	}`)
	var task BoundTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for name, got := range map[string]*string{
		"schemaVersion": task.SchemaVersion,
		"runId":         task.RunID,
		"agentCode":     task.AgentCode,
		"templateId":    task.TemplateID,
		"positionType":  task.PositionType,
		"callbackUrl":   task.CallbackURL,
	} {
		if got == nil {
			t.Errorf("%s must be populated", name)
		}
	}
	if task.TemplateInputData == nil || len(*task.TemplateInputData) != 1 {
		t.Errorf("templateInputData = %v", task.TemplateInputData)
	}
	if task.RuntimeContext == nil || task.Upstream == nil || task.GlobalParameters == nil {
		t.Error("runtimeContext / upstream / globalParameters must be populated")
	}
	if got := task.Language(); got == nil || *got != "zh-CN" {
		t.Errorf("language = %v, want zh-CN", got)
	}
}

// ---------------------------------------------------------------------------
// TaskParams
// ---------------------------------------------------------------------------

func TestTaskParamsFromValue(t *testing.T) {
	params := TaskParamsFromValue(map[string]any{
		"securities":         []any{"AAPL.US"},
		"symbols":            []any{"MSFT.US"},
		"financial_analysis": map[string]any{"x": 1},
		"chart_period":       "1D",
	})
	if params.Securities == nil || params.Symbols == nil || params.FinancialAnalysis == nil {
		t.Error("mapped fields must be populated")
	}
	if params.CompanyAnnouncement != nil || params.ConsensusEstimate != nil || params.ResearchReport != nil {
		t.Error("absent fields must stay nil")
	}
	if params.Extra["chart_period"] != "1D" {
		t.Errorf("unmapped keys must land in Extra, got %v", params.Extra)
	}
	if len(params.Extra) != 1 {
		t.Errorf("Extra = %v, want only chart_period", params.Extra)
	}
}

// 非对象输入退回全空结构。
func TestTaskParamsFromValueRejectsNonObject(t *testing.T) {
	for _, value := range []any{nil, "text", 42, []any{"a"}} {
		params := TaskParamsFromValue(value)
		if params.Securities != nil || params.Symbols != nil || len(params.Extra) != 0 {
			t.Errorf("TaskParamsFromValue(%v) should be empty, got %+v", value, params)
		}
	}
}

// 普通模板入参不是证据:EvidenceRefs 永远为空。
func TestTaskParamsEvidenceRefsIsAlwaysEmpty(t *testing.T) {
	params := TaskParamsFromValue(map[string]any{"research_report": "doc-1", "chart_period": "1D"})
	if refs := params.EvidenceRefs(); len(refs) != 0 {
		t.Errorf("EvidenceRefs must stay empty, got %v", refs)
	}
}
