package mota

// Agent 注册表与每 Agent 容量。对应 Rust 侧 src/platform/registry.rs。

import (
	"context"
	"sync"
	"time"
)

// semaphore 是计数信号量,支持非阻塞获取与带 deadline 的阻塞获取。
// Rust 侧用 tokio::sync::Semaphore 的 try_acquire_owned / acquire_owned 两种取法,
// 这里用带缓冲 channel 等价实现。
type semaphore struct{ slots chan struct{} }

func newSemaphore(permits int) *semaphore {
	if permits < 1 {
		permits = 1
	}
	return &semaphore{slots: make(chan struct{}, permits)}
}

// TryAcquire 非阻塞获取一个许可。
func (s *semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire 在 ctx 结束前阻塞等待一个许可。ctx 先结束则返回 false。
func (s *semaphore) Acquire(ctx context.Context) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// AcquireTimeout 在 budget 内等待一个许可。budget <= 0 时直接失败,
// 与 Rust 侧「剩余预算为零即判 execution_timeout」一致。
func (s *semaphore) AcquireTimeout(ctx context.Context, budget time.Duration) bool {
	if budget <= 0 {
		return false
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Release 归还一个许可。
func (s *semaphore) Release() {
	select {
	case <-s.slots:
	default:
	}
}

// AgentRuntime 是一个 Agent 的活动配置与容量许可。
type AgentRuntime struct {
	Config *AgentConfig
	// ExecutionSlots 限制并发执行中的任务数。
	ExecutionSlots *semaphore
	// AdmissionSlots 限制「执行中 + 排队中」的任务总数。
	AdmissionSlots *semaphore
}

func newAgentRuntime(config *AgentConfig) *AgentRuntime {
	executionCapacity := config.MaxConcurrency
	if executionCapacity < 1 {
		executionCapacity = 1
	}
	admissionCapacity := executionCapacity + config.QueueCapacity
	if admissionCapacity < 1 {
		admissionCapacity = 1
	}
	return &AgentRuntime{
		Config:         config,
		ExecutionSlots: newSemaphore(executionCapacity),
		AdmissionSlots: newSemaphore(admissionCapacity),
	}
}

// AgentRegistry 按 slug(小写)保存活动 Agent 运行态。
type AgentRegistry struct {
	mu     sync.RWMutex
	agents map[string]*AgentRuntime
}

func NewAgentRegistry(agents []*AgentConfig) *AgentRegistry {
	registry := &AgentRegistry{agents: make(map[string]*AgentRuntime, len(agents))}
	for _, agent := range agents {
		registry.agents[agent.Slug] = newAgentRuntime(agent)
	}
	return registry
}

func (r *AgentRegistry) Get(slug string) *AgentRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[slug]
}

// Replace 热替换一个 Agent 的配置。容量参数未变时复用原有信号量,
// 否则热更新提示词/密钥会让容量瞬时翻倍。
func (r *AgentRegistry) Replace(agent *AgentConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.agents[agent.Slug]; existing != nil &&
		existing.Config.MaxConcurrency == agent.MaxConcurrency &&
		existing.Config.QueueCapacity == agent.QueueCapacity {
		r.agents[agent.Slug] = &AgentRuntime{
			Config:         agent,
			ExecutionSlots: existing.ExecutionSlots,
			AdmissionSlots: existing.AdmissionSlots,
		}
		return
	}
	r.agents[agent.Slug] = newAgentRuntime(agent)
}

func (r *AgentRegistry) Remove(slug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.agents[slug]
	delete(r.agents, slug)
	return existed
}

// ReplaceAll 整体替换注册表内容。容量参数不变的 Agent 复用已有信号量，
// 避免多实例刷新时让在途任务与新任务各持一套容量、瞬时突破上限。
func (r *AgentRegistry) ReplaceAll(agents []*AgentConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]*AgentRuntime, len(agents))
	for _, agent := range agents {
		if existing := r.agents[agent.Slug]; existing != nil &&
			existing.Config.MaxConcurrency == agent.MaxConcurrency &&
			existing.Config.QueueCapacity == agent.QueueCapacity {
			next[agent.Slug] = &AgentRuntime{
				Config: agent, ExecutionSlots: existing.ExecutionSlots,
				AdmissionSlots: existing.AdmissionSlots,
			}
			continue
		}
		next[agent.Slug] = newAgentRuntime(agent)
	}
	r.agents = next
}

func (r *AgentRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}

func (r *AgentRegistry) IsEmpty() bool { return r.Len() == 0 }

func (r *AgentRegistry) EnabledLen() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, runtime := range r.agents {
		if runtime.Config.Enabled {
			count++
		}
	}
	return count
}
