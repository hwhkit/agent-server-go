package mota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// dispatcherAgent 构造一个走进程内 Processor 的 Agent 配置(不 spawn 子进程)。
func dispatcherAgent(slug string, enabled bool, maxConcurrency, queueCapacity int) *AgentConfig {
	return &AgentConfig{
		Slug: slug, Name: slug, Enabled: enabled,
		ExecutorKind: ExecutorKindRust, ExecutionMode: ExecutionModeLocal,
		PermissionMode: "dontAsk", TimeoutSecs: 600,
		MaxConcurrency: maxConcurrency, QueueCapacity: queueCapacity, Revision: 1,
	}
}

// newTestDispatcher 组装一个分发器,并返回它与 Registry 以便测试直接抢占许可。
func newTestDispatcher(
	t *testing.T,
	agents []*AgentConfig,
	globalPermits int,
	defaultSlug string,
	processor Processor,
) (*AgentDispatcher, *AgentRegistry, *semaphore) {
	t.Helper()
	registry := NewAgentRegistry(agents)
	executor := newTestExecutor(t, executorTestConfig(t), processor)
	global := newSemaphore(globalPermits)
	return NewAgentDispatcher(registry, executor, global, defaultSlug), registry, global
}

// wantDispatchCode 断言 err 是带指定 Code 的 *DispatchError。
func wantDispatchCode(t *testing.T, err error, code string) *DispatchError {
	t.Helper()
	var dispatchErr *DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("expected a *DispatchError, got %T: %v", err, err)
	}
	if dispatchErr.Code != code {
		t.Fatalf("code = %q, want %q (detail: %s)", dispatchErr.Code, code, dispatchErr.Detail)
	}
	return dispatchErr
}

// ---------------------------------------------------------------------------
// 单元:normalizeAgentSlug / remainingBudget
// ---------------------------------------------------------------------------

func TestNormalizeAgentSlug(t *testing.T) {
	t.Parallel()
	accepted := map[string]string{
		"local-pi":       "local-pi",
		"  Local-Pi  ":   "local-pi",
		"LOCAL-PI":       "local-pi",
		"\tresearch\n":   "research",
		"Mixed-Case-123": "mixed-case-123",
	}
	for input, want := range accepted {
		got, ok := normalizeAgentSlug(input)
		if !ok || got != want {
			t.Errorf("%q => (%q, %v), want (%q, true)", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "   ", "\t\n"} {
		if _, ok := normalizeAgentSlug(input); ok {
			t.Errorf("%q must be rejected", input)
		}
	}
}

func TestRemainingBudget(t *testing.T) {
	t.Parallel()
	// 刚开始时几乎等于原始预算。
	started := time.Now()
	if got := remainingBudget(started, time.Minute); got > time.Minute || got < 59*time.Second {
		t.Errorf("remaining = %v, want close to a minute", got)
	}
	// 已经超时的情况必须钳到 0(不能是负数,否则下游的 <=0 判断会错乱)。
	if got := remainingBudget(time.Now().Add(-time.Hour), time.Minute); got != 0 {
		t.Errorf("remaining = %v, want 0", got)
	}
	if got := remainingBudget(time.Now(), 0); got > 0 {
		t.Errorf("remaining = %v, want 0 for a zero budget", got)
	}
}

// ---------------------------------------------------------------------------
// resolve:agentCode 与默认 Agent
// ---------------------------------------------------------------------------

func TestDispatcherResolvesAgentCode(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("from research")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("research", true, 2, 4)}, 4, "", processor)

	// slug 命中时大小写与首尾空白都不敏感。
	for _, code := range []string{"research", "RESEARCH", "  Research  "} {
		out, err := dispatcher.Execute(context.Background(), code, &TaskContext{}, map[string]any{}, time.Minute)
		if err != nil {
			t.Fatalf("code %q: %v", code, err)
		}
		if out.Summary != "from research" {
			t.Errorf("code %q: summary = %#v", code, out.Summary)
		}
	}
	if processor.callCount() != 3 {
		t.Errorf("processor calls = %d, want 3", processor.callCount())
	}
}

// 没给 agentCode、或给了但没命中,都回落到默认 Agent。
func TestDispatcherFallsBackToDefaultAgent(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("from default")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("fallback", true, 2, 4)}, 4, "fallback", processor)

	for _, code := range []string{"", "   ", "no-such-agent"} {
		out, err := dispatcher.Execute(context.Background(), code, &TaskContext{}, map[string]any{}, time.Minute)
		if err != nil {
			t.Fatalf("code %q: %v", code, err)
		}
		if out.Summary != "from default" {
			t.Errorf("code %q: summary = %#v", code, out.Summary)
		}
	}
}

