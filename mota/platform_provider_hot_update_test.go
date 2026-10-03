package mota

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

type providerSnapshotBlockingProcessor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *providerSnapshotBlockingProcessor) Process(
	ctx context.Context, _ *TaskContext,
) (*ProcessedOutput, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return &ProcessedOutput{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestExecutorProviderHotUpdateDoesNotWaitForInFlightTask(t *testing.T) {
	cfg := executorTestConfig(t)
	processor := &providerSnapshotBlockingProcessor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	executor := newTestExecutor(t, cfg, processor)
	runtime := executorRuntime("fundamental_analyst", ExecutorKindRust, ExecutionModeLocal)
	runDone := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), runtime, &TaskContext{}, nil, time.Second)
		runDone <- err
	}()
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}

	next := clonePlatformConfig(cfg)
	next.Gateway.Model = "hot-updated-model"
	updated := make(chan struct{})
	go func() {
		executor.SetConfig(next)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Provider hot update waited for the in-flight task")
	}
	if got := executor.snapshot().config.Gateway.Model; got != "hot-updated-model" {
		t.Fatalf("new task snapshot model = %q, want hot-updated-model", got)
	}

	close(processor.release)
	if err := <-runDone; err != nil {
		t.Fatalf("in-flight task failed: %v", err)
	}
}

func TestApplyConfigConcurrentWithFirstExecutorNeverLeavesOldSnapshot(t *testing.T) {
	cfg := executorTestConfig(t)
	for iteration := 0; iteration < 100; iteration++ {
		current := clonePlatformConfig(cfg)
		current.Gateway.Model = "old-model"
		next := clonePlatformConfig(cfg)
		next.Gateway.Model = "new-model"
		state := &PlatformState{Config: current, Processor: PlaceholderProcessor{}, HTTPClient: http.DefaultClient}
		start := make(chan struct{})
		var executor *AgentExecutor
		var executorErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			executor, executorErr = state.Executor()
		}()
		go func() {
			defer wait.Done()
			<-start
			state.ApplyConfig(next)
		}()
		close(start)
		wait.Wait()
		if executorErr != nil {
			t.Fatalf("iteration %d: build executor: %v", iteration, executorErr)
		}
		if got := executor.snapshot().config.Gateway.Model; got != "new-model" {
			t.Fatalf("iteration %d: executor retained %q, want new-model", iteration, got)
		}
	}
}

