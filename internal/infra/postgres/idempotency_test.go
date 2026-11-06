package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/infra/postgres"
)

// Push 幂等存储集成测试：Begin 决策矩阵、run_id 唯一防线、并发单 New、
// 陈旧接管与租约归属。门禁同 store_test.go（TEST_DATABASE_URL）。

func TestPushIdempotencyDecisionMatrix(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store, err := postgres.NewPushIdempotencyStore(ctx, db)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	suffix := time.Now().UnixNano()
	key := fmt.Sprintf("it-idem-key-%d", suffix)
	runID := fmt.Sprintf("it-idem-run-%d", suffix)
	hash := "hash-a"

	// 1. 无记录 → New。
	decision, err := store.Begin(ctx, key, runID, hash)
	if err != nil || decision.Kind != platform.IdemNew {
		t.Fatalf("first begin = %+v err=%v", decision, err)
	}
	// 2. processing 未陈旧 → InProgress。
	decision, err = store.Begin(ctx, key, runID, hash)
	if err != nil || decision.Kind != platform.IdemInProgress {
		t.Fatalf("second begin = %+v err=%v", decision, err)
	}
	// 3. 同键不同体 → Conflict。
	decision, err = store.Begin(ctx, key, runID, "hash-b")
	if err != nil || decision.Kind != platform.IdemConflict {
		t.Fatalf("hash mismatch = %+v err=%v", decision, err)
	}
	// 4. 同 run 不同键 → Conflict（run_id 唯一防线）。
	decision, err = store.Begin(ctx, key+"-other", runID, hash)
	if err != nil || decision.Kind != platform.IdemConflict {
		t.Fatalf("cross-key same run = %+v err=%v", decision, err)
	}
	// 5. sync_done → SyncCached 回放；响应体缺失是不自洽 → Conflict 由
	//    CompleteSync 保证不会发生，这里验证正常回放。
	if err := store.CompleteSync(ctx, key, runID, hash, `{"payload_info":"ok"}`); err != nil {
		t.Fatalf("complete sync: %v", err)
	}
	decision, err = store.Begin(ctx, key, runID, hash)
	if err != nil || decision.Kind != platform.IdemSyncCached || decision.ResponseBody != `{"payload_info":"ok"}` {
		t.Fatalf("cached replay = %+v err=%v", decision, err)
	}
	// 6. async_accepted → AsyncAccepted。
	keyAsync := fmt.Sprintf("it-idem-async-%d", suffix)
	runAsync := fmt.Sprintf("it-idem-async-run-%d", suffix)
	if _, err := store.Begin(ctx, keyAsync, runAsync, hash); err != nil {
		t.Fatalf("async begin: %v", err)
	}
	if err := store.AcceptAsync(ctx, keyAsync, runAsync, hash); err != nil {
		t.Fatalf("accept async: %v", err)
	}
	decision, err = store.Begin(ctx, keyAsync, runAsync, hash)
	if err != nil || decision.Kind != platform.IdemAsyncAccepted {
		t.Fatalf("async replay = %+v err=%v", decision, err)
	}
	// 7. sync_failed → SyncFailed 终态回放（阻止重试再执行副作用）。
	keyFailed := fmt.Sprintf("it-idem-failed-%d", suffix)
	runFailed := fmt.Sprintf("it-idem-failed-run-%d", suffix)
	if _, err := store.Begin(ctx, keyFailed, runFailed, hash); err != nil {
		t.Fatalf("failed begin: %v", err)
	}
	if err := store.FailSync(ctx, keyFailed, runFailed, hash, "usage_unavailable"); err != nil {
		t.Fatalf("fail sync: %v", err)
	}
	decision, err = store.Begin(ctx, keyFailed, runFailed, hash)
	if err != nil || decision.Kind != platform.IdemSyncFailed || decision.ResponseBody != "usage_unavailable" {
		t.Fatalf("failed replay = %+v err=%v", decision, err)
	}
}

