package mota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// executorRuntime 构造一个指定岗位与执行方式的 AgentRuntime。
func executorRuntime(role string, kind ExecutorKind, mode ExecutionMode) *AgentRuntime {
	return newAgentRuntime(&AgentConfig{
		Slug: "exec-agent", Name: "exec", Enabled: true,
		ExecutorKind: kind, ExecutionMode: mode,
		SystemPrompt: role, PermissionMode: "dontAsk",
		TimeoutSecs: 600, MaxConcurrency: 1, QueueCapacity: 10, Revision: 3,
	})
}

// executorTestConfig 返回一份把 workspace 根目录指向临时目录的平台配置。
func executorTestConfig(t *testing.T) *PlatformConfig {
	t.Helper()
	cfg := testPlatformConfig(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents", "exec-agent"), 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg.WorkspaceRoot = root
	return cfg
}

// newTestExecutor 组装一个可用的执行器。
func newTestExecutor(t *testing.T, cfg *PlatformConfig, processor Processor) *AgentExecutor {
	t.Helper()
	executor, err := NewAgentExecutor(processor, http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("build executor: %v", err)
	}
	return executor
}

// stubProcessor 是可控的进程内处理器替身。
//
// calls 必须是原子的:分发器的并发测试会从多个 goroutine 同时调 Process。
type stubProcessor struct {
	output *ProcessedOutput
	err    error
	delay  time.Duration
	calls  atomic.Int64
}

// callCount 返回累计调用次数。
func (s *stubProcessor) callCount() int64 { return s.calls.Load() }

func (s *stubProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.output, nil
}

// wantExecutionCode 断言 err 是带指定 Code 的 *ExecutionError。
func wantExecutionCode(t *testing.T, err error, code string) *ExecutionError {
	t.Helper()
	var execErr *ExecutionError
	if !errors.As(err, &execErr) {
		t.Fatalf("expected an *ExecutionError, got %T: %v", err, err)
	}
	if execErr.Code != code {
		t.Fatalf("code = %q, want %q (detail: %s)", execErr.Code, code, execErr.Detail)
	}
	return execErr
}

// ---------------------------------------------------------------------------
// ExecutionError
// ---------------------------------------------------------------------------

func TestExecutionErrorPublicMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"execution_timeout":       "agent execution timed out",
		"executor_unavailable":    "agent executor is unavailable",
		"output_too_large":        "agent output exceeded the configured limit",
		"input_too_large":         "agent input exceeded the configured limit",
		"executor_failed":         "agent execution failed",
		"invalid_executor_output": "agent execution failed",
		"processor_failed":        "agent execution failed",
		"":                        "agent execution failed",
	}
	for code, want := range cases {
		if got := newExecutionError(code, "detail").PublicMessage(); got != want {
			t.Errorf("code %q => %q, want %q", code, got, want)
		}
	}
	// Detail 只出现在 Error(),不进 PublicMessage。
	err := newExecutionError("executor_failed", "/usr/local/bin/pi: no such file")
	if strings.Contains(err.PublicMessage(), "/usr/local/bin/pi") {
		t.Error("PublicMessage must not leak internal detail")
	}
	if !strings.Contains(err.Error(), "/usr/local/bin/pi") {
		t.Error("Error() should carry the detail for logs")
	}
}

func TestExecutionErrorExecutorOutput(t *testing.T) {
	t.Parallel()
	if newExecutionError("executor_failed", "x").ExecutorResultJSON() != nil {
		t.Error("an error without executor output must serialize to nil")
	}
	err := newExecutionError("executor_failed", "x").
		WithExecutorOutput(map[string]any{"gate": "WARN"})
	raw := err.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, `"gate":"WARN"`) {
		t.Errorf("executor output = %v, want the JSON payload", raw)
	}
	// 字符串原样返回,不做二次转义。
	textErr := newExecutionError("executor_failed", "x").WithExecutorOutput("plain text")
	if raw := textErr.ExecutorResultJSON(); raw == nil || *raw != "plain text" {
		t.Errorf("string executor output = %v, want it verbatim", raw)
	}
}

// ---------------------------------------------------------------------------
// deepCopyJSONValue
// ---------------------------------------------------------------------------

// 归一化必须在副本上做,否则运行记录里的模型原文会被改写。
func TestDeepCopyJSONValueIsolatesNestedMutations(t *testing.T) {
	t.Parallel()
	original := map[string]any{
		"outer": map[string]any{"inner": []any{map[string]any{"k": "v"}}},
		"list":  []any{1.0, "two"},
	}
	copied, ok := deepCopyJSONValue(original).(map[string]any)
	if !ok {
		t.Fatal("expected a map copy")
	}
	copied["outer"].(map[string]any)["inner"].([]any)[0].(map[string]any)["k"] = "mutated"
	copied["list"].([]any)[0] = 99.0
	if original["outer"].(map[string]any)["inner"].([]any)[0].(map[string]any)["k"] != "v" {
		t.Error("mutating the copy must not affect the original nested map")
	}
	if original["list"].([]any)[0] != 1.0 {
		t.Error("mutating the copy must not affect the original slice")
	}
}

// ---------------------------------------------------------------------------
// 分发矩阵
// ---------------------------------------------------------------------------

func TestExecuteRejectsZeroTimeout(t *testing.T) {
	t.Parallel()
	executor := newTestExecutor(t, executorTestConfig(t), &stubProcessor{})
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := executor.Execute(context.Background(),
			executorRuntime("", ExecutorKindRust, ExecutionModeLocal),
			&TaskContext{}, map[string]any{}, timeout)
		execErr := wantExecutionCode(t, err, "execution_timeout")
		if execErr.Detail != "Agent deadline elapsed before execution started" {
			t.Errorf("detail = %q", execErr.Detail)
		}
	}
}

// rust 执行器优先于执行方式匹配:(rust, gateway) 也走进程内处理器。
func TestExecuteDispatchesRustKindRegardlessOfMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []ExecutionMode{ExecutionModeLocal, ExecutionModeGateway} {
		processor := &stubProcessor{output: PlaceholderOutput("done")}
		executor := newTestExecutor(t, executorTestConfig(t), processor)
		out, err := executor.Execute(context.Background(),
			executorRuntime("", ExecutorKindRust, mode),
			&TaskContext{}, map[string]any{}, time.Minute)
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		if out.Summary != "done" || processor.callCount() != 1 {
			t.Errorf("mode %s: processor was not used (calls=%d)", mode, processor.callCount())
		}
	}
}

func TestExecuteMapsProcessorFailure(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{err: errors.New("analysis blew up")}
	executor := newTestExecutor(t, executorTestConfig(t), processor)
	_, err := executor.Execute(context.Background(),
		executorRuntime("", ExecutorKindRust, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, time.Minute)
	execErr := wantExecutionCode(t, err, "processor_failed")
	if !strings.Contains(execErr.Detail, "analysis blew up") {
		t.Errorf("detail = %q, want the processor error", execErr.Detail)
	}
}

// 处理器超时必须报 execution_timeout,即使它不响应 ctx 取消。
func TestExecuteTimesOutStuckProcessor(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("late"), delay: 5 * time.Second}
	executor := newTestExecutor(t, executorTestConfig(t), processor)
	started := time.Now()
	_, err := executor.Execute(context.Background(),
		executorRuntime("", ExecutorKindRust, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 80*time.Millisecond)
	execErr := wantExecutionCode(t, err, "execution_timeout")
	if execErr.Detail != "Rust processor exceeded its deadline" {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("execute waited %v, want it to give up near the deadline", elapsed)
	}
}

// 不支持的执行器 × 执行方式组合。
func TestExecuteRejectsUnsupportedCombinations(t *testing.T) {
	t.Parallel()
	executor := newTestExecutor(t, executorTestConfig(t), &stubProcessor{})
	// Goose 没有本机 CLI 实现；DeepSeek Harness 已同步 Rust 版 dsh headless。
	for _, kind := range []ExecutorKind{ExecutorKindGoose} {
		_, err := executor.Execute(context.Background(),
			executorRuntime("", kind, ExecutionModeLocal),
			&TaskContext{}, map[string]any{}, time.Minute)
		execErr := wantExecutionCode(t, err, "executor_unavailable")
		want := string(kind) + " does not support local execution"
		if execErr.Detail != want {
			t.Errorf("detail = %q, want %q", execErr.Detail, want)
		}
	}
}

// Claude 被禁用时立即失败,不去碰 workspace。
func TestExecuteClaudeDisabled(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = false
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, time.Minute)
	execErr := wantExecutionCode(t, err, "executor_unavailable")
	if execErr.Detail != "Claude Code executor is disabled" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// ---------------------------------------------------------------------------
// workspace 逃逸检查
// ---------------------------------------------------------------------------

func TestResolveWorkspaceRejectsEscapesAndMissingPaths(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	executor := newTestExecutor(t, cfg, &stubProcessor{})

	if _, execErr := executor.resolveWorkspace("agents/exec-agent"); execErr != nil {
		t.Fatalf("a valid workspace must resolve: %v", execErr)
	}
	// 不存在的路径。
	execErr := rejectedWorkspace(t, executor, "agents/missing")
	if execErr.Code != "executor_unavailable" || !strings.Contains(execErr.Detail, "resolve workspace") {
		t.Errorf("missing workspace => %q / %q", execErr.Code, execErr.Detail)
	}
	// 符号链接指到根目录之外。
	outside := t.TempDir()
	link := filepath.Join(cfg.WorkspaceRoot, "agents", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	execErr = rejectedWorkspace(t, executor, "agents/escape")
	if execErr.Code != "executor_unavailable" || execErr.Detail != "workspace escaped configured root" {
		t.Errorf("escaping symlink => %q / %q", execErr.Code, execErr.Detail)
	}
}

// rejectedWorkspace 断言某个相对路径会被 resolveWorkspace 拒绝。
func rejectedWorkspace(t *testing.T, executor *AgentExecutor, relative string) *ExecutionError {
	t.Helper()
	_, execErr := executor.resolveWorkspace(relative)
	if execErr == nil {
		t.Fatalf("expected %q to be rejected", relative)
	}
	return execErr
}

// 根目录前缀相同但不是子目录的兄弟路径不能放行(如 /root-evil vs /root)。
func TestResolveWorkspaceRejectsSiblingWithSharedPrefix(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "root-evil")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	cfg := testPlatformConfig(t)
	cfg.WorkspaceRoot = root
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	if _, execErr := executor.resolveWorkspace("../root-evil"); execErr == nil {
		t.Error("a sibling directory sharing the root prefix must be rejected")
	}
}

// ---------------------------------------------------------------------------
// conservativeRolePayload
// ---------------------------------------------------------------------------

// 对应 rs conservative_payloads_obey_each_role_primary_contract。
// 每个岗位的保守结构都必须自己就能通过 missingRoleFields —— 否则失败路径会被
// 平台判 PAYLOAD_SCHEMA_INVALID。
func TestConservativePayloadsObeyEachRolePrimaryContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role    string
		isFinal bool
	}{
		{"fundamental_analyst", false},
		{"technical_analyst", false},
		{"strategist", false},
		{"risk_manager", false},
		{"execution_trader", false},
		{"portfolio_manager", false},
		{"portfolio_manager", true},
	}
	for _, item := range cases {
		payload := conservativeRolePayload(item.role, item.isFinal, "test failure", "AAPL.US", true)
		if missing := missingRoleFields(payload, item.role, item.isFinal); len(missing) > 0 {
			t.Errorf("role=%s isFinal=%v missing=%v payload=%s",
				item.role, item.isFinal, missing, jsonString(payload))
		}
	}
	strategist := conservativeRolePayload("strategist", false, "no direction", "", false)
	if signals, ok := strategist["signals"].([]any); !ok || len(signals) != 0 {
		t.Errorf("strategist signals = %#v, want an empty array", strategist["signals"])
	}
	if rationale, ok := strategist["rationale"].(string); !ok || rationale == "" {
		t.Errorf("strategist rationale = %#v, want non-empty text", strategist["rationale"])
	}
	// 未知岗位落到基本面形状。
	unknown := conservativeRolePayload("quant_researcher", false, "why", "", false)
	for _, key := range []string{"assessments", "evidenceRefs", "summary", "extensions"} {
		if _, ok := unknown[key]; !ok {
			t.Errorf("unknown role payload is missing %q", key)
		}
	}
}

