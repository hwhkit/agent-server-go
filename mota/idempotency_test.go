package mota

// Push 幂等状态的行为测试。对应 Rust 侧 src/mota/idempotency.rs 的语义。
//
// 两个后端跑同一套用例:内存后端(单元测试用)与 PostgreSQL 后端(生产用)。
// 后者未配置 TEST_DATABASE_URL 时跳过。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// forEachPushIdempotencyBackend 对两个后端各跑一遍同一份用例。
//
// 两套实现必须给出逐字段一致的决策,否则「本地测试绿、生产行为不同」。
func forEachPushIdempotencyBackend(t *testing.T, fn func(t *testing.T, store *PushIdempotencyStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		fn(t, NewInMemoryPushIdempotencyStore())
	})
	t.Run("postgres", func(t *testing.T) {
		databaseURL := requireTestDatabase(t)
		store, err := ConnectPushIdempotencyStore(context.Background(), databaseURL)
		if err != nil {
			t.Fatalf("connect Push idempotency store: %v", err)
		}
		t.Cleanup(store.Close)
		fn(t, store)
	})
}

// pushLease 是一组自洽的幂等三元组,键唯一,避免与其它用例互相干扰。
type pushLease struct {
	key         string
	runID       string
	requestHash string
}

func newPushLease() pushLease {
	return pushLease{
		key:         unique("idem-key"),
		runID:       unique("idem-run"),
		requestHash: unique("idem-hash"),
	}
}

func mustBegin(t *testing.T, store *PushIdempotencyStore, lease pushLease) *PushDecision {
	t.Helper()
	decision, err := store.Begin(context.Background(), lease.key, lease.runID, lease.requestHash)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return decision
}

func requireDecision(t *testing.T, decision *PushDecision, expected PushDecisionKind) {
	t.Helper()
	if decision.Kind != expected {
		t.Fatalf("expected decision %s, got %s", pushDecisionName(expected), pushDecisionName(decision.Kind))
	}
}

func pushDecisionName(kind PushDecisionKind) string {
	switch kind {
	case PushNew:
		return "PushNew"
	case PushInProgress:
		return "PushInProgress"
	case PushSyncCached:
		return "PushSyncCached"
	case PushSyncFailed:
		return "PushSyncFailed"
	case PushAsyncAccepted:
		return "PushAsyncAccepted"
	case PushConflict:
		return "PushConflict"
	default:
		return "unknown"
	}
}

// backdatePushLease 把记录的 updated_at 往前拨,用来构造陈旧租约。
//
// 租约陈旧阈值是 15 分钟,测试不可能真等,只能直接改时间戳。
func backdatePushLease(t *testing.T, store *PushIdempotencyStore, key string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Unix() - int64(age.Seconds())
	if store.IsInMemory() {
		store.mu.Lock()
		defer store.mu.Unlock()
		record, ok := store.records[key]
		if !ok {
			t.Fatalf("no in-memory record for key %q", key)
		}
		record.updatedAt = stamp
		return
	}
	tag, err := store.pool.Exec(context.Background(),
		`UPDATE push_idempotency SET updated_at = $2 WHERE key = $1`, key, stamp)
	if err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate lease: expected 1 row, got %d", tag.RowsAffected())
	}
}

// forceSyncDoneWithoutBody 构造「状态是 sync_done 但没有响应体」的不自洽记录。
func forceSyncDoneWithoutBody(t *testing.T, store *PushIdempotencyStore, key string) {
	t.Helper()
	if store.IsInMemory() {
		store.mu.Lock()
		defer store.mu.Unlock()
		record, ok := store.records[key]
		if !ok {
			t.Fatalf("no in-memory record for key %q", key)
		}
		record.status = pushStatusSyncDone
		record.hasResponse = false
		record.responseBody = ""
		return
	}
	tag, err := store.pool.Exec(context.Background(),
		`UPDATE push_idempotency SET status = $2, response_body = NULL WHERE key = $1`,
		key, pushStatusSyncDone)
	if err != nil {
		t.Fatalf("force sync_done without body: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("force sync_done without body: expected 1 row, got %d", tag.RowsAffected())
	}
}

// ---------------------------------------------------------------------------
// Begin 的五个分支
// ---------------------------------------------------------------------------

// 无记录 → 新租约。
func TestPushIdempotencyBeginGrantsNewLease(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		requireDecision(t, mustBegin(t, store, newPushLease()), PushNew)
	})
}

// 同键第二次进入 → InProgress,不会重复执行。
func TestPushIdempotencyBeginReturnsInProgressWhileRunning(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)
		requireDecision(t, mustBegin(t, store, lease), PushInProgress)
	})
}