func TestProviderAliasGroupPreventsRestartRollback(t *testing.T) {
	base := testPlatformConfig(t)
	withHarness, _, err := UpdateProvider(base, "deepseek_harness", map[string]any{
		"binary":   "/tmp/dsh-test",
		"base_url": "https://gateway-old.example.test",
		"api_key":  "old-secret",
		"model":    "old-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := StoredProviderUpdateGroup(withHarness, "deepseek_harness")
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := UpdateProvider(withHarness, "gateway", map[string]any{
		"base_url": "https://gateway-new.example.test",
		"api_key":  "new-secret",
		"model":    "new-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := StoredProviderUpdateGroup(updated, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	for id, value := range group {
		saved[id] = value
	}

	restarted := clonePlatformConfig(base)
	for _, id := range providerIDs {
		if value := saved[id]; value != nil {
			if err := applyProviderValue(restarted, id, value, false); err != nil {
				t.Fatalf("reload %s: %v", id, err)
			}
		}
	}
	if restarted.Gateway.BaseURL != "https://gateway-new.example.test" ||
		restarted.Gateway.Model != "new-model" || restarted.Gateway.APIKey != "new-secret" {
		t.Fatalf("gateway rolled back after restart: %+v", restarted.Gateway)
	}
	if restarted.LocalAgents.DeepseekHarnessBinary != "/tmp/dsh-test" {
		t.Fatalf("deepseek binary = %q", restarted.LocalAgents.DeepseekHarnessBinary)
	}

	withPi, _, err := UpdateProvider(base, "pi_local", map[string]any{
		"binary": "pi-test", "default_model": "shared-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = StoredProviderUpdateGroup(withPi, "pi_local")
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err = UpdateProvider(withPi, "codex_local", map[string]any{
		"binary": "codex-test", "default_model": "shared-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err = StoredProviderUpdateGroup(updated, "codex_local")
	if err != nil {
		t.Fatal(err)
	}
	for id, value := range group {
		saved[id] = value
	}
	restarted = clonePlatformConfig(base)
	for _, id := range providerIDs {
		if value := saved[id]; value != nil {
			if err := applyProviderValue(restarted, id, value, false); err != nil {
				t.Fatalf("reload %s: %v", id, err)
			}
		}
	}
	if restarted.LocalAgents.DefaultModel != "shared-new" {
		t.Fatalf("shared local model rolled back to %q", restarted.LocalAgents.DefaultModel)
	}
	if restarted.LocalAgents.PiBinary != "pi-test" || restarted.LocalAgents.CodexBinary != "codex-test" {
		t.Fatalf("private alias fields were lost: %+v", restarted.LocalAgents)
	}
}

func TestParseProviderIfMatch(t *testing.T) {
	request, err := http.NewRequest(http.MethodPut, "/api/admin/providers/gateway", nil)
	if err != nil {
		t.Fatal(err)
	}
	if revision, err := parseProviderIfMatch(request); err != nil || revision != nil {
		t.Fatalf("missing If-Match = (%v, %v)", revision, err)
	}
	request.Header.Set("If-Match", `"7"`)
	revision, err := parseProviderIfMatch(request)
	if err != nil || revision == nil || *revision != 7 {
		t.Fatalf("quoted If-Match = (%v, %v)", revision, err)
	}
	for _, value := range []string{"7", `W/"7"`, `"-1"`, `"1", "2"`} {
		request.Header.Set("If-Match", value)
		if _, err := parseProviderIfMatch(request); err == nil {
			t.Errorf("If-Match %q should fail", value)
		}
	}
}

func TestAgentRegistryRefreshConvergesAcrossInstances(t *testing.T) {
	writer := newTestStore(t)
	reader := newTestStore(t)
	agents, revision, err := LoadConsistentAgentSnapshot(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	state := &PlatformState{
		Store: reader, Registry: NewAgentRegistry(agents), agentRefreshDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go state.runAgentRefresh(ctx, revision)
	t.Cleanup(func() {
		cancel()
		<-state.agentRefreshDone
	})

	agent := seedAgent(t, writer, "registry-refresh")
	waitForCondition(t, 6*time.Second, "remote registry create", func() bool {
		return state.Registry.Get(agent.Slug) != nil
	})

	disabled := false
	rotated := "rotated-registry-secret-value"
	input := &AgentInput{
		Slug: agent.Slug, Name: "updated", Enabled: &disabled,
		ExecutorKind: agent.ExecutorKind, ExecutionMode: agent.ExecutionMode,
		SigningSecret: &rotated, SignatureEncoding: agent.SignatureEncoding,
		WorkspacePath: agent.WorkspacePath, PermissionMode: agent.PermissionMode,
	}
	if err := input.Validate(false); err != nil {
		t.Fatal(err)
	}
	updated, err := writer.UpdateAgentIfRevision(
		context.Background(), agent.Slug, input, &agent.Revision)
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 6*time.Second, "remote registry update", func() bool {
		runtime := state.Registry.Get(agent.Slug)
		return runtime != nil && runtime.Config.Revision == updated.Revision &&
			runtime.Config.SigningSecret == rotated && !runtime.Config.Enabled
	})

	if deleted, err := writer.DeleteAgent(context.Background(), agent.Slug); err != nil || !deleted {
		t.Fatalf("delete = %t, %v", deleted, err)
	}
	waitForCondition(t, 6*time.Second, "remote registry delete", func() bool {
		return state.Registry.Get(agent.Slug) == nil
	})
}