// 兜底标的:有则用,没有则 SPY.US。
func TestConservativePayloadFallbackSymbol(t *testing.T) {
	t.Parallel()
	withSymbol := conservativeRolePayload("portfolio_manager", false, "r", "0700.HK", true)
	allocation := withSymbol["target_allocation"].([]any)[0].(map[string]any)
	if allocation["symbol"] != "0700.HK" {
		t.Errorf("symbol = %#v, want 0700.HK", allocation["symbol"])
	}
	withoutSymbol := conservativeRolePayload("portfolio_manager", false, "r", "", false)
	allocation = withoutSymbol["target_allocation"].([]any)[0].(map[string]any)
	if allocation["symbol"] != "SPY.US" {
		t.Errorf("symbol = %#v, want the SPY.US default", allocation["symbol"])
	}
}

// 组合经理两个职能的保守结构必须不同(职能二要 timestamp/report_markdown)。
func TestConservativePayloadPortfolioManagerBranches(t *testing.T) {
	t.Parallel()
	process := conservativeRolePayload("portfolio_manager", false, "r", "AAPL.US", true)
	final := conservativeRolePayload("portfolio_manager", true, "r", "AAPL.US", true)
	if _, ok := process["decision_type"]; !ok {
		t.Error("职能一 must carry decision_type")
	}
	if _, ok := process["timestamp"]; ok {
		t.Error("职能一 must not carry timestamp")
	}
	for _, key := range []string{"timestamp", "orders", "report_markdown"} {
		if _, ok := final[key]; !ok {
			t.Errorf("职能二 is missing %q", key)
		}
	}
	if _, ok := final["decision_type"]; ok {
		t.Error("职能二 must not carry decision_type")
	}
	// timestamp 必须是可解析的 RFC3339。
	if _, err := parseTimestamp(final["timestamp"].(string)); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", final["timestamp"], err)
	}
}

// 保守结构里的原因文案统一带前缀。
func TestConservativePayloadExplanationWording(t *testing.T) {
	t.Parallel()
	payload := conservativeRolePayload("risk_manager", false, "数据不足以支撑决策", "", false)
	check := payload["checks"].([]any)[0].(map[string]any)
	want := "任务未生成可采纳结果。具体原因：数据不足以支撑决策"
	if check["reasoning"] != want {
		t.Errorf("reasoning = %#v, want %q", check["reasoning"], want)
	}
	if check["dimension"] != "OTHERS" || check["status"] != "UNKNOWN" {
		t.Errorf("check = %#v, want OTHERS/UNKNOWN", check)
	}
	if suggestions, ok := payload["hedging_suggestions"].([]any); !ok || len(suggestions) != 1 {
		t.Errorf("hedging_suggestions = %#v, want exactly one entry", payload["hedging_suggestions"])
	}
}

// ---------------------------------------------------------------------------
// failureDefaultOutput
// ---------------------------------------------------------------------------

func TestFailureDefaultOutputShape(t *testing.T) {
	t.Parallel()
	// 基本面岗位:保守结构自带 summary,直接采用。
	out := failureDefaultOutput("fundamental_analyst", false, "无法完成", "AAPL.US", true)
	if out.Summary != "任务未生成可采纳结果。具体原因：无法完成" {
		t.Errorf("summary = %#v", out.Summary)
	}
	if assessments, ok := out.Assessments.([]any); !ok || len(assessments) != 0 {
		t.Errorf("assessments = %#v, want an empty array", out.Assessments)
	}
	if len(out.EvidenceRefs) != 1 {
		t.Fatalf("evidenceRefs = %#v, want one task ref", out.EvidenceRefs)
	}
	ref := out.EvidenceRefs[0].(map[string]any)
	if ref["sourceType"] != "TASK_INPUT" || ref["sourceId"] != "task" {
		t.Errorf("evidence ref = %#v", ref)
	}
	if out.Usage != nil {
		t.Error("the failure path must not report usage")
	}

	// 风控岗位:保守结构没有 summary 键,退化为「保守缺省原因：…」。
	risk := failureDefaultOutput("risk_manager", false, "无法完成", "", false)
	if risk.Summary != "保守缺省原因：无法完成" {
		t.Errorf("risk summary = %#v", risk.Summary)
	}
	if assessments, ok := risk.Assessments.([]any); !ok || len(assessments) != 0 {
		t.Errorf("risk assessments = %#v, want an empty array", risk.Assessments)
	}
}

// 失败路径短路:ForceFailureDefault 时不碰处理器、不碰 CLI。
func TestExecuteShortCircuitsForceFailureDefault(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("should not run")}
	executor := newTestExecutor(t, executorTestConfig(t), processor)
	task := &TaskContext{
		ForceFailureDefault: true,
		TaskBrief:           map[string]any{"userCustomInstructions": "数据不足以支撑决策，请返回保守缺省结果。"},
		SymbolPool:          []string{"NVDA.US"},
	}
	// 即使配成了本机 Pi(二进制不存在),也不该真的去 spawn。
	out, err := executor.Execute(context.Background(),
		executorRuntime("portfolio_manager", ExecutorKindPi, ExecutionModeLocal),
		task, map[string]any{}, time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processor.callCount() != 0 {
		t.Error("the failure path must not invoke the processor")
	}
	payload := out.PayloadResult.(map[string]any)
	if payload["decision_type"] != "NO_ACTION" {
		t.Errorf("decision_type = %#v, want NO_ACTION", payload["decision_type"])
	}
	allocation := payload["target_allocation"].([]any)[0].(map[string]any)
	if allocation["symbol"] != "NVDA.US" {
		t.Errorf("symbol = %#v, want the pool head NVDA.US", allocation["symbol"])
	}
}

// 兜底标的优先取 symbolPool 首项,其次 upstreamSymbols 首项。
func TestOutputPolicyFallbackSymbolPrecedence(t *testing.T) {
	t.Parallel()
	policy := newOutputPolicy(&TaskContext{
		SymbolPool: []string{"AAPL.US"}, UpstreamSymbols: []string{"MSFT.US"},
	})
	if symbol, ok := policy.fallbackSymbol(); !ok || symbol != "AAPL.US" {
		t.Errorf("fallback = %q/%v, want AAPL.US", symbol, ok)
	}
	policy = newOutputPolicy(&TaskContext{UpstreamSymbols: []string{"MSFT.US"}})
	if symbol, ok := policy.fallbackSymbol(); !ok || symbol != "MSFT.US" {
		t.Errorf("fallback = %q/%v, want MSFT.US", symbol, ok)
	}
	policy = newOutputPolicy(&TaskContext{})
	if _, ok := policy.fallbackSymbol(); ok {
		t.Error("no pool and no upstream symbols must yield ok=false")
	}
}

// ---------------------------------------------------------------------------
// roleSummaryText
// ---------------------------------------------------------------------------

// 对应 rs role_summary_uses_role_specific_human_text。
func TestRoleSummaryUsesRoleSpecificHumanText(t *testing.T) {
	t.Parallel()
	got := roleSummaryText(
		usageJSON(t, `{"rationale":"strategy rationale","signals":[]}`),
		"strategist", nil, nil)
	if got != "strategy rationale" {
		t.Errorf("strategist summary = %q", got)
	}
	got = roleSummaryText(
		usageJSON(t, `{"execution_summary":"two orders","orders":[]}`),
		"execution_trader", nil, nil)
	if got != "two orders" {
		t.Errorf("execution_trader summary = %q", got)
	}
	// candidate 自带 payload_info 时优先用它。
	got = roleSummaryText(
		usageJSON(t, `{"gate":"PASS","checks":[]}`),
		"risk_manager", nil,
		usageJSON(t, `{"payload_info":"风险检查通过：现有持仓未超限。"}`))
	if got != "风险检查通过：现有持仓未超限。" {
		t.Errorf("risk_manager summary = %q", got)
	}
}

// fallbackReason 优先于一切。
func TestRoleSummaryFallbackReasonWins(t *testing.T) {
	t.Parallel()
	reason := "模型返回内容不是可解析的 JSON 对象"
	got := roleSummaryText(
		usageJSON(t, `{"summary":"ignored"}`), "fundamental_analyst", &reason,
		usageJSON(t, `{"payload_info":"also ignored"}`))
	if got != "保守缺省原因：模型返回内容不是可解析的 JSON 对象" {
		t.Errorf("summary = %q", got)
	}
}

// 各岗位的字段优先级。
func TestRoleSummaryPreferredKeyOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role    string
		payload string
		want    string
	}{
		{"strategist", `{"rationale":"a","report_markdown":"b","summary":"c"}`, "a"},
		{"strategist", `{"report_markdown":"b","summary":"c"}`, "b"},
		{"strategist", `{"summary":"c"}`, "c"},
		{"portfolio_manager", `{"rationale":"a","report_markdown":"b"}`, "a"},
		{"risk_manager", `{"interpretation":"a","summary":"b","rationale":"c"}`, "a"},
		{"risk_manager", `{"summary":"b","rationale":"c"}`, "b"},
		{"execution_trader", `{"execution_summary":"a","summary":"b"}`, "a"},
		{"fundamental_analyst", `{"summary":"a","rationale":"b"}`, "a"},
		{"technical_analyst", `{"rationale":"b","report_markdown":"c"}`, "b"},
		// 空白值跳过,继续找下一个键。
		{"strategist", `{"rationale":"   ","summary":"c"}`, "c"},
		// 取到的文本会 trim。
		{"strategist", `{"rationale":"  padded  "}`, "padded"},
	}
	for _, item := range cases {
		got := roleSummaryText(usageJSON(t, item.payload), item.role, nil, nil)
		if got != item.want {
			t.Errorf("role=%s payload=%s => %q, want %q", item.role, item.payload, got, item.want)
		}
	}
}

// 所有优先字段都拿不到时的按岗位计数文案。
func TestRoleSummaryCountFallbacks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role    string
		payload string
		want    string
	}{
		{"risk_manager", `{"gate":"BREACH","checks":[{},{},{}]}`, "风险门控：BREACH；已完成 3 项检查。"},
		{"risk_manager", `{}`, "风险门控：UNKNOWN；已完成 0 项检查。"},
		{"risk_manager", `{"gate":"WARN","checks":"not an array"}`, "风险门控：WARN；已完成 0 项检查。"},
		{"execution_trader", `{"orders":[{},{}]}`, "已生成 2 条订单建议。"},
		{"portfolio_manager", `{"orders":[]}`, "已生成 0 条订单建议。"},
		{"technical_analyst", `{"signals":[{}]}`, "已生成 1 条交易信号。"},
		{"strategist", `{"signals":[{},{},{},{}]}`, "已生成 4 条交易信号。"},
	}
	for _, item := range cases {
		got := roleSummaryText(usageJSON(t, item.payload), item.role, nil, nil)
		if got != item.want {
			t.Errorf("role=%s payload=%s => %q, want %q", item.role, item.payload, got, item.want)
		}
	}
	// 未收录岗位:candidate 是非空文本就用它,否则「任务已完成。」
	if got := roleSummaryText(usageJSON(t, `{}`), "quant", nil, "model said this"); got != "model said this" {
		t.Errorf("summary = %q, want the candidate text", got)
	}
	if got := roleSummaryText(usageJSON(t, `{}`), "quant", nil, "   "); got != "任务已完成。" {
		t.Errorf("summary = %q, want the generic completion text", got)
	}
	if got := roleSummaryText(usageJSON(t, `{}`), "quant", nil, nil); got != "任务已完成。" {
		t.Errorf("summary = %q, want the generic completion text", got)
	}
	if got := roleSummaryText(usageJSON(t, `{}`), "fundamental_analyst", nil, "  raw  "); got != "raw" {
		t.Errorf("summary = %q, want the trimmed candidate text", got)
	}
}