// 默认 Agent 配了却没注册要显式报错,不能静默降级成通用的 not-found 文案。
func TestDispatcherReportsUnregisteredDefaultAgent(t *testing.T) {
	t.Parallel()
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("other", true, 1, 1)}, 4, "missing-default", &stubProcessor{})
	_, err := dispatcher.Execute(context.Background(), "", &TaskContext{}, map[string]any{}, time.Minute)
	dispatchErr := wantDispatchCode(t, err, "agent_not_found")
	if dispatchErr.Detail != "default Agent missing-default is not registered" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
}

// 既没命中也没有默认 Agent。
func TestDispatcherReportsNoUsableAgent(t *testing.T) {
	t.Parallel()
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("other", true, 1, 1)}, 4, "", &stubProcessor{})
	for _, code := range []string{"", "no-such-agent"} {
		_, err := dispatcher.Execute(context.Background(), code, &TaskContext{}, map[string]any{}, time.Minute)
		dispatchErr := wantDispatchCode(t, err, "agent_not_found")
		want := "task agentCode did not match a registered Agent and no default Agent is configured"
		if dispatchErr.Detail != want {
			t.Errorf("code %q: detail = %q", code, dispatchErr.Detail)
		}
	}
}

// 停用的 Agent 不执行 —— 即使 agentCode 精确命中。
func TestDispatcherRejectsDisabledAgent(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("should not run")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("paused", false, 2, 4)}, 4, "", processor)
	_, err := dispatcher.Execute(context.Background(), "paused", &TaskContext{}, map[string]any{}, time.Minute)
	dispatchErr := wantDispatchCode(t, err, "agent_disabled")
	if dispatchErr.Detail != "Agent paused is disabled" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
	if processor.callCount() != 0 {
		t.Error("a disabled Agent must not reach the executor")
	}
}

// 停用检查发生在准入闸之前:不该白占一个准入许可。
func TestDispatcherDisabledCheckPrecedesAdmission(t *testing.T) {
	t.Parallel()
	dispatcher, registry, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("paused", false, 1, 0)}, 4, "", &stubProcessor{})
	runtime := registry.Get("paused")
	for i := 0; i < 3; i++ {
		_, err := dispatcher.Execute(context.Background(), "paused", &TaskContext{}, map[string]any{}, time.Minute)
		wantDispatchCode(t, err, "agent_disabled")
	}
	// 准入闸从未被占用过,现在应该还能拿到许可。
	if !runtime.AdmissionSlots.TryAcquire() {
		t.Error("admission slots leaked on the disabled path")
	}
	runtime.AdmissionSlots.Release()
}

// ---------------------------------------------------------------------------
// 准入阶梯
// ---------------------------------------------------------------------------

// 准入闸是**非阻塞**的:满了立刻 agent_overloaded,不排队白等掉整个预算。
func TestDispatcherRejectsWhenAdmissionFull(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok")}
	// MaxConcurrency=1 且 QueueCapacity=0 → 准入容量 1。
	dispatcher, registry, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("busy", true, 1, 0)}, 4, "", processor)
	runtime := registry.Get("busy")
	if !runtime.AdmissionSlots.TryAcquire() {
		t.Fatal("could not pre-acquire the admission slot")
	}
	defer runtime.AdmissionSlots.Release()

	started := time.Now()
	_, err := dispatcher.Execute(context.Background(), "busy", &TaskContext{}, map[string]any{}, 30*time.Second)
	dispatchErr := wantDispatchCode(t, err, "agent_overloaded")
	if dispatchErr.Detail != "Agent queue is full" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
	// 必须立刻失败,不能等 30s。
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("admission rejection took %v, want it immediate", elapsed)
	}
	if processor.callCount() != 0 {
		t.Error("an overloaded Agent must not reach the executor")
	}
}

// 该 Agent 的执行闸被占满时在剩余预算内等待,超时判 execution_timeout。
func TestDispatcherTimesOutWaitingForAgentExecutionSlot(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok")}
	// 准入容量 = 1 + 4 = 5,够用;执行容量 1,被外部占住。
	dispatcher, registry, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("serial", true, 1, 4)}, 4, "", processor)
	runtime := registry.Get("serial")
	if !runtime.ExecutionSlots.TryAcquire() {
		t.Fatal("could not pre-acquire the execution slot")
	}
	defer runtime.ExecutionSlots.Release()

	started := time.Now()
	_, err := dispatcher.Execute(context.Background(), "serial", &TaskContext{}, map[string]any{}, 120*time.Millisecond)
	dispatchErr := wantDispatchCode(t, err, "execution_timeout")
	if dispatchErr.Detail != "deadline elapsed waiting for Agent execution slot" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("waited %v, want it to give up near the deadline", elapsed)
	}
	if processor.callCount() != 0 {
		t.Error("the executor must not run when the slot was never acquired")
	}
	// 准入许可要归还,否则后续请求会莫名 agent_overloaded。
	if !runtime.AdmissionSlots.TryAcquire() {
		t.Error("admission slots leaked on the execution-slot timeout path")
	}
	runtime.AdmissionSlots.Release()
}

