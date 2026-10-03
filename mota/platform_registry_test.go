package mota

import (
	"context"
	"sync"
	"testing"
	"time"
)

func registryAgent(slug string, concurrency, queue int) *AgentConfig {
	return &AgentConfig{
		Slug: slug, Name: slug, Enabled: true,
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeGateway,
		MaxConcurrency: concurrency, QueueCapacity: queue,
	}
}

// ---------------------------------------------------------------------------
// semaphore
// ---------------------------------------------------------------------------

func TestSemaphoreTryAcquireHonoursCapacity(t *testing.T) {
	sem := newSemaphore(2)
	if !sem.TryAcquire() || !sem.TryAcquire() {
		t.Fatal("the first two acquisitions must succeed")
	}
	if sem.TryAcquire() {
		t.Error("acquiring beyond capacity must fail")
	}
	sem.Release()
	if !sem.TryAcquire() {
		t.Error("a released permit must become available again")
	}
}

// 容量下限为 1:配置成 0 或负数也不能变成「永不放行」。
func TestSemaphoreClampsCapacityToAtLeastOne(t *testing.T) {
	for _, permits := range []int{0, -5} {
		sem := newSemaphore(permits)
		if !sem.TryAcquire() {
			t.Errorf("newSemaphore(%d) must still allow one permit", permits)
		}
		if sem.TryAcquire() {
			t.Errorf("newSemaphore(%d) must cap at one permit", permits)
		}
	}
}

func TestSemaphoreAcquireBlocksUntilRelease(t *testing.T) {
	sem := newSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatal("first acquire failed")
	}
	acquired := make(chan bool, 1)
	go func() { acquired <- sem.Acquire(context.Background()) }()

	select {
	case <-acquired:
		t.Fatal("Acquire must block while the semaphore is full")
	case <-time.After(50 * time.Millisecond):
	}

	sem.Release()
	select {
	case ok := <-acquired:
		if !ok {
			t.Error("Acquire should succeed once a permit is released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not wake up after Release")
	}
}

func TestSemaphoreAcquireRespectsContextCancellation(t *testing.T) {
	sem := newSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatal("first acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sem.Acquire(ctx) {
		t.Error("Acquire must fail when the context is already cancelled")
	}
}

// AcquireTimeout:预算 <= 0 直接失败(对应 rs 的「剩余预算为零即判超时」)。
func TestSemaphoreAcquireTimeoutRejectsZeroBudget(t *testing.T) {
	sem := newSemaphore(1)
	for _, budget := range []time.Duration{0, -time.Second} {
		if !sem.AcquireTimeout(context.Background(), time.Second) {
			t.Fatal("setup acquire failed")
		}
		if sem.AcquireTimeout(context.Background(), budget) {
			t.Errorf("budget %s must fail immediately", budget)
		}
		sem.Release()
	}
}

func TestSemaphoreAcquireTimeoutExpires(t *testing.T) {
	sem := newSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatal("first acquire failed")
	}
	started := time.Now()
	if sem.AcquireTimeout(context.Background(), 60*time.Millisecond) {
		t.Error("AcquireTimeout must fail when the budget elapses")
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Errorf("AcquireTimeout returned after %s, want it to wait out the budget", elapsed)
	}
}

// Release 在没有已获取许可时不得 panic,也不得凭空造出容量。
func TestSemaphoreReleaseWithoutAcquireIsSafe(t *testing.T) {
	sem := newSemaphore(1)
	sem.Release()
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("capacity should still be one")
	}
	if sem.TryAcquire() {
		t.Error("spurious releases must not add capacity")
	}
}

func TestSemaphoreIsConcurrencySafe(t *testing.T) {
	const permits = 4
	sem := newSemaphore(permits)
	var mu sync.Mutex
	active, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sem.Acquire(context.Background()) {
				return
			}
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			sem.Release()
		}()
	}
	wg.Wait()
	if peak > permits {
		t.Errorf("peak concurrency = %d, want <= %d", peak, permits)
	}
}