// 同一个幂等键换 runId → Conflict。
func TestPushIdempotencyBeginConflictsOnDifferentRunID(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		other := lease
		other.runID = unique("idem-run-other")
		requireDecision(t, mustBegin(t, store, other), PushConflict)
	})
}

// 同一个幂等键换请求体 → Conflict。
func TestPushIdempotencyBeginConflictsOnDifferentRequestHash(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		other := lease
		other.requestHash = unique("idem-hash-other")
		requireDecision(t, mustBegin(t, store, other), PushConflict)
	})
}

// runId 在签名 body 内，幂等键却是未签名 header：同一个已签 runId
// 换一个键也不能再执行。
func TestPushIdempotencyRunIDCannotMoveToDifferentKey(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		other := lease
		other.key = unique("idem-key-other")
		requireDecision(t, mustBegin(t, store, other), PushConflict)
	})
}

// sync_done → 回放缓存的响应体。
func TestPushIdempotencyCompleteSyncReplaysBody(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		body := `{"contract_version":"V2","payload_result":{"assessments":[]}}`
		if err := store.CompleteSync(context.Background(),
			lease.key, lease.runID, lease.requestHash, body); err != nil {
			t.Fatalf("CompleteSync: %v", err)
		}

		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushSyncCached)
		if decision.ResponseBody != body {
			t.Fatalf("replayed body mismatch:\n got: %s\nwant: %s", decision.ResponseBody, body)
		}
	})
}

func TestPushIdempotencyFailSyncIsTerminal(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)
		if err := store.FailSync(context.Background(),
			lease.key, lease.runID, lease.requestHash, "invalid_result"); err != nil {
			t.Fatalf("FailSync: %v", err)
		}
		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushSyncFailed)
		if decision.ResponseBody != "invalid_result" {
			t.Fatalf("failure reason = %q", decision.ResponseBody)
		}
	})
}

func TestPushIdempotencyFinalizationSurvivesCanceledContext(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		body := `{"contract_version":"V2"}`
		if err := store.CompleteSync(cancelled,
			lease.key, lease.runID, lease.requestHash, body); err != nil {
			t.Fatalf("CompleteSync with canceled request context: %v", err)
		}
		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushSyncCached)
		if decision.ResponseBody != body {
			t.Fatalf("response body = %q", decision.ResponseBody)
		}
	})
}

func TestPushIdempotencyReleaseSurvivesCanceledContext(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.Release(cancelled, lease.key, lease.runID, lease.requestHash); err != nil {
			t.Fatalf("Release with canceled request context: %v", err)
		}
		requireDecision(t, mustBegin(t, store, lease), PushNew)
	})
}

// async_accepted → 回放 202,不带响应体。
func TestPushIdempotencyAcceptAsyncReplaysAccepted(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		if err := store.AcceptAsync(context.Background(),
			lease.key, lease.runID, lease.requestHash); err != nil {
			t.Fatalf("AcceptAsync: %v", err)
		}

		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushAsyncAccepted)
		if decision.ResponseBody != "" {
			t.Fatalf("async replay must not carry a body, got %q", decision.ResponseBody)
		}
	})
}

// ---------------------------------------------------------------------------
// 租约接管
// ---------------------------------------------------------------------------

// processing 超过 15 分钟 → 允许接管,返回 New。
func TestPushIdempotencyStaleLeaseIsTakenOver(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		backdatePushLease(t, store, lease.key, 16*time.Minute)
		requireDecision(t, mustBegin(t, store, lease), PushNew)
	})
}

// 未到阈值的 processing 不被抢占 —— 抢早了等于重复执行一次有副作用的任务。
func TestPushIdempotencyFreshLeaseIsNotTakenOver(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		backdatePushLease(t, store, lease.key, 14*time.Minute)
		requireDecision(t, mustBegin(t, store, lease), PushInProgress)
	})
}

// 已完成的记录永不被接管,哪怕早就过了陈旧阈值。
func TestPushIdempotencyCompletedRecordIsNeverTakenOver(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		body := `{"contract_version":"V2"}`
		if err := store.CompleteSync(context.Background(),
			lease.key, lease.runID, lease.requestHash, body); err != nil {
			t.Fatalf("CompleteSync: %v", err)
		}
		backdatePushLease(t, store, lease.key, 24*time.Hour)

		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushSyncCached)
		if decision.ResponseBody != body {
			t.Fatalf("replayed body mismatch: %s", decision.ResponseBody)
		}
	})
}

// 状态已完成却没有响应体是不自洽的:判 Conflict,而不是回放空响应。
func TestPushIdempotencySyncDoneWithoutBodyIsConflict(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		forceSyncDoneWithoutBody(t, store, lease.key)
		requireDecision(t, mustBegin(t, store, lease), PushConflict)
	})
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

