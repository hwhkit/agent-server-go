package mota

// PostgreSQL 真库集成测试。运行时必须显式提供隔离数据库 TEST_DATABASE_URL,
// 未提供则整体跳过。对应 rs tests/postgres_store_test.rs 的
// get_run_reads_single_record_and_a2a_task_snapshot。
//
// A2A 的 GetTask 依赖按主键单读运行记录,并且要能取回写入的 Task 快照。
// 内存后端测不到真实 SQL,这里补上。

import (
	"context"
	"testing"
)

func TestA2APostgresGetRunReadsSingleRecordAndTaskSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	slug := uniqueSlug("pg-a2a")
	secret := "a2a-integration-signing-secret"
	agent, err := store.CreateAgent(ctx, &AgentInput{
		Slug: slug, Name: "A2A integration", ExecutorKind: ExecutorKindPi,
		ExecutionMode: ExecutionModeLocal, SigningSecret: &secret,
		SignatureEncoding: "hex", WorkspacePath: "agents", PermissionMode: "dontAsk",
		TimeoutSecs: uint64Ptr(30), MaxConcurrency: intPtr(2), QueueCapacity: intPtr(4),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = store.DeleteAgent(ctx, slug) })

	// 未知记录返回 nil,而不是报错。
	missing, err := store.GetRun(ctx, slug, "task-missing")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil for an unknown run, got %+v", missing)
	}

	tasks := NewA2APostgresTaskStore(store)
	taskID := unique("task")
	request := &A2AStoredRequest{
		Channel: "a2a", ContextID: "ctx-pg-1", MessageID: "msg-pg-1",
		Structured: true,
		Input:      map[string]any{"positionType": "FUNDAMENTAL_ANALYST"},
	}
	outcome, err := tasks.Start(ctx, agent, taskID, request)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if outcome != A2AStartCreated {
		t.Fatalf("outcome = %v, want Created", outcome)
	}
	// 同一 (slug, task id) 再次登记必须判冲突。
	outcome2, err := tasks.Start(ctx, agent, taskID, request)
	if err != nil {
		t.Fatalf("Start (duplicate): %v", err)
	}
	if outcome2 != A2AStartConflict {
		t.Fatalf("outcome = %v, want Conflict", outcome2)
	}

	// 执行中:没有快照可复原,但 contextId 必须能从请求信封里取回。
	running, err := tasks.Get(ctx, slug, taskID)
	if err != nil {
		t.Fatalf("Get (running): %v", err)
	}
	if running == nil {
		t.Fatal("expected a running task")
	}
	if running.Status.State != A2ATaskStateWorking {
		t.Errorf("state = %v, want Working", running.Status.State)
	}
	if running.ContextID != "ctx-pg-1" {
		t.Errorf("contextId = %q, want ctx-pg-1", running.ContextID)
	}

	completed := &A2ATask{
		ID: taskID, ContextID: "ctx-pg-1",
		Status: NewA2ATaskStatus(A2ATaskStateCompleted),
		Artifacts: []A2AArtifact{{
			ArtifactID: "artifact-pg-1",
			Parts:      []A2APart{A2APartData(map[string]any{"payloadResult": map[string]any{"assessments": []any{}}})},
		}},
	}
	executorOutput := `{"raw":true}`
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := tasks.Finish(cancelled, slug, completed, 42, nil, &executorOutput); err != nil {
		t.Fatalf("Finish with canceled request context: %v", err)
	}

	restored, err := tasks.Get(ctx, slug, taskID)
	if err != nil {
		t.Fatalf("Get (restored): %v", err)
	}
	if restored.Status.State != A2ATaskStateCompleted {
		t.Errorf("state = %v, want Completed", restored.Status.State)
	}
	if restored.ContextID != "ctx-pg-1" {
		t.Errorf("contextId = %q", restored.ContextID)
	}
	if len(restored.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(restored.Artifacts))
	}
	if restored.Artifacts[0].ArtifactID != "artifact-pg-1" {
		t.Errorf("artifactId = %q", restored.Artifacts[0].ArtifactID)
	}

	// 终态记录进入管理台运行记录,并带上 revision / 执行器快照。
	run, err := store.GetRun(ctx, slug, taskID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run == nil {
		t.Fatal("expected a run record")
	}
	if run.Status != "completed" {
		t.Errorf("status = %q, want completed", run.Status)
	}
	if run.DurationMs == nil || *run.DurationMs != 42 {
		t.Errorf("duration_ms = %v, want 42", run.DurationMs)
	}
	if run.ResponseCode == nil || *run.ResponseCode != 200 {
		t.Errorf("response_code = %v, want 200", run.ResponseCode)
	}
	if run.AgentRevision != agent.Revision {
		t.Errorf("agent_revision = %d, want %d", run.AgentRevision, agent.Revision)
	}
	if run.ExecutorKind == nil || *run.ExecutorKind != "pi" {
		t.Errorf("executor_kind = %v, want pi", run.ExecutorKind)
	}
	if run.ExecutorOutput == nil || *run.ExecutorOutput != executorOutput {
		t.Errorf("executor_output = %v, want %q", run.ExecutorOutput, executorOutput)
	}
	if run.Input == nil || !containsSubstr(*run.Input, "ctx-pg-1") {
		t.Errorf("input = %v, want to contain ctx-pg-1", run.Input)
	}

	// 重复终结必须失败(状态已不是 running)。
	if err := tasks.Finish(ctx, slug, completed, 42, nil, nil); err == nil {
		t.Error("expected an error finishing an already-finished task")
	}
}

func containsSubstr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
