package postgres_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/infra/postgres"
)

// 队列集成测试：认领/租约/幂等回传/清扫的 SQL 语义（与 AGENT-PULL-API 对齐）。
// 门禁与 store_test.go 相同：TEST_DATABASE_URL 未设置时跳过。

// sweepStale 清掉此前测试运行遗留的过期租约（agent_pull_tasks 是共享表，
// 全局在途租约计数会被陈旧 leased 行顶满）。MaxAttempts=1 让所有过期租约
// 直接落终态而不是回队。
func sweepStale(t *testing.T, q *postgres.TaskQueueStore) {
	t.Helper()
	if _, err := q.Sweep(context.Background(), connector.SweepPolicy{
		MaxAttempts: 1, WorkerOfflineAfterMiss: 3, RenewInterval: 30 * time.Second,
	}); err != nil {
		t.Fatalf("pre-test sweep: %v", err)
	}
}

func uniqueRunID(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func enqueueTask(t *testing.T, q *postgres.TaskQueueStore, runID, slug string, limit int, ttl, queueTTL time.Duration) {
	t.Helper()
	err := q.Enqueue(context.Background(), connector.NewTask{
		RunID: runID, AgentSlug: slug, AgentRevision: 1, ExecutorKind: "remote-pull",
		Payload: []byte(`{"taskBrief":"t"}`), ExecutionTimeout: ttl,
		QueueTTL: queueTTL, AdmissionLimit: limit,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", runID, err)
	}
}

func TestQueueAdmissionAndClaim(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	q := postgres.NewTaskQueueStore(db)
	sweepStale(t, q)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "it-q-" + suffix
	w1, w2, w3 := "w1-"+suffix, "w2-"+suffix, "w3-"+suffix

	// 准入上限 = queued+leased < 2。
	enqueueTask(t, q, uniqueRunID("q1"), slug, 2, time.Hour, time.Hour)
	enqueueTask(t, q, uniqueRunID("q2"), slug, 2, time.Hour, time.Hour)
	err := q.Enqueue(ctx, connector.NewTask{
		RunID: uniqueRunID("q3"), AgentSlug: slug, AgentRevision: 1,
		Payload: []byte(`{}`), ExecutionTimeout: time.Hour, QueueTTL: time.Hour, AdmissionLimit: 2,
	})
	if err != connector.ErrQueueFull {
		t.Fatalf("third enqueue = %v, want ErrQueueFull", err)
	}

	policy := connector.ClaimPolicy{LeaseTTL: 90 * time.Second, MaxOutstandingLeases: 64}
	// FIFO：先入先出。
	task, outcome, err := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug}, Capacity: 4, Policy: policy})
	if err != nil || outcome != connector.ClaimDelivered || task.Attempts != 1 || task.LeaseOwner != w1 {
		t.Fatalf("claim = %+v/%v/%v", task, outcome, err)
	}
	if task.FirstDeliveredAt.IsZero() || task.ExecutionDeadline.IsZero() {
		t.Error("claim must stamp first delivery and deadline")
	}
	// scope 外的 slug 认领不到。
	_, outcome, err = q.Claim(ctx, connector.ClaimRequest{WorkerID: w2, Slugs: []string{"other-slug"}, Policy: policy})
	if err != nil || outcome != connector.ClaimEmpty {
		t.Fatalf("out-of-scope claim = %v/%v", outcome, err)
	}
	// worker capacity=1：w1 已持有 1 个租约，再认领被限流。
	_, outcome, err = q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug}, Capacity: 1, Policy: policy})
	if err != nil || outcome != connector.ClaimRateLimited {
		t.Fatalf("capacity claim = %v/%v", outcome, err)
	}
	// 全局在途租约上限。
	_, outcome, err = q.Claim(ctx, connector.ClaimRequest{WorkerID: w3, Slugs: []string{slug},
		Policy: connector.ClaimPolicy{LeaseTTL: time.Minute, MaxOutstandingLeases: 0}})
	// MaxOutstandingLeases=0 表示不设全局闸（跳过），应正常派发第二条。
	if err != nil || outcome != connector.ClaimDelivered {
		t.Fatalf("global-cap-off claim = %v/%v", outcome, err)
	}
	// 双实例检测：w1 仍持有 1 个活跃租约。
	held, err := q.WorkerLeaseCount(ctx, w1)
	if err != nil || held != 1 {
		t.Fatalf("worker lease count = %d/%v, want 1", held, err)
	}
}

