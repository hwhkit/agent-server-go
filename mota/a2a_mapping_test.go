package mota

// 对应 rs src/a2a/mapping.rs 的单测。

import (
	"strings"
	"testing"
)

func a2aMappingTestAgent(systemPrompt string) *AgentConfig {
	return &AgentConfig{
		ID: 1, Slug: "desk", Name: "Desk", Enabled: true,
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeLocal,
		SigningSecret: "0123456789abcdef", SignatureEncoding: "hex",
		SystemPrompt: systemPrompt, WorkspacePath: ".", PermissionMode: "dontAsk",
		TimeoutSecs: 600, MaxConcurrency: 1, QueueCapacity: 10, Revision: 1,
	}
}

func a2aMappingMessage(parts []A2APart) *A2AMessage {
	return &A2AMessage{MessageID: "msg-1", Parts: parts}
}

func TestA2AMapDataPartIsParsedAsBoundTaskEnvelope(t *testing.T) {
	envelope := map[string]any{
		"positionType": "TECHNICAL_ANALYST",
		"templateInputData": []any{
			map[string]any{"name": "securities", "value": []any{"AAPL", "MSFT"}},
		},
		"globalParameters": map[string]any{
			"language": "zh-CN", "symbolPool": []any{"AAPL", "MSFT"}, "universeMode": "RESTRICTED",
		},
	}
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartData(envelope)}),
		a2aMappingTestAgent("strategist"), "task-1")

	if !mapped.Structured {
		t.Error("expected structured mapping")
	}
	if mapped.Context.PositionType == nil || *mapped.Context.PositionType != "TECHNICAL_ANALYST" {
		t.Errorf("positionType = %v", mapped.Context.PositionType)
	}
	if mapped.Context.Language == nil || *mapped.Context.Language != "zh-CN" {
		t.Errorf("language = %v", mapped.Context.Language)
	}
	if len(mapped.Context.SymbolPool) != 2 || mapped.Context.SymbolPool[0] != "AAPL" {
		t.Errorf("symbolPool = %v", mapped.Context.SymbolPool)
	}
	if mapped.Context.UniverseMode == nil || *mapped.Context.UniverseMode != "RESTRICTED" {
		t.Errorf("universeMode = %v", mapped.Context.UniverseMode)
	}
	params, ok := mapped.Context.Params.(map[string]any)
	if !ok {
		t.Fatalf("params is not an object: %v", mapped.Context.Params)
	}
	securities, ok := params["securities"].([]any)
	if !ok || len(securities) == 0 || securities[0] != "AAPL" {
		t.Errorf("params.securities = %v", params["securities"])
	}
	if mapped.Context.RunID == nil || *mapped.Context.RunID != "task-1" {
		t.Errorf("runId = %v", mapped.Context.RunID)
	}
}

func TestA2AMapPositionTypeFallsBackToAgentRole(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartData(map[string]any{"templateInputData": []any{}})}),
		a2aMappingTestAgent("risk_manager"), "task-1")
	if mapped.Context.PositionType == nil || *mapped.Context.PositionType != "RISK_MANAGER" {
		t.Errorf("positionType = %v", mapped.Context.PositionType)
	}
}

func TestA2AMapTextOnlyMessageBecomesUserInstructions(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartText("请评估 AAPL 的基本面")}),
		a2aMappingTestAgent("fundamental_analyst"), "task-9")
	if mapped.Structured {
		t.Error("expected text-channel mapping")
	}
	brief, ok := mapped.Context.TaskBrief.(map[string]any)
	if !ok {
		t.Fatalf("taskBrief is not an object: %v", mapped.Context.TaskBrief)
	}
	if brief["userCustomInstructions"] != "请评估 AAPL 的基本面" {
		t.Errorf("userCustomInstructions = %v", brief["userCustomInstructions"])
	}
	if mapped.Context.PositionType == nil || *mapped.Context.PositionType != "FUNDAMENTAL_ANALYST" {
		t.Errorf("positionType = %v", mapped.Context.PositionType)
	}
}

