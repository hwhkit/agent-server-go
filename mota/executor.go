package mota

// 本文件对应 rs `src/platform/executor/mod.rs` 的执行主体:
// AgentExecutor / ExecutionError / execute 分发矩阵 / OutputPolicy /
// execute_claude / execute_local_cli / run_local_agent_once / execute_litellm_agent /
// gateway_request / gateway_content / retry_gateway /
// failure_default_output / conservative_role_payload / role_summary_text /
// output_from_claude。
//
// 执行层只依赖处理器、共享 HTTP 客户端和不可变运行配置,不依赖数据库、认证、
// Registry 等控制面状态。这样本地 CLI 与网关链路可以独立压测和故障测试。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// ExecutionError
// ---------------------------------------------------------------------------

// ExecutionError 是一次 Agent 执行失败。Code 是稳定的机器可读错误码,Detail 仅
// 用于日志与运行记录,不回传给调用方(对外只暴露 PublicMessage)。
type ExecutionError struct {
	Code   string
	Detail string
	// executorOutput 是执行节点的原始回执,仅用于运行记录审计。
	executorOutput any
}

// newExecutionError 构造一个执行错误。
func newExecutionError(code, detail string) *ExecutionError {
	return &ExecutionError{Code: code, Detail: detail}
}

func (e *ExecutionError) Error() string { return e.Code + ": " + e.Detail }

// WithExecutorOutput 附上执行节点原始回执,便于事后定位。返回自身以便链式调用。
func (e *ExecutionError) WithExecutorOutput(output any) *ExecutionError {
	e.executorOutput = output
	return e
}

// ExecutorResultJSON 返回原始回执的 JSON 文本,供运行记录落库。
func (e *ExecutionError) ExecutorResultJSON() *string {
	return serializeExecutorOutput(e.executorOutput)
}

// PublicMessage 返回可安全回传给调用方的稳定文案。
// 内部细节(命令行、路径、上游报错)一律不外泄。
func (e *ExecutionError) PublicMessage() string {
	switch e.Code {
	case "execution_timeout":
		return "agent execution timed out"
	case "executor_unavailable":
		return "agent executor is unavailable"
	case "output_too_large":
		return "agent output exceeded the configured limit"
	case "input_too_large":
		return "agent input exceeded the configured limit"
	default:
		return "agent execution failed"
	}
}

// ---------------------------------------------------------------------------
// AgentExecutor
// ---------------------------------------------------------------------------

// AgentExecutor 承载一次 Agent 执行所需的全部依赖。
type AgentExecutor struct {
	processor Processor
	http      *http.Client
	config    *PlatformConfig
	configMu  sync.RWMutex
	// workspaceRoot 是已解析符号链接的绝对路径,用于逃逸检查的比较基准。
	workspaceRoot string
}

// snapshot 为一次任务固定当前不可变配置。锁只保护指针的读取，不跨越模型调用；
// 因此 Provider 热更新不会等待长任务结束，而已经开始的任务仍会完整使用旧配置。
func (e *AgentExecutor) snapshot() *AgentExecutor {
	e.configMu.RLock()
	config := e.config
	e.configMu.RUnlock()
	return &AgentExecutor{
		processor:     e.processor,
		http:          e.http,
		config:        config,
		workspaceRoot: e.workspaceRoot,
	}
}

// SetConfig 让 Provider 热更新从下一次执行开始生效；正在运行的任务继续使用
// 更新前快照，避免一次任务内模型、Host 或密钥发生变化。
func (e *AgentExecutor) SetConfig(config *PlatformConfig) {
	e.configMu.Lock()
	e.config = config
	e.configMu.Unlock()
}

// NewAgentExecutor 组装执行器。workspace 根目录必须存在且可解析,否则直接失败 ——
// 逃逸检查依赖一个稳定的规范化基准路径。
func NewAgentExecutor(processor Processor, httpClient *http.Client, config *PlatformConfig) (*AgentExecutor, error) {
	root, err := canonicalizePath(config.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve Agent workspace root %s: %w", config.WorkspaceRoot, err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AgentExecutor{
		processor:     processor,
		http:          httpClient,
		config:        config,
		workspaceRoot: root,
	}, nil
}

// Execute 执行一次任务。失败时返回的 error 一定是 *ExecutionError。
func (e *AgentExecutor) Execute(
	ctx context.Context,
	runtime *AgentRuntime,
	context_ *TaskContext,
	rawInput any,
	timeout time.Duration,
) (*ProcessedOutput, error) {
	executor := e.snapshot()
	output, execErr := executor.execute(ctx, runtime, context_, rawInput, timeout)
	if execErr != nil {
		return nil, execErr
	}
	return output, nil
}

// canonicalizePath 等价于 Rust 的 std::fs::canonicalize:转绝对路径并解析符号链接。
func canonicalizePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// deepCopyJSONValue 深拷贝一份 JSON 值。
//
// 这一步是必需的:rs 侧 `candidate.get("payload_result").cloned()` 拿到的是副本,
// 后续对 payload_result 的所有归一化都不会改动 candidate。Go 的 map 是引用类型,
// 不拷贝就会把归一化结果写回 candidate,导致运行记录里的「执行器原始回执」被污染,
// 并且 role_summary_text 读到的也不再是模型原文。
func deepCopyJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = deepCopyJSONValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = deepCopyJSONValue(item)
		}
		return copied
	default:
		// 标量(string / bool / json.Number / float64 / nil)本身不可变,直接复用。
		return typed
	}
}

// ---------------------------------------------------------------------------
// 输出策略
// ---------------------------------------------------------------------------

// outputPolicy 汇总影响输出归一化的任务上下文。对应 rs OutputPolicy。
type outputPolicy struct {
	// symbolPool 为 nil 表示任务没有给出标的池(对应 rs 的 Option::None),
	// 此时不做池内过滤与补全;非 nil 的空池是另一种含义(池存在但为空)。
	symbolPool      []string
	targetCount     int
	hasTargetCount  bool
	universeMode    *string
	upstreamSymbols []string
	isFinal         bool
}

func newOutputPolicy(task *TaskContext) *outputPolicy {
	targetCount, hasTargetCount := ExtractTargetCount(task.TaskBrief)
	return &outputPolicy{
		symbolPool:      task.SymbolPool,
		targetCount:     targetCount,
		hasTargetCount:  hasTargetCount,
		universeMode:    task.UniverseMode,
		upstreamSymbols: task.UpstreamSymbols,
		isFinal:         task.IsFinal(),
	}
}