func TestQueueRenewBranches(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	q := postgres.NewTaskQueueStore(db)
	sweepStale(t, q)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "it-q-renew-" + suffix
	w1, intruder := "w1-"+suffix, "intruder-"+suffix
	runID := uniqueRunID("r")
	enqueueTask(t, q, runID, slug, 4, time.Hour, time.Hour)

	policy := connector.ClaimPolicy{LeaseTTL: 90 * time.Second}
	if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug}, Policy: policy}); outcome != connector.ClaimDelivered {
		t.Fatal("claim failed")
	}
	if got, err := q.Renew(ctx, runID, w1, 90*time.Second); got != connector.RenewOK || err != nil {
		t.Errorf("owner renew = %v/%v", got, err)
	}
	if got, _ := q.Renew(ctx, runID, intruder, 90*time.Second); got != connector.RenewLeaseLost {
		t.Errorf("foreign renew = %v, want LeaseLost", got)
	}
	if err := q.RequestAbort(ctx, runID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got, _ := q.Renew(ctx, runID, w1, 90*time.Second); got != connector.RenewAbort {
		t.Errorf("renew after abort = %v, want Abort", got)
	}
	if got, _ := q.Renew(ctx, "run-does-not-exist", w1, time.Minute); got != connector.RenewUnknown {
		t.Errorf("unknown run renew = %v, want Unknown", got)
	}

	// 墙钟已过 → Deadline。
	lateSlug := "it-q-late-" + suffix
	lateRun := uniqueRunID("late")
	enqueueTask(t, q, lateRun, lateSlug, 4, time.Hour, time.Hour)
	if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{lateSlug}, Policy: policy}); outcome != connector.ClaimDelivered {
		t.Fatal("claim late failed")
	}
	// 把 execution_dead_ns 拨到过去，模拟墙钟耗尽。
	if _, err := db.Pool.Exec(ctx,
		`UPDATE agent_pull_tasks SET execution_dead_ns = $2 WHERE run_id = $1`,
		lateRun, time.Now().Add(-time.Minute).UnixNano()); err != nil {
		t.Fatalf("backdate deadline: %v", err)
	}
	if got, _ := q.Renew(ctx, lateRun, w1, 90*time.Second); got != connector.RenewDeadline {
		t.Errorf("renew past deadline = %v, want Deadline", got)
	}
}

func TestQueueCompleteIdempotencyAndSupersede(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	q := postgres.NewTaskQueueStore(db)
	sweepStale(t, q)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "it-q-cmp-" + suffix
	w1, wA := "w1-"+suffix, "wA-"+suffix
	runID := uniqueRunID("c")
	enqueueTask(t, q, runID, slug, 4, time.Hour, time.Hour)
	policy := connector.ClaimPolicy{LeaseTTL: 90 * time.Second}
	if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug}, Policy: policy}); outcome != connector.ClaimDelivered {
		t.Fatal("claim failed")
	}

	result := []byte(`{"status":"succeeded","payload":{"summary":"ok"}}`)
	out, err := q.Complete(ctx, connector.CompleteRequest{RunID: runID, WorkerID: w1, Status: "succeeded", Result: result})
	if err != nil || !out.Accepted || out.RunStatus != "completed" {
		t.Fatalf("complete = %+v/%v", out, err)
	}
	// 同体重复回传 → 幂等重放 accepted。
	out, err = q.Complete(ctx, connector.CompleteRequest{RunID: runID, WorkerID: w1, Status: "succeeded", Result: result})
	if err != nil || !out.Accepted {
		t.Errorf("replay = %+v/%v, want accepted", out, err)
	}
	// 不同体 → RESULT_CONFLICT，以首次为准。
	out, err = q.Complete(ctx, connector.CompleteRequest{RunID: runID, WorkerID: w1, Status: "succeeded", Result: []byte(`{"status":"succeeded","payload":{"summary":"different"}}`)})
	if err != nil || out.Accepted || !out.Conflict {
		t.Errorf("conflicting replay = %+v/%v, want conflict", out, err)
	}
	task, _ := q.Get(ctx, runID)
	if task.State != connector.TaskCompleted {
		t.Errorf("state = %s", task.State)
	}

	// 租约失效后迟到 → 仅归档 superseded。
	lateSlug := "it-q-late2-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	lateRun := uniqueRunID("l")
	enqueueTask(t, q, lateRun, lateSlug, 4, time.Hour, time.Hour)
	if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: wA, Slugs: []string{lateSlug},
		Policy: connector.ClaimPolicy{LeaseTTL: time.Nanosecond}}); outcome != connector.ClaimDelivered {
		t.Fatal("claim with tiny ttl failed")
	}
	time.Sleep(5 * time.Millisecond) // 租约即刻过期
	out, err = q.Complete(ctx, connector.CompleteRequest{RunID: lateRun, WorkerID: wA, Status: "succeeded", Result: result})
	if err != nil || out.Accepted || out.RunStatus != "superseded" {
		t.Errorf("late complete = %+v/%v, want superseded", out, err)
	}
	// 未知 runId。
	if _, err := q.Complete(ctx, connector.CompleteRequest{RunID: "no-such-run", WorkerID: "w", Status: "succeeded"}); err != connector.ErrTaskNotFound {
		t.Errorf("unknown complete = %v", err)
	}
}

