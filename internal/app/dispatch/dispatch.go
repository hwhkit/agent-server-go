// Package dispatch 提供运行分发用例：准入阶梯（准入闸 → Agent 执行闸 →
// 全局执行闸）、本机/托管执行引擎调用、远端连接器队列路由，以及统一的
// 分发错误分类。
//
// 本包只依赖 domain（端口与值对象）与标准库；执行引擎经 Engine 端口注入
// （infra/execengine 的 AgentExecutor 结构上满足），队列经 connector.TaskQueue
// 端口注入（infra/postgres 实现）。容量闸原语与 dynamic 共用 app/gate。
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ir-agent-server/internal/app/gate"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
)

// Error 是一次分发失败。Code 是稳定的机器可读错误码，Detail 仅用于日志
// 与运行记录（对外暴露口径由 api 层决定）。
type Error struct {
	Code   string
	Detail string
}

// NewError 构造分发错误。
func NewError(code, detail string) *Error { return &Error{Code: code, Detail: detail} }

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

// Engine 是执行引擎端口。infra/execengine 的 *AgentExecutor 结构上满足。
type Engine interface {
	Execute(ctx context.Context, runtime *agent.ResolvedConfig, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// Resolver 按 slug 定位智能体配置快照；未命中返回 agent.ErrNotFound。
type Resolver interface {
	Resolve(ctx context.Context, slug string) (*agent.ResolvedConfig, error)
}

// RemotePolicy 是远端队列路由的策略参数（来自 AGENT_API 配置段）。
type RemotePolicy struct {
	// QueueTTL 是入队 → 首次派发的存活期。
	QueueTTL time.Duration
	// SyncHoldMax 是有界同步等待窗口上限。
	SyncHoldMax time.Duration
}

// Config 是 Service 的装配参数。
type Config struct {
	Resolver Resolver
	Engine   Engine
	// Queue 为 nil 时 remote_pull 模式不可用（AGENT_API 未启用）。
	Queue connector.TaskQueue
	// DefaultSlug 为空表示没有配置默认智能体。
	DefaultSlug          string
	Remote               RemotePolicy
	GlobalMaxConcurrency int
}

// Service 是分发用例服务。并发安全。
type Service struct {
	resolver    Resolver
	engine      Engine
	queue       connector.TaskQueue
	defaultSlug string
	remote      RemotePolicy
	globalSlots *gate.Semaphore
	slots       gate.Registry
}

// NewService 组装分发服务。
func NewService(cfg Config) *Service {
	return &Service{
		resolver:    cfg.Resolver,
		engine:      cfg.Engine,
		queue:       cfg.Queue,
		defaultSlug: strings.TrimSpace(strings.ToLower(cfg.DefaultSlug)),
		remote:      cfg.Remote,
		globalSlots: gate.NewSemaphore(cfg.GlobalMaxConcurrency),
	}
}

// acquireSlot 在剩余预算内取一个许可；失败一律判 execution_timeout。
func acquireSlot(ctx context.Context, slots *gate.Semaphore, budget time.Duration, label string) *Error {
	if !slots.AcquireTimeout(ctx, budget) {
		return NewError("execution_timeout",
			fmt.Sprintf("deadline elapsed waiting for %s", label))
	}
	return nil
}

// remainingBudget 返回 timeout 扣掉已耗时后的剩余量（不会为负）。
func remainingBudget(started time.Time, timeout time.Duration) time.Duration {
	remaining := timeout - time.Since(started)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Execute 分发并执行一次任务。失败时返回的 error 一定是 *Error。
//
// 准入阶梯的顺序是有意的（与既有实现一致）：
//  1. 准入闸（执行中 + 排队中）—— **非阻塞**，满了立刻判 agent_overloaded，
//     让过载请求快速失败而不是排在后面白等掉整个预算；
//  2. 该智能体的执行闸 —— 在剩余预算内阻塞等待；
//  3. 全局执行闸 —— 同样在剩余预算内阻塞等待。
//
// 每一步都用「原始预算减去已耗时」重新计算剩余时间，整条链路共享同一个
// 截止时刻。remote_pull 模式不走本机闸门，容量由队列 SQL 计数约束。
func (s *Service) Execute(
	ctx context.Context,
	agentCode string,
	task *platform.TaskContext,
	rawInput any,
	timeout time.Duration,
) (*platform.ProcessedOutput, error) {
	started := time.Now()
	config, dispatchErr := s.resolve(ctx, agentCode)
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if !config.Enabled {
		return nil, NewError("agent_disabled",
			fmt.Sprintf("Agent %s is disabled", config.Slug))
	}

	if config.ExecutionMode == exec.ModeRemotePull {
		if s.queue == nil {
			return nil, NewError("executor_unavailable",
				"Agent is configured as remote_pull but AGENT_API is disabled")
		}
		return s.executeRemote(ctx, config, task, timeout)
	}

	entry := s.slots.Get(config.Slug, config.MaxConcurrency, config.QueueCapacity)
	if !entry.Admission.TryAcquire() {
		return nil, NewError("agent_overloaded", "Agent queue is full")
	}
	defer entry.Admission.Release()

	if dispatchErr := acquireSlot(ctx, entry.Execution,
		remainingBudget(started, timeout), "Agent execution slot"); dispatchErr != nil {
		return nil, dispatchErr
	}
	defer entry.Execution.Release()

	if dispatchErr := acquireSlot(ctx, s.globalSlots,
		remainingBudget(started, timeout), "global execution slot"); dispatchErr != nil {
		return nil, dispatchErr
	}
	defer s.globalSlots.Release()

	remaining := remainingBudget(started, timeout)
	if remaining <= 0 {
		return nil, NewError("execution_timeout", "Agent deadline elapsed before execution")
	}

	output, err := s.engine.Execute(ctx, config, task, rawInput, remaining)
	if err != nil {
		// 执行器的错误码原样透出；Detail 用 Display 形式（"code: detail"）。
		var failure *exec.Failure
		if errors.As(err, &failure) {
			return nil, NewError(failure.Code, failure.Error())
		}
		var coded interface{ ErrorCode() string }
		if errors.As(err, &coded) {
			return nil, NewError(coded.ErrorCode(), err.Error())
		}
		return nil, NewError("executor_failed", err.Error())
	}
	return output, nil
}

// resolve 定位要执行的智能体。
//
// agentCode 命中即用；没命中时回落到默认智能体——「给了 agentCode 但没命中」
// 与「没给 agentCode」走同一条回落路径。默认智能体配了却没注册要显式报错，
// 否则会静默降级成 agent_not_found，让运维分不清是配置写错还是任务写错。
func (s *Service) resolve(ctx context.Context, agentCode string) (*agent.ResolvedConfig, *Error) {
	if slug, ok := normalizeAgentSlug(agentCode); ok {
		if config, err := s.resolver.Resolve(ctx, slug); err == nil && config != nil {
			return config, nil
		}
	}
	if s.defaultSlug != "" {
		config, err := s.resolver.Resolve(ctx, s.defaultSlug)
		if err == nil && config != nil {
			return config, nil
		}
		return nil, NewError("agent_not_found",
			fmt.Sprintf("default Agent %s is not registered", s.defaultSlug))
	}
	return nil, NewError("agent_not_found",
		"task agentCode did not match a registered Agent and no default Agent is configured")
}

// normalizeAgentSlug 去空白并转小写；空值返回 ok=false。
func normalizeAgentSlug(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return strings.ToLower(value), true
}

// executeRemote 把任务送入连接器队列并在有界窗口内同步等待。
// 超窗不取消任务：结果照常落 runs，调用方按 202/轮询语义处理。
func (s *Service) executeRemote(
	ctx context.Context,
	config *agent.ResolvedConfig,
	task *platform.TaskContext,
	timeout time.Duration,
) (*platform.ProcessedOutput, error) {
	runID := "run_" + randomToken(12)
	if task.RunID != nil && strings.TrimSpace(*task.RunID) != "" {
		runID = strings.TrimSpace(*task.RunID)
	}
	payload, err := json.Marshal(map[string]any{
		"taskBrief":      task.TaskBrief,
		"params":         task.Params,
		"positionType":   task.PositionType,
		"language":       task.Language,
		"symbolPool":     task.SymbolPool,
		"universeMode":   task.UniverseMode,
		"runtimeContext": task.RuntimeContext,
	})
	if err != nil {
		return nil, NewError("invalid_input", "encode remote task payload: "+err.Error())
	}
	err = s.queue.Enqueue(ctx, connector.NewTask{
		RunID:            runID,
		AgentSlug:        config.Slug,
		AgentRevision:    config.Revision,
		ExecutorKind:     string(config.ExecutorKind),
		Payload:          payload,
		ExecutionTimeout: config.Timeout(),
		QueueTTL:         s.remote.QueueTTL,
		AdmissionLimit:   config.MaxConcurrency + config.QueueCapacity,
	})
	if err != nil {
		if errors.Is(err, connector.ErrQueueFull) {
			return nil, NewError("agent_overloaded",
				fmt.Sprintf("Agent %s remote queue is full", config.Slug))
		}
		return nil, NewError("executor_unavailable", err.Error())
	}

	waitBudget := s.remote.SyncHoldMax
	if timeout > 0 && timeout < waitBudget {
		waitBudget = timeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitBudget)
	defer cancel()
	state, resultJSON, err := s.queue.WaitTerminal(waitCtx, runID)
	if err != nil {
		// 等待超窗/调用方断开：任务仍在远端执行，结果会落 runs。
		return nil, NewError("execution_timeout",
			fmt.Sprintf("remote task %s still running after sync hold window", runID))
	}
	return remoteResultToOutput(state, resultJSON)
}

// remoteCompleteDoc 是连接器 complete 结果文档（AGENT-PULL-API §3）。
type remoteCompleteDoc struct {
	Status  string          `json:"status"`
	Payload json.RawMessage `json:"payload"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
		TotalTokens  uint64 `json:"totalTokens"`
	} `json:"usage"`
}

// remoteResultToOutput 把 complete 结果文档映射回 ProcessedOutput。
func remoteResultToOutput(state connector.TaskState, resultJSON []byte) (*platform.ProcessedOutput, error) {
	var doc remoteCompleteDoc
	if err := json.Unmarshal(resultJSON, &doc); err != nil {
		return nil, NewError("invalid_executor_output", "remote result is not valid JSON")
	}
	if state != connector.TaskCompleted {
		code := "executor_failed"
		message := "remote execution failed"
		if doc.Error != nil {
			if doc.Error.Code != "" {
				code = doc.Error.Code
			}
			if doc.Error.Message != "" {
				message = doc.Error.Message
			}
		} else if state == connector.TaskCanceled {
			code, message = "aborted", "remote execution was canceled"
		}
		return nil, NewError(code, message)
	}
	output := &platform.ProcessedOutput{PayloadResult: json.RawMessage(doc.Payload)}
	if doc.Usage != nil {
		output.Usage = &platform.BoundRunUsage{
			SchemaVersion:    platform.SchemaUsageV2,
			Status:           "FINAL",
			PromptTokens:     doc.Usage.InputTokens,
			CompletionTokens: doc.Usage.OutputTokens,
		}
		if output.Usage.PromptTokens+output.Usage.CompletionTokens == 0 && doc.Usage.TotalTokens > 0 {
			output.Usage.PromptTokens = doc.Usage.TotalTokens
		}
	}
	return output, nil
}

// randomToken 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