func TestA2AMapTextIsMergedIntoStructuredBriefWithoutOverwriting(t *testing.T) {
	envelope := map[string]any{
		"taskBrief":         map[string]any{"userCustomInstructions": "覆盖全部 2 只"},
		"templateInputData": []any{},
	}
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartData(envelope), A2APartText("附加:只看现金流")}),
		a2aMappingTestAgent("fundamental_analyst"), "task-1")
	brief := mapped.Context.TaskBrief.(map[string]any)
	instructions := brief["userCustomInstructions"].(string)
	if !strings.Contains(instructions, "覆盖全部 2 只") {
		t.Errorf("instructions = %q", instructions)
	}
	if !strings.Contains(instructions, "只看现金流") {
		t.Errorf("instructions = %q", instructions)
	}
}

func TestA2AMapFailurePathKeywordIsDetectedThroughText(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartText("现有信息不足以支撑结论")}),
		a2aMappingTestAgent("fundamental_analyst"), "task-1")
	if !mapped.Context.ForceFailureDefault {
		t.Error("expected ForceFailureDefault to be true")
	}
}

func TestA2AMapMultipleTextPartsAreConcatenated(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartText("第一段"), A2APartText("第二段")}),
		a2aMappingTestAgent("strategist"), "task-1")
	brief := mapped.Context.TaskBrief.(map[string]any)
	if brief["userCustomInstructions"] != "第一段\n第二段" {
		t.Errorf("instructions = %v", brief["userCustomInstructions"])
	}
}

func TestA2AMapNonObjectDataFallsBackToTextChannel(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartData([]any{1, 2, 3}), A2APartText("看这个")}),
		a2aMappingTestAgent("strategist"), "task-1")
	if mapped.Structured {
		t.Error("expected text-channel mapping for non-object data")
	}
	params := mapped.Context.Params.(map[string]any)
	input, ok := params["input"].([]any)
	if !ok || len(input) != 3 {
		t.Errorf("params.input = %v", params["input"])
	}
}

func TestA2AMapCustomPromptLeavesPositionTypeUnset(t *testing.T) {
	mapped := A2AMapRequest(
		a2aMappingMessage([]A2APart{A2APartText("hi")}),
		a2aMappingTestAgent("你是一个特殊助手"), "task-1")
	if mapped.Context.PositionType != nil {
		t.Errorf("positionType = %v, want nil", mapped.Context.PositionType)
	}
}

func TestA2AArtifactCarriesPayloadResultThenSummary(t *testing.T) {
	output := &ProcessedOutput{
		PayloadResult: map[string]any{"signals": []any{map[string]any{"symbol": "AAPL"}}},
		Summary:       "两条信号",
		Extensions:    []any{},
	}
	artifact := A2AArtifactFromOutput("artifact-1", a2aMappingTestAgent("technical_analyst"), output)

	if artifact.ArtifactID != "artifact-1" {
		t.Errorf("artifactId = %q", artifact.ArtifactID)
	}
	if len(artifact.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(artifact.Parts))
	}
	payload, ok := A2APayloadFromArtifact(&artifact)
	if !ok {
		t.Fatal("expected a data part")
	}
	payloadMap := payload.(map[string]any)
	result := payloadMap["payloadResult"].(map[string]any)
	signals := result["signals"].([]any)
	if signals[0].(map[string]any)["symbol"] != "AAPL" {
		t.Errorf("symbol = %v", signals[0])
	}
	if payloadMap["contractVersion"] != "V2" {
		t.Errorf("contractVersion = %v", payloadMap["contractVersion"])
	}
	if artifact.Parts[1].Text == nil || *artifact.Parts[1].Text != "两条信号" {
		t.Errorf("summary text = %v", artifact.Parts[1].Text)
	}
	if artifact.Parts[0].MediaType == nil || *artifact.Parts[0].MediaType != "application/json" {
		t.Errorf("mediaType = %v", artifact.Parts[0].MediaType)
	}
}

func TestA2AArtifactOmitsEmptySummaryPart(t *testing.T) {
	output := &ProcessedOutput{
		PayloadResult: map[string]any{"assessments": []any{}},
		Summary:       "   ",
		Extensions:    []any{},
	}
	artifact := A2AArtifactFromOutput("a-1", a2aMappingTestAgent("fundamental_analyst"), output)
	if len(artifact.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(artifact.Parts))
	}
}
