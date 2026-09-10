package mota

// 协议入口到动态 Agent 的统一分发层。
// 对应 Rust 侧 src/platform/dispatcher.rs。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// DispatchError
// ---------------------------------------------------------------------------

// DispatchError 是一次分发失败。Code 是稳定的机器可读错误码。
//
// 分发层自身产生五种码:
//   - agent_not_found     —— agentCode 没命中,且没有(或未注册)默认 Agent
//   - agent_disabled      —— 命中的 Agent 被停用
//   - agent_overloaded    —— 准入闸满(执行中 + 排队中已达上限)
//   - execution_timeout   —— 等许可或开工前预算已耗尽
//   - executor_unavailable —— 信号量已关闭(Go 侧不可达,见 acquireSlot 注释)
//
// 除此之外,执行器自身的错误码会原样透出(executor_failed /
// invalid_executor_output / output_too_large / input_too_large / processor_failed 等)。
type DispatchError struct {
	Code   string
	Detail string
}

func newDispatchError(code, detail string) *DispatchError {
	return &DispatchError{Code: code, Detail: detail}
}

func (e *DispatchError) Error() string { return e.Code + ": " + e.Detail }

// ---------------------------------------------------------------------------
// AgentDispatcher
// ---------------------------------------------------------------------------

// AgentDispatcher 按 agentCode(或默认 slug)把任务分发到 Registry 中的 Agent,
// 并在执行前依次过三道容量闸。
type AgentDispatcher struct {
	registry             *AgentRegistry
	executor             *AgentExecutor
	globalExecutionSlots *semaphore
	// defaultAgentSlug 为空表示没有配置默认 Agent。
	defaultAgentSlug string
}

// NewAgentDispatcher 组装分发器。
func NewAgentDispatcher(
	registry *AgentRegistry,
	executor *AgentExecutor,
	globalExecutionSlots *semaphore,
	defaultAgentSlug string,
) *AgentDispatcher {
	return &AgentDispatcher{
		registry:             registry,
		executor:             executor,
		globalExecutionSlots: globalExecutionSlots,
		defaultAgentSlug:     defaultAgentSlug,
	}
}

// Execute 分发并执行一次任务。失败时返回的 error 一定是 *DispatchError。
//
// 准入阶梯的顺序是有意的(与 rs 一致):
//  1. 准入闸(执行中 + 排队中)—— **非阻塞**,满了立刻判 agent_overloaded。
//     这一道先来,是为了让过载请求快速失败,而不是排在后面白等掉整个预算。
//  2. 该 Agent 的执行闸 —— 在剩余预算内阻塞等待。
//  3. 全局执行闸 —— 同样在剩余预算内阻塞等待。
//
// 每一步都用「原始预算减去已耗时」重新计算剩余时间,所以整条链路共享同一个截止时刻。
func (d *AgentDispatcher) Execute(
	ctx context.Context,
	agentCode string,
	task *TaskContext,
	rawInput any,
	timeout time.Duration,
) (*ProcessedOutput, error) {
	started := time.Now()
	runtime, dispatchErr := d.resolve(agentCode)
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if !runtime.Config.Enabled {
		return nil, newDispatchError("agent_disabled",
			fmt.Sprintf("Agent %s is disabled", runtime.Config.Slug))
	}

	if !runtime.AdmissionSlots.TryAcquire() {
		return nil, newDispatchError("agent_overloaded", "Agent queue is full")
	}
	defer runtime.AdmissionSlots.Release()

	if dispatchErr := acquireSlot(ctx, runtime.ExecutionSlots,
		remainingBudget(started, timeout), "Agent execution slot"); dispatchErr != nil {
		return nil, dispatchErr
	}
	defer runtime.ExecutionSlots.Release()

	if dispatchErr := acquireSlot(ctx, d.globalExecutionSlots,
		remainingBudget(started, timeout), "global execution slot"); dispatchErr != nil {
		return nil, dispatchErr
	}
	defer d.globalExecutionSlots.Release()

	remaining := remainingBudget(started, timeout)
	if remaining <= 0 {
		return nil, newDispatchError("execution_timeout",
			"Agent deadline elapsed before execution")
	}

	output, err := d.executor.Execute(ctx, runtime, task, rawInput, remaining)
	if err != nil {
		// 执行器的错误码原样透出;Detail 用 Display 形式("code: detail"),
		// 与 rs 的 error.to_string() 一致。
		var execution *ExecutionError
		if errors.As(err, &execution) {
			return nil, newDispatchError(execution.Code, execution.Error())
		}
		return nil, newDispatchError("executor_failed", err.Error())
	}
	return output, nil
}

// resolve 定位要执行的 Agent。
//
// agentCode 命中即用;没命中时回落到默认 Agent —— 注意「给了 agentCode 但没命中」
// 与「没给 agentCode」走的是同一条回落路径。默认 Agent 配了却没注册要显式报错,
// 否则会静默降级成 agent_not_found,让运维分不清是配置写错还是任务写错。
func (d *AgentDispatcher) resolve(agentCode string) (*AgentRuntime, *DispatchError) {
	if slug, ok := normalizeAgentSlug(agentCode); ok {
		if runtime := d.registry.Get(slug); runtime != nil {
			return runtime, nil
		}
	}
	if d.defaultAgentSlug != "" {
		if runtime := d.registry.Get(d.defaultAgentSlug); runtime != nil {
			return runtime, nil
		}
		return nil, newDispatchError("agent_not_found",
			fmt.Sprintf("default Agent %s is not registered", d.defaultAgentSlug))
	}
	return nil, newDispatchError("agent_not_found",
		"task agentCode did not match a registered Agent and no default Agent is configured")
}

// normalizeAgentSlug 去空白并转小写;空值返回 ok=false。
func normalizeAgentSlug(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return strings.ToLower(value), true
}

// acquireSlot 在剩余预算内取一个许可。
//
// 预算已耗尽或等待超时都判 execution_timeout。rs 还有一条
// executor_unavailable "{label} is closed" 分支对应 tokio Semaphore 被 close();
// Go 的 semaphore 是无缓冲 channel 包装,没有关闭语义,该分支不可达 ——
// 错误码保留在文档里以便两侧对照。
func acquireSlot(ctx context.Context, slots *semaphore, budget time.Duration, label string) *DispatchError {
	if budget <= 0 {
		return newDispatchError("execution_timeout",
			fmt.Sprintf("deadline elapsed waiting for %s", label))
	}
	if !slots.AcquireTimeout(ctx, budget) {
		return newDispatchError("execution_timeout",
			fmt.Sprintf("deadline elapsed waiting for %s", label))
	}
	return nil
}

// remainingBudget 返回 timeout 扣掉已耗时后的剩余量(不会为负)。
func remainingBudget(started time.Time, timeout time.Duration) time.Duration {
	remaining := timeout - time.Since(started)
	if remaining < 0 {
		return 0
	}
	return remaining
}