// 对应 rs transport_unwraps_stringified_result_envelope_from_payload_info。
func TestTransportUnwrapsStringifiedResultEnvelopeFromPayloadInfo(t *testing.T) {
	t.Parallel()
	nested := jsonString(map[string]any{
		"payload_info":   "风险检查通过：现有持仓未超限，无新增风险敞口。",
		"payload_result": map[string]any{"gate": "PASS", "checks": []any{}},
	})
	output := &ProcessedOutput{
		Assessments:   []any{},
		Summary:       nested,
		Extensions:    []any{},
		EvidenceRefs:  []any{},
		PayloadResult: map[string]any{"gate": "PASS", "checks": []any{}},
	}
	if got := output.ToPayload().PayloadInfo; got != "风险检查通过：现有持仓未超限，无新增风险敞口。" {
		t.Errorf("payload_info = %q", got)
	}
}

// ---------------------------------------------------------------------------
// extractCandidate
// ---------------------------------------------------------------------------

func TestExtractCandidatePrefersStructuredOutput(t *testing.T) {
	t.Parallel()
	// structured_output 优先于 result。
	got := extractCandidate(usageJSON(t, `{"structured_output":{"a":1},"result":{"b":2}}`))
	if _, ok := jsonField(got, "a"); !ok {
		t.Errorf("got %#v, want the structured_output object", got)
	}
	// 只有 result。
	got = extractCandidate(usageJSON(t, `{"result":{"b":2}}`))
	if _, ok := jsonField(got, "b"); !ok {
		t.Errorf("got %#v, want the result object", got)
	}
	// 两者都没有:整个信封就是候选。
	got = extractCandidate(usageJSON(t, `{"c":3}`))
	if _, ok := jsonField(got, "c"); !ok {
		t.Errorf("got %#v, want the whole envelope", got)
	}
	// 字符串候选走剥壳提取。
	fenced := "```json\n{\"d\":4}\n```"
	got = extractCandidate(map[string]any{"result": fenced})
	if _, ok := jsonField(got, "d"); !ok {
		t.Errorf("got %#v, want the fenced JSON unwrapped", got)
	}
	// structured_output 显式为 null 时仍然优先(与 rs 的 get().cloned() 一致)。
	got = extractCandidate(usageJSON(t, `{"structured_output":null,"result":{"b":2}}`))
	if got != nil {
		t.Errorf("got %#v, want nil from the explicit null", got)
	}
}

// ---------------------------------------------------------------------------
// normalizeEvidenceRefs
// ---------------------------------------------------------------------------

func TestNormalizeEvidenceRefs(t *testing.T) {
	t.Parallel()
	// 只保留三个允许键。
	object := map[string]any{"evidenceRefs": []any{map[string]any{
		"sourceType": "MARKET_DATA", "sourceId": "eodhd", "asOf": "2026-01-02",
		"url": "https://leak", "note": "drop me",
	}}}
	normalizeEvidenceRefs(object)
	ref := object["evidenceRefs"].([]any)[0].(map[string]any)
	if len(ref) != 3 {
		t.Errorf("ref = %#v, want exactly the three allowed keys", ref)
	}
	for _, key := range []string{"sourceType", "sourceId", "asOf"} {
		if _, ok := ref[key]; !ok {
			t.Errorf("ref is missing %q", key)
		}
	}

	// sourceType / sourceId 缺失或空白的条目被丢弃;全部不合格时整键删除。
	for _, raw := range []string{
		`{"evidenceRefs":[{"sourceId":"x"}]}`,
		`{"evidenceRefs":[{"sourceType":"x"}]}`,
		`{"evidenceRefs":[{"sourceType":"  ","sourceId":"x"}]}`,
		`{"evidenceRefs":[{"sourceType":"x","sourceId":"\t"}]}`,
		`{"evidenceRefs":[{"sourceType":1,"sourceId":"x"}]}`,
		`{"evidenceRefs":["not an object"]}`,
		`{"evidenceRefs":[]}`,
	} {
		object := usageJSON(t, raw).(map[string]any)
		normalizeEvidenceRefs(object)
		if _, ok := object["evidenceRefs"]; ok {
			t.Errorf("%s: evidenceRefs should have been removed, got %#v", raw, object["evidenceRefs"])
		}
	}

	// 混合:只留合格的那条。
	object = usageJSON(t, `{"evidenceRefs":[{"sourceType":"A","sourceId":"1"},{"sourceType":""},{"sourceType":"B","sourceId":"2"}]}`).(map[string]any)
	normalizeEvidenceRefs(object)
	if refs := object["evidenceRefs"].([]any); len(refs) != 2 {
		t.Errorf("kept %d refs, want 2", len(refs))
	}

	// 非数组不动。
	object = map[string]any{"evidenceRefs": "nope"}
	normalizeEvidenceRefs(object)
	if object["evidenceRefs"] != "nope" {
		t.Errorf("a non-array evidenceRefs must be left alone, got %#v", object["evidenceRefs"])
	}
}

// ---------------------------------------------------------------------------
// missingRequiredKeys
// ---------------------------------------------------------------------------

// 这一层只看键是否存在(空白值不算缺失),唯一例外是风控的空 checks。
func TestMissingRequiredKeysOnlyChecksPresence(t *testing.T) {
	t.Parallel()
	// 基本面:键存在即可,值为空白也放行。
	object := usageJSON(t, `{"assessments":"  "}`).(map[string]any)
	if missing := missingRequiredKeys(object, "fundamental_analyst", "fundamental_analyst", false); len(missing) > 0 {
		t.Errorf("missing = %v, want none (presence only)", missing)
	}
	object = usageJSON(t, `{}`).(map[string]any)
	if missing := missingRequiredKeys(object, "fundamental_analyst", "fundamental_analyst", false); len(missing) != 1 {
		t.Errorf("missing = %v, want [assessments]", missing)
	}

	// 风控:空 checks 数组算缺失。
	object = usageJSON(t, `{"gate":"PASS","checks":[]}`).(map[string]any)
	if missing := missingRequiredKeys(object, "risk_manager", "risk_manager", false); len(missing) != 1 || missing[0] != "checks" {
		t.Errorf("missing = %v, want [checks]", missing)
	}
	// 非空 checks 通过。
	object = usageJSON(t, `{"gate":"PASS","checks":[{}]}`).(map[string]any)
	if missing := missingRequiredKeys(object, "risk_manager", "risk_manager", false); len(missing) > 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	// checks 存在但不是数组:走「只看存在」的分支,不算缺失。
	object = usageJSON(t, `{"gate":"PASS","checks":"oops"}`).(map[string]any)
	if missing := missingRequiredKeys(object, "risk_manager", "risk_manager", false); len(missing) > 0 {
		t.Errorf("missing = %v, want none for a non-array checks", missing)
	}
	// 空 checks 的特判只对风控生效:执行岗的空 orders 不算缺失。
	object = usageJSON(t, `{"orders":[]}`).(map[string]any)
	if missing := missingRequiredKeys(object, "execution_trader", "execution_trader", false); len(missing) > 0 {
		t.Errorf("missing = %v, want none for an empty orders array", missing)
	}
	// 组合经理职能二的三个必填键。
	object = usageJSON(t, `{"orders":[]}`).(map[string]any)
	missing := missingRequiredKeys(object, "portfolio_manager", "portfolio_manager", true)
	if len(missing) != 2 {
		t.Errorf("missing = %v, want timestamp and report_markdown", missing)
	}
}

// ---------------------------------------------------------------------------
// itemsHaveTicker / normalizedSymbolPool
// ---------------------------------------------------------------------------

func TestItemsHaveTicker(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		`[{"ticker":"AAPL.US"}]`:                true,
		`[{"symbol":"AAPL.US"},{"ticker":"X"}]`: true,
		`[{"symbol":"AAPL.US"}]`:                false,
		`[{"ticker":123}]`:                      false,
		`[]`:                                    false,
		`"not an array"`:                        false,
		`null`:                                  false,
	}
	for raw, want := range cases {
		if got := itemsHaveTicker(usageJSON(t, raw)); got != want {
			t.Errorf("%s => %v, want %v", raw, got, want)
		}
	}
}