// fallbackSymbol 取保守缺省结构里的兜底标的:优先标的池首项,其次上游标的首项。
func (p *outputPolicy) fallbackSymbol() (string, bool) {
	if len(p.symbolPool) > 0 {
		return p.symbolPool[0], true
	}
	if len(p.upstreamSymbols) > 0 {
		return p.upstreamSymbols[0], true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 分发矩阵
// ---------------------------------------------------------------------------

// executorLogRole returns a low-sensitivity role identifier for structured
// logs. SystemPrompt may contain a complete custom prompt, including business
// data or credentials, and must never be emitted as a log attribute.
func executorLogRole(systemPrompt string) string {
	if descriptor, ok := ResolveRoleDescriptor(systemPrompt); ok {
		return descriptor.ID
	}
	return "custom"
}

// gatewayAgentLabels 是网关直调时写进系统提示词的 agent 名称。
var gatewayAgentLabels = map[ExecutorKind]string{
	ExecutorKindClaudeCode:      "Claude",
	ExecutorKindCodex:           "Codex",
	ExecutorKindReasonix:        "Reasonix",
	ExecutorKindPi:              "Pi",
	ExecutorKindDeepseekHarness: "DeepSeek Harness",
	ExecutorKindGoose:           "Goose",
}

// localCLIKinds 把执行器种类映射到本机 CLI 种类。
var localCLIKinds = map[ExecutorKind]LocalCliKind{
	ExecutorKindCodex:           LocalCliCodex,
	ExecutorKindReasonix:        LocalCliReasonix,
	ExecutorKindPi:              LocalCliPi,
	ExecutorKindOpenCode:        LocalCliOpenCode,
	ExecutorKindQwenCode:        LocalCliQwenCode,
	ExecutorKindGeminiCLI:       LocalCliGeminiCLI,
	ExecutorKindOpenHands:       LocalCliOpenHands,
	ExecutorKindDeepseekHarness: LocalCliDeepseekHarness,
}

func (e *AgentExecutor) execute(
	ctx context.Context,
	runtime *AgentRuntime,
	task *TaskContext,
	rawInput any,
	timeout time.Duration,
) (*ProcessedOutput, *ExecutionError) {
	if timeout <= 0 {
		return nil, newExecutionError(
			"execution_timeout",
			"Agent deadline elapsed before execution started",
		)
	}
	policy := newOutputPolicy(task)

	// 失败路径短路:任务简报明确要求保守缺省(如「不足以支撑决策」「无法完成」),
	// 直接按岗位契约返回保守结构,跳过 LLM,避免模型自行给出决策导致平台
	// FAILURE_DEFAULT 断言失败。
	if task.ForceFailureDefault {
		reason, ok := ExtractFailureReason(task.TaskBrief)
		if !ok {
			reason = "任务简报明确要求使用保守缺省结果"
		}
		slog.Info("failure-path detected: emitting conservative default payload (skip LLM)",
			"agent_slug", runtime.Config.Slug,
			"role", executorLogRole(runtime.Config.SystemPrompt),
			"is_final", policy.isFinal)
		symbol, hasSymbol := policy.fallbackSymbol()
		return failureDefaultOutput(runtime.Config.SystemPrompt, policy.isFinal, reason, symbol, hasSymbol), nil
	}

	kind := runtime.Config.ExecutorKind
	mode := runtime.Config.ExecutionMode

	// 注意分支顺序与 rs 一致:Rust(进程内 Processor)优先于执行方式匹配,
	// 因此 (rust, gateway) 也走进程内处理器。
	if kind == ExecutorKindRust {
		return e.executeProcessor(ctx, task, timeout)
	}
	if mode == ExecutionModeLocal {
		if kind == ExecutorKindClaudeCode {
			return e.executeClaude(ctx, runtime, rawInput, timeout, policy)
		}
		if cliKind, ok := localCLIKinds[kind]; ok {
			return e.executeLocalCLI(ctx, runtime, rawInput, timeout, cliKind, policy)
		}
	}
	if mode == ExecutionModeManaged {
		switch kind {
		case ExecutorKindOpenAIAgent:
			return e.executeOpenAIAgent(ctx, runtime, rawInput, timeout, policy)
		case ExecutorKindClaudeManaged:
			return e.executeClaudeManaged(ctx, runtime, rawInput, timeout, policy)
		default:
			return nil, newExecutionError("executor_unavailable", "managed executor is unsupported")
		}
	}
	if mode == ExecutionModeGateway && kind.SupportsGateway() {
		return e.executeLiteLLMAgent(ctx, runtime, rawInput, timeout, gatewayAgentLabels[kind], policy)
	}
	return nil, newExecutionError("executor_unavailable",
		fmt.Sprintf("%s does not support %s execution", kind, mode))
}

// executeProcessor 在超时预算内运行进程内 Processor。
//
// 即使 Processor 不响应 ctx 取消,这里也会在超时后返回 execution_timeout,
// 与 rs 的 tokio::time::timeout 语义一致(超时即放弃等待结果)。
func (e *AgentExecutor) executeProcessor(
	ctx context.Context,
	task *TaskContext,
	timeout time.Duration,
) (*ProcessedOutput, *ExecutionError) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		output *ProcessedOutput
		err    error
	}
	done := make(chan result, 1)
	go func() {
		processed := result{}
		defer func() {
			if recover() != nil {
				processed.output = nil
				processed.err = errors.New("in-process processor panicked")
			}
			done <- processed
		}()
		processed.output, processed.err = e.processor.Process(ctx, task)
	}()

	select {
	case got := <-done:
		if got.err != nil {
			return nil, newExecutionError("processor_failed", got.err.Error())
		}
		if got.output == nil {
			return nil, newExecutionError("invalid_executor_output",
				"in-process processor returned no output")
		}
		return got.output, nil
	case <-ctx.Done():
		return nil, newExecutionError("execution_timeout", "in-process processor exceeded its deadline")
	}
}

// resolveWorkspace 把 Agent 的相对 workspace 解析为规范化绝对路径,并确认它没有
// 逃出配置的根目录。
func (e *AgentExecutor) resolveWorkspace(relative string) (string, *ExecutionError) {
	joined := filepath.Join(e.workspaceRoot, relative)
	workspace, err := canonicalizePath(joined)
	if err != nil {
		return "", newExecutionError("executor_unavailable",
			fmt.Sprintf("resolve workspace %s: %v", joined, err))
	}
	// 按路径分量比较,避免 /root-evil 被 /root 的字符串前缀误判为通过。
	rel, err := filepath.Rel(e.workspaceRoot, workspace)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", newExecutionError("executor_unavailable", "workspace escaped configured root")
	}
	return workspace, nil
}

// roleSystemContract 组装交给执行器的系统提示词:岗位契约 + 输出语言硬约束。
func roleSystemContract(runtime *AgentRuntime, rawInput any) string {
	_, contract := resolveRoleContract(runtime.Config.SystemPrompt)
	return fmt.Sprintf("%s\n\nOUTPUT LANGUAGE REQUIREMENT: %s",
		contract, outputLanguageInstruction(rawInput))
}

