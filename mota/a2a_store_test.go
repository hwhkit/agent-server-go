package mota

// 对应 rs src/a2a/store.rs 的单测(内存后端)。PostgreSQL 后端的等价覆盖在
// a2a_postgres_test.go(需要 TEST_DATABASE_URL)。

import (
	"context"
	"testing"
)

func a2aStoreTestAgent() *AgentConfig {
	return &AgentConfig{
		ID: 1, Slug: "desk", Name: "Desk", Enabled: true,
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeLocal,
		SigningSecret: "0123456789abcdef", SignatureEncoding: "hex",
		SystemPrompt: "strategist", WorkspacePath: ".", PermissionMode: "dontAsk",
		TimeoutSecs: 600, MaxConcurrency: 1, QueueCapacity: 10, Revision: 4,
	}
}

func a2aStoreTestRequest(contextID string) *A2AStoredRequest {
	return &A2AStoredRequest{
		Channel: "a2a", ContextID: contextID, MessageID: "msg-1",
		Structured: true, Input: map[string]any{"positionType": "STRATEGIST"},
	}
}

func a2aStoreCompletedTask(taskID, contextID string) *A2ATask {
	return &A2ATask{
		ID: taskID, ContextID: contextID,
		Status: NewA2ATaskStatus(A2ATaskStateCompleted),
		Artifacts: []A2AArtifact{{
			ArtifactID: "artifact-1",
			Parts:      []A2APart{A2APartText("done")},
		}},
	}
}

func TestA2AStoreRunningTaskReportsWorkingWithContextID(t *testing.T) {
	store := NewA2AInMemoryTaskStore()
	outcome, err := store.Start(context.Background(), a2aStoreTestAgent(), "task-1", a2aStoreTestRequest("ctx-1"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != A2AStartCreated {
		t.Fatalf("outcome = %v, want Created", outcome)
	}
	task, err := store.Get(context.Background(), "desk", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("expected a task")
	}
	if task.Status.State != A2ATaskStateWorking {
		t.Errorf("state = %v, want Working", task.Status.State)
	}
	if task.ContextID != "ctx-1" {
		t.Errorf("contextId = %q, want ctx-1", task.ContextID)
	}
	if len(task.Artifacts) != 0 {
		t.Error("expected no artifacts while running")
	}
}

func TestA2AStoreFinishedTaskRoundTripsArtifacts(t *testing.T) {
	store := NewA2AInMemoryTaskStore()
	agent := a2aStoreTestAgent()
	ctx := context.Background()
	if _, err := store.Start(ctx, agent, "task-1", a2aStoreTestRequest("ctx-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, "desk", a2aStoreCompletedTask("task-1", "ctx-1"), 120, nil, nil); err != nil {
		t.Fatal(err)
	}

	task, err := store.Get(ctx, "desk", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != A2ATaskStateCompleted {
		t.Errorf("state = %v, want Completed", task.Status.State)
	}
	if task.ContextID != "ctx-1" {
		t.Errorf("contextId = %q", task.ContextID)
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(task.Artifacts))
	}
	if task.Artifacts[0].Parts[0].Text == nil || *task.Artifacts[0].Parts[0].Text != "done" {
		t.Errorf("artifact text = %v, want done", task.Artifacts[0].Parts[0].Text)
	}
}

func TestA2AStoreDuplicateTaskIDIsAConflict(t *testing.T) {
	store := NewA2AInMemoryTaskStore()
	agent := a2aStoreTestAgent()
	ctx := context.Background()
	if _, err := store.Start(ctx, agent, "task-1", a2aStoreTestRequest("ctx-1")); err != nil {
		t.Fatal(err)
	}
	outcome, err := store.Start(ctx, agent, "task-1", a2aStoreTestRequest("ctx-2"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != A2AStartConflict {
		t.Fatalf("outcome = %v, want Conflict", outcome)
	}
}

func TestA2AStoreFinishingTwiceIsRejected(t *testing.T) {
	store := NewA2AInMemoryTaskStore()
	agent := a2aStoreTestAgent()
	ctx := context.Background()
	if _, err := store.Start(ctx, agent, "task-1", a2aStoreTestRequest("ctx-1")); err != nil {
		t.Fatal(err)
	}
	task := a2aStoreCompletedTask("task-1", "ctx-1")
	if err := store.Finish(ctx, "desk", task, 10, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, "desk", task, 10, nil, nil); err == nil {
		t.Fatal("expected an error finishing an already-finished task")
	}
}

func TestA2AStoreUnknownTaskIsNil(t *testing.T) {
	store := NewA2AInMemoryTaskStore()
	task, err := store.Get(context.Background(), "desk", "nope")
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatal("expected nil for an unknown task")
	}
}

// 升级前写入的旧记录里没有 Task 快照,不能让 GetTask 直接失败。
func TestA2AStoreCorruptSnapshotDegradesToASynthesizedTask(t *testing.T) {
	corrupt := "not json"
	task := a2aRestoreTask("task-1", "completed", &corrupt)
	if task.ID != "task-1" {
		t.Errorf("id = %q", task.ID)
	}
	if task.Status.State != A2ATaskStateCompleted {
		t.Errorf("state = %v, want Completed", task.Status.State)
	}
	if len(task.Artifacts) != 0 {
		t.Error("expected no artifacts for a corrupt snapshot")
	}
}