func TestPushIdempotencyConcurrentBeginGrantsSingleLease(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store, err := postgres.NewPushIdempotencyStore(ctx, db)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	key := fmt.Sprintf("it-idem-conc-%d", time.Now().UnixNano())
	runID := key + "-run"

	var wg sync.WaitGroup
	newCount := 0
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := store.Begin(ctx, key, runID, "same-hash")
			if err != nil {
				t.Errorf("concurrent begin: %v", err)
				return
			}
			if decision.Kind == platform.IdemNew {
				mu.Lock()
				newCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if newCount != 1 {
		t.Errorf("concurrent Begin granted %d leases, want exactly 1", newCount)
	}
}

func TestPushIdempotencyStaleTakeoverAndRelease(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store, err := postgres.NewPushIdempotencyStore(ctx, db)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	key := fmt.Sprintf("it-idem-stale-%d", time.Now().UnixNano())
	runID := key + "-run"
	hash := "h"

	if _, err := store.Begin(ctx, key, runID, hash); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// 把 updated_at 拨回陈旧阈值之前。
	stale := time.Now().Unix() - int64(platform.PushLeaseStaleAfter/time.Second) - 60
	if _, err := db.Pool.Exec(ctx,
		`UPDATE push_idempotency SET updated_at = $2 WHERE key = $1`, key, stale); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	decision, err := store.Begin(ctx, key, runID, hash)
	if err != nil || decision.Kind != platform.IdemNew {
		t.Fatalf("stale takeover = %+v err=%v", decision, err)
	}

	// Release 只删自己的 processing 记录；他人归属不匹配时记录保留。
	if err := store.Release(ctx, key, runID, "wrong-hash"); err != nil {
		t.Fatalf("release with wrong hash: %v", err)
	}
	decision, _ = store.Begin(ctx, key, runID, hash)
	if decision.Kind != platform.IdemInProgress {
		t.Errorf("foreign release must not drop the lease: %+v", decision)
	}
	if err := store.Release(ctx, key, runID, hash); err != nil {
		t.Fatalf("release: %v", err)
	}
	decision, err = store.Begin(ctx, key, runID, hash)
	if err != nil || decision.Kind != platform.IdemNew {
		t.Fatalf("after release = %+v err=%v", decision, err)
	}
	// 租约已不属于旧请求：终态写入报 lease lost。
	if err := store.CompleteSync(ctx, key, runID, "stale-hash", "{}"); err != platform.ErrPushLeaseLost {
		t.Errorf("foreign complete = %v, want ErrPushLeaseLost", err)
	}
}

// TestSweepStaleProcessing 覆盖 A5：陈旧 processing 租约被清扫、新鲜
// processing 与全部终态行（含同期写入的）保留——终态按协议永不时间清理。
func TestSweepStaleProcessing(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store, err := postgres.NewPushIdempotencyStore(ctx, db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour).Unix()
	fresh := time.Now().Unix()
	rows := []struct {
		key, runID, status string
		updatedAt          int64
	}{
		{"it-sweep-stale", "run-sweep-stale", "processing", old},
		{"it-sweep-fresh", "run-sweep-fresh", "processing", fresh},
		{"it-sweep-done", "run-sweep-done", "sync_done", old}, // 终态：陈旧也保留
	}
	for _, row := range rows {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO push_idempotency (key, run_id, request_hash, status, response_body, updated_at)
			 VALUES ($1, $2, $3, $4, NULL, $5) ON CONFLICT DO NOTHING`,
			row.key, row.runID, "hash-"+row.key, row.status, row.updatedAt); err != nil {
			t.Fatalf("seed %s: %v", row.key, err)
		}
		t.Cleanup(func() {
			_, _ = db.Pool.Exec(ctx, `DELETE FROM push_idempotency WHERE key = $1`, row.key)
		})
	}

	removed, err := store.SweepStaleProcessing(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the stale processing row)", removed)
	}
	var alive int
	if err := db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM push_idempotency WHERE key IN ('it-sweep-fresh','it-sweep-done')`).Scan(&alive); err != nil {
		t.Fatal(err)
	}
	if alive != 2 {
		t.Errorf("survivors = %d, want 2 (fresh processing + stale terminal)", alive)
	}
}
