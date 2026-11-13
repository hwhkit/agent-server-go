package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/connector"
)

// TaskQueueStore 实现 connector.TaskQueue（agent_pull_tasks 表）。
// SQL 语义自旧栈 agent_pull_store.go 逐字移植：SKIP LOCKED 认领、
// 租约续期五分支、幂等 complete（重放/冲突/superseded 归档）、五步清扫。
type TaskQueueStore struct{ db *DB }

// NewTaskQueueStore 构建队列仓储。
func NewTaskQueueStore(db *DB) *TaskQueueStore { return &TaskQueueStore{db: db} }

var _ connector.TaskQueue = (*TaskQueueStore)(nil)

func nowNS() int64 { return time.Now().UnixNano() }

// pullTaskColumns 对可空列统一 COALESCE，保证 scan 进非指针字段。
const pullTaskColumns = `
	run_id, agent_slug, agent_revision, executor_kind, payload_json, state,
	COALESCE(lease_owner, ''), COALESCE(lease_expires_ns, 0), attempts,
	COALESCE(first_delivered_ns, 0), COALESCE(execution_dead_ns, 0),
	queue_expires_ns, abort_requested, COALESCE(result_json, ''),
	created_ns, COALESCE(completed_ns, 0)`

func scanPullTask(row pgx.Row) (*connector.Task, error) {
	var (
		t            connector.Task
		state        string
		payload      string
		leaseOwner   string
		leaseExpNS   int64
		firstDelivNS int64
		execDeadNS   int64
		queueExpNS   int64
		resultJSON   string
		createdNS    int64
		completedNS  int64
	)
	err := row.Scan(&t.RunID, &t.AgentSlug, &t.AgentRevision, &t.ExecutorKind, &payload,
		&state, &leaseOwner, &leaseExpNS, &t.Attempts, &firstDelivNS, &execDeadNS,
		&queueExpNS, &t.AbortRequested, &resultJSON, &createdNS, &completedNS)
	if err != nil {
		return nil, err
	}
	t.State = connector.TaskState(state)
	t.Payload = []byte(payload)
	t.Result = []byte(resultJSON)
	t.LeaseOwner = leaseOwner
	t.LeaseExpiresAt = nsTime(leaseExpNS)
	t.FirstDeliveredAt = nsTime(firstDelivNS)
	t.ExecutionDeadline = nsTime(execDeadNS)
	t.QueueExpiresAt = nsTime(queueExpNS)
	t.CreatedAt = nsTime(createdNS)
	t.CompletedAt = nsTime(completedNS)
	return &t, nil
}

func nsTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// Enqueue 入队并登记运行记录（同事务）。准入：queued+leased < AdmissionLimit。
func (s *TaskQueueStore) Enqueue(ctx context.Context, task connector.NewTask) error {
	now := nowNS()
	var count int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks
		 WHERE agent_slug = $1 AND state IN ('queued','leased')`,
		task.AgentSlug).Scan(&count); err != nil {
		return fmt.Errorf("count pull queue for %s: %w", task.AgentSlug, err)
	}
	if count >= task.AdmissionLimit {
		return connector.ErrQueueFull
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_pull_tasks
		   (run_id, agent_slug, agent_revision, executor_kind, payload_json, state,
		    execution_dead_ns, queue_expires_ns, created_ns, created_at)
		 VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$8,$9)`,
		task.RunID, task.AgentSlug, task.AgentRevision, task.ExecutorKind, string(task.Payload),
		now+task.ExecutionTimeout.Nanoseconds(), now+task.QueueTTL.Nanoseconds(),
		now, nowTS()); err != nil {
		return fmt.Errorf("insert pull task: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_runs
		   (agent_slug, run_id, agent_revision, executor_kind, execution_mode,
		    status, request_json, created_at)
		 VALUES ($1,$2,$3,$4,'remote_pull','running',$5,$6)
		 ON CONFLICT (agent_slug, run_id) DO NOTHING`,
		task.AgentSlug, task.RunID, task.AgentRevision, task.ExecutorKind,
		string(task.Payload), nowTS()); err != nil {
		return fmt.Errorf("record pull run: %w", err)
	}
	return tx.Commit(ctx)
}

// Get 读取任务快照。
func (s *TaskQueueStore) Get(ctx context.Context, runID string) (*connector.Task, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+pullTaskColumns+` FROM agent_pull_tasks WHERE run_id = $1`, runID)
	task, err := scanPullTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connector.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	return task, nil
}

// WaitTerminal 250ms 轮询直至终态（与旧栈节奏一致）。
func (s *TaskQueueStore) WaitTerminal(ctx context.Context, runID string) (connector.TaskState, []byte, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := s.Get(ctx, runID)
		if err != nil {
			return "", nil, err
		}
		if task.State.Terminal() {
			return task.State, task.Result, nil
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Claim 用 SKIP LOCKED 弹出一条 queued 任务并建立租约（单条 UPDATE…RETURNING）。
func (s *TaskQueueStore) Claim(ctx context.Context, req connector.ClaimRequest) (*connector.Task, connector.ClaimOutcome, error) {
	now := nowNS()
	var outstanding int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased'`).Scan(&outstanding); err != nil {
		return nil, connector.ClaimEmpty, fmt.Errorf("count outstanding leases: %w", err)
	}
	if req.Policy.MaxOutstandingLeases > 0 && outstanding >= req.Policy.MaxOutstandingLeases {
		return nil, connector.ClaimRateLimited, nil
	}
	if req.Capacity > 0 {
		var held int
		if err := s.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased' AND lease_owner = $1`,
			req.WorkerID).Scan(&held); err != nil {
			return nil, connector.ClaimEmpty, fmt.Errorf("count worker leases: %w", err)
		}
		if held >= req.Capacity {
			return nil, connector.ClaimRateLimited, nil
		}
	}
	var claimed string
	err := s.db.Pool.QueryRow(ctx, `
		UPDATE agent_pull_tasks t SET
		  state = 'leased',
		  lease_owner = $1,
		  lease_expires_ns = $2,
		  attempts = t.attempts + 1,
		  first_delivered_ns = COALESCE(t.first_delivered_ns, $3)
		WHERE t.run_id = (
		  SELECT run_id FROM agent_pull_tasks
		  WHERE state = 'queued' AND agent_slug = ANY($4)
		  ORDER BY created_ns ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1
		)
		RETURNING t.run_id`,
		req.WorkerID, now+req.Policy.LeaseTTL.Nanoseconds(), now, req.Slugs).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connector.ClaimEmpty, nil
	}
	if err != nil {
		return nil, connector.ClaimEmpty, fmt.Errorf("claim pull task: %w", err)
	}
	task, err := s.Get(ctx, claimed)
	if err != nil {
		return nil, connector.ClaimEmpty, fmt.Errorf("reload claimed task: %w", err)
	}
	return task, connector.ClaimDelivered, nil
}

// WorkerLeaseCount 返回该 worker 当前持有的活跃租约数（双实例检测：
// 服务端可见租约多于自报在途数即疑似分叉）。
func (s *TaskQueueStore) WorkerLeaseCount(ctx context.Context, workerID string) (int, error) {
	var count int
	err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased' AND lease_owner = $1`,
		workerID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count worker leases: %w", err)
	}
	return count, nil
}

