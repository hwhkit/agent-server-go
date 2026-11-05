package postgres_test

// A2A 任务存储集成测试（agent_runs 复用形态）。门禁同 store_test.go。

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"ir-agent-server/internal/infra/postgres"
)

func TestA2ATaskStoreLifecycle(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store := postgres.NewA2ATaskStore(db)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "a2a-desk-" + suffix
	taskID := "task-" + suffix
	requestJSON, _ := json.Marshal(map[string]any{
		"channel": "a2a", "contextId": "ctx-1", "messageId": "msg-1",
		"structured": true, "input": map[string]any{"positionType": "STRATEGIST"},
	})

	// Start：running 登记；重复 Start 冲突。
	created, err := store.Start(ctx, slug, taskID, 4, "pi", "local", requestJSON)
	if err != nil || !created {
		t.Fatalf("start = %v/%v", created, err)
	}
	created, err = store.Start(ctx, slug, taskID, 4, "pi", "local", requestJSON)
	if err != nil || created {
		t.Fatalf("duplicate start = %v/%v, want conflict", created, err)
	}

	// Get：执行中返回请求信封。
	status, payload, found, err := store.Get(ctx, slug, taskID)
	if err != nil || !found || status != "running" {
		t.Fatalf("get = %s/%v/%v", status, found, err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope["contextId"] != "ctx-1" {
		t.Errorf("stored envelope = %s", payload)
	}
	_, _, found, err = store.Get(ctx, slug, "task-does-not-exist")
	if err != nil || found {
		t.Errorf("missing get = %v/%v", found, err)
	}

	// Finish：写入 Task 快照；重复 Finish 被 running 守卫拒绝。
	snapshot, _ := json.Marshal(map[string]any{
		"id": taskID, "contextId": "ctx-1",
		"status": map[string]any{"state": "TASK_STATE_COMPLETED", "timestamp": "2026-11-05T00:00:00Z"},
	})
	errCode := "test_error"
	if err := store.Finish(ctx, slug, taskID, "completed", 1234, &errCode, snapshot, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := store.Finish(ctx, slug, taskID, "completed", 1, nil, snapshot, nil); err == nil {
		t.Error("second finish must fail the running guard")
	}
	status, payload, _, err = store.Get(ctx, slug, taskID)
	if err != nil || status != "completed" {
		t.Fatalf("get after finish = %s/%v", status, err)
	}
	var restored map[string]any
	if err := json.Unmarshal(payload, &restored); err != nil || restored["id"] != taskID {
		t.Errorf("snapshot payload = %s", payload)
	}

	// FromPool 构造与原构造等价（迁移期与旧栈共池）。
	fromPool := postgres.NewA2ATaskStoreFromPool(db.Pool)
	if _, _, found, err := fromPool.Get(ctx, slug, taskID); err != nil || !found {
		t.Errorf("from-pool get = %v/%v", found, err)
	}
}