// 释放自己的 processing 租约后,同键请求可以重新执行。
func TestPushIdempotencyReleaseFreesOwnProcessingLease(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		if err := store.Release(context.Background(),
			lease.key, lease.runID, lease.requestHash); err != nil {
			t.Fatalf("Release: %v", err)
		}
		requireDecision(t, mustBegin(t, store, lease), PushNew)
	})
}

// 不是自己的租约不能释放,否则一个请求能把别人的执行状态抹掉。
func TestPushIdempotencyReleaseIgnoresForeignLease(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		if err := store.Release(context.Background(),
			lease.key, unique("other-run"), lease.requestHash); err != nil {
			t.Fatalf("Release: %v", err)
		}
		requireDecision(t, mustBegin(t, store, lease), PushInProgress)
	})
}

// 已完成的记录不会被 Release 删掉 —— 否则缓存的响应会凭空消失。
func TestPushIdempotencyReleaseKeepsCompletedRecord(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)

		body := `{"contract_version":"V2"}`
		if err := store.CompleteSync(context.Background(),
			lease.key, lease.runID, lease.requestHash, body); err != nil {
			t.Fatalf("CompleteSync: %v", err)
		}
		if err := store.Release(context.Background(),
			lease.key, lease.runID, lease.requestHash); err != nil {
			t.Fatalf("Release: %v", err)
		}

		decision := mustBegin(t, store, lease)
		requireDecision(t, decision, PushSyncCached)
		if decision.ResponseBody != body {
			t.Fatalf("replayed body mismatch: %s", decision.ResponseBody)
		}
	})
}

// 租约丢失后提交终态必须报错,而不是静默写出一条无主记录。
func TestPushIdempotencyCompleteSyncFailsAfterLeaseLost(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()
		requireDecision(t, mustBegin(t, store, lease), PushNew)
		if err := store.Release(context.Background(),
			lease.key, lease.runID, lease.requestHash); err != nil {
			t.Fatalf("Release: %v", err)
		}

		err := store.CompleteSync(context.Background(),
			lease.key, lease.runID, lease.requestHash, `{}`)
		if !errors.Is(err, errPushLeaseLost) {
			t.Fatalf("expected errPushLeaseLost, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

// 并发 Begin 只能有一个拿到 New,其余都是 InProgress。
//
// 这条守护的是 PostgreSQL 路径:行不存在时 FOR UPDATE 锁不到东西,插入必须
// ON CONFLICT DO NOTHING,否则后到的请求会撞唯一键约束变成 500。
func TestPushIdempotencyConcurrentBeginGrantsSingleLease(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		lease := newPushLease()

		const callers = 12
		var newCount, inProgressCount atomic.Int64
		errs := make(chan error, callers)

		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				decision, err := store.Begin(context.Background(),
					lease.key, lease.runID, lease.requestHash)
				if err != nil {
					errs <- err
					return
				}
				switch decision.Kind {
				case PushNew:
					newCount.Add(1)
				case PushInProgress:
					inProgressCount.Add(1)
				default:
					errs <- errors.New("unexpected decision " + pushDecisionName(decision.Kind))
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}

		if got := newCount.Load(); got != 1 {
			t.Fatalf("expected exactly 1 PushNew, got %d", got)
		}
		if got := inProgressCount.Load(); got != callers-1 {
			t.Fatalf("expected %d PushInProgress, got %d", callers-1, got)
		}
	})
}

func TestPushIdempotencyConcurrentDifferentKeysShareSingleRunFence(t *testing.T) {
	forEachPushIdempotencyBackend(t, func(t *testing.T, store *PushIdempotencyStore) {
		base := newPushLease()
		leases := []pushLease{base, base}
		leases[1].key = unique("idem-key-other")

		results := make(chan PushDecisionKind, len(leases))
		errs := make(chan error, len(leases))
		var wg sync.WaitGroup
		for _, lease := range leases {
			lease := lease
			wg.Add(1)
			go func() {
				defer wg.Done()
				decision, err := store.Begin(
					context.Background(), lease.key, lease.runID, lease.requestHash)
				if err != nil {
					errs <- err
					return
				}
				results <- decision.Kind
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		var newCount, conflictCount int
		for kind := range results {
			switch kind {
			case PushNew:
				newCount++
			case PushConflict:
				conflictCount++
			default:
				t.Fatalf("unexpected decision %s", pushDecisionName(kind))
			}
		}
		if newCount != 1 || conflictCount != 1 {
			t.Fatalf("new=%d conflict=%d, want one of each", newCount, conflictCount)
		}
	})
}