// 有上游标的时优先保留上游意图;执行/组合岗没有上游时不按 symbolPool 伪造订单。
func TestNormalizedSymbolPoolPrecedence(t *testing.T) {
	t.Parallel()
	withUpstream := &outputPolicy{
		symbolPool: []string{"AAPL.US"}, upstreamSymbols: []string{"MSFT.US", "NVDA.US"},
	}
	if got := normalizedSymbolPool(withUpstream, "execution_trader"); len(got) != 2 || got[0] != "MSFT.US" {
		t.Errorf("got %v, want the upstream symbols", got)
	}
	if got := normalizedSymbolPool(withUpstream, "fundamental_analyst"); len(got) != 2 {
		t.Errorf("got %v, want the upstream symbols for any role", got)
	}

	poolOnly := &outputPolicy{symbolPool: []string{"AAPL.US"}}
	for _, role := range []string{"execution_trader", "portfolio_manager"} {
		if got := normalizedSymbolPool(poolOnly, role); got != nil {
			t.Errorf("role %s: got %v, want nil (do not fabricate orders)", role, got)
		}
	}
	for _, role := range []string{"fundamental_analyst", "technical_analyst", "strategist", "risk_manager"} {
		if got := normalizedSymbolPool(poolOnly, role); len(got) != 1 || got[0] != "AAPL.US" {
			t.Errorf("role %s: got %v, want the symbol pool", role, got)
		}
	}
	// 没有池也没有上游:保持 nil(表示「没有池」,不做过滤与补全)。
	if got := normalizedSymbolPool(&outputPolicy{}, "fundamental_analyst"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// alignRiskChecks
// ---------------------------------------------------------------------------

func TestAlignRiskChecksPadsAndTruncates(t *testing.T) {
	t.Parallel()
	// 少于目标 → 补占位。
	object := usageJSON(t, `{"checks":[{"dimension":"VAR","status":"OK","reasoning":"r"}]}`).(map[string]any)
	alignRiskChecks(object, &outputPolicy{targetCount: 3, hasTargetCount: true})
	checks := object["checks"].([]any)
	if len(checks) != 3 {
		t.Fatalf("checks = %d, want 3", len(checks))
	}
	if checks[0].(map[string]any)["dimension"] != "VAR" {
		t.Error("the model's own check must stay first")
	}
	placeholder := checks[1].(map[string]any)
	if placeholder["dimension"] != "OTHERS" || placeholder["status"] != "UNKNOWN" ||
		placeholder["reasoning"] != riskCheckPlaceholderReasoning {
		t.Errorf("placeholder = %#v", placeholder)
	}

	// 多于目标 → 截断。
	object = usageJSON(t, `{"checks":[{"n":1},{"n":2},{"n":3},{"n":4}]}`).(map[string]any)
	alignRiskChecks(object, &outputPolicy{targetCount: 2, hasTargetCount: true})
	if checks := object["checks"].([]any); len(checks) != 2 {
		t.Errorf("checks = %d, want 2", len(checks))
	}

	// 恰好等于目标 → 不变。
	object = usageJSON(t, `{"checks":[{"n":1},{"n":2}]}`).(map[string]any)
	alignRiskChecks(object, &outputPolicy{targetCount: 2, hasTargetCount: true})
	if checks := object["checks"].([]any); len(checks) != 2 {
		t.Errorf("checks = %d, want 2", len(checks))
	}

	// 空数组也补齐。
	object = usageJSON(t, `{"checks":[]}`).(map[string]any)
	alignRiskChecks(object, &outputPolicy{targetCount: 2, hasTargetCount: true})
	if checks := object["checks"].([]any); len(checks) != 2 {
		t.Errorf("checks = %d, want 2", len(checks))
	}
}

// 目标条数缺失或越界(1..20 之外)时不动 checks。
func TestAlignRiskChecksIgnoresOutOfRangeTargets(t *testing.T) {
	t.Parallel()
	for _, policy := range []*outputPolicy{
		{hasTargetCount: false, targetCount: 3},
		{hasTargetCount: true, targetCount: 0},
		{hasTargetCount: true, targetCount: 21},
		{hasTargetCount: true, targetCount: -1},
	} {
		object := usageJSON(t, `{"checks":[{"n":1}]}`).(map[string]any)
		alignRiskChecks(object, policy)
		if checks := object["checks"].([]any); len(checks) != 1 {
			t.Errorf("policy %+v: checks = %d, want 1 (untouched)", policy, len(checks))
		}
	}
	// checks 不是数组时不动。
	object := map[string]any{"checks": "nope"}
	alignRiskChecks(object, &outputPolicy{targetCount: 3, hasTargetCount: true})
	if object["checks"] != "nope" {
		t.Errorf("a non-array checks must be left alone, got %#v", object["checks"])
	}
}

// ---------------------------------------------------------------------------
// enforceRiskGate
// ---------------------------------------------------------------------------

func TestEnforceRiskGateNormalizesEnum(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"PASS":         "PASS",
		"pass":         "PASS",
		"  PASSED  ":   "PASS",
		"APPROVED":     "PASS",
		"已放行":          "PASS",
		"BREACH":       "BREACH",
		"blocked":      "BREACH",
		"已拦截":          "BREACH",
		"违规":           "BREACH",
		"WARN":         "WARN",
		"whatever":     "WARN",
		"":             "WARN",
		"NEEDS_REVIEW": "WARN",
	}
	for input, want := range cases {
		object := map[string]any{"gate": input, "checks": []any{}}
		enforceRiskGate(object)
		if object["gate"] != want {
			t.Errorf("gate %q => %#v, want %q", input, object["gate"], want)
		}
	}
	// gate 不是字符串时不归一(但一致性规则仍可能覆盖它)。
	object := map[string]any{"gate": 42.0, "checks": []any{}}
	enforceRiskGate(object)
	if object["gate"] != 42.0 {
		t.Errorf("a non-string gate must be left alone, got %#v", object["gate"])
	}
}

// gate 与 checks 一致性:模型常给「PASS + 一条 WARN」这种自相矛盾的组合。
func TestEnforceRiskGateConsistencyWithChecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		object string
		want   string
	}{
		{"全 OK 保持 PASS", `{"gate":"PASS","checks":[{"status":"OK"},{"status":"OK"}]}`, "PASS"},
		{"有 WARN 必须降级", `{"gate":"PASS","checks":[{"status":"OK"},{"status":"WARN"}]}`, "WARN"},
		{"有 UNKNOWN 必须降级", `{"gate":"PASS","checks":[{"status":"UNKNOWN"}]}`, "WARN"},
		{"有 CRIT 升到 BREACH", `{"gate":"PASS","checks":[{"status":"WARN"},{"status":"CRIT"}]}`, "BREACH"},
		{"CRIT 覆盖模型的 WARN", `{"gate":"WARN","checks":[{"status":"CRIT"}]}`, "BREACH"},
		{"模型 BREACH + 全 OK 保留 BREACH", `{"gate":"BREACH","checks":[{"status":"OK"}]}`, "BREACH"},
		{"状态小写与空白也识别", `{"gate":"PASS","checks":[{"status":"  crit  "}]}`, "BREACH"},
		{"缺 status 视为 OK", `{"gate":"PASS","checks":[{}]}`, "PASS"},
		{"status 非字符串视为 OK", `{"gate":"PASS","checks":[{"status":9}]}`, "PASS"},
		{"空 checks 保持 PASS", `{"gate":"PASS","checks":[]}`, "PASS"},
	}
	for _, item := range cases {
		object := usageJSON(t, item.object).(map[string]any)
		enforceRiskGate(object)
		if object["gate"] != item.want {
			t.Errorf("%s: gate = %#v, want %q", item.name, object["gate"], item.want)
		}
	}
}

// hedging_suggestions 兜底:非 PASS 时必须非空,PASS 时补空数组。
func TestEnforceRiskGateHedgingFallback(t *testing.T) {
	t.Parallel()
	object := usageJSON(t, `{"gate":"PASS","checks":[{"status":"OK"}]}`).(map[string]any)
	enforceRiskGate(object)
	if suggestions, ok := object["hedging_suggestions"].([]any); !ok || len(suggestions) != 0 {
		t.Errorf("PASS => %#v, want an empty array", object["hedging_suggestions"])
	}

	object = usageJSON(t, `{"gate":"PASS","checks":[{"status":"WARN"}]}`).(map[string]any)
	enforceRiskGate(object)
	suggestions, ok := object["hedging_suggestions"].([]any)
	if !ok || len(suggestions) != 1 || suggestions[0] != "维持现有仓位，暂缓调仓" {
		t.Errorf("WARN => %#v, want the single hedging suggestion", object["hedging_suggestions"])
	}

	// 模型已给出 hedging_suggestions 时不覆盖(即使是空数组)。
	object = usageJSON(t, `{"gate":"PASS","checks":[{"status":"CRIT"}],"hedging_suggestions":[]}`).(map[string]any)
	enforceRiskGate(object)
	if suggestions := object["hedging_suggestions"].([]any); len(suggestions) != 0 {
		t.Errorf("an existing hedging_suggestions must not be overwritten, got %#v", suggestions)
	}
	object = usageJSON(t, `{"gate":"PASS","checks":[{"status":"OK"}],"hedging_suggestions":["自定义"]}`).(map[string]any)
	enforceRiskGate(object)
	if suggestions := object["hedging_suggestions"].([]any); len(suggestions) != 1 || suggestions[0] != "自定义" {
		t.Errorf("an existing hedging_suggestions must not be overwritten, got %#v", suggestions)
	}
}

// ---------------------------------------------------------------------------
// outputFromClaude:整条归一化管线
// ---------------------------------------------------------------------------

// runOutputFromClaude 是断言成功路径的小包装。
func runOutputFromClaude(t *testing.T, envelope any, role string, policy *outputPolicy) *ProcessedOutput {
	t.Helper()
	output, execErr := outputFromClaude(envelope, executorRuntime(role, ExecutorKindPi, ExecutionModeLocal), policy)
	if execErr != nil {
		t.Fatalf("unexpected execution error: %v", execErr)
	}
	return output
}

// payloadOf 取出归一化后的 payload_result 对象。
func payloadOf(t *testing.T, output *ProcessedOutput) map[string]any {
	t.Helper()
	object, ok := asJSONObject(output.PayloadResult)
	if !ok {
		t.Fatalf("payload_result is %T, want an object", output.PayloadResult)
	}
	return object
}

// 基本面岗位的最小可用输出:payload_result 包裹与裸对象等价。
func TestOutputFromClaudeAcceptsWrappedAndBareResult(t *testing.T) {
	t.Parallel()
	body := `{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":210.5,"reasoning":"cheap","score":7}],"summary":"ok"}`
	wrapped := runOutputFromClaude(t, usageJSON(t, `{"payload_result":`+body+`}`),
		"fundamental_analyst", &outputPolicy{})
	bare := runOutputFromClaude(t, usageJSON(t, body), "fundamental_analyst", &outputPolicy{})
	// evidenceRefs 里有生成时刻的 asOf,比对前先摘掉。
	wrappedPayload, barePayload := payloadOf(t, wrapped), payloadOf(t, bare)
	delete(wrappedPayload, "evidenceRefs")
	delete(barePayload, "evidenceRefs")
	if jsonString(wrappedPayload) != jsonString(barePayload) {
		t.Errorf("wrapped and bare results diverged:\n%s\n%s",
			jsonString(wrappedPayload), jsonString(barePayload))
	}
	if wrapped.Summary != "ok" {
		t.Errorf("summary = %#v, want ok", wrapped.Summary)
	}
}

// 归一化不得改写 candidate:运行记录里的执行器原文必须保持原样。
func TestOutputFromClaudeKeepsExecutorOutputPristine(t *testing.T) {
	t.Parallel()
	envelope := usageJSON(t, `{"payload_result":{
		"assessments":[{"symbol":"aapl","rating":"strong buy","fair_value":"210.5 美元","reasoning":"r","score":"99","leaked":"drop me"}],
		"summary":"s","extra_top_level":"drop me too"
	}}`)
	output := runOutputFromClaude(t, envelope, "fundamental_analyst",
		&outputPolicy{symbolPool: []string{"AAPL.US"}})

	// 归一化结果:symbol 标准化、rating 归一、score clamp、多余键裁掉。
	item := payloadOf(t, output)["assessments"].([]any)[0].(map[string]any)
	if item["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %#v, want AAPL.US", item["symbol"])
	}
	if item["rating"] != "STRONG_BUY" {
		t.Errorf("rating = %#v, want STRONG_BUY", item["rating"])
	}
	if item["score"] != 10.0 {
		t.Errorf("score = %#v, want 10 (clamped)", item["score"])
	}
	// fair_value 会把「210.5 美元」这类带单位的字符串解析成数字。
	if item["fair_value"] != 210.5 {
		t.Errorf("fair_value = %#v, want 210.5", item["fair_value"])
	}
	if _, leaked := item["leaked"]; leaked {
		t.Error("unknown item keys must be trimmed")
	}
	if _, leaked := payloadOf(t, output)["extra_top_level"]; leaked {
		t.Error("unknown top-level keys must be trimmed")
	}

	// candidate 原文没有被动过。
	original := output.ExecutorOutput
	rawItem, _ := jsonField(original, "payload_result")
	items, _ := asJSONArray(mustField(t, rawItem, "assessments"))
	first := items[0].(map[string]any)
	if first["symbol"] != "aapl" || first["rating"] != "strong buy" {
		t.Errorf("executor output was mutated: %#v", first)
	}
	if first["fair_value"] != "210.5 美元" {
		t.Errorf("executor output fair_value was normalized in place: %#v", first["fair_value"])
	}
	if _, ok := first["leaked"]; !ok {
		t.Error("executor output lost the model's original extra key")
	}
	if _, ok := jsonField(rawItem, "extra_top_level"); !ok {
		t.Error("executor output lost the model's original top-level key")
	}
}

// mustField 读一个必须存在的字段。
func mustField(t *testing.T, value any, key string) any {
	t.Helper()
	field, ok := jsonField(value, key)
	if !ok {
		t.Fatalf("missing key %q in %#v", key, value)
	}
	return field
}

// 非对象候选 → 保守结构 + 「保守缺省原因」摘要。
func TestOutputFromClaudeFallsBackForNonObjectCandidate(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`"just text"`, `[1,2]`, `42`, `true`, `null`} {
		output := runOutputFromClaude(t, usageJSON(t, raw), "risk_manager", &outputPolicy{})
		payload := payloadOf(t, output)
		if payload["gate"] != "WARN" {
			t.Errorf("%s: gate = %#v, want the conservative WARN", raw, payload["gate"])
		}
		if checks := payload["checks"].([]any); len(checks) != 1 {
			t.Errorf("%s: checks = %#v, want one conservative check", raw, payload["checks"])
		}
		want := "保守缺省原因：模型返回内容不是可解析的 JSON 对象"
		if output.Summary != want {
			t.Errorf("%s: summary = %#v, want %q", raw, output.Summary, want)
		}
	}
}