// 全局闸被占满时同样判 execution_timeout,并归还前两道许可。
func TestDispatcherTimesOutWaitingForGlobalSlot(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok")}
	dispatcher, registry, global := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("gated", true, 2, 4)}, 1, "", processor)
	if !global.TryAcquire() {
		t.Fatal("could not pre-acquire the global slot")
	}
	defer global.Release()

	_, err := dispatcher.Execute(context.Background(), "gated", &TaskContext{}, map[string]any{}, 120*time.Millisecond)
	dispatchErr := wantDispatchCode(t, err, "execution_timeout")
	if dispatchErr.Detail != "deadline elapsed waiting for global execution slot" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
	if processor.callCount() != 0 {
		t.Error("the executor must not run without the global slot")
	}
	runtime := registry.Get("gated")
	if !runtime.AdmissionSlots.TryAcquire() || !runtime.ExecutionSlots.TryAcquire() {
		t.Error("slots leaked on the global-slot timeout path")
	}
}

// 预算一开始就为零时,连许可都不去等。
func TestDispatcherRejectsZeroBudgetBeforeWaiting(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("quick", true, 2, 4)}, 4, "", processor)
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := dispatcher.Execute(context.Background(), "quick", &TaskContext{}, map[string]any{}, timeout)
		dispatchErr := wantDispatchCode(t, err, "execution_timeout")
		if dispatchErr.Detail != "deadline elapsed waiting for Agent execution slot" {
			t.Errorf("timeout %v: detail = %q", timeout, dispatchErr.Detail)
		}
	}
	if processor.callCount() != 0 {
		t.Error("a zero budget must not reach the executor")
	}
}

// ctx 取消时不再干等许可。
func TestDispatcherHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	dispatcher, registry, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("cancelled", true, 1, 4)}, 4, "", &stubProcessor{})
	runtime := registry.Get("cancelled")
	if !runtime.ExecutionSlots.TryAcquire() {
		t.Fatal("could not pre-acquire the execution slot")
	}
	defer runtime.ExecutionSlots.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := dispatcher.Execute(ctx, "cancelled", &TaskContext{}, map[string]any{}, 30*time.Second)
	wantDispatchCode(t, err, "execution_timeout")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("waited %v after cancellation, want it immediate", elapsed)
	}
}

// 全部许可在成功路径上也要归还,否则第二次请求就会卡住。
func TestDispatcherReleasesSlotsOnSuccess(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok")}
	dispatcher, registry, global := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("serial", true, 1, 0)}, 1, "", processor)
	// 串行跑三次:任何一道闸泄漏都会让第二次 agent_overloaded 或超时。
	for i := 0; i < 3; i++ {
		if _, err := dispatcher.Execute(context.Background(), "serial",
			&TaskContext{}, map[string]any{}, 5*time.Second); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if processor.callCount() != 3 {
		t.Errorf("processor calls = %d, want 3", processor.callCount())
	}
	runtime := registry.Get("serial")
	if !runtime.AdmissionSlots.TryAcquire() || !runtime.ExecutionSlots.TryAcquire() || !global.TryAcquire() {
		t.Error("slots leaked on the success path")
	}
}

// 执行失败时同样要归还许可。
func TestDispatcherReleasesSlotsOnExecutorFailure(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{err: errors.New("boom")}
	dispatcher, registry, global := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("failing", true, 1, 0)}, 1, "", processor)
	for i := 0; i < 3; i++ {
		_, err := dispatcher.Execute(context.Background(), "failing",
			&TaskContext{}, map[string]any{}, 5*time.Second)
		wantDispatchCode(t, err, "processor_failed")
	}
	runtime := registry.Get("failing")
	if !runtime.AdmissionSlots.TryAcquire() || !runtime.ExecutionSlots.TryAcquire() || !global.TryAcquire() {
		t.Error("slots leaked on the failure path")
	}
}

// ---------------------------------------------------------------------------
// 执行器错误码透传
// ---------------------------------------------------------------------------