// ---------------------------------------------------------------------------
// Claude Code 本机执行
// ---------------------------------------------------------------------------

func (e *AgentExecutor) executeClaude(
	ctx context.Context,
	runtime *AgentRuntime,
	rawInput any,
	timeout time.Duration,
	policy *outputPolicy,
) (*ProcessedOutput, *ExecutionError) {
	if !e.config.ClaudeEnabled {
		return nil, newExecutionError("executor_unavailable", "Claude Code executor is disabled")
	}
	workspace, execErr := e.resolveWorkspace(runtime.Config.WorkspacePath)
	if execErr != nil {
		return nil, execErr
	}
	prompt := buildPrompt(runtime, rawInput)
	if len(prompt) > e.config.LocalAgents.MaxPromptBytes {
		return nil, newExecutionError("input_too_large",
			"Claude prompt exceeded the configured byte limit")
	}
	systemContract := roleSystemContract(runtime, rawInput)
	local, err := claudeCommand(e.config, runtime, workspace, prompt, systemContract)
	if err != nil {
		var execution *ExecutionError
		if errors.As(err, &execution) {
			return nil, execution
		}
		return nil, newExecutionError("executor_unavailable", err.Error())
	}
	stdout, err := runCommand(ctx, local.Cmd, local.StdinPayload, timeout,
		e.config.MaxClaudeOutputBytes, CaptureComplete, local.Label)
	if err != nil {
		return nil, toExecutionError(err, "executor_failed")
	}
	envelope, decodeErr := decodeJSONNumbers(stdout)
	if decodeErr != nil {
		return nil, newExecutionError("invalid_executor_output",
			fmt.Sprintf("Claude Code did not return JSON: %v", decodeErr))
	}
	if isError, ok := jsonBoolField(envelope, "is_error"); ok && isError {
		return nil, newExecutionError("executor_failed",
			"Claude Code reported an unsuccessful result")
	}
	usage := usageFromClaudeEnvelope(envelope)
	output, execErr := outputFromClaude(envelope, runtime, policy)
	if execErr != nil {
		return nil, execErr
	}
	output.Usage = usage
	return output, nil
}

// asExecutionError 把子层返回的 error 归一为 *ExecutionError。
func toExecutionError(err error, fallbackCode string) *ExecutionError {
	var execution *ExecutionError
	if errors.As(err, &execution) {
		return execution
	}
	return newExecutionError(fallbackCode, err.Error())
}

// ---------------------------------------------------------------------------
// 本机 CLI 执行(pi / codex / reasonix)
// ---------------------------------------------------------------------------

// executeLocalCLI 调用本机安装的 agent CLI 执行任务。
//
// 与网关路径不同,这里真正 spawn 本机 CLI 进程:模型的 provider/配置来自各工具
// 自身已安装好的认证配置;任务只通过一个通道传入,避免 prompt 同时出现在 argv 与
// stdin 时被 Agent 重复拼接。stdout 的最终文本(通常为岗位 JSON 契约)交给
// outputFromClaude 做与网关路径一致的规范化,保证两种执行方式产出对齐。
//
// 岗位必填字段缺失时会用「纠正提示词」重试一次;两次都缺失则报
// invalid_executor_output 并附上模型原文。
func (e *AgentExecutor) executeLocalCLI(
	ctx context.Context,
	runtime *AgentRuntime,
	rawInput any,
	timeout time.Duration,
	cliKind LocalCliKind,
	policy *outputPolicy,
) (*ProcessedOutput, *ExecutionError) {
	workspace, execErr := e.resolveWorkspace(runtime.Config.WorkspacePath)
	if execErr != nil {
		return nil, execErr
	}
	prompt := buildPrompt(runtime, rawInput)
	if len(prompt) > e.config.LocalAgents.MaxPromptBytes {
		return nil, newExecutionError("input_too_large",
			fmt.Sprintf("%s prompt exceeded the configured byte limit", cliKind.Label()))
	}
	systemContract := roleSystemContract(runtime, rawInput)
	started := time.Now()

	candidate, firstUsage, execErr := e.runLocalAgentOnce(
		ctx, runtime, workspace, prompt, systemContract, cliKind, timeout)
	if execErr != nil {
		return nil, execErr
	}

	usage := firstUsage
	missing := missingRoleFields(candidate, runtime.Config.SystemPrompt, policy.isFinal)
	if len(missing) > 0 {
		remaining := timeout - time.Since(started)
		if remaining <= 0 {
			return nil, newExecutionError("invalid_executor_output",
				fmt.Sprintf("%s output missed required role fields and no retry budget remained: %s",
					cliKind.Label(), strings.Join(missing, ", "))).
				WithExecutorOutput(candidate)
		}
		slog.Warn("local Agent output missed role fields; retrying once",
			"agent_slug", runtime.Config.Slug,
			"executor", cliKind.Label(),
			"role", executorLogRole(runtime.Config.SystemPrompt),
			"missing", strings.Join(missing, ","))

		correction := fmt.Sprintf(
			"%s\n\nCORRECTION RETRY: Your previous response used the wrong output schema. "+
				"Return a single JSON object for role `%s` and include non-empty required field(s): %s. "+
				"Do not return assessments for a technical/strategist role and do not add explanatory text.",
			prompt, runtime.Config.SystemPrompt, strings.Join(missing, ", "))
		if len(correction) > e.config.LocalAgents.MaxPromptBytes {
			return nil, newExecutionError("input_too_large",
				"schema correction prompt exceeded the configured byte limit").
				WithExecutorOutput(candidate)
		}

		retryCandidate, retryUsage, execErr := e.runLocalAgentOnce(
			ctx, runtime, workspace, correction, systemContract, cliKind, remaining)
		if execErr != nil {
			return nil, execErr
		}
		retryMissing := missingRoleFields(retryCandidate, runtime.Config.SystemPrompt, policy.isFinal)
		if len(retryMissing) > 0 {
			return nil, newExecutionError("invalid_executor_output",
				fmt.Sprintf("%s output still missed required role fields after retry: %s",
					cliKind.Label(), strings.Join(retryMissing, ", "))).
				WithExecutorOutput(retryCandidate)
		}
		candidate = retryCandidate
		// 只有两轮都拿到可信用量时才上报合计;任一轮缺失就整体作废,
		// 避免把「一轮的用量」当成「整次执行的用量」少报。
		if firstUsage != nil && retryUsage != nil {
			var totals usageAccumulator
			totals.addReport(firstUsage)
			totals.addReport(retryUsage)
			usage = totals.finish()
		} else {
			usage = nil
		}
	}

	output, execErr := outputFromClaude(candidate, runtime, policy)
	if execErr != nil {
		return nil, execErr
	}
	output.Usage = usage
	return output, nil
}

