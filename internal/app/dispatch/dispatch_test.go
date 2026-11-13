package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 测试假件
// ---------------------------------------------------------------------------

type fakeResolver struct {
	configs map[string]*agent.ResolvedConfig
}

func (f fakeResolver) Resolve(_ context.Context, slug string) (*agent.ResolvedConfig, error) {
	if cfg, ok := f.configs[slug]; ok {
		return cfg, nil
	}
	return nil, agent.ErrNotFound
}

type fakeEngine struct {
	mu          sync.Mutex
	output      *platform.ProcessedOutput
	err         error
	block       chan struct{}
	lastTimeout time.Duration
	calls       int
}

func (f *fakeEngine) Execute(_ context.Context, _ *agent.ResolvedConfig, _ *platform.TaskContext,
	_ any, timeout time.Duration) (*platform.ProcessedOutput, error) {
	f.mu.Lock()
	f.calls++
	f.lastTimeout = timeout
	block, err, out := f.block, f.err, f.output
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return out, err
}

type fakeQueue struct {
	enqueued   []connector.NewTask
	enqueueErr error
	waitState  connector.TaskState
	waitResult []byte
	waitBlock  bool
}

func (f *fakeQueue) Enqueue(_ context.Context, task connector.NewTask) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.enqueued = append(f.enqueued, task)
	return nil
}
func (f *fakeQueue) Get(context.Context, string) (*connector.Task, error) {
	return nil, connector.ErrTaskNotFound
}
func (f *fakeQueue) WaitTerminal(ctx context.Context, _ string) (connector.TaskState, []byte, error) {
	if f.waitBlock {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	return f.waitState, f.waitResult, nil
}
func (f *fakeQueue) Claim(context.Context, connector.ClaimRequest) (*connector.Task, connector.ClaimOutcome, error) {
	return nil, connector.ClaimEmpty, nil
}
func (f *fakeQueue) WorkerLeaseCount(context.Context, string) (int, error) {
	return 0, nil
}
func (f *fakeQueue) Renew(context.Context, string, string, time.Duration) (connector.RenewOutcome, error) {
	return connector.RenewUnknown, nil
}
func (f *fakeQueue) RequestAbort(context.Context, string) error { return nil }
func (f *fakeQueue) Complete(context.Context, connector.CompleteRequest) (connector.CompleteOutcome, error) {
	return connector.CompleteOutcome{}, nil
}
func (f *fakeQueue) QueueDepth(context.Context, []string) (int, error) { return 0, nil }
func (f *fakeQueue) Sweep(context.Context, connector.SweepPolicy) (connector.SweepStats, error) {
	return connector.SweepStats{}, nil
}
func (f *fakeQueue) RecordDirectRun(context.Context, connector.DirectRun) (string, error) {
	return "succeeded", nil
}

// codedError 模拟引擎的 *ExecutionError（结构性 ErrorCode 探测）。
type codedError struct{ code, msg string }

func (e *codedError) Error() string     { return e.code + ": " + e.msg }
func (e *codedError) ErrorCode() string { return e.code }

func localConfig(slug string) *agent.ResolvedConfig {
	return &agent.ResolvedConfig{
		Slug: slug, Name: slug, Enabled: true,
		ExecutorKind: exec.KindPi, ExecutionMode: exec.ModeLocal,
		TimeoutSecs: 600, MaxConcurrency: 2, QueueCapacity: 2, Revision: 1,
	}
}

func wantDispatchError(t *testing.T, err error, code string) *Error {
	t.Helper()
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("expected *dispatch.Error, got %T: %v", err, err)
	}
	if dispatchErr.Code != code {
		t.Fatalf("code = %q, want %q (detail: %s)", dispatchErr.Code, code, dispatchErr.Detail)
	}
	return dispatchErr
}

// ---------------------------------------------------------------------------
// 本机路径：准入阶梯
// ---------------------------------------------------------------------------