// ---------------------------------------------------------------------------
// AgentRuntime 容量
// ---------------------------------------------------------------------------

func TestAgentRuntimeCapacityMath(t *testing.T) {
	runtime := newAgentRuntime(registryAgent("cap", 3, 7))

	// 执行槽 = max_concurrency。
	for i := 0; i < 3; i++ {
		if !runtime.ExecutionSlots.TryAcquire() {
			t.Fatalf("execution slot %d should be available", i)
		}
	}
	if runtime.ExecutionSlots.TryAcquire() {
		t.Error("execution slots must cap at max_concurrency")
	}

	// 准入槽 = max_concurrency + queue_capacity。
	for i := 0; i < 10; i++ {
		if !runtime.AdmissionSlots.TryAcquire() {
			t.Fatalf("admission slot %d should be available", i)
		}
	}
	if runtime.AdmissionSlots.TryAcquire() {
		t.Error("admission slots must cap at max_concurrency + queue_capacity")
	}
}

func TestAgentRuntimeCapacityFloors(t *testing.T) {
	runtime := newAgentRuntime(registryAgent("floor", 0, 0))
	if !runtime.ExecutionSlots.TryAcquire() {
		t.Error("execution capacity must be at least 1")
	}
	if !runtime.AdmissionSlots.TryAcquire() {
		t.Error("admission capacity must be at least 1")
	}
}

// ---------------------------------------------------------------------------
// AgentRegistry
// ---------------------------------------------------------------------------

func TestRegistryGetAndRemove(t *testing.T) {
	registry := NewAgentRegistry([]*AgentConfig{
		registryAgent("alpha", 1, 1),
		registryAgent("beta", 1, 1),
	})
	if registry.Len() != 2 {
		t.Errorf("Len = %d, want 2", registry.Len())
	}
	if registry.IsEmpty() {
		t.Error("IsEmpty must be false")
	}
	if registry.Get("alpha") == nil {
		t.Error("alpha should be registered")
	}
	if registry.Get("missing") != nil {
		t.Error("an unknown slug must return nil")
	}
	if !registry.Remove("alpha") {
		t.Error("Remove should report true for an existing slug")
	}
	if registry.Remove("alpha") {
		t.Error("Remove should report false the second time")
	}
	if registry.Get("alpha") != nil {
		t.Error("alpha should be gone")
	}
	if registry.Len() != 1 {
		t.Errorf("Len = %d, want 1", registry.Len())
	}
}

func TestRegistryEnabledLen(t *testing.T) {
	disabled := registryAgent("disabled", 1, 1)
	disabled.Enabled = false
	registry := NewAgentRegistry([]*AgentConfig{registryAgent("enabled", 1, 1), disabled})
	if registry.Len() != 2 {
		t.Errorf("Len = %d, want 2", registry.Len())
	}
	if registry.EnabledLen() != 1 {
		t.Errorf("EnabledLen = %d, want 1", registry.EnabledLen())
	}
}

// 容量参数不变的热更新必须复用信号量,否则改提示词会瞬时放大容量。
func TestRegistryReplacePreservesPermitsWhenCapacityUnchanged(t *testing.T) {
	registry := NewAgentRegistry([]*AgentConfig{registryAgent("hot", 2, 3)})
	before := registry.Get("hot")

	// 占满执行槽。
	if !before.ExecutionSlots.TryAcquire() || !before.ExecutionSlots.TryAcquire() {
		t.Fatal("failed to fill execution slots")
	}

	updated := registryAgent("hot", 2, 3)
	updated.SystemPrompt = "changed prompt"
	registry.Replace(updated)

	after := registry.Get("hot")
	if after.ExecutionSlots != before.ExecutionSlots {
		t.Error("execution semaphore must be reused when capacity is unchanged")
	}
	if after.AdmissionSlots != before.AdmissionSlots {
		t.Error("admission semaphore must be reused when capacity is unchanged")
	}
	if after.ExecutionSlots.TryAcquire() {
		t.Error("in-flight permits must survive a hot config update")
	}
	if after.Config.SystemPrompt != "changed prompt" {
		t.Error("the new config must be visible")
	}
}