func TestQueueSweepRedispatchAndTTL(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	q := postgres.NewTaskQueueStore(db)
	sweepStale(t, q)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "it-q-sw-" + suffix
	w1 := "w1-" + suffix

	// 过期租约 + attempts 未到上限 → 重回队列。
	runA := uniqueRunID("a")
	enqueueTask(t, q, runA, slug, 8, time.Hour, time.Hour)
	if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug},
		Policy: connector.ClaimPolicy{LeaseTTL: time.Nanosecond}}); outcome != connector.ClaimDelivered {
		t.Fatal("claim a failed")
	}
	time.Sleep(5 * time.Millisecond)
	stats, err := q.Sweep(ctx, connector.SweepPolicy{MaxAttempts: 3, WorkerOfflineAfterMiss: 3, RenewInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Requeued < 1 {
		t.Errorf("requeued = %d", stats.Requeued)
	}
	task, _ := q.Get(ctx, runA)
	if task.State != connector.TaskQueued || task.Attempts != 1 {
		t.Errorf("task after sweep = %s attempts=%d", task.State, task.Attempts)
	}

	// attempts 到上限的过期租约 → failed(lease_expired)，且 agent_runs 同步终态。
	for i := 0; i < 2; i++ {
		if _, outcome, _ := q.Claim(ctx, connector.ClaimRequest{WorkerID: w1, Slugs: []string{slug},
			Policy: connector.ClaimPolicy{LeaseTTL: time.Nanosecond}}); outcome != connector.ClaimDelivered {
			t.Fatalf("reclaim %d failed", i)
		}
		time.Sleep(5 * time.Millisecond)
		if i == 0 {
			if _, err := q.Sweep(ctx, connector.SweepPolicy{MaxAttempts: 3, WorkerOfflineAfterMiss: 3, RenewInterval: 30 * time.Second}); err != nil {
				t.Fatalf("mid sweep: %v", err)
			}
		}
	}
	stats, err = q.Sweep(ctx, connector.SweepPolicy{MaxAttempts: 3, WorkerOfflineAfterMiss: 3, RenewInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("final sweep: %v", err)
	}
	if stats.TerminalLease < 1 {
		t.Errorf("terminal lease = %d", stats.TerminalLease)
	}
	task, _ = q.Get(ctx, runA)
	if task.State != connector.TaskFailed {
		t.Errorf("exhausted task state = %s", task.State)
	}

	// 队列 TTL 耗尽 → failed(queue_timeout)。
	ttlSlug := "it-q-ttl-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	runB := uniqueRunID("b")
	enqueueTask(t, q, runB, ttlSlug, 8, time.Hour, time.Nanosecond)
	time.Sleep(5 * time.Millisecond)
	stats, err = q.Sweep(ctx, connector.SweepPolicy{MaxAttempts: 3, WorkerOfflineAfterMiss: 3, RenewInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("ttl sweep: %v", err)
	}
	if stats.TerminalQueue < 1 {
		t.Errorf("terminal queue = %d", stats.TerminalQueue)
	}
	task, _ = q.Get(ctx, runB)
	if task.State != connector.TaskFailed {
		t.Errorf("ttl task state = %s", task.State)
	}
}
