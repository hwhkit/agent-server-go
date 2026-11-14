// Package gate 提供执行容量闸原语：许可闸（Semaphore）与按智能体缓存的
// 准入/执行双闸注册表（Registry）。dispatch 与 dynamic 两个用例层共用；
// 此前迁移期两包各自内聚，P6 切换完成后按原 slots.go 注明的计划归并至此。
package gate

import (
	"context"
	"sync"
	"time"
)

// Semaphore 是许可闸。无关闭语义（与旧栈一致；rs 侧 close 分支在 Go 不可达）。
type Semaphore struct{ slots chan struct{} }

// NewSemaphore 返回 permits 容量的许可闸；permits < 1 时按 1 处理。
func NewSemaphore(permits int) *Semaphore {
	if permits < 1 {
		permits = 1
	}
	return &Semaphore{slots: make(chan struct{}, permits)}
}

// TryAcquire 非阻塞获取一个许可。
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// AcquireTimeout 在 budget 内等待一个许可。budget <= 0 时直接失败，
// 与「剩余预算为零即判 execution_timeout」的既有口径一致。
func (s *Semaphore) AcquireTimeout(ctx context.Context, budget time.Duration) bool {
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
func (s *Semaphore) Release() { <-s.slots }

// Entry 是一个智能体的双闸（准入 + 执行）。配置容量变化时整组重建，
// 在途任务继续持有旧闸（与旧栈注册表收敛语义一致：释放归还到获取时的闸）。
//
// maxConc/queueCap 记录创建时的容量配置，仅供 Registry 内部比较；Entry
// 本身必须经 Registry.Get 获取，不得跨包手工构造。
type Entry struct {
	Admission *Semaphore
	Execution *Semaphore

	maxConc  int
	queueCap int
}

// Registry 按键缓存 Entry。零值可用。
type Registry struct {
	mu      sync.Mutex
	entries map[string]*Entry
}

// Get 返回 slug 对应的双闸；容量配置变化时整组重建（在途任务继续持有
// 旧闸），配置未变时复用既有闸。
func (r *Registry) Get(slug string, maxConc, queueCap int) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]*Entry{}
	}
	if entry, ok := r.entries[slug]; ok && entry.maxConc == maxConc && entry.queueCap == queueCap {
		return entry
	}
	executionCapacity := maxConc
	if executionCapacity < 1 {
		executionCapacity = 1
	}
	admissionCapacity := executionCapacity + queueCap
	if admissionCapacity < 1 {
		admissionCapacity = 1
	}
	entry := &Entry{
		Admission: NewSemaphore(admissionCapacity),
		Execution: NewSemaphore(executionCapacity),
		maxConc:   maxConc,
		queueCap:  queueCap,
	}
	r.entries[slug] = entry
	return entry
}