// Renew 续租。五分支语义与 AGENT-PULL-API §2 一致。
func (s *TaskQueueStore) Renew(ctx context.Context, runID, workerID string, leaseTTL time.Duration) (connector.RenewOutcome, error) {
	now := nowNS()
	tag, err := s.db.Pool.Exec(ctx, `
		UPDATE agent_pull_tasks
		SET lease_expires_ns = $1
		WHERE run_id = $2 AND state = 'leased'
		  AND lease_owner = $3
		  AND lease_expires_ns > $4
		  AND execution_dead_ns > $4`,
		now+leaseTTL.Nanoseconds(), runID, workerID, now)
	if err != nil {
		return connector.RenewUnknown, fmt.Errorf("renew lease: %w", err)
	}
	if tag.RowsAffected() == 1 {
		task, err := s.Get(ctx, runID)
		if err != nil {
			return connector.RenewUnknown, err
		}
		if task.AbortRequested {
			return connector.RenewAbort, nil
		}
		return connector.RenewOK, nil
	}
	task, err := s.Get(ctx, runID)
	if errors.Is(err, connector.ErrTaskNotFound) {
		return connector.RenewUnknown, nil
	}
	if err != nil {
		return connector.RenewUnknown, err
	}
	switch {
	case task.State.Terminal():
		return connector.RenewUnknown, nil
	case !task.ExecutionDeadline.IsZero() && task.ExecutionDeadline.UnixNano() <= now:
		return connector.RenewDeadline, nil
	default:
		return connector.RenewLeaseLost, nil
	}
}

// RequestAbort 置取消位。
func (s *TaskQueueStore) RequestAbort(ctx context.Context, runID string) error {
	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_pull_tasks SET abort_requested = true WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("request abort: %w", err)
	}
	return nil
}