func TestExecuteSuccessPassesRemainingBudget(t *testing.T) {
	t.Parallel()
	engine := &fakeEngine{output: &platform.ProcessedOutput{Summary: "ok"}}
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"demo": localConfig("demo")}},
		Engine:               engine,
		GlobalMaxConcurrency: 4,
	})
	out, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Summary != "ok" {
		t.Errorf("output = %+v", out)
	}
	if engine.lastTimeout <= 0 || engine.lastTimeout > 10*time.Second {
		t.Errorf("engine timeout = %v", engine.lastTimeout)
	}
}

func TestAdmissionFullFailsFast(t *testing.T) {
	t.Parallel()
	cfg := localConfig("demo")
	cfg.MaxConcurrency, cfg.QueueCapacity = 1, 0 // 准入闸只有 1 个许可
	block := make(chan struct{})
	engine := &fakeEngine{block: block, output: &platform.ProcessedOutput{}}
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"demo": cfg}},
		Engine:               engine,
		GlobalMaxConcurrency: 4,
	})
	first := make(chan error, 1)
	go func() {
		_, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, 5*time.Second)
		first <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		engine.mu.Lock()
		calls := engine.calls
		engine.mu.Unlock()
		if calls == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	_, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, 5*time.Second)
	wantDispatchError(t, err, "agent_overloaded")
	if time.Since(started) > time.Second {
		t.Error("admission rejection must be immediate, not budget-bound")
	}
	close(block) // 放行第一个任务，回收 goroutine
	if err := <-first; err != nil {
		t.Errorf("first execute: %v", err)
	}
}

func TestDisabledAgentRejected(t *testing.T) {
	t.Parallel()
	cfg := localConfig("demo")
	cfg.Enabled = false
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"demo": cfg}},
		Engine:               &fakeEngine{},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, time.Second)
	wantDispatchError(t, err, "agent_disabled")
}

func TestResolveFallsBackToDefaultAgent(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"fallback": localConfig("fallback")}},
		Engine:               &fakeEngine{output: &platform.ProcessedOutput{}},
		DefaultSlug:          "fallback",
		GlobalMaxConcurrency: 1,
	})
	// 未知 code 与空 code 走同一条回落路径。
	if _, err := svc.Execute(context.Background(), "ghost", &platform.TaskContext{}, nil, time.Second); err != nil {
		t.Errorf("explicit miss must fall back: %v", err)
	}
	if _, err := svc.Execute(context.Background(), "  ", &platform.TaskContext{}, nil, time.Second); err != nil {
		t.Errorf("empty code must fall back: %v", err)
	}
}

func TestDefaultAgentMissingIsExplicit(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{}},
		Engine:               &fakeEngine{},
		DefaultSlug:          "ghost-default",
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "x", &platform.TaskContext{}, nil, time.Second)
	dispatchErr := wantDispatchError(t, err, "agent_not_found")
	if !strings.Contains(dispatchErr.Detail, "ghost-default") {
		t.Errorf("detail must name the unregistered default: %q", dispatchErr.Detail)
	}
}

func TestEngineErrorCodesPassThrough(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"demo": localConfig("demo")}},
		Engine:               &fakeEngine{err: &codedError{code: "input_too_large", msg: "prompt exceeded limit"}},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, time.Second)
	dispatchErr := wantDispatchError(t, err, "input_too_large")
	if dispatchErr.Detail != "input_too_large: prompt exceeded limit" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}
}

func TestExecFailureMapping(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"demo": localConfig("demo")}},
		Engine:               &fakeEngine{err: &exec.Failure{Code: "executor_failed", Message: "cli crashed"}},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "demo", &platform.TaskContext{}, nil, time.Second)
	wantDispatchError(t, err, "executor_failed")
}

// ---------------------------------------------------------------------------
// 远端路径：队列路由与有界同步等待
// ---------------------------------------------------------------------------

func remoteConfig(slug string) *agent.ResolvedConfig {
	cfg := localConfig(slug)
	cfg.ExecutorKind = exec.KindRemotePull
	cfg.ExecutionMode = exec.ModeRemotePull
	return cfg
}

