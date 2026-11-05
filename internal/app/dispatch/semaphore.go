// Package dispatch 提供运行分发用例：准入阶梯（准入闸 → Agent 执行闸 →
// 全局执行闸）、本机/托管执行引擎调用、远端连接器队列路由，以及统一的
// 分发错误分类。
//
// 本包只依赖 domain（端口与值对象）与标准库；执行引擎经 Engine 端口注入
// （infra/execengine 的 AgentExecutor 结构上满足），队列经 connector.TaskQueue
// 端口注入（infra/postgres 实现）。
package dispatch

import (
	"context"
	"fmt"
	"time"
)

// semaphore 是许可闸。无关闭语义（与旧栈一致；rs 侧 close 分支在 Go 不可达）。
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

// AcquireTimeout 在 budget 内等待一个许可。budget <= 0 时直接失败，
// 与「剩余预算为零即判 execution_timeout」的既有口径一致。
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
func (s *semaphore) Release() { <-s.slots }

// acquireSlot 在剩余预算内取一个许可；失败一律判 execution_timeout。
func acquireSlot(ctx context.Context, slots *semaphore, budget time.Duration, label string) *Error {
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