// 对象但缺岗位必填字段 → 换成保守结构,并说明缺了哪些字段。
func TestOutputFromClaudeFallsBackForMissingRequiredFields(t *testing.T) {
	t.Parallel()
	// 风控给了 gate 但 checks 是空数组:键存在却空,平台 minItems:1 仍会拒。
	output := runOutputFromClaude(t, usageJSON(t, `{"gate":"PASS","checks":[]}`),
		"risk_manager", &outputPolicy{})
	payload := payloadOf(t, output)
	if checks := payload["checks"].([]any); len(checks) != 1 {
		t.Fatalf("checks = %#v, want one conservative check", payload["checks"])
	}
	if payload["gate"] != "WARN" {
		t.Errorf("gate = %#v, want WARN", payload["gate"])
	}
	summary, _ := output.Summary.(string)
	if !strings.HasPrefix(summary, "保守缺省原因：模型输出缺少或未填充岗位必填字段：") {
		t.Errorf("summary = %q", summary)
	}
	if !strings.Contains(summary, "checks") {
		t.Errorf("summary should name the missing field, got %q", summary)
	}

	// 技术分析师返回了基本面的 assessments:缺 signals。
	output = runOutputFromClaude(t, usageJSON(t, `{"assessments":[{"symbol":"AAPL.US"}]}`),
		"technical_analyst", &outputPolicy{})
	payload = payloadOf(t, output)
	if _, ok := payload["signals"]; !ok {
		t.Error("the conservative technical payload must carry signals")
	}
	if _, leaked := payload["assessments"]; leaked {
		t.Error("assessments must not survive on a technical_analyst payload")
	}
}

// 缺必填时的保守结构也要按标的池补全主数组。
func TestOutputFromClaudeConservativeFallbackFillsSymbolPool(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t, `{"nothing":"useful"}`),
		"fundamental_analyst", &outputPolicy{symbolPool: []string{"AAPL.US", "MSFT.US"}})
	assessments := payloadOf(t, output)["assessments"].([]any)
	if len(assessments) != 2 {
		t.Fatalf("assessments = %d, want one per pool symbol", len(assessments))
	}
	got := []string{
		assessments[0].(map[string]any)["symbol"].(string),
		assessments[1].(map[string]any)["symbol"].(string),
	}
	if got[0] != "AAPL.US" || got[1] != "MSFT.US" {
		t.Errorf("symbols = %v, want the pool order", got)
	}
}

// evidenceRefs 为空时补一条带 asOf 的任务引用,并写回 payload_result。
func TestOutputFromClaudeAddsTaskEvidenceRef(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t,
		`{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":1,"reasoning":"r"}],"summary":"s"}`),
		"fundamental_analyst", &outputPolicy{})
	if len(output.EvidenceRefs) != 1 {
		t.Fatalf("evidenceRefs = %#v, want one task ref", output.EvidenceRefs)
	}
	ref := output.EvidenceRefs[0].(map[string]any)
	if ref["sourceType"] != "TASK_INPUT" || ref["sourceId"] != "task" {
		t.Errorf("ref = %#v", ref)
	}
	if _, err := parseTimestamp(ref["asOf"].(string)); err != nil {
		t.Errorf("asOf %q is not RFC3339: %v", ref["asOf"], err)
	}
	// 同一条也要写回 payload_result,否则平台侧看不到证据引用。
	inPayload := payloadOf(t, output)["evidenceRefs"].([]any)
	if len(inPayload) != 1 || inPayload[0].(map[string]any)["sourceId"] != "task" {
		t.Errorf("payload_result.evidenceRefs = %#v", inPayload)
	}
}

// 模型给了合格证据引用时保留,不追加任务引用。
func TestOutputFromClaudeKeepsModelEvidenceRefs(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t, `{
		"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":1,"reasoning":"r"}],
		"summary":"s",
		"evidenceRefs":[{"sourceType":"MARKET_DATA","sourceId":"eodhd","asOf":"2026-01-02","junk":"x"}]
	}`), "fundamental_analyst", &outputPolicy{})
	if len(output.EvidenceRefs) != 1 {
		t.Fatalf("evidenceRefs = %#v", output.EvidenceRefs)
	}
	ref := output.EvidenceRefs[0].(map[string]any)
	if ref["sourceId"] != "eodhd" {
		t.Errorf("ref = %#v, want the model's own ref", ref)
	}
	if _, leaked := ref["junk"]; leaked {
		t.Error("evidence ref keys must be whitelisted")
	}
}

// extensions 一律规整为数组(模型常给对象或省略)。
func TestOutputFromClaudeNormalizesExtensions(t *testing.T) {
	t.Parallel()
	base := `{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":1,"reasoning":"r"}],"summary":"s"`
	for _, raw := range []string{
		base + `}`,
		base + `,"extensions":{"a":1}}`,
		base + `,"extensions":null}`,
		base + `,"extensions":"nope"}`,
	} {
		output := runOutputFromClaude(t, usageJSON(t, raw), "fundamental_analyst", &outputPolicy{})
		if array, ok := output.Extensions.([]any); !ok || len(array) != 0 {
			t.Errorf("%s: extensions = %#v, want an empty array", raw, output.Extensions)
		}
		if array, ok := asJSONArray(payloadOf(t, output)["extensions"]); !ok || len(array) != 0 {
			t.Errorf("%s: payload extensions = %#v", raw, payloadOf(t, output)["extensions"])
		}
	}
	// 已经是数组时保留内容。
	output := runOutputFromClaude(t, usageJSON(t, base+`,"extensions":[{"k":"v"}]}`),
		"fundamental_analyst", &outputPolicy{})
	if array, ok := output.Extensions.([]any); !ok || len(array) != 1 {
		t.Errorf("extensions = %#v, want the model array preserved", output.Extensions)
	}
}

// 风控整链:checks 条数对齐 → 一致性覆盖 gate → hedging 兜底。
func TestOutputFromClaudeRiskManagerFullChain(t *testing.T) {
	t.Parallel()
	envelope := usageJSON(t, `{"payload_result":{
		"gate":"pass",
		"checks":[{"dimension":"VAR","status":"warn","reasoning":"接近上限"}]
	}}`)
	output := runOutputFromClaude(t, envelope, "risk_manager",
		&outputPolicy{targetCount: 3, hasTargetCount: true})
	payload := payloadOf(t, output)
	checks := payload["checks"].([]any)
	if len(checks) != 3 {
		t.Fatalf("checks = %d, want 3 (aligned to the brief)", len(checks))
	}
	if checks[0].(map[string]any)["status"] != "WARN" {
		t.Errorf("status = %#v, want WARN", checks[0].(map[string]any)["status"])
	}
	// 补位的 UNKNOWN 让 gate 无法停在 PASS。
	if payload["gate"] != "WARN" {
		t.Errorf("gate = %#v, want WARN", payload["gate"])
	}
	if suggestions := payload["hedging_suggestions"].([]any); len(suggestions) != 1 {
		t.Errorf("hedging_suggestions = %#v, want the non-empty fallback", suggestions)
	}
	// 归一化结果自身必须过 schema。
	if missing := missingRoleFields(payload, "risk_manager", false); len(missing) > 0 {
		t.Errorf("normalized risk payload still misses %v", missing)
	}
}

// 全 OK 的风控输出保持 PASS,hedging 补空数组。
func TestOutputFromClaudeRiskManagerKeepsPass(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t, `{
		"gate":"PASS",
		"checks":[{"dimension":"VAR","status":"OK","reasoning":"充裕"},
		          {"dimension":"LEVERAGE","status":"通过","reasoning":"无杠杆"}]
	}`), "risk_manager", &outputPolicy{})
	payload := payloadOf(t, output)
	if payload["gate"] != "PASS" {
		t.Errorf("gate = %#v, want PASS", payload["gate"])
	}
	if suggestions, ok := payload["hedging_suggestions"].([]any); !ok || len(suggestions) != 0 {
		t.Errorf("hedging_suggestions = %#v, want an empty array", payload["hedging_suggestions"])
	}
	if len(payload["checks"].([]any)) != 2 {
		t.Error("checks must not be padded without a target count")
	}
}

// checks 不是数组时整条风控加工链被跳过(gate 归一、条数对齐、一致性都不跑)。
//
// 这是 rs 的实际行为:那三段逻辑嵌在 `if let Some(items) = obj[item_field].as_array_mut()`
// 里面。此时未归一的 gate(如小写 "pass")会原样透出,而 missingRoleFields 也不会
// 报错 —— checks 键存在、非空白字符串、且 gate 不在 {WARN,BREACH} 里。
// 记录下来是为了让这条已知缺口在两侧保持一致,而不是被 Go 侧「顺手修掉」。
func TestOutputFromClaudeSkipsRiskChainWhenChecksNotArray(t *testing.T) {
	t.Parallel()
	output, execErr := outputFromClaude(usageJSON(t, `{"gate":"pass","checks":"oops"}`),
		executorRuntime("risk_manager", ExecutorKindPi, ExecutionModeLocal), &outputPolicy{})
	if execErr != nil {
		t.Fatalf("unexpected execution error: %v", execErr)
	}
	payload := payloadOf(t, output)
	if payload["gate"] != "pass" {
		t.Errorf("gate = %#v, want the un-normalized \"pass\" (chain skipped)", payload["gate"])
	}
	if payload["checks"] != "oops" {
		t.Errorf("checks = %#v, want it untouched", payload["checks"])
	}
}

// 归一化后仍不合规时报 invalid_executor_output 并附模型原文。
func TestOutputFromClaudeReportsRemainingViolationsWithOriginal(t *testing.T) {
	t.Parallel()
	// 技术分析师给出的 signal 完全没有 symbol,且任务没有标的池可用来补全。
	// (reasoning 空缺、entry_zone 形状错这类问题归一化能自己修好,symbol 不能。)
	envelope := usageJSON(t, `{"signals":[{"bias":"LONG_BIAS","note":"MARKER"}]}`)
	_, execErr := outputFromClaude(envelope,
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal), &outputPolicy{})
	if execErr == nil {
		t.Fatal("expected an execution error")
	}
	if execErr.Code != "invalid_executor_output" {
		t.Fatalf("code = %q", execErr.Code)
	}
	if !strings.HasPrefix(execErr.Detail, "normalized output still violates required role structure: ") {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if !strings.Contains(execErr.Detail, "signals[0].symbol") {
		t.Errorf("detail = %q, want it to name signals[0].symbol", execErr.Detail)
	}
	// 附带的必须是模型原文(含被裁掉的 note 键)。
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "MARKER") {
		t.Errorf("executor output = %v, want the model original", raw)
	}
}

// 归一化能自愈的情况不该报错:缺 reasoning、entry_zone 形状错都能修好。
func TestOutputFromClaudeRepairsRecoverableSignalDefects(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t,
		usageJSON(t, `{"signals":[{"symbol":"AAPL.US","bias":"看多","entry_zone":[1,2,3]}]}`),
		"technical_analyst", &outputPolicy{})
	signal := payloadOf(t, output)["signals"].([]any)[0].(map[string]any)
	if signal["bias"] != "LONG_BIAS" {
		t.Errorf("bias = %#v, want LONG_BIAS", signal["bias"])
	}
	// 长度不是 2 的 entry_zone 被清空(空数组是合法形状)。
	if zone, ok := signal["entry_zone"].([]any); !ok || len(zone) != 0 {
		t.Errorf("entry_zone = %#v, want an empty array", signal["entry_zone"])
	}
	if reasoning, ok := signal["reasoning"].(string); !ok || reasoning == "" {
		t.Errorf("reasoning = %#v, want the placeholder text", signal["reasoning"])
	}
}

