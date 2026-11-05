package dynamic

// 容量闸原语（与 app/dispatch 同构；迁移期各自内聚，P6 之后如仍重复再抽公共包）。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type semaphore struct{ slots chan struct{} }

func newSemaphore(permits int) *semaphore {
	if permits < 1 {
		permits = 1
	}
	return &semaphore{slots: make(chan struct{}, permits)}
}

func (s *semaphore) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

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

func (s *semaphore) Release() { <-s.slots }

// slotEntry 是一个智能体的双闸（准入 + 执行）。配置容量变化时整组重建，
// 在途任务继续持有旧闸（释放归还到获取时的闸）。
type slotEntry struct {
	admission *semaphore
	execution *semaphore
	maxConc   int
	queueCap  int
}

type slotRegistry struct {
	mu      sync.Mutex
	entries map[string]*slotEntry
}

func (r *slotRegistry) get(slug string, maxConc, queueCap int) *slotEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]*slotEntry{}
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
	entry := &slotEntry{
		admission: newSemaphore(admissionCapacity),
		execution: newSemaphore(executionCapacity),
		maxConc:   maxConc,
		queueCap:  queueCap,
	}
	r.entries[slug] = entry
	return entry
}

// randomToken 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
