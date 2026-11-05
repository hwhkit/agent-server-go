// Package exec 定义执行端口：任何“可以干活”的后端（本机 CLI、托管 API、
// litellm 网关）实现 Executor，应用层通过它统一调度，不感知后端差异。
//
// 远端连接器（remote-pull）不实现本端口——它经由队列认领任务，
// 见 domain/connector。
package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Kind 是执行器种类（pi、codex、opencode、qwen、gemini、openhands、
// claude、deepseek、openai_agent、claude_managed、remote-pull 等）。
type Kind string

// KindRemotePull 是远端连接器队列拉取模式的固定 kind。
const KindRemotePull Kind = "remote-pull"

// Mode 是执行模式。
type Mode string

const (
	// ModeLocal 调用本机安装的 CLI 子进程。
	ModeLocal Mode = "local"
	// ModeGateway 经 litellm 网关 HTTP 直调。
	ModeGateway Mode = "gateway"
	// ModeManaged 由 OpenAI / Anthropic 托管 Agent 管理会话与上下文。
	ModeManaged Mode = "managed"
	// ModeRemotePull 任务进入 Pull 队列，由远端连接器认领执行（存量值为下划线形态）。
	ModeRemotePull Mode = "remote_pull"
)

// Valid 报告模式是否为已知值。
func (m Mode) Valid() bool {
	switch m {
	case ModeLocal, ModeGateway, ModeManaged, ModeRemotePull:
		return true
	}
	return false
}

// Binding 是「执行器种类 × 执行模式」的绑定值对象。
type Binding struct {
	Kind Kind
	Mode Mode
}

// Validate 校验绑定一致性：remote-pull 模式与 kind 必须互相匹配，
// 其余模式不得使用 remote-pull kind。
func (b Binding) Validate() error {
	if b.Kind == "" {
		return fmt.Errorf("executor kind is required")
	}
	if !b.Mode.Valid() {
		return fmt.Errorf("execution mode must be local, gateway, managed, or remote-pull")
	}
	if b.Mode == ModeRemotePull && b.Kind != KindRemotePull {
		return fmt.Errorf("mode %q requires kind %q, got %q", ModeRemotePull, KindRemotePull, b.Kind)
	}
	if b.Mode != ModeRemotePull && b.Kind == KindRemotePull {
		return fmt.Errorf("kind %q requires remote-pull mode", KindRemotePull)
	}
	return nil
}

// Remote 报告绑定是否为远端连接器队列模式（不走 Executor 端口）。
func (b Binding) Remote() bool { return b.Mode == ModeRemotePull }

// Task 是一次执行派发。
type Task struct {
	RunID         string
	AgentSlug     string
	AgentRevision int64
	PositionCode  string
	// Attempt 是第几次派发（1 起）。有副作用的执行方必须据此做幂等。
	Attempt int
	// Input 是任务载荷（bound-task v1 的 input 部分）。
	Input json.RawMessage
	// Deadline 是墙钟上限；到期后调度层不再接受结果。
	Deadline time.Time
}

// Usage 是 token 计量。JSON 键与平台 usage 信封一致（inputTokens/
// outputTokens/totalTokens），agent_runs.usage_json 以此格式落地。
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

// Normalize 在 TotalTokens 缺失时用输入+输出回填，保证计量恒正口径
// （对齐 bound-task 计量规则：上报 usage 时 total > 0）。
func (u Usage) Normalize() Usage {
	if u.TotalTokens == 0 && u.InputTokens+u.OutputTokens > 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

// Result 是一次成功执行的结果。
type Result struct {
	Output     json.RawMessage
	Usage      Usage
	DurationMS int64
}

// Failure 是分类后的执行失败。Retryable 决定调度层是否按重试策略重派。
// Message 不得包含凭据或供应商原始响应。
type Failure struct {
	Code      string
	Message   string
	Retryable bool
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }

// Executor 是执行端口。实现必须并发安全；ctx 携带任务级截止时间，
// 实现必须响应取消并清理子进程/在途请求。
//
// 失败返回 *Failure 以携带分类信息；其他 error 视为不可重试的内部错误。
type Executor interface {
	Execute(ctx context.Context, task Task) (Result, error)
}

// Registry 按绑定解析执行器。后端未配置或未注册时返回带原因的错误，
// 调度层据此把运行落为 failed(backend_unavailable)。
type Registry interface {
	ExecutorFor(binding Binding) (Executor, error)
}

// String 返回模式名。
func (m Mode) String() string { return string(m) }