// 执行器的错误码原样透出,Detail 用 "code: detail" 的 Display 形式。
func TestDispatcherPassesThroughExecutorErrorCodes(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{err: errors.New("analysis blew up")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("failing", true, 2, 4)}, 4, "", processor)
	_, err := dispatcher.Execute(context.Background(), "failing", &TaskContext{}, map[string]any{}, time.Minute)
	dispatchErr := wantDispatchCode(t, err, "processor_failed")
	if dispatchErr.Detail != "processor_failed: analysis blew up" {
		t.Errorf("detail = %q, want the Display form", dispatchErr.Detail)
	}
}

// 不支持的执行器组合走 executor_unavailable,同样是透传而非分发层自造。
func TestDispatcherPassesThroughExecutorUnavailable(t *testing.T) {
	t.Parallel()
	unsupported := dispatcherAgent("goose-local", true, 2, 4)
	unsupported.ExecutorKind = ExecutorKindGoose
	unsupported.ExecutionMode = ExecutionModeLocal
	dispatcher, _, _ := newTestDispatcher(t, []*AgentConfig{unsupported}, 4, "", &stubProcessor{})
	_, err := dispatcher.Execute(context.Background(), "goose-local", &TaskContext{}, map[string]any{}, time.Minute)
	dispatchErr := wantDispatchCode(t, err, "executor_unavailable")
	if dispatchErr.Detail != "executor_unavailable: goose does not support local execution" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
}

// 执行器自身超时的码也照原样透出(不会被分发层的超时文案盖掉)。
func TestDispatcherPassesThroughExecutorTimeout(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("late"), delay: 5 * time.Second}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("slow", true, 2, 4)}, 4, "", processor)
	_, err := dispatcher.Execute(context.Background(), "slow", &TaskContext{}, map[string]any{}, 120*time.Millisecond)
	dispatchErr := wantDispatchCode(t, err, "execution_timeout")
	if dispatchErr.Detail != "execution_timeout: Rust processor exceeded its deadline" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
}

// 分发层要把剩余预算(而不是原始预算)交给执行器。
func TestDispatcherPassesRemainingBudgetToExecutor(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("late"), delay: 5 * time.Second}
	dispatcher, registry, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("shrinking", true, 1, 4)}, 4, "", processor)
	runtime := registry.Get("shrinking")
	if !runtime.ExecutionSlots.TryAcquire() {
		t.Fatal("could not pre-acquire the execution slot")
	}
	// 200ms 后放掉执行许可,此时总预算 400ms 只剩约 200ms。
	go func() {
		time.Sleep(200 * time.Millisecond)
		runtime.ExecutionSlots.Release()
	}()

	started := time.Now()
	_, err := dispatcher.Execute(context.Background(), "shrinking", &TaskContext{}, map[string]any{}, 400*time.Millisecond)
	wantDispatchCode(t, err, "execution_timeout")
	// 如果错传了原始预算,总耗时会接近 600ms 而不是 400ms。
	if elapsed := time.Since(started); elapsed > 550*time.Millisecond {
		t.Errorf("total elapsed %v, want it bounded by the original 400ms budget", elapsed)
	}
}

// 并发压测:准入闸的容量上限必须被严格遵守,且不泄漏许可。
func TestDispatcherConcurrentAdmissionIsBounded(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("ok"), delay: 30 * time.Millisecond}
	dispatcher, registry, global := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("hot", true, 2, 2)}, 4, "", processor)

	const attempts = 24
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			_, err := dispatcher.Execute(context.Background(), "hot",
				&TaskContext{}, map[string]any{}, 5*time.Second)
			results <- err
		}()
	}
	succeeded, overloaded := 0, 0
	for i := 0; i < attempts; i++ {
		switch err := <-results; {
		case err == nil:
			succeeded++
		default:
			dispatchErr := wantDispatchCode(t, err, "agent_overloaded")
			_ = dispatchErr
			overloaded++
		}
	}
	if succeeded+overloaded != attempts {
		t.Fatalf("succeeded=%d overloaded=%d, want them to sum to %d", succeeded, overloaded, attempts)
	}
	if succeeded == 0 {
		t.Error("expected at least some requests to get through")
	}
	// 全部收敛后所有闸都应空闲。
	runtime := registry.Get("hot")
	for i := 0; i < 4; i++ {
		if !runtime.AdmissionSlots.TryAcquire() {
			t.Fatalf("admission slot %d leaked", i)
		}
	}
	for i := 0; i < 2; i++ {
		if !runtime.ExecutionSlots.TryAcquire() {
			t.Fatalf("execution slot %d leaked", i)
		}
	}
	for i := 0; i < 4; i++ {
		if !global.TryAcquire() {
			t.Fatalf("global slot %d leaked", i)
		}
	}
}
