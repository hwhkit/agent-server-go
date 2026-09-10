package mota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Processor 模式
// ---------------------------------------------------------------------------

// 嵌入模式直接跑进程内处理器,**不消费 agentCode 与 timeout**(与 rs 一致)。
func TestProcessorBackendIgnoresAgentCodeAndTimeout(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("embedded")}
	backend := NewProcessorBackend(processor)
	if backend.IsAgentBackend() {
		t.Error("a processor backend must not report itself as the agent backend")
	}
	// agentCode 随便给、预算给 0,都照样执行。
	for _, code := range []string{"", "anything", "  Mixed  "} {
		out, err := backend.Execute(context.Background(), code, &TaskContext{}, map[string]any{}, 0)
		if err != nil {
			t.Fatalf("code %q: %v", code, err)
		}
		if out.Summary != "embedded" {
			t.Errorf("code %q: summary = %#v", code, out.Summary)
		}
	}
	if processor.callCount() != 3 {
		t.Errorf("processor calls = %d, want 3", processor.callCount())
	}
}

// Processor 的错误原样上抛(不包成 DispatchError)。
func TestProcessorBackendPropagatesRawError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("processor said no")
	backend := NewProcessorBackend(&stubProcessor{err: sentinel})
	_, err := backend.Execute(context.Background(), "", &TaskContext{}, map[string]any{}, time.Minute)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the sentinel error unwrapped", err)
	}
	var dispatchErr *DispatchError
	if errors.As(err, &dispatchErr) {
		t.Error("the processor backend must not wrap errors as DispatchError")
	}
}

// TaskContext 原样透传给处理器。
func TestProcessorBackendPassesTaskContext(t *testing.T) {
	t.Parallel()
	var seen *TaskContext
	backend := NewProcessorBackend(processorFunc(func(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
		seen = task
		return PlaceholderOutput("ok"), nil
	}))
	runID := "run-42"
	task := &TaskContext{RunID: &runID, SymbolPool: []string{"AAPL.US"}}
	if _, err := backend.Execute(context.Background(), "", task, map[string]any{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if seen != task {
		t.Fatal("the processor must receive the caller's TaskContext")
	}
}

// processorFunc 把普通函数适配成 Processor。
type processorFunc func(ctx context.Context, task *TaskContext) (*ProcessedOutput, error)

func (f processorFunc) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	return f(ctx, task)
}

// ---------------------------------------------------------------------------
// Agent 模式
// ---------------------------------------------------------------------------

func TestAgentBackendDelegatesToDispatcher(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("dispatched")}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("research", true, 2, 4)}, 4, "", processor)
	backend := NewAgentBackend(dispatcher)
	if !backend.IsAgentBackend() {
		t.Error("an agent backend must report itself as such")
	}
	out, err := backend.Execute(context.Background(), "research", &TaskContext{}, map[string]any{}, time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Summary != "dispatched" {
		t.Errorf("summary = %#v", out.Summary)
	}
}

// Agent 模式下的失败是 *DispatchError,协议层要能按 Code 分流。
func TestAgentBackendSurfacesDispatchError(t *testing.T) {
	t.Parallel()
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("other", true, 2, 4)}, 4, "", &stubProcessor{})
	backend := NewAgentBackend(dispatcher)
	_, err := backend.Execute(context.Background(), "nope", &TaskContext{}, map[string]any{}, time.Minute)
	wantDispatchCode(t, err, "agent_not_found")
}

// Agent 模式确实会消费 timeout(与 Processor 模式相反)。
func TestAgentBackendHonoursTimeout(t *testing.T) {
	t.Parallel()
	processor := &stubProcessor{output: PlaceholderOutput("late"), delay: 5 * time.Second}
	dispatcher, _, _ := newTestDispatcher(t,
		[]*AgentConfig{dispatcherAgent("slow", true, 2, 4)}, 4, "", processor)
	backend := NewAgentBackend(dispatcher)
	_, err := backend.Execute(context.Background(), "slow", &TaskContext{}, map[string]any{}, 120*time.Millisecond)
	wantDispatchCode(t, err, "execution_timeout")
}

// 空后端是编程错误,要给出可诊断的报错而不是 panic。
func TestEmptyBackendReportsMisconfiguration(t *testing.T) {
	t.Parallel()
	backend := &TaskExecutionBackend{}
	_, err := backend.Execute(context.Background(), "", &TaskContext{}, map[string]any{}, time.Minute)
	if err == nil {
		t.Fatal("expected an error from an empty backend")
	}
	if err.Error() != "task execution backend has neither a dispatcher nor a processor" {
		t.Errorf("err = %v", err)
	}
}
