package mota

// Push、Pull 与动态 Agent 共用的任务执行后端。
// 对应 Rust 侧 src/task_execution.rs。

import (
	"context"
	"errors"
	"time"
)

// TaskExecutionBackend 是协议入口(Push / Pull / A2A)统一使用的执行后端。
//
// 对应 rs 的两变体枚举:
//   - Agent 模式:生产平台,按 agentCode / 默认 slug 分发到 Registry 中的 Agent;
//   - Processor 模式:平台关闭时的嵌入/测试模式,直接跑进程内处理器。
//
// Go 侧用「恰好一个字段非 nil」表达这个枚举。
type TaskExecutionBackend struct {
	dispatcher *AgentDispatcher
	processor  Processor
}

// NewAgentBackend 构造生产平台后端。
func NewAgentBackend(dispatcher *AgentDispatcher) *TaskExecutionBackend {
	return &TaskExecutionBackend{dispatcher: dispatcher}
}

// NewProcessorBackend 构造嵌入/测试后端。
func NewProcessorBackend(processor Processor) *TaskExecutionBackend {
	return &TaskExecutionBackend{processor: processor}
}

// IsAgentBackend 报告当前是否为平台分发模式。
func (b *TaskExecutionBackend) IsAgentBackend() bool { return b.dispatcher != nil }

// Execute 执行一次任务。
//
// Processor 模式**不消费 agentCode 与 timeout** —— 与 rs 一致:嵌入模式没有
// Registry 可以分发,也没有容量闸需要排队,超时由调用方(协议层)自己兜。
func (b *TaskExecutionBackend) Execute(
	ctx context.Context,
	agentCode string,
	task *TaskContext,
	rawInput any,
	timeout time.Duration,
) (*ProcessedOutput, error) {
	if b.dispatcher != nil {
		return b.dispatcher.Execute(ctx, agentCode, task, rawInput, timeout)
	}
	if b.processor == nil {
		return nil, errors.New("task execution backend has neither a dispatcher nor a processor")
	}
	return b.processor.Process(ctx, task)
}