func TestRemoteEnqueueAndResultMapping(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{
		waitState: connector.TaskCompleted,
		waitResult: []byte(`{"status":"succeeded","payload":{"summary":"done"},` +
			`"usage":{"inputTokens":0,"outputTokens":0,"totalTokens":9414}}`),
	}
	runID := "run_explicit_1"
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"remote-demo": remoteConfig("remote-demo")}},
		Engine:               &fakeEngine{},
		Queue:                queue,
		Remote:               RemotePolicy{QueueTTL: time.Hour, SyncHoldMax: 300 * time.Second},
		GlobalMaxConcurrency: 1,
	})
	task := &platform.TaskContext{RunID: &runID, TaskBrief: "brief"}
	out, err := svc.Execute(context.Background(), "remote-demo", task, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("execute remote: %v", err)
	}
	if len(queue.enqueued) != 1 {
		t.Fatalf("enqueued = %d", len(queue.enqueued))
	}
	enq := queue.enqueued[0]
	if enq.RunID != runID || enq.AgentSlug != "remote-demo" || enq.AdmissionLimit != 4 {
		t.Errorf("enqueue = %+v", enq)
	}
	var payload map[string]any
	if err := json.Unmarshal(enq.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["taskBrief"] != "brief" {
		t.Errorf("payload taskBrief = %v", payload["taskBrief"])
	}
	if out.PayloadResult == nil {
		t.Fatal("payload result missing")
	}
	if out.Usage == nil || out.Usage.SchemaVersion != platform.SchemaUsageV2 ||
		out.Usage.Status != "FINAL" || out.Usage.PromptTokens != 9414 {
		t.Errorf("usage = %+v（total 兜底回填应生效）", out.Usage)
	}
}

func TestRemoteFailureAndCancelMapping(t *testing.T) {
	t.Parallel()
	failed := &fakeQueue{waitState: connector.TaskFailed,
		waitResult: []byte(`{"status":"failed","error":{"code":"model_overloaded","message":"upstream busy"}}`)}
	svc := NewService(Config{
		Resolver: fakeResolver{configs: map[string]*agent.ResolvedConfig{"r": remoteConfig("r")}},
		Queue:    failed, Remote: RemotePolicy{QueueTTL: time.Hour, SyncHoldMax: time.Second},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "r", &platform.TaskContext{}, nil, time.Second)
	dispatchErr := wantDispatchError(t, err, "model_overloaded")
	if dispatchErr.Detail != "upstream busy" {
		t.Errorf("detail = %q", dispatchErr.Detail)
	}

	canceled := &fakeQueue{waitState: connector.TaskCanceled, waitResult: []byte(`{"status":"aborted"}`)}
	svc.queue = canceled
	_, err = svc.Execute(context.Background(), "r", &platform.TaskContext{}, nil, time.Second)
	wantDispatchError(t, err, "aborted")
}

func TestRemoteQueueFullIsOverloaded(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"r": remoteConfig("r")}},
		Queue:                &fakeQueue{enqueueErr: connector.ErrQueueFull},
		Remote:               RemotePolicy{QueueTTL: time.Hour, SyncHoldMax: time.Second},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "r", &platform.TaskContext{}, nil, time.Second)
	wantDispatchError(t, err, "agent_overloaded")
}

func TestRemoteSyncHoldTimeoutKeepsTaskRunning(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"r": remoteConfig("r")}},
		Queue:                &fakeQueue{waitBlock: true},
		Remote:               RemotePolicy{QueueTTL: time.Hour, SyncHoldMax: 80 * time.Millisecond},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "r", &platform.TaskContext{}, nil, 10*time.Second)
	dispatchErr := wantDispatchError(t, err, "execution_timeout")
	if !strings.Contains(dispatchErr.Detail, "still running") {
		t.Errorf("detail must state the task keeps running: %q", dispatchErr.Detail)
	}
}

func TestRemoteUnavailableWithoutQueue(t *testing.T) {
	t.Parallel()
	svc := NewService(Config{
		Resolver:             fakeResolver{configs: map[string]*agent.ResolvedConfig{"r": remoteConfig("r")}},
		GlobalMaxConcurrency: 1,
	})
	_, err := svc.Execute(context.Background(), "r", &platform.TaskContext{}, nil, time.Second)
	wantDispatchError(t, err, "executor_unavailable")
}