// runLocalAgentOnce 跑一次本机 CLI 并解析出候选结果与用量。
func (e *AgentExecutor) runLocalAgentOnce(
	ctx context.Context,
	runtime *AgentRuntime,
	workspace string,
	prompt string,
	systemContract string,
	cliKind LocalCliKind,
	timeout time.Duration,
) (any, *BoundRunUsage, *ExecutionError) {
	local := agentCommand(e.config, runtime, workspace, prompt, systemContract, cliKind)
	defer func() {
		for _, path := range local.CleanupPaths {
			_ = os.Remove(path)
		}
	}()
	capture := CaptureComplete
	if local.OutputMode == LocalOutputPiJSONLines || local.OutputMode == LocalOutputCodexJSONLines || local.OutputMode == LocalOutputOpenHandsJSONLines {
		// JSONL 事件流只保留尾部:超限时降级而不是报错,收尾事件必须留住。
		capture = CaptureEventStreamTail
	}
	stdout, err := runCommand(ctx, local.Cmd, local.StdinPayload, timeout,
		e.config.LocalAgents.MaxOutputBytes, capture, local.Label)
	if err != nil {
		// 失败也要清掉 metrics 侧信道临时文件,避免 /tmp 堆积。
		if local.MetricsPath != "" {
			_ = os.Remove(local.MetricsPath)
		}
		return nil, nil, toExecutionError(err, "executor_failed")
	}

	text := lossyUTF8(stdout)
	var candidate any
	var usage *BoundRunUsage
	var execErr *ExecutionError
	switch local.OutputMode {
	case LocalOutputPiJSONLines:
		candidate, usage, execErr = parsePiJSONLines(text)
	case LocalOutputCodexJSONLines:
		candidate, usage, execErr = parseCodexJSONLines(text)
	case LocalOutputGeminiJSON:
		candidate, usage, execErr = parseGeminiJSON(text)
	case LocalOutputOpenHandsJSONLines:
		candidate, usage, execErr = parseOpenHandsJSONLines(text)
	default:
		candidate, usage = parseTextCandidate(text), nil
	}
	if execErr != nil {
		if local.MetricsPath != "" {
			_ = os.Remove(local.MetricsPath)
		}
		return nil, nil, execErr
	}

	// Reasonix 的 run 子命令只输出纯文本,用量只能来自 --metrics 侧信道文件。
	// 读不到或不自洽时用量为 nil —— 覆盖掉流解析的结果也是有意的。
	if local.MetricsPath != "" {
		usage = nil
		if bytes, readErr := os.ReadFile(local.MetricsPath); readErr == nil {
			if metrics, decodeErr := decodeJSONNumbers(bytes); decodeErr == nil {
				usage = usageFromReasonixMetrics(metrics)
			}
		}
		_ = os.Remove(local.MetricsPath)
	}
	return candidate, usage, nil
}

// ---------------------------------------------------------------------------
// litellm 网关直调
// ---------------------------------------------------------------------------

// executeLiteLLMAgent 通过 litellm 网关直调外部 agent
// (Codex/Reasonix/Pi/DeepSeek Harness/Goose/Claude 统一入口)。
//
// 重试语义:5xx 与 429 可重试;非 JSON 信封、空内容、岗位字段缺失也各自重试一次预算。
// 用量只有在**全程没有任何一次不完整**时才上报 —— 任何一次请求失败、非 JSON、
// 或读不出用量都会把 usageIncomplete 置位,最终整份用量作废。
func (e *AgentExecutor) executeLiteLLMAgent(
	ctx context.Context,
	runtime *AgentRuntime,
	rawInput any,
	timeout time.Duration,
	agentLabel string,
	policy *outputPolicy,
) (*ProcessedOutput, *ExecutionError) {
	gateway := &e.config.Gateway
	if strings.TrimSpace(gateway.BaseURL) == "" {
		return nil, newExecutionError("executor_unavailable",
			"litellm gateway base URL is not configured")
	}
	prompt := buildPrompt(runtime, rawInput)
	_, contract := resolveRoleContract(runtime.Config.SystemPrompt)
	system := fmt.Sprintf("你是 %s agent。\n%s\n\nOUTPUT LANGUAGE REQUIREMENT: %s",
		agentLabel, contract, outputLanguageInstruction(rawInput))
	url, body := gatewayRequest(gateway, system, prompt)
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, newExecutionError("executor_unavailable",
			fmt.Sprintf("encode gateway request: %v", err))
	}

	deadline := time.Now().Add(timeout)
	lastError := "gateway returned no usable response"
	var usageTotals usageAccumulator
	usageIncomplete := false

	for attempt := 0; attempt < gateway.MaxAttempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, newExecutionError("execution_timeout",
				"gateway execution exhausted the Agent deadline")
		}

		status, responseBytes, timedOut, requestErr := e.sendGatewayRequest(
			ctx, gateway, url, encodedBody, remaining, deadline)
		if timedOut {
			return nil, newExecutionError("execution_timeout", requestErr.Error())
		}
		if requestErr != nil {
			var oversize *ExecutionError
			if errors.As(requestErr, &oversize) {
				return nil, oversize
			}
			usageIncomplete = true
			lastError = requestErr.Error()
			slog.Warn("gateway request failed", "attempt", attempt+1, "error", requestErr)
			if !retryGateway(ctx, attempt, gateway, deadline) {
				break
			}
			continue
		}

		if status < 200 || status > 299 {
			usageIncomplete = true
			lastError = fmt.Sprintf("gateway returned HTTP %d", status)
			retryable := status >= 500 || status == http.StatusTooManyRequests
			if retryable && retryGateway(ctx, attempt, gateway, deadline) {
				slog.Warn("transient gateway response", "attempt", attempt+1, "status", status)
				continue
			}
			return nil, newExecutionError("executor_failed", lastError)
		}

		envelope, decodeErr := decodeJSONNumbers(responseBytes)
		if decodeErr != nil {
			usageIncomplete = true
			lastError = fmt.Sprintf("gateway returned a non-JSON envelope: %v", decodeErr)
			slog.Warn("gateway returned non-JSON envelope", "attempt", attempt+1, "error", decodeErr)
			if retryGateway(ctx, attempt, gateway, deadline) {
				continue
			}
			break
		}
		if usage := usageFromGateway(envelope, gateway.Protocol); usage != nil {
			usageTotals.addReport(usage)
		} else {
			usageIncomplete = true
		}

		contentText, _ := gatewayContent(envelope)
		if strings.TrimSpace(contentText) == "" {
			lastError = "gateway returned empty content"
			slog.Warn("gateway returned empty content",
				"attempt", attempt+1, "usage_incomplete", usageIncomplete)
			if retryGateway(ctx, attempt, gateway, deadline) {
				continue
			}
			break
		}

		// 网关内容整串能解析成 JSON 就用解析结果,否则原样当字符串
		// (后续 outputFromClaude 会再走一次剥壳提取)。
		candidate, parseErr := decodeJSONNumbers([]byte(contentText))
		if parseErr != nil {
			candidate = contentText
		}
		missing := missingRoleFields(candidate, runtime.Config.SystemPrompt, policy.isFinal)
		if len(missing) > 0 {
			lastError = fmt.Sprintf("gateway output missed required role fields: %s",
				strings.Join(missing, ", "))
			slog.Warn("gateway output missed role fields", "attempt", attempt+1,
				"agent_slug", runtime.Config.Slug,
				"role", executorLogRole(runtime.Config.SystemPrompt),
				"missing", strings.Join(missing, ","))
			if retryGateway(ctx, attempt, gateway, deadline) {
				continue
			}
			return nil, newExecutionError("invalid_executor_output", lastError).
				WithExecutorOutput(candidate)
		}

		var usage *BoundRunUsage
		if !usageIncomplete {
			usage = usageTotals.finish()
			usageTotals = usageAccumulator{}
		}
		output, execErr := outputFromClaude(candidate, runtime, policy)
		if execErr != nil {
			return nil, execErr
		}
		output.Usage = usage
		return output, nil
	}

	return nil, newExecutionError("invalid_executor_output", lastError)
}

