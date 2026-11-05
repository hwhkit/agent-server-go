package a2a

// RestoreTask 复原语义测试（承接旧栈 a2a_store_test 的内存后端场景：
// 存储形态本身由 infra/postgres 集成测试覆盖，这里只测纯复原逻辑）。

import (
	"encoding/json"
	"testing"

	"ir-agent-server/internal/domain/platform"
)

func storedRequestEnvelope(contextID string) []byte {
	encoded, err := json.Marshal(&StoredRequest{
		Channel: "a2a", ContextID: contextID, MessageID: "msg-1",
		Structured: true, Input: map[string]any{"positionType": "STRATEGIST"},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestRestoreRunningTaskSynthesizesWorking(t *testing.T) {
	t.Parallel()
	task := RestoreTask("task-1", "running", storedRequestEnvelope("ctx-1"))
	if task.ID != "task-1" || task.ContextID != "ctx-1" {
		t.Fatalf("task = %+v", task)
	}
	if task.Status.State != platform.A2ATaskStateWorking {
		t.Errorf("state = %s, want WORKING", task.Status.State)
	}
	if len(task.Artifacts) != 0 {
		t.Errorf("running task must not carry artifacts: %+v", task.Artifacts)
	}
}

func TestRestoreCompletedTaskUsesSnapshot(t *testing.T) {
	t.Parallel()
	completed := &platform.A2ATask{
		ID: "task-2", ContextID: "ctx-2",
		Status: platform.NewA2ATaskStatus(platform.A2ATaskStateCompleted),
		Artifacts: []platform.A2AArtifact{{
			ArtifactID: "artifact-1",
			Parts:      []platform.A2APart{platform.A2APartText("done")},
		}},
	}
	snapshot, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	task := RestoreTask("task-2", "completed", snapshot)
	if task.Status.State != platform.A2ATaskStateCompleted || len(task.Artifacts) != 1 {
		t.Fatalf("task = %+v", task)
	}
	if task.Artifacts[0].ArtifactID != "artifact-1" {
		t.Errorf("artifact = %+v", task.Artifacts[0])
	}
}

func TestRestoreCorruptSnapshotFallsBackToWorking(t *testing.T) {
	t.Parallel()
	// 终态但快照损坏（升级前旧记录）：合成状态返回，不报错。
	task := RestoreTask("task-3", "failed", []byte("{corrupt"))
	if task.Status.State != platform.A2ATaskStateFailed {
		t.Errorf("state = %s, want FAILED from run status", task.Status.State)
	}
	// 未知状态串回退 WORKING。
	unknown := RestoreTask("task-4", "weird", nil)
	if unknown.Status.State != platform.A2ATaskStateWorking {
		t.Errorf("state = %s, want WORKING fallback", unknown.Status.State)
	}
}