// 组合经理职能二用 ticker,职能一用 symbol。
func TestOutputFromClaudePortfolioManagerTickerVsSymbol(t *testing.T) {
	t.Parallel()
	final := runOutputFromClaude(t, usageJSON(t, `{
		"timestamp":"2026-01-02T03:04:05Z",
		"report_markdown":"报告",
		"orders":[]
	}`), "portfolio_manager", &outputPolicy{isFinal: true, upstreamSymbols: []string{"AAPL.US"}})
	orders := payloadOf(t, final)["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("orders = %#v, want one upstream-derived order", orders)
	}
	order := orders[0].(map[string]any)
	if order["ticker"] != "AAPL.US" {
		t.Errorf("职能二 order = %#v, want a ticker field", order)
	}
	if _, ok := order["symbol"]; ok {
		t.Error("职能二 must not carry symbol")
	}

	process := runOutputFromClaude(t, usageJSON(t, `{
		"decision_type":"REBALANCE","rationale":"理由",
		"orders":[],
		"target_allocation":[{"symbol":"AAPL.US","target_weight_pct":100,"reasoning":"r"}]
	}`), "portfolio_manager", &outputPolicy{upstreamSymbols: []string{"AAPL.US"}})
	orders = payloadOf(t, process)["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("orders = %#v", orders)
	}
	order = orders[0].(map[string]any)
	if order["symbol"] != "AAPL.US" {
		t.Errorf("职能一 order = %#v, want a symbol field", order)
	}
	if _, ok := order["ticker"]; ok {
		t.Error("职能一 must not carry ticker")
	}
}

// 没有上游订单意图时不按 symbolPool 伪造订单,空 orders 要能保持空。
func TestOutputFromClaudeDoesNotFabricateOrdersFromPool(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t, `{"orders":[]}`), "execution_trader",
		&outputPolicy{symbolPool: []string{"AAPL.US", "MSFT.US"}})
	if orders := payloadOf(t, output)["orders"].([]any); len(orders) != 0 {
		t.Errorf("orders = %#v, want them left empty", orders)
	}
	// 但基本面岗位会按池补全。
	output = runOutputFromClaude(t, usageJSON(t, `{"assessments":[],"summary":"s"}`),
		"fundamental_analyst", &outputPolicy{symbolPool: []string{"AAPL.US", "MSFT.US"}})
	if assessments := payloadOf(t, output)["assessments"].([]any); len(assessments) != 2 {
		t.Errorf("assessments = %d, want one per pool symbol", len(assessments))
	}
}

// 未收录岗位:只做 evidenceRefs / extensions 规范化,不裁剪不归一。
func TestOutputFromClaudeLeavesUnknownRoleAlone(t *testing.T) {
	t.Parallel()
	output := runOutputFromClaude(t, usageJSON(t,
		`{"assessments":[],"custom_key":"kept","evidenceRefs":[{"sourceType":"A","sourceId":"1"}]}`),
		"quant_researcher", &outputPolicy{})
	payload := payloadOf(t, output)
	if payload["custom_key"] != "kept" {
		t.Errorf("custom_key = %#v, want it preserved for an unlisted role", payload["custom_key"])
	}
	if array, ok := asJSONArray(payload["extensions"]); !ok || len(array) != 0 {
		t.Errorf("extensions = %#v, want an empty array", payload["extensions"])
	}
}

// ---------------------------------------------------------------------------
// gatewayRequest / gatewayContent / retryGateway
// ---------------------------------------------------------------------------

func TestGatewayRequestShapes(t *testing.T) {
	t.Parallel()
	anthropic := &GatewayConfig{
		BaseURL: "https://gw.example.com/", Model: "claude-x",
		MaxTokens: 4096, Protocol: GatewayProtocolAnthropic,
	}
	url, body := gatewayRequest(anthropic, "SYSTEM", "PROMPT")
	if url != "https://gw.example.com/v1/messages" {
		t.Errorf("url = %q", url)
	}
	if body["system"] != "SYSTEM" || body["model"] != "claude-x" || body["max_tokens"] != uint64(4096) {
		t.Errorf("body = %#v", body)
	}
	messages := body["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" ||
		messages[0].(map[string]any)["content"] != "PROMPT" {
		t.Errorf("messages = %#v", messages)
	}

	openai := &GatewayConfig{
		BaseURL: "https://gw.example.com", Model: "gpt-x",
		MaxTokens: 8192, Protocol: GatewayProtocolOpenAI,
	}
	url, body = gatewayRequest(openai, "SYSTEM", "PROMPT")
	if url != "https://gw.example.com/v1/chat/completions" {
		t.Errorf("url = %q", url)
	}
	if _, ok := body["system"]; ok {
		t.Error("OpenAI protocol must carry the system prompt as a message, not a top-level key")
	}
	messages = body["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" ||
		messages[1].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %#v", messages)
	}
	// 多个尾部斜杠都要去掉。
	if url, _ := gatewayRequest(&GatewayConfig{BaseURL: "https://a///"}, "s", "p"); url != "https://a/v1/chat/completions" {
		t.Errorf("url = %q", url)
	}
}

func TestGatewayContentExtraction(t *testing.T) {
	t.Parallel()
	// Anthropic 形状优先。
	text, ok := gatewayContent(usageJSON(t, `{"content":[{"type":"text","text":"anthropic"}],
		"choices":[{"message":{"content":"openai"}}]}`))
	if !ok || text != "anthropic" {
		t.Errorf("got (%q, %v), want anthropic", text, ok)
	}
	// 回退到 OpenAI 形状。
	text, ok = gatewayContent(usageJSON(t, `{"choices":[{"message":{"content":"openai"}}]}`))
	if !ok || text != "openai" {
		t.Errorf("got (%q, %v), want openai", text, ok)
	}
	// content 存在但形状不对时也要回退。
	text, ok = gatewayContent(usageJSON(t, `{"content":[],"choices":[{"message":{"content":"openai"}}]}`))
	if !ok || text != "openai" {
		t.Errorf("got (%q, %v), want the OpenAI fallback", text, ok)
	}
	text, ok = gatewayContent(usageJSON(t, `{"content":[{"type":"thinking"}],"choices":[{"message":{"content":"openai"}}]}`))
	if !ok || text != "openai" {
		t.Errorf("got (%q, %v), want the OpenAI fallback", text, ok)
	}
	// 两种形状都没有。
	for _, raw := range []string{`{}`, `{"choices":[]}`, `{"choices":[{}]}`,
		`{"choices":[{"message":{}}]}`, `{"choices":"nope"}`, `{"content":"nope"}`, `[]`} {
		if _, ok := gatewayContent(usageJSON(t, raw)); ok {
			t.Errorf("%s unexpectedly produced content", raw)
		}
	}
}

func TestRetryGatewayBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	far := time.Now().Add(time.Hour)
	// 最后一次尝试不再重试。
	if retryGateway(ctx, 2, &GatewayConfig{MaxAttempts: 3}, far) {
		t.Error("the final attempt must not retry")
	}
	if !retryGateway(ctx, 0, &GatewayConfig{MaxAttempts: 3}, far) {
		t.Error("an early attempt with budget must retry")
	}
	// 剩余预算不足以等待退避时放弃。
	if retryGateway(ctx, 0, &GatewayConfig{MaxAttempts: 3}, time.Now().Add(10*time.Millisecond)) {
		t.Error("a deadline shorter than the backoff must not retry")
	}
	// ctx 取消时立即放弃。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if retryGateway(cancelled, 0, &GatewayConfig{MaxAttempts: 3}, far) {
		t.Error("a cancelled context must not retry")
	}
	// 退避确实等待了(250ms × 2^0)。
	started := time.Now()
	if !retryGateway(ctx, 0, &GatewayConfig{MaxAttempts: 5}, far) {
		t.Fatal("expected a retry")
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Errorf("backoff waited %v, want about 250ms", elapsed)
	}
}

// ---------------------------------------------------------------------------
// executeLiteLLMAgent(打真实 httptest 网关)
// ---------------------------------------------------------------------------

// gatewayStub 是可编排应答序列的假网关。
type gatewayStub struct {
	server    *httptest.Server
	responses []gatewayStubResponse
	requests  []map[string]any
	headers   []http.Header
	mu        chan struct{}
}

type gatewayStubResponse struct {
	status int
	body   string
	// contentLength 大于 0 时伪造一个虚高的 Content-Length 头。
	contentLength int
}

func newGatewayStub(t *testing.T, responses ...gatewayStubResponse) *gatewayStub {
	t.Helper()
	stub := &gatewayStub{responses: responses, mu: make(chan struct{}, 1)}
	stub.mu <- struct{}{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stub.mu
		index := len(stub.requests)
		var decoded map[string]any
		_ = json.NewDecoder(r.Body).Decode(&decoded)
		stub.requests = append(stub.requests, decoded)
		stub.headers = append(stub.headers, r.Header.Clone())
		stub.mu <- struct{}{}

		if index >= len(stub.responses) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"stub exhausted"}`))
			return
		}
		response := stub.responses[index]
		if response.contentLength > 0 {
			w.Header().Set("Content-Length", fmt.Sprint(response.contentLength))
		}
		w.WriteHeader(response.status)
		_, _ = w.Write([]byte(response.body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *gatewayStub) callCount() int {
	<-s.mu
	count := len(s.requests)
	s.mu <- struct{}{}
	return count
}

// gatewayExecutor 组装一个指向假网关的执行器。
func gatewayExecutor(t *testing.T, stub *gatewayStub, protocol GatewayProtocol, maxAttempts int) *AgentExecutor {
	t.Helper()
	cfg := executorTestConfig(t)
	cfg.Gateway.BaseURL = stub.server.URL
	cfg.Gateway.APIKey = "test-key"
	cfg.Gateway.Model = "test-model"
	cfg.Gateway.Protocol = protocol
	cfg.Gateway.MaxAttempts = maxAttempts
	return newTestExecutor(t, cfg, &stubProcessor{})
}

// anthropicEnvelope 生成一份 Anthropic 形状的网关回执。
func anthropicEnvelope(content string) string {
	return jsonString(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": content}},
		"usage": map[string]any{
			"input_tokens": 100, "cache_read_input_tokens": 20,
			"cache_creation_input_tokens": 0, "output_tokens": 30,
		},
	})
}

const gatewayGoodPayload = `{"payload_result":{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":210,"reasoning":"cheap"}],"summary":"网关摘要"}}`

func TestExecuteLiteLLMHappyPath(t *testing.T) {
	t.Parallel()
	stub := newGatewayStub(t, gatewayStubResponse{status: 200, body: anthropicEnvelope(gatewayGoodPayload)})
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
	out, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Summary != "网关摘要" {
		t.Errorf("summary = %#v", out.Summary)
	}
	if stub.callCount() != 1 {
		t.Errorf("gateway was called %d times, want 1", stub.callCount())
	}
	// 用量来自网关回执:prompt = 100 + 20 + 0。
	if out.Usage == nil {
		t.Fatal("expected usage from the gateway envelope")
	}
	if out.Usage.PromptTokens != 120 || out.Usage.CompletionTokens != 30 {
		t.Errorf("usage = %+v, want 120/30", out.Usage)
	}
	wantUint64(t, "cachedInputTokens", out.Usage.CachedInputTokens, 20)

	// 系统提示词里带 agent 名与岗位契约,请求头带 Anthropic 认证。
	body := stub.requests[0]
	system, _ := body["system"].(string)
	if !strings.HasPrefix(system, "你是 Pi agent。") {
		t.Errorf("system prompt = %q", system)
	}
	if !strings.Contains(system, "OUTPUT LANGUAGE REQUIREMENT: ") {
		t.Error("system prompt must carry the output language requirement")
	}
	if got := stub.headers[0].Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := stub.headers[0].Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
}

func TestExecuteLiteLLMOpenAIAuthAndShape(t *testing.T) {
	t.Parallel()
	envelope := jsonString(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": gatewayGoodPayload}}},
		"usage": map[string]any{
			"prompt_tokens": 40, "completion_tokens": 10, "total_tokens": 50,
		},
	})
	stub := newGatewayStub(t, gatewayStubResponse{status: 200, body: envelope})
	executor := gatewayExecutor(t, stub, GatewayProtocolOpenAI, 3)
	out, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindCodex, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 50 {
		t.Errorf("usage = %+v, want a 50-token total", out.Usage)
	}
	if got := stub.headers[0].Get("authorization"); got != "Bearer test-key" {
		t.Errorf("authorization = %q", got)
	}
	if got := stub.headers[0].Get("anthropic-version"); got != "" {
		t.Errorf("OpenAI protocol must not send anthropic-version, got %q", got)
	}
	// 系统提示词按 OpenAI 形状放进 messages[0]。
	messages := stub.requests[0]["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Errorf("messages = %#v", messages)
	}
	if !strings.HasPrefix(messages[0].(map[string]any)["content"].(string), "你是 Codex agent。") {
		t.Errorf("system message = %#v", messages[0])
	}
}

// 5xx 与 429 可重试;重试成功后仍能拿到结果,但用量因中途不完整而作废。
func TestExecuteLiteLLMRetriesTransientStatuses(t *testing.T) {
	t.Parallel()
	for _, transient := range []int{500, 502, 503, 429} {
		stub := newGatewayStub(t,
			gatewayStubResponse{status: transient, body: `{"error":"transient"}`},
			gatewayStubResponse{status: 200, body: anthropicEnvelope(gatewayGoodPayload)},
		)
		executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
		out, err := executor.Execute(context.Background(),
			executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
			&TaskContext{}, map[string]any{}, 30*time.Second)
		if err != nil {
			t.Fatalf("status %d: %v", transient, err)
		}
		if stub.callCount() != 2 {
			t.Errorf("status %d: called %d times, want 2", transient, stub.callCount())
		}
		if out.Usage != nil {
			t.Errorf("status %d: usage = %+v, want nil after an incomplete attempt", transient, out.Usage)
		}
	}
}

// 4xx(非 429)不重试,直接 executor_failed。
func TestExecuteLiteLLMDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 403, 404, 422} {
		stub := newGatewayStub(t,
			gatewayStubResponse{status: status, body: `{"error":"client"}`},
			gatewayStubResponse{status: 200, body: anthropicEnvelope(gatewayGoodPayload)},
		)
		executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
		_, err := executor.Execute(context.Background(),
			executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
			&TaskContext{}, map[string]any{}, 30*time.Second)
		execErr := wantExecutionCode(t, err, "executor_failed")
		want := fmt.Sprintf("gateway returned HTTP %d", status)
		if execErr.Detail != want {
			t.Errorf("detail = %q, want %q", execErr.Detail, want)
		}
		if stub.callCount() != 1 {
			t.Errorf("status %d: called %d times, want 1 (no retry)", status, stub.callCount())
		}
	}
}