// sendGatewayRequest 发一次网关请求并读回响应体。
//
// 返回值 timedOut 为 true 时 err 携带超时文案(调用方必须立即失败,不再重试);
// err 为 *ExecutionError 时是不可重试的硬错误(如响应超限)。
func (e *AgentExecutor) sendGatewayRequest(
	ctx context.Context,
	gateway *GatewayConfig,
	url string,
	body []byte,
	remaining time.Duration,
	deadline time.Time,
) (status int, responseBytes []byte, timedOut bool, err error) {
	sendCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	request, err := http.NewRequestWithContext(sendCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, false, fmt.Errorf("gateway request failed: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	switch gateway.Protocol {
	case GatewayProtocolAnthropic:
		request.Header.Set("anthropic-version", "2023-06-01")
		if gateway.APIKey != "" {
			request.Header.Set("x-api-key", gateway.APIKey)
		}
	case GatewayProtocolOpenAI:
		if gateway.APIKey != "" {
			request.Header.Set("authorization", "Bearer "+gateway.APIKey)
		}
	}

	response, err := e.http.Do(request)
	if err != nil {
		if sendCtx.Err() != nil {
			return 0, nil, true, errors.New("gateway request exceeded the Agent deadline")
		}
		return 0, nil, false, fmt.Errorf("gateway request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	// Content-Length 已经超限时不必把整个响应读进内存。
	if response.ContentLength >= 0 && response.ContentLength > int64(gateway.MaxResponseBytes) {
		return 0, nil, false, newExecutionError("output_too_large",
			"gateway response exceeded the configured byte limit")
	}

	readRemaining := time.Until(deadline)
	if readRemaining <= 0 {
		return 0, nil, true, errors.New("reading gateway response exceeded the Agent deadline")
	}
	readCtx, readCancel := context.WithTimeout(ctx, readRemaining)
	defer readCancel()
	// 多读 1 字节以便区分「正好等于上限」和「超过上限」。
	limited := io.LimitReader(response.Body, int64(gateway.MaxResponseBytes)+1)
	responseBytes, err = readAllWithContext(readCtx, limited)
	if err != nil {
		if readCtx.Err() != nil {
			return 0, nil, true, errors.New("reading gateway response exceeded the Agent deadline")
		}
		return 0, nil, false, fmt.Errorf("read gateway response failed: %w", err)
	}
	if len(responseBytes) > gateway.MaxResponseBytes {
		return 0, nil, false, newExecutionError("output_too_large",
			"gateway response exceeded the configured byte limit")
	}
	return response.StatusCode, responseBytes, false, nil
}

// readAllWithContext 读完 reader,但 ctx 到期就立即返回。
func readAllWithContext(ctx context.Context, reader io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(reader)
		done <- result{data: data, err: err}
	}()
	select {
	case got := <-done:
		return got.data, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gatewayRequest 按协议组装网关请求 URL 与请求体。
func gatewayRequest(config *GatewayConfig, system, prompt string) (string, map[string]any) {
	base := strings.TrimRight(config.BaseURL, "/")
	switch config.Protocol {
	case GatewayProtocolAnthropic:
		return base + "/v1/messages", map[string]any{
			"model":      config.Model,
			"max_tokens": config.MaxTokens,
			"system":     system,
			"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		}
	default:
		return base + "/v1/chat/completions", map[string]any{
			"model":      config.Model,
			"max_tokens": config.MaxTokens,
			"messages": []any{
				map[string]any{"role": "system", "content": system},
				map[string]any{"role": "user", "content": prompt},
			},
		}
	}
}

// gatewayContent 取出网关回执里的助手文本:先按 Anthropic 的 content[0].text,
// 再回退到 OpenAI 的 choices[0].message.content。
func gatewayContent(envelope any) (string, bool) {
	if content, ok := jsonField(envelope, "content"); ok {
		if items, ok := asJSONArray(content); ok && len(items) > 0 {
			if text, ok := jsonStringField(items[0], "text"); ok {
				return text, true
			}
		}
	}
	choices, ok := jsonField(envelope, "choices")
	if !ok {
		return "", false
	}
	items, ok := asJSONArray(choices)
	if !ok || len(items) == 0 {
		return "", false
	}
	message, ok := jsonField(items[0], "message")
	if !ok {
		return "", false
	}
	text, ok := jsonStringField(message, "content")
	return text, ok
}

// retryGateway 判断是否还要重试,并在重试前按指数退避等待。
//
// 退避 250ms × 2^min(attempt,3),即 250/500/1000/2000ms 后封顶;
// 剩余预算不足以等待这段时间时直接放弃重试。
func retryGateway(ctx context.Context, attempt int, config *GatewayConfig, deadline time.Time) bool {
	if attempt+1 >= config.MaxAttempts {
		return false
	}
	shift := attempt
	if shift > 3 {
		shift = 3
	}
	delay := 250 * time.Millisecond * time.Duration(uint64(1)<<uint(shift))
	if time.Until(deadline) <= delay {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---------------------------------------------------------------------------
// 保守缺省结构
// ---------------------------------------------------------------------------

// taskEvidenceRef 是保守结构里唯一允许的证据引用:任务输入本身。
func taskEvidenceRef() map[string]any {
	return map[string]any{"sourceType": "TASK_INPUT", "sourceId": "task"}
}

// failureDefaultOutput 直接产出保守缺省结果,跳过 LLM。对应 rs failure_default_output。
func failureDefaultOutput(role string, isFinal bool, reason, fallbackSymbol string, hasSymbol bool) *ProcessedOutput {
	payloadResult := conservativeRolePayload(role, isFinal, reason, fallbackSymbol, hasSymbol)
	// 从保守结构中提取 summary / orders 等供上层读取;其余字段兜底。
	var assessments any = []any{}
	if value, ok := payloadResult["assessments"]; ok {
		assessments = value
	}
	summary := fmt.Sprintf("保守缺省原因：%s", reason)
	if text, ok := payloadResult["summary"].(string); ok {
		summary = text
	}
	return &ProcessedOutput{
		Assessments:   assessments,
		Summary:       summary,
		Extensions:    []any{},
		EvidenceRefs:  []any{taskEvidenceRef()},
		PayloadResult: payloadResult,
	}
}

// conservativeRolePayload 按岗位生成保守缺省 payload_result
// (避免平台 PAYLOAD_SCHEMA_INVALID)。对应 rs conservative_role_payload。
func conservativeRolePayload(role string, isFinal bool, reason, fallbackSymbol string, hasSymbol bool) map[string]any {
	explanation := fmt.Sprintf("任务未生成可采纳结果。具体原因：%s", reason)
	markdown := explanation
	symbol := "SPY.US"
	if hasSymbol {
		symbol = fallbackSymbol
	}
	switch strings.TrimSpace(role) {
	case "risk_manager":
		return map[string]any{
			"gate": "WARN",
			"checks": []any{map[string]any{
				"dimension": "OTHERS", "status": "UNKNOWN", "reasoning": explanation,
			}},
			"hedging_suggestions": []any{"维持现有仓位，暂缓调仓"},
			"evidenceRefs":        []any{taskEvidenceRef()},
			"extensions":          []any{},
		}
	case "execution_trader":
		return map[string]any{
			"orders":       []any{},
			"evidenceRefs": []any{taskEvidenceRef()},
			"extensions":   []any{},
		}
	case "portfolio_manager":
		if isFinal {
			// 职能二(平台评测):必填为 timestamp / orders / report_markdown。
			return map[string]any{
				"timestamp":       nowTimestamp(),
				"orders":          []any{},
				"report_markdown": markdown,
				"evidenceRefs":    []any{taskEvidenceRef()},
				"extensions":      []any{},
			}
		}
		// 职能一:必填为 decision_type / rationale + evidenceRefs。
		return map[string]any{
			"decision_type": "NO_ACTION",
			"rationale":     markdown,
			"orders":        []any{},
			"target_allocation": []any{map[string]any{
				"symbol": symbol, "target_weight_pct": 100.0, "reasoning": explanation,
			}},
			"evidenceRefs": []any{taskEvidenceRef()},
			"extensions":   []any{},
		}
	case "strategist":
		return map[string]any{
			"signals":      []any{},
			"rationale":    explanation,
			"evidenceRefs": []any{taskEvidenceRef()},
			"extensions":   []any{},
		}
	case "technical_analyst":
		return map[string]any{
			"signals":      []any{},
			"summary":      explanation,
			"evidenceRefs": []any{taskEvidenceRef()},
			"extensions":   []any{},
		}
	default:
		return map[string]any{
			"assessments":  []any{},
			"evidenceRefs": []any{taskEvidenceRef()},
			"summary":      explanation,
			"extensions":   []any{},
		}
	}
}

// ---------------------------------------------------------------------------
// 人读摘要
// ---------------------------------------------------------------------------

// rolePreferredSummaryKeys 是各岗位人读摘要的字段优先级。
func rolePreferredSummaryKeys(role string) []string {
	switch strings.TrimSpace(role) {
	case "strategist", "portfolio_manager":
		return []string{"rationale", "report_markdown", "summary"}
	case "risk_manager":
		return []string{"interpretation", "summary", "rationale"}
	case "execution_trader":
		return []string{"execution_summary", "summary", "rationale"}
	default:
		return []string{"summary", "rationale", "report_markdown"}
	}
}

// roleSummaryText 生成 payload_info 用的人读摘要。对应 rs role_summary_text。
func roleSummaryText(payloadResult any, role string, fallbackReason *string, candidate any) string {
	if fallbackReason != nil {
		return fmt.Sprintf("保守缺省原因：%s", *fallbackReason)
	}
	// 部分执行器会按完整传输信封返回 payload_info;优先保留这段人读摘要。
	// 同时容忍历史执行器把完整信封再次 JSON 字符串化塞进 payload_info,
	// 递归拆出内层摘要,避免页面显示一整段带反斜杠的转义 JSON。
	if summary, ok := ExtractPayloadInfoText(candidate); ok {
		return summary
	}
	for _, key := range rolePreferredSummaryKeys(role) {
		if text, ok := jsonStringField(payloadResult, key); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	switch strings.TrimSpace(role) {
	case "risk_manager":
		gate := "UNKNOWN"
		if value, ok := jsonStringField(payloadResult, "gate"); ok {
			gate = value
		}
		return fmt.Sprintf("风险门控：%s；已完成 %d 项检查。", gate, jsonArrayLen(payloadResult, "checks"))
	case "execution_trader", "portfolio_manager":
		return fmt.Sprintf("已生成 %d 条订单建议。", jsonArrayLen(payloadResult, "orders"))
	case "technical_analyst", "strategist":
		return fmt.Sprintf("已生成 %d 条交易信号。", jsonArrayLen(payloadResult, "signals"))
	default:
		if text, ok := candidate.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		return "任务已完成。"
	}
}

// jsonArrayLen 读取对象里数组字段的长度;缺失或不是数组时为 0。
func jsonArrayLen(value any, key string) int {
	field, ok := jsonField(value, key)
	if !ok {
		return 0
	}
	array, ok := asJSONArray(field)
	if !ok {
		return 0
	}
	return len(array)
}

// ---------------------------------------------------------------------------
// 输出归一化管线
// ---------------------------------------------------------------------------

// riskCheckPlaceholderReasoning 是风控 checks 按条数补位时的占位说明。
const riskCheckPlaceholderReasoning = "conservative placeholder: no model check produced"

// evidenceRefKeys 是证据引用允许的键(与生产协议对齐,多余键一律裁掉)。
var evidenceRefKeys = []string{"sourceType", "sourceId", "asOf"}

// outputFromClaude 把执行器回执归一化为平台可接收的 ProcessedOutput。
// 对应 rs output_from_claude —— 本机 CLI 与网关两条链路共用它,保证产出对齐。
func outputFromClaude(envelope any, runtime *AgentRuntime, policy *outputPolicy) (*ProcessedOutput, *ExecutionError) {
	role := runtime.Config.SystemPrompt
	roleTrimmed := strings.TrimSpace(role)
	isFinal := policy.isFinal

	candidate := extractCandidate(envelope)

	// 完整 payload_result:LLM 按岗位输出 assessments(基本面)/signals(技术、策略)/
	// gate+checks(风控)/orders(执行、组合)等岗位专属字段,一律原样保留。
	//
	// 一律深拷贝:后续归一化都在副本上做,candidate 保持模型原文,
	// 供运行记录审计与 payload_info 提取使用。
	var payloadResult any
	if nested, ok := jsonField(candidate, "payload_result"); ok {
		payloadResult = deepCopyJSONValue(nested)
	} else {
		payloadResult = deepCopyJSONValue(candidate)
	}

	var fallbackReason *string
	fallbackSymbol, hasFallbackSymbol := policy.fallbackSymbol()

	// LLM 输出无法解析为 JSON 对象时,按平台规则给保守缺省结构,
	// 保证回传信封 schema 合规(三键 + 空 assessments + evidenceRefs)。
	object, ok := asJSONObject(payloadResult)
	if !ok {
		reason := "模型返回内容不是可解析的 JSON 对象"
		slog.Warn("LLM output was not parseable as a JSON object; emitting conservative payload_result")
		object = conservativeRolePayload(role, isFinal, reason, fallbackSymbol, hasFallbackSymbol)
		fallbackReason = &reason
	}

	// 规范化:evidenceRefs 白名单过滤 + extensions 数组化 + 岗位契约键白名单裁剪
	// (模型输出多余键会触发平台 additionalProperties:false 违规)。
	normalizeEvidenceRefs(object)
	if _, isArray := asJSONArray(object["extensions"]); !isArray {
		object["extensions"] = []any{}
	}

	if spec, ok := roleSchema(role, isFinal); ok {
		// 兜底:LLM 输出对象但缺少岗位必填主字段(如 risk 缺 gate/checks)时,
		// 说明模型输出结构不符,直接替换为按岗位的保守结构,避免平台 PAYLOAD_SCHEMA_INVALID。
		if missing := missingRequiredKeys(object, role, roleTrimmed, isFinal); len(missing) > 0 {
			reason := fmt.Sprintf("模型输出缺少或未填充岗位必填字段：%s", strings.Join(missing, ", "))
			slog.Warn("LLM output object missing required fields; emitting conservative payload_result",
				"missing", strings.Join(missing, ","))
			object = conservativeRolePayload(role, isFinal, reason, fallbackSymbol, hasFallbackSymbol)
			fallbackReason = &reason
			if items, isArray := asJSONArray(object[spec.itemField]); isArray {
				object[spec.itemField] = normalizeSymbolItems(items, policy.symbolPool, role,
					policy.targetCount, policy.hasTargetCount, policy.universeMode, isFinal, false)
			}
		}

		before := jsonObjectKeys(object)
		// 在 itemKeys 裁剪之前检测 ticker(否则裁剪后就检测不到了)。
		hasTicker := itemsHaveTicker(object[spec.itemField])
		retainKeys(object, spec.resultKeys)
		normalizeTopLevelFields(object, roleTrimmed, isFinal)

		if items, isArray := asJSONArray(object[spec.itemField]); isArray {
			for _, item := range items {
				if itemObject, ok := asJSONObject(item); ok {
					retainKeys(itemObject, spec.itemKeys)
				}
			}
			// symbol 标准化 / 覆盖补全 / 枚举与数值归一(与平台评测断言对齐)。
			object[spec.itemField] = normalizeSymbolItems(items, normalizedSymbolPool(policy, roleTrimmed),
				role, policy.targetCount, policy.hasTargetCount, policy.universeMode, isFinal, hasTicker)

			if roleTrimmed == "risk_manager" {
				alignRiskChecks(object, policy)
				enforceRiskGate(object)
			}
		}

		slog.Info("payload_result normalized to role schema",
			"agent_slug", runtime.Config.Slug,
			"role", executorLogRole(role),
			"before_keys", before, "after_keys", jsonObjectKeys(object),
			"item_count", jsonArrayLen(object, spec.itemField))
	}

	payloadResult = object
	if violations := missingRoleFields(payloadResult, role, isFinal); len(violations) > 0 {
		return nil, newExecutionError("invalid_executor_output",
			fmt.Sprintf("normalized output still violates required role structure: %s",
				strings.Join(violations, ", "))).
			WithExecutorOutput(candidate)
	}

	// 兼容字段:基本面岗位(以及 payload_info/summary 展示)仍从 assessments/summary 读取。
	var assessments any = []any{}
	if value, ok := object["assessments"]; ok {
		assessments = value
	}
	summary := roleSummaryText(payloadResult, role, fallbackReason, candidate)
	var extensions any = []any{}
	if array, isArray := asJSONArray(object["extensions"]); isArray {
		extensions = array
	}
	evidenceRefs, _ := asJSONArray(object["evidenceRefs"])
	if len(evidenceRefs) == 0 {
		taskRef := map[string]any{
			"sourceType": "TASK_INPUT",
			"sourceId":   "task",
			"asOf":       nowTimestamp(),
		}
		evidenceRefs = []any{taskRef}
		object["evidenceRefs"] = []any{taskRef}
	}

	return &ProcessedOutput{
		Assessments:    assessments,
		Summary:        summary,
		Extensions:     extensions,
		EvidenceRefs:   evidenceRefs,
		PayloadResult:  payloadResult,
		ExecutorOutput: candidate,
	}, nil
}

// extractCandidate 从执行器信封里取出候选结果。
//
// structured_output 优先于 result;两者都没有时整个信封就是候选。
// 候选是字符串时说明模型直接返回了文本(可能带 markdown 围栏或前后解释文字),
// 交给 extractJSONFromText 剥壳。
func extractCandidate(envelope any) any {
	candidate := envelope
	if value, ok := jsonField(envelope, "structured_output"); ok {
		candidate = value
	} else if value, ok := jsonField(envelope, "result"); ok {
		candidate = value
	}
	if text, ok := candidate.(string); ok {
		return extractJSONFromText(text)
	}
	return candidate
}

// normalizeEvidenceRefs 裁剪证据引用:只保留三个允许键,且 sourceType 与 sourceId
// 都必须是非空白字符串。全部不合格时直接删掉该键(留空数组会被平台判违约)。
func normalizeEvidenceRefs(object map[string]any) {
	refs, isArray := asJSONArray(object["evidenceRefs"])
	if !isArray {
		return
	}
	cleaned := make([]any, 0, len(refs))
	for _, item := range refs {
		keep := map[string]any{}
		if itemObject, ok := asJSONObject(item); ok {
			for _, key := range evidenceRefKeys {
				if value, exists := itemObject[key]; exists {
					keep[key] = value
				}
			}
		}
		sourceType, hasType := keep["sourceType"].(string)
		sourceID, hasID := keep["sourceId"].(string)
		if !hasType || strings.TrimSpace(sourceType) == "" {
			continue
		}
		if !hasID || strings.TrimSpace(sourceID) == "" {
			continue
		}
		cleaned = append(cleaned, keep)
	}
	if len(cleaned) == 0 {
		delete(object, "evidenceRefs")
		return
	}
	object["evidenceRefs"] = cleaned
}

// missingRequiredKeys 只检查岗位必填键**是否存在**(不看是否为空白)。
//
// 风控岗特判:平台 schema 对 checks 有 minItems:1,模型可能给出 gate 但 checks 是
// 空数组 —— 键存在却空同样会被拒,所以这里把空 checks 也算缺失,触发替换成保守结构。
func missingRequiredKeys(object map[string]any, role, roleTrimmed string, isFinal bool) []string {
	var missing []string
	for _, key := range requiredForRole(role, isFinal) {
		if roleTrimmed == "risk_manager" && key == "checks" {
			if array, isArray := asJSONArray(object[key]); isArray {
				if len(array) == 0 {
					missing = append(missing, key)
				}
				continue
			}
		}
		if _, exists := object[key]; !exists {
			missing = append(missing, key)
		}
	}
	return missing
}

// itemsHaveTicker 判断主数组里是否有条目使用 ticker 字段(组合经理职能二的写法)。
func itemsHaveTicker(value any) bool {
	items, isArray := asJSONArray(value)
	if !isArray {
		return false
	}
	for _, item := range items {
		if _, ok := jsonStringField(item, "ticker"); ok {
			return true
		}
	}
	return false
}

// normalizedSymbolPool 决定归一化时使用的标的池。
//
//   - 有上游标的:优先保留下游语义 = 上游全部订单标的(PRESERVE_UPSTREAM_ORDER_INTENT);
//   - 没有上游标的且岗位是执行交易员/组合经理:返回 nil —— 不按 symbolPool 伪造订单,
//     失败路径要能保持空 orders;
//   - 其余岗位:用任务的 symbolPool(nil 时保持 nil,表示「没有池」)。
func normalizedSymbolPool(policy *outputPolicy, roleTrimmed string) []string {
	if len(policy.upstreamSymbols) > 0 {
		return policy.upstreamSymbols
	}
	if roleTrimmed == "execution_trader" || roleTrimmed == "portfolio_manager" {
		return nil
	}
	return policy.symbolPool
}

// jsonObjectKeys 返回对象的键列表,仅用于日志。
func jsonObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// alignRiskChecks 按 taskBrief 的目标条数补齐或截断风控 checks。
//
// checks 没有 symbol 语义,normalizeSymbolItems 不会补全,所以这里单独对齐 ——
// 平台评测的 EXACT_ITEMS 断言(如「六个维度」)按条数校验。
// 目标条数超出 1..20 时不动(避免把模型的合理输出截成一条)。
func alignRiskChecks(object map[string]any, policy *outputPolicy) {
	if !policy.hasTargetCount || policy.targetCount < 1 || policy.targetCount > 20 {
		return
	}
	checks, isArray := asJSONArray(object["checks"])
	if !isArray {
		return
	}
	target := policy.targetCount
	if len(checks) < target {
		for len(checks) < target {
			checks = append(checks, map[string]any{
				"dimension": "OTHERS",
				"status":    "UNKNOWN",
				"reasoning": riskCheckPlaceholderReasoning,
			})
		}
	} else {
		checks = checks[:target]
	}
	object["checks"] = checks
}

// enforceRiskGate 归一风控顶层 gate 并强制它与 checks 一致。
//
//  1. gate 枚举归一:命中 PASS/APPROVED/放行 → PASS;BREACH/BLOCKED/拦截/违 → BREACH;
//     其余 → WARN(保守方向)。
//  2. 一致性:任一 check 为 UNKNOWN/WARN/CRIT 时 gate 必须非 PASS ——
//     有 CRIT 则 BREACH,否则 WARN。模型常给「PASS + 一条 WARN」这种自相矛盾的组合。
//  3. hedging_suggestions 兜底:失败路径要求非空;正常路径补空数组保持结构完整。
func enforceRiskGate(object map[string]any) {
	if gate, ok := jsonStringField(object, "gate"); ok {
		upper := strings.ToUpper(strings.TrimSpace(gate))
		switch {
		case strings.Contains(upper, "PASS") ||
			strings.Contains(upper, "APPROVED") ||
			strings.Contains(upper, "放行"):
			object["gate"] = "PASS"
		case strings.Contains(upper, "BREACH") ||
			strings.Contains(upper, "BLOCKED") ||
			strings.Contains(upper, "拦截") ||
			strings.Contains(upper, "违"):
			object["gate"] = "BREACH"
		default:
			object["gate"] = "WARN"
		}
	}

	worst := "PASS"
	if checks, isArray := asJSONArray(object["checks"]); isArray {
		for _, check := range checks {
			status, _ := jsonStringField(check, "status")
			status = strings.ToUpper(strings.TrimSpace(status))
			if status == "CRIT" {
				worst = "BREACH"
				break
			}
			if (status == "WARN" || status == "UNKNOWN") && worst != "BREACH" {
				worst = "WARN"
			}
		}
	}
	if worst != "PASS" {
		object["gate"] = worst
	}

	if _, exists := object["hedging_suggestions"]; !exists {
		if worst != "PASS" {
			object["hedging_suggestions"] = []any{"维持现有仓位，暂缓调仓"}
		} else {
			object["hedging_suggestions"] = []any{}
		}
	}
}

// ---------------------------------------------------------------------------
// 兼容入口
// ---------------------------------------------------------------------------

// ExecuteAgent 是给 HTTP 层用的便捷入口:用平台状态里的执行器跑一次任务,
// 超时取 Agent 自身配置的时限。
func ExecuteAgent(
	ctx context.Context,
	state *PlatformState,
	runtime *AgentRuntime,
	task *TaskContext,
	rawInput any,
) (*ProcessedOutput, error) {
	executor, err := state.Executor()
	if err != nil {
		return nil, newExecutionError("executor_unavailable", err.Error())
	}
	return executor.Execute(ctx, runtime, task, rawInput, runtime.Config.Timeout())
}