// Complete 幂等回传。重放判定用请求体 SHA-256；reaper 判死的终态任务，
// 迟到结果一律仅归档为 superseded。
func (s *TaskQueueStore) Complete(ctx context.Context, req connector.CompleteRequest) (connector.CompleteOutcome, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return connector.CompleteOutcome{}, fmt.Errorf("begin complete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		state       string
		owner       *string
		expiresNS   *int64
		storedJSON  *string
		createdNS   int64
		completedBy *string
	)
	err = tx.QueryRow(ctx, `
		SELECT state, lease_owner, lease_expires_ns, result_json, created_ns, completed_by
		FROM agent_pull_tasks WHERE run_id = $1 FOR UPDATE`,
		req.RunID).Scan(&state, &owner, &expiresNS, &storedJSON, &createdNS, &completedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return connector.CompleteOutcome{}, connector.ErrTaskNotFound
	}
	if err != nil {
		return connector.CompleteOutcome{}, fmt.Errorf("load task for complete: %w", err)
	}
	resultJSON := string(req.Result)

	current := connector.TaskState(state)
	if current.Terminal() {
		if completedBy != nil && *completedBy == "worker" {
			if storedJSON != nil && hashEqual(*storedJSON, resultJSON) {
				return connector.CompleteOutcome{Accepted: true, RunStatus: state}, nil
			}
			return connector.CompleteOutcome{Accepted: false, RunStatus: state, Conflict: true}, nil
		}
		if err := archiveSuperseded(ctx, tx, req.RunID, resultJSON); err != nil {
			return connector.CompleteOutcome{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return connector.CompleteOutcome{}, err
		}
		return connector.CompleteOutcome{Accepted: false, RunStatus: string(connector.TaskSuperseded)}, nil
	}

	now := nowNS()
	leaseValid := state == string(connector.TaskLeased) && owner != nil && *owner == req.WorkerID &&
		expiresNS != nil && *expiresNS > now
	if !leaseValid {
		if err := archiveSuperseded(ctx, tx, req.RunID, resultJSON); err != nil {
			return connector.CompleteOutcome{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return connector.CompleteOutcome{}, err
		}
		return connector.CompleteOutcome{Accepted: false, RunStatus: string(connector.TaskSuperseded)}, nil
	}

	finalState := mapCompleteStatus(req.Status)
	durationMS := (now - createdNS) / int64(time.Millisecond)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_pull_tasks SET
		  state = $2, result_json = $3, completed_ns = $4, completed_by = 'worker',
		  lease_owner = NULL, lease_expires_ns = NULL
		WHERE run_id = $1`,
		req.RunID, string(finalState), resultJSON, now); err != nil {
		return connector.CompleteOutcome{}, fmt.Errorf("finalize pull task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_runs SET
		  status = $2, result_json = $3, error_code = $4,
		  completed_at = $5, duration_ms = $6
		WHERE run_id = $1`,
		req.RunID, runStatusForRuns(finalState), resultJSON, nilIfEmpty(req.ErrorCode),
		nowTS(), durationMS); err != nil {
		return connector.CompleteOutcome{}, fmt.Errorf("finalize pull run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return connector.CompleteOutcome{}, err
	}
	return connector.CompleteOutcome{Accepted: true, RunStatus: string(finalState)}, nil
}

// QueueDepth 返回指定 slug 集合的 queued 任务数。
func (s *TaskQueueStore) QueueDepth(ctx context.Context, slugs []string) (int, error) {
	var depth int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state = 'queued' AND agent_slug = ANY($1)`,
		slugs).Scan(&depth); err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return depth, nil
}

// Sweep 五步清扫：过期租约重派 / 落终态、队列 TTL、agent_runs 同步、worker 离线。
func (s *TaskQueueStore) Sweep(ctx context.Context, policy connector.SweepPolicy) (connector.SweepStats, error) {
	var stats connector.SweepStats
	now := nowNS()

	tag, err := s.db.Pool.Exec(ctx, `
		UPDATE agent_pull_tasks SET state='queued', lease_owner=NULL, lease_expires_ns=NULL
		WHERE state='leased' AND lease_expires_ns < $1
		  AND attempts < $2 AND execution_dead_ns > $1 AND NOT abort_requested`,
		now, policy.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("requeue expired leases: %w", err)
	}
	stats.Requeued = tag.RowsAffected()

	tag, err = s.db.Pool.Exec(ctx, `
		UPDATE agent_pull_tasks t SET
		  state = CASE WHEN t.abort_requested THEN 'canceled' ELSE 'failed' END,
		  completed_ns = $1,
		  result_json = json_build_object('error', json_build_object(
		    'code', CASE WHEN t.abort_requested THEN 'aborted'
		                 WHEN t.execution_dead_ns <= $1 THEN 'task_timeout'
		                 ELSE 'lease_expired' END))::text
		WHERE t.state='leased' AND t.lease_expires_ns < $1
		  AND (t.attempts >= $2 OR t.execution_dead_ns <= $1 OR t.abort_requested)`,
		now, policy.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("terminate expired leases: %w", err)
	}
	stats.TerminalLease = tag.RowsAffected()

	tag, err = s.db.Pool.Exec(ctx, `
		UPDATE agent_pull_tasks SET state='failed', completed_ns=$1,
		  result_json = json_build_object('error', json_build_object('code','queue_timeout'))::text
		WHERE state='queued' AND queue_expires_ns < $1`, now)
	if err != nil {
		return stats, fmt.Errorf("expire queued tasks: %w", err)
	}
	stats.TerminalQueue = tag.RowsAffected()

	if stats.TerminalLease+stats.TerminalQueue > 0 {
		if _, err := s.db.Pool.Exec(ctx, `
			UPDATE agent_runs r SET
			  status = t.state, completed_at = $1::text,
			  result_json = t.result_json,
			  error_code = NULLIF(t.result_json, '')::jsonb #>> '{error,code}',
			  duration_ms = ($2 - t.created_ns) / 1000000
			FROM agent_pull_tasks t
			WHERE t.run_id = r.run_id AND r.status = 'running'
			  AND t.state IN ('failed','canceled')`,
			nowTS(), now); err != nil {
			return stats, fmt.Errorf("sync terminal runs: %w", err)
		}
	}

	renewInterval := policy.RenewInterval
	if renewInterval <= 0 {
		renewInterval = connector.DefaultRenewAfterSeconds * time.Second
	}
	threshold := int64(policy.WorkerOfflineAfterMiss) * renewInterval.Nanoseconds()
	tag, err = s.db.Pool.Exec(ctx,
		`UPDATE agent_pull_workers SET online=false, capacity=0, in_flight=0
		 WHERE online AND last_heartbeat_ns < $1`, now-threshold)
	if err != nil {
		return stats, fmt.Errorf("mark workers offline: %w", err)
	}
	stats.WorkersOffline = tag.RowsAffected()
	return stats, nil
}

// RecordDirectRun 幂等登记机器侧直接提交的运行（POST /agent-api/v1/runs）：
// 直接落 agent_runs 终态，不经 agent_pull_tasks 租约；同 (slug, runId)
// 重放返回既有状态。slug 未注册返回 connector.ErrUnknownAgent。
func (s *TaskQueueStore) RecordDirectRun(ctx context.Context, run connector.DirectRun) (string, error) {
	var exists bool
	err := s.db.Pool.QueryRow(ctx,
		`SELECT true FROM agents WHERE slug = $1`, run.Slug).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", connector.ErrUnknownAgent
	}
	if err != nil {
		return "", fmt.Errorf("check agent for direct run: %w", err)
	}
	var status string
	err = s.db.Pool.QueryRow(ctx, `
		INSERT INTO agent_runs
		  (agent_slug, run_id, agent_revision, executor_kind, execution_mode,
		   status, duration_ms, error_code, request_json, result_json,
		   created_at, completed_at)
		VALUES ($1,$2,(SELECT revision FROM agents WHERE slug=$1),'remote-pull','remote_pull',
		        $3,$4,NULLIF($5,''),$6,$7,$8,$8)
		ON CONFLICT (agent_slug, run_id) DO NOTHING
		RETURNING status`,
		run.Slug, run.RunID, run.Status, run.DurationMs, run.ErrorCode,
		string(run.Input), string(run.Result), nowTS()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// 重放：读既有终态。
		err = s.db.Pool.QueryRow(ctx,
			`SELECT status FROM agent_runs WHERE agent_slug = $1 AND run_id = $2`,
			run.Slug, run.RunID).Scan(&status)
		if err != nil {
			return "", fmt.Errorf("load replayed direct run: %w", err)
		}
		return status, nil
	}
	if err != nil {
		return "", fmt.Errorf("record direct run: %w", err)
	}
	return status, nil
}

// archiveSuperseded 把迟到结果归档进 agent_runs(status=superseded)。
func archiveSuperseded(ctx context.Context, tx pgx.Tx, runID, resultJSON string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (agent_slug, run_id, agent_revision, executor_kind,
		                        execution_mode, status, request_json, result_json,
		                        created_at, completed_at)
		SELECT agent_slug, run_id, agent_revision, executor_kind, 'remote_pull',
		       'superseded', payload_json, $2, $3, $3
		FROM agent_pull_tasks WHERE run_id = $1
		ON CONFLICT (agent_slug, run_id) DO UPDATE
		  SET status='superseded', result_json=$2, completed_at=$3`,
		runID, resultJSON, nowTS()); err != nil {
		return fmt.Errorf("archive superseded result: %w", err)
	}
	return nil
}

func mapCompleteStatus(status string) connector.TaskState {
	switch status {
	case "succeeded":
		return connector.TaskCompleted
	case "aborted":
		return connector.TaskCanceled
	default:
		return connector.TaskFailed
	}
}

func runStatusForRuns(state connector.TaskState) string {
	switch state {
	case connector.TaskCompleted:
		return "completed"
	case connector.TaskCanceled:
		return "canceled"
	default:
		return "failed"
	}
}

func hashEqual(a, b string) bool {
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sumA[:]) == hex.EncodeToString(sumB[:])
}

// QueueTotals 返回队列在途统计（仪表盘）。
func (s *TaskQueueStore) QueueTotals(ctx context.Context) (connector.QueueTotals, error) {
	var totals connector.QueueTotals
	err := s.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE state = 'queued'),
		       COUNT(*) FILTER (WHERE state = 'leased')
		FROM agent_pull_tasks`).Scan(&totals.Queued, &totals.Leased)
	if err != nil {
		return totals, fmt.Errorf("queue totals: %w", err)
	}
	return totals, nil
}