// 重试预算耗尽后报最后一次的原因。
func TestExecuteLiteLLMExhaustsAttempts(t *testing.T) {
	t.Parallel()
	stub := newGatewayStub(t,
		gatewayStubResponse{status: 500, body: `{"e":1}`},
		gatewayStubResponse{status: 500, body: `{"e":2}`},
	)
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 2)
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	execErr := wantExecutionCode(t, err, "executor_failed")
	if execErr.Detail != "gateway returned HTTP 500" {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if stub.callCount() != 2 {
		t.Errorf("called %d times, want 2 (max_attempts)", stub.callCount())
	}
}

// 非 JSON 信封:重试一次,仍失败则 invalid_executor_output。
func TestExecuteLiteLLMRejectsNonJSONEnvelope(t *testing.T) {
	t.Parallel()
	stub := newGatewayStub(t,
		gatewayStubResponse{status: 200, body: `<html>gateway down</html>`},
		gatewayStubResponse{status: 200, body: `still not json`},
	)
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 2)
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	execErr := wantExecutionCode(t, err, "invalid_executor_output")
	if !strings.HasPrefix(execErr.Detail, "gateway returned a non-JSON envelope: ") {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if stub.callCount() != 2 {
		t.Errorf("called %d times, want 2", stub.callCount())
	}
}

// 空内容也走重试;耗尽后报 empty content。
func TestExecuteLiteLLMRejectsEmptyContent(t *testing.T) {
	t.Parallel()
	empty := anthropicEnvelope("   ")
	stub := newGatewayStub(t,
		gatewayStubResponse{status: 200, body: empty},
		gatewayStubResponse{status: 200, body: empty},
	)
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 2)
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	execErr := wantExecutionCode(t, err, "invalid_executor_output")
	if execErr.Detail != "gateway returned empty content" {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if stub.callCount() != 2 {
		t.Errorf("called %d times, want 2", stub.callCount())
	}
}

// 岗位字段缺失:重试一次;第二次好了就采用。
func TestExecuteLiteLLMRetriesMissingRoleFields(t *testing.T) {
	t.Parallel()
	stub := newGatewayStub(t,
		gatewayStubResponse{status: 200, body: anthropicEnvelope(`{"assessments":[]}`)},
		gatewayStubResponse{status: 200, body: anthropicEnvelope(
			`{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[1,2],"reasoning":"r"}]}}`)},
	)
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
	out, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.callCount() != 2 {
		t.Errorf("called %d times, want 2", stub.callCount())
	}
	if signals := payloadOf(t, out)["signals"].([]any); len(signals) != 1 {
		t.Errorf("signals = %#v", signals)
	}
}

// 岗位字段始终缺失:最后一次报 invalid_executor_output 并附模型原文。
func TestExecuteLiteLLMReportsPersistentMissingRoleFields(t *testing.T) {
	t.Parallel()
	body := anthropicEnvelope(`{"assessments":[],"marker":"MODEL_RAW"}`)
	stub := newGatewayStub(t,
		gatewayStubResponse{status: 200, body: body},
		gatewayStubResponse{status: 200, body: body},
	)
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 2)
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	execErr := wantExecutionCode(t, err, "invalid_executor_output")
	if !strings.HasPrefix(execErr.Detail, "gateway output missed required role fields: ") {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if !strings.Contains(execErr.Detail, "signals") {
		t.Errorf("detail = %q, want it to name signals", execErr.Detail)
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "MODEL_RAW") {
		t.Errorf("executor output = %v, want the model original", raw)
	}
}

// 网关未配置基址。
func TestExecuteLiteLLMRequiresBaseURL(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	cfg.Gateway.BaseURL = "   "
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	execErr := wantExecutionCode(t, err, "executor_unavailable")
	if execErr.Detail != "litellm gateway base URL is not configured" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// 响应超限:Content-Length 与实际字节数两条门禁都要拦住。
func TestExecuteLiteLLMRejectsOversizedResponses(t *testing.T) {
	t.Parallel()
	t.Run("按 Content-Length 提前拦截", func(t *testing.T) {
		stub := newGatewayStub(t, gatewayStubResponse{
			status: 200, body: anthropicEnvelope(gatewayGoodPayload), contentLength: 1 << 30,
		})
		executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
		_, err := executor.Execute(context.Background(),
			executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
			&TaskContext{}, map[string]any{}, 30*time.Second)
		execErr := wantExecutionCode(t, err, "output_too_large")
		if execErr.Detail != "gateway response exceeded the configured byte limit" {
			t.Errorf("detail = %q", execErr.Detail)
		}
	})
	t.Run("按实际字节数拦截", func(t *testing.T) {
		stub := newGatewayStub(t, gatewayStubResponse{
			status: 200, body: anthropicEnvelope(strings.Repeat("x", 4096)),
		})
		cfg := executorTestConfig(t)
		cfg.Gateway.BaseURL = stub.server.URL
		cfg.Gateway.Protocol = GatewayProtocolAnthropic
		cfg.Gateway.MaxAttempts = 3
		cfg.Gateway.MaxResponseBytes = 256
		executor := newTestExecutor(t, cfg, &stubProcessor{})
		_, err := executor.Execute(context.Background(),
			executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
			&TaskContext{}, map[string]any{}, 30*time.Second)
		wantExecutionCode(t, err, "output_too_large")
	})
}

// 预算耗尽:超时必须报 execution_timeout,而不是被当成可重试错误。
func TestExecuteLiteLLMTimesOut(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		_, _ = w.Write([]byte(anthropicEnvelope(gatewayGoodPayload)))
	}))
	t.Cleanup(slow.Close)
	cfg := executorTestConfig(t)
	cfg.Gateway.BaseURL = slow.URL
	cfg.Gateway.Protocol = GatewayProtocolAnthropic
	cfg.Gateway.MaxAttempts = 3
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	started := time.Now()
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 150*time.Millisecond)
	wantExecutionCode(t, err, "execution_timeout")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("waited %v, want it to give up near the deadline", elapsed)
	}
}

// 网关拿不到用量时整份作废(不能拿一次的用量冒充整次执行)。
func TestExecuteLiteLLMDropsUsageWhenUnavailable(t *testing.T) {
	t.Parallel()
	envelope := jsonString(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": gatewayGoodPayload}},
	})
	stub := newGatewayStub(t, gatewayStubResponse{status: 200, body: envelope})
	executor := gatewayExecutor(t, stub, GatewayProtocolAnthropic, 3)
	out, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindPi, ExecutionModeGateway),
		&TaskContext{}, map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Usage != nil {
		t.Errorf("usage = %+v, want nil when the envelope reports none", out.Usage)
	}
}

// ---------------------------------------------------------------------------
// executeLocalCLI(用假 CLI 脚本驱动真实子进程)
// ---------------------------------------------------------------------------

// writeFakeCLI 生成一个可执行脚本并返回其路径。
func writeFakeCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	return path
}

// localCLIExecutor 组装一个把 Pi 指向假脚本的执行器。
func localCLIExecutor(t *testing.T, script string) (*AgentExecutor, *PlatformConfig) {
	t.Helper()
	cfg := executorTestConfig(t)
	cfg.LocalAgents.PiBinary = writeFakeCLI(t, script)
	return newTestExecutor(t, cfg, &stubProcessor{}), cfg
}

// piEventLine 生成一条 Pi message_end 事件(单行 JSON)。
func piEventLine(text, stopReason string, input, output uint64) string {
	return jsonString(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"content":    []any{map[string]any{"type": "text", "text": text}},
			"stopReason": stopReason,
			"usage":      map[string]any{"input": input, "output": output},
		},
	})
}

func TestExecuteLocalCLIHappyPath(t *testing.T) {
	t.Parallel()
	payload := `{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[190,200],"reasoning":"突破"}]}}`
	executor, _ := localCLIExecutor(t, "cat <<'EOF'\n"+piEventLine(payload, "stop", 100, 20)+"\nEOF\n")
	out, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	signal := payloadOf(t, out)["signals"].([]any)[0].(map[string]any)
	if signal["symbol"] != "AAPL.US" || signal["bias"] != "LONG_BIAS" {
		t.Errorf("signal = %#v", signal)
	}
	if out.Usage == nil || out.Usage.PromptTokens != 100 || out.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want 100/20", out.Usage)
	}
}