// 容量参数变化时重建信号量。
func TestRegistryReplaceRebuildsPermitsWhenCapacityChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
		queue       int
	}{
		{"max_concurrency changed", 4, 3},
		{"queue_capacity changed", 2, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewAgentRegistry([]*AgentConfig{registryAgent("hot", 2, 3)})
			before := registry.Get("hot")
			if !before.ExecutionSlots.TryAcquire() || !before.ExecutionSlots.TryAcquire() {
				t.Fatal("failed to fill execution slots")
			}
			registry.Replace(registryAgent("hot", tc.concurrency, tc.queue))
			after := registry.Get("hot")
			if after.ExecutionSlots == before.ExecutionSlots {
				t.Error("execution semaphore must be rebuilt when capacity changes")
			}
			if !after.ExecutionSlots.TryAcquire() {
				t.Error("a rebuilt semaphore must start empty")
			}
		})
	}
}

// Replace 也用于新增。
func TestRegistryReplaceInsertsUnknownSlug(t *testing.T) {
	registry := NewAgentRegistry(nil)
	if !registry.IsEmpty() {
		t.Fatal("a fresh registry must be empty")
	}
	registry.Replace(registryAgent("new", 1, 1))
	if registry.Get("new") == nil {
		t.Error("Replace must insert an unknown slug")
	}
	if registry.Len() != 1 {
		t.Errorf("Len = %d, want 1", registry.Len())
	}
}

func TestRegistryReplaceAllSwapsContents(t *testing.T) {
	registry := NewAgentRegistry([]*AgentConfig{registryAgent("old", 2, 2)})
	before := registry.Get("old")
	if !before.ExecutionSlots.TryAcquire() {
		t.Fatal("failed to take a permit")
	}

	registry.ReplaceAll([]*AgentConfig{registryAgent("fresh-a", 1, 1), registryAgent("fresh-b", 1, 1)})
	if registry.Get("old") != nil {
		t.Error("ReplaceAll must drop agents that are no longer present")
	}
	if registry.Len() != 2 {
		t.Errorf("Len = %d, want 2", registry.Len())
	}
	if registry.Get("fresh-a") == nil || registry.Get("fresh-b") == nil {
		t.Error("ReplaceAll must install the new agents")
	}
}

// 多实例快照刷新在容量不变时必须复用信号量，避免在途任务与新任务各拿一套许可。
func TestRegistryReplaceAllPreservesPermitsWhenCapacityIsStable(t *testing.T) {
	registry := NewAgentRegistry([]*AgentConfig{registryAgent("same", 1, 1)})
	before := registry.Get("same")
	if !before.ExecutionSlots.TryAcquire() {
		t.Fatal("failed to take a permit")
	}
	registry.ReplaceAll([]*AgentConfig{registryAgent("same", 1, 1)})
	after := registry.Get("same")
	if after.ExecutionSlots != before.ExecutionSlots {
		t.Error("ReplaceAll must preserve semaphores when capacity is unchanged")
	}
	if after.ExecutionSlots.TryAcquire() {
		t.Error("the in-flight permit must remain occupied after refresh")
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	registry := NewAgentRegistry([]*AgentConfig{registryAgent("shared", 2, 2)})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				registry.Replace(registryAgent("shared", 2, 2))
			case 1:
				_ = registry.Get("shared")
			case 2:
				_ = registry.Len()
			default:
				_ = registry.EnabledLen()
			}
		}(i)
	}
	wg.Wait()
	if registry.Get("shared") == nil {
		t.Error("the agent should still be registered")
	}
}