// 岗位字段缺失 → 带纠正提示词重试一次;第二次好了就采用。
func TestExecuteLocalCLIRetriesOnMissingRoleFields(t *testing.T) {
	t.Parallel()
	good := `{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[1,2],"reasoning":"r"}]}}`
	// 用一个计数文件区分第一次与第二次调用,并把收到的提示词落盘以便断言。
	stateDir := t.TempDir()
	script := `
COUNT_FILE=` + filepath.Join(stateDir, "count") + `
PROMPT_FILE=` + filepath.Join(stateDir, "prompt") + `
COUNT=$( [ -f "$COUNT_FILE" ] && cat "$COUNT_FILE" || echo 0 )
COUNT=$((COUNT+1))
echo "$COUNT" > "$COUNT_FILE"
# Pi 的提示词是最后一个位置参数。
eval "PROMPT=\${$#}"
printf '%s' "$PROMPT" >> "$PROMPT_FILE"
if [ "$COUNT" = "1" ]; then
  cat <<'FIRST'
` + piEventLine(`{"assessments":[]}`, "stop", 10, 2) + `
FIRST
else
  cat <<'SECOND'
` + piEventLine(good, "stop", 30, 5) + `
SECOND
fi
`
	executor, _ := localCLIExecutor(t, script)
	out, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count, _ := os.ReadFile(filepath.Join(stateDir, "count"))
	if strings.TrimSpace(string(count)) != "2" {
		t.Errorf("CLI was invoked %q times, want 2", strings.TrimSpace(string(count)))
	}
	prompts, _ := os.ReadFile(filepath.Join(stateDir, "prompt"))
	if !strings.Contains(string(prompts), "CORRECTION RETRY:") {
		t.Error("the retry prompt must carry the CORRECTION RETRY preamble")
	}
	if !strings.Contains(string(prompts), "role `technical_analyst`") {
		t.Error("the retry prompt must name the role")
	}
	if !strings.Contains(string(prompts), "signals") {
		t.Error("the retry prompt must list the missing fields")
	}
	// 两轮用量都拿到了 → 合计上报。
	if out.Usage == nil {
		t.Fatal("expected aggregated usage")
	}
	if out.Usage.PromptTokens != 40 || out.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v, want 40/7 (10+30 / 2+5)", out.Usage)
	}
	if out.Usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", out.Usage.InvocationCount)
	}
}

// 重试后仍缺字段 → invalid_executor_output,并附第二次的原文。
func TestExecuteLocalCLIFailsAfterRetry(t *testing.T) {
	t.Parallel()
	bad := piEventLine(`{"assessments":[],"marker":"SECOND_TRY"}`, "stop", 10, 2)
	executor, _ := localCLIExecutor(t, "cat <<'EOF'\n"+bad+"\nEOF\n")
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "invalid_executor_output")
	if !strings.HasPrefix(execErr.Detail, "Pi output still missed required role fields after retry: ") {
		t.Errorf("detail = %q", execErr.Detail)
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "SECOND_TRY") {
		t.Errorf("executor output = %v, want the retry original", raw)
	}
}

// 任一轮拿不到用量时整份作废。
func TestExecuteLocalCLIDropsUsageWhenARetryLacksIt(t *testing.T) {
	t.Parallel()
	good := `{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[1,2],"reasoning":"r"}]}}`
	stateDir := t.TempDir()
	// 第一轮没有 usage 字段,第二轮有。
	firstEvent := jsonString(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant", "stopReason": "stop",
			"content": []any{map[string]any{"type": "text", "text": `{"assessments":[]}`}},
		},
	})
	script := `
COUNT_FILE=` + filepath.Join(stateDir, "count") + `
COUNT=$( [ -f "$COUNT_FILE" ] && cat "$COUNT_FILE" || echo 0 )
COUNT=$((COUNT+1))
echo "$COUNT" > "$COUNT_FILE"
if [ "$COUNT" = "1" ]; then
  cat <<'FIRST'
` + firstEvent + `
FIRST
else
  cat <<'SECOND'
` + piEventLine(good, "stop", 30, 5) + `
SECOND
fi
`
	executor, _ := localCLIExecutor(t, script)
	out, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Usage != nil {
		t.Errorf("usage = %+v, want nil when one attempt lacked usage", out.Usage)
	}
}

// Pi 报告失败的 stopReason → executor_failed。
func TestExecuteLocalCLIPropagatesPiFailure(t *testing.T) {
	t.Parallel()
	event := piEventLine("provider exploded", "error", 10, 2)
	executor, _ := localCLIExecutor(t, "cat <<'EOF'\n"+event+"\nEOF\n")
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "executor_failed")
	if execErr.Detail != "Pi final assistant message reported an unsuccessful stop reason" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// 提示词超过配置上限 → input_too_large,不 spawn 进程。
func TestExecuteLocalCLIRejectsOversizedPrompt(t *testing.T) {
	t.Parallel()
	executor, cfg := localCLIExecutor(t, "echo should-not-run\n")
	cfg.LocalAgents.MaxPromptBytes = 16
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "input_too_large")
	if execErr.Detail != "Pi prompt exceeded the configured byte limit" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// Claude 的提示词上限用同一份配置,但错误文案不同。
func TestExecuteClaudeRejectsOversizedPrompt(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.LocalAgents.MaxPromptBytes = 16
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "input_too_large")
	if execErr.Detail != "Claude prompt exceeded the configured byte limit" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// 二进制不存在 → executor_unavailable。
func TestExecuteLocalCLIMissingBinary(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	cfg.LocalAgents.PiBinary = filepath.Join(t.TempDir(), "definitely-not-here")
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindPi, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	wantExecutionCode(t, err, "executor_unavailable")
}

// Reasonix 的用量来自 --metrics 侧信道文件,读完必须删掉。
func TestExecuteLocalCLIReasonixReadsAndRemovesMetrics(t *testing.T) {
	t.Parallel()
	payload := `{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[1,2],"reasoning":"r"}]}}`
	metricsBody := `{"promptTokens":210,"completionTokens":40,"requestCount":2,"cacheHitTokens":15}`
	// 把 --metrics 的路径抄下来,便于测试断言它已被删除。
	recorded := filepath.Join(t.TempDir(), "metrics-path")
	script := `
METRICS=""
PREV=""
for ARG in "$@"; do
  if [ "$PREV" = "--metrics" ]; then METRICS="$ARG"; fi
  PREV="$ARG"
done
printf '%s' "$METRICS" > ` + recorded + `
cat > "$METRICS" <<'METRICS_EOF'
` + metricsBody + `
METRICS_EOF
cat <<'OUT'
` + payload + `
OUT
`
	cfg := executorTestConfig(t)
	cfg.LocalAgents.ReasonixBinary = writeFakeCLI(t, script)
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	out, err := executor.Execute(context.Background(),
		executorRuntime("technical_analyst", ExecutorKindReasonix, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Usage == nil {
		t.Fatal("expected usage from the metrics side channel")
	}
	if out.Usage.PromptTokens != 210 || out.Usage.CompletionTokens != 40 {
		t.Errorf("usage = %+v, want 210/40", out.Usage)
	}
	if out.Usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", out.Usage.InvocationCount)
	}
	wantUint64(t, "cachedInputTokens", out.Usage.CachedInputTokens, 15)

	pathBytes, readErr := os.ReadFile(recorded)
	if readErr != nil {
		t.Fatalf("read recorded metrics path: %v", readErr)
	}
	if _, statErr := os.Stat(strings.TrimSpace(string(pathBytes))); !os.IsNotExist(statErr) {
		t.Errorf("metrics file %q was not removed (stat err: %v)", pathBytes, statErr)
	}
}

// metrics 文件缺失或不自洽时用量为 nil —— 纯文本输出本身拿不到用量。
func TestExecuteLocalCLIReasonixWithoutUsableMetrics(t *testing.T) {
	t.Parallel()
	payload := `{"payload_result":{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[1,2],"reasoning":"r"}]}}`
	for name, metricsScript := range map[string]string{
		"文件没写出来": ``,
		"内容不是 JSON": `
PREV=""; for ARG in "$@"; do if [ "$PREV" = "--metrics" ]; then printf 'not json' > "$ARG"; fi; PREV="$ARG"; done`,
		"标记为推算值": `
PREV=""; for ARG in "$@"; do if [ "$PREV" = "--metrics" ]; then printf '{"promptTokens":1,"completionTokens":1,"requestCount":1,"estimated":true}' > "$ARG"; fi; PREV="$ARG"; done`,
	} {
		cfg := executorTestConfig(t)
		cfg.LocalAgents.ReasonixBinary = writeFakeCLI(t, metricsScript+"\ncat <<'OUT'\n"+payload+"\nOUT\n")
		executor := newTestExecutor(t, cfg, &stubProcessor{})
		out, err := executor.Execute(context.Background(),
			executorRuntime("technical_analyst", ExecutorKindReasonix, ExecutionModeLocal),
			&TaskContext{}, map[string]any{}, 20*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Usage != nil {
			t.Errorf("%s: usage = %+v, want nil", name, out.Usage)
		}
	}
}

// ---------------------------------------------------------------------------
// executeClaude(用假 claude 脚本驱动真实子进程)
// ---------------------------------------------------------------------------

func TestExecuteClaudeHappyPath(t *testing.T) {
	t.Parallel()
	envelope := jsonString(map[string]any{
		"result":    `{"payload_result":{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":210,"reasoning":"r"}],"summary":"claude 摘要"}}`,
		"is_error":  false,
		"num_turns": 3,
		"usage":     map[string]any{"input_tokens": 80, "cache_read_input_tokens": 20, "output_tokens": 12},
	})
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.ClaudeBinary = writeFakeCLI(t, "cat > /dev/null\ncat <<'OUT'\n"+envelope+"\nOUT\n")
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	out, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Summary != "claude 摘要" {
		t.Errorf("summary = %#v", out.Summary)
	}
	if out.Usage == nil {
		t.Fatal("expected usage from the Claude envelope")
	}
	if out.Usage.PromptTokens != 100 || out.Usage.CompletionTokens != 12 {
		t.Errorf("usage = %+v, want 100/12 (80+20 prompt)", out.Usage)
	}
	if out.Usage.InvocationCount != 3 {
		t.Errorf("invocationCount = %d, want 3 (num_turns)", out.Usage.InvocationCount)
	}
}

// is_error 为 true → executor_failed。
func TestExecuteClaudeRejectsErrorEnvelope(t *testing.T) {
	t.Parallel()
	envelope := jsonString(map[string]any{"is_error": true, "result": "boom"})
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.ClaudeBinary = writeFakeCLI(t, "cat > /dev/null\ncat <<'OUT'\n"+envelope+"\nOUT\n")
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "executor_failed")
	if execErr.Detail != "Claude Code reported an unsuccessful result" {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// 非 JSON stdout → invalid_executor_output(与本机 CLI 的纯文本路径不同,
// Claude 的 --output-format json 必须给出 JSON 信封)。
func TestExecuteClaudeRejectsNonJSONOutput(t *testing.T) {
	t.Parallel()
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.ClaudeBinary = writeFakeCLI(t, "cat > /dev/null\necho 'not json at all'\n")
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	_, err := executor.Execute(context.Background(),
		executorRuntime("fundamental_analyst", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	execErr := wantExecutionCode(t, err, "invalid_executor_output")
	if !strings.HasPrefix(execErr.Detail, "Claude Code did not return JSON: ") {
		t.Errorf("detail = %q", execErr.Detail)
	}
}

// Claude 不做纠正重试(与本机 CLI 不同):缺字段直接走保守结构。
func TestExecuteClaudeUsesConservativePayloadWithoutRetry(t *testing.T) {
	t.Parallel()
	envelope := jsonString(map[string]any{
		"result": `{"assessments":[]}`,
		"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	cfg := executorTestConfig(t)
	cfg.ClaudeEnabled = true
	cfg.ClaudeBinary = writeFakeCLI(t, "cat > /dev/null\ncat <<'OUT'\n"+envelope+"\nOUT\n")
	executor := newTestExecutor(t, cfg, &stubProcessor{})
	out, err := executor.Execute(context.Background(),
		executorRuntime("risk_manager", ExecutorKindClaudeCode, ExecutionModeLocal),
		&TaskContext{}, map[string]any{}, 20*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload := payloadOf(t, out)
	if payload["gate"] != "WARN" {
		t.Errorf("gate = %#v, want the conservative WARN", payload["gate"])
	}
	if checks := payload["checks"].([]any); len(checks) != 1 {
		t.Errorf("checks = %#v, want one conservative check", checks)
	}
	// 保守替换不影响用量上报。
	if out.Usage == nil || out.Usage.PromptTokens != 1 {
		t.Errorf("usage = %+v, want the reported counts", out.Usage)
	}
}
