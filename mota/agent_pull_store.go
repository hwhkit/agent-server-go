package mota

// Agent Pull Connector 的 PostgreSQL 存储:任务队列、租约状态机、连接器与 worker。
// 端点契约与状态机依据 doc/AGENT-PULL-API.md §6/§10。
//
// 设计要点:
//   - 队列、租约、重试计数全部落库,平台重启/发版后原样存活(§8);
//   - 容量约束用 SQL 计数而非内存信号量:remote-pull 不消耗本机
//     GlobalExecution 闸,由「每 Agent 容量 + 全局在途租约上限」约束(§6.4);
//   - 时间列用 unix 纳秒 BIGINT(比较与索引),审计时间沿用 TEXT RFC3339。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PullTaskState 是 agent_pull_tasks.state 的取值。
type PullTaskState string

const (
	PullTaskQueued     PullTaskState = "queued"
	PullTaskLeased     PullTaskState = "leased"
	PullTaskCompleted  PullTaskState = "completed"
	PullTaskFailed     PullTaskState = "failed"
	PullTaskCanceled   PullTaskState = "canceled"
	PullTaskSuperseded PullTaskState = "superseded"
)

// pullTerminalStates 是不可再变更的终态集合。
var pullTerminalStates = map[PullTaskState]bool{
	PullTaskCompleted:  true,
	PullTaskFailed:     true,
	PullTaskCanceled:   true,
	PullTaskSuperseded: true,
}

// PullTask 是一条队列任务的完整快照。
type PullTask struct {
	RunID            string
	AgentSlug        string
	AgentRevision    int64
	ExecutorKind     string
	PayloadJSON      string
	State            PullTaskState
	LeaseOwner       string
	LeaseExpiresNS   int64
	Attempts         int
	FirstDeliveredNS int64
	ExecutionDeadNS  int64
	QueueExpiresNS   int64
	AbortRequested   bool
	ResultJSON       string
	CreatedNS        int64
	CompletedNS      int64
}

// PullOverloadedError 表示 Agent 队列(queued+leased)已满,对应 agent_overloaded。
var PullOverloadedError = errors.New("pull queue is full")

// PullGlobalCapError 表示全局在途租约达到上限(§6.4)。
var PullGlobalCapError = errors.New("global outstanding lease cap reached")

// AgentPullStore 承载 agent_pull_* 表的全部访问。
type AgentPullStore struct {
	pool *pgxpool.Pool
}

// agentPullSchemaStatements 返回建表 DDL;由 AgentStore.initialize 统一执行,
// 保证一个迁移点。
func agentPullSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS agent_pull_tasks (
                run_id              TEXT PRIMARY KEY,
                agent_slug          TEXT NOT NULL,
                agent_revision      BIGINT NOT NULL,
                executor_kind       TEXT NOT NULL DEFAULT '',
                payload_json        TEXT NOT NULL,
                state               TEXT NOT NULL,
                lease_owner         TEXT,
                lease_expires_ns    BIGINT,
                attempts            INT NOT NULL DEFAULT 0,
                first_delivered_ns  BIGINT,
                execution_dead_ns   BIGINT,
                queue_expires_ns    BIGINT NOT NULL,
                abort_requested     BOOLEAN NOT NULL DEFAULT false,
                result_json         TEXT,
                completed_by        TEXT,
                created_ns          BIGINT NOT NULL,
                created_at          TEXT NOT NULL,
                completed_ns        BIGINT)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_pop ON agent_pull_tasks(agent_slug, created_ns) WHERE state = 'queued'`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_lease_expiry ON agent_pull_tasks(lease_expires_ns) WHERE state = 'leased'`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_queue_expiry ON agent_pull_tasks(queue_expires_ns) WHERE state = 'queued'`,
		agentRunsSchemaSQL,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_created ON agent_runs(created_at DESC)`,
		`ALTER TABLE agent_pull_tasks ADD COLUMN IF NOT EXISTS completed_by TEXT`,
		`CREATE TABLE IF NOT EXISTS agent_pull_workers (
                worker_id         TEXT NOT NULL,
                connector_id      BIGINT NOT NULL,
                in_flight         INT NOT NULL DEFAULT 0,
                capacity          INT NOT NULL DEFAULT 0,
                capabilities      TEXT NOT NULL DEFAULT '',
                last_heartbeat_ns BIGINT NOT NULL,
                online            BOOLEAN NOT NULL DEFAULT true,
                PRIMARY KEY(worker_id, connector_id))`,
		`CREATE TABLE IF NOT EXISTS agent_pull_connectors (
                id             BIGSERIAL PRIMARY KEY,
                name           TEXT NOT NULL UNIQUE,
                token_hash     TEXT NOT NULL UNIQUE,
                scopes         TEXT NOT NULL DEFAULT '',
                status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','rotated')),
                grace_until_ns BIGINT,
                created_at     TEXT NOT NULL)`,
	}
}

// NewAgentPullStore 在既有连接池上构建(与平台存储共用同一个库)。
func NewAgentPullStore(pool *pgxpool.Pool) (*AgentPullStore, error) {
	store := &AgentPullStore{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range agentPullSchemaStatements() {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return nil, fmt.Errorf("agent pull schema: %w", err)
		}
	}
	return store, nil
}

func nowNS() int64 { return time.Now().UnixNano() }

// pullTaskColumns 对可空列统一 COALESCE,保证 scan 进非指针字段。
const pullTaskColumns = `
	run_id, agent_slug, agent_revision, executor_kind, payload_json, state,
	COALESCE(lease_owner, ''), COALESCE(lease_expires_ns, 0), attempts,
	COALESCE(first_delivered_ns, 0), COALESCE(execution_dead_ns, 0),
	queue_expires_ns, abort_requested, COALESCE(result_json, ''),
	created_ns, COALESCE(completed_ns, 0)`

func scanPullTask(row pgx.Row) (*PullTask, error) {
	var t PullTask
	var state string
	if err := row.Scan(&t.RunID, &t.AgentSlug, &t.AgentRevision, &t.ExecutorKind, &t.PayloadJSON,
		&state, &t.LeaseOwner, &t.LeaseExpiresNS, &t.Attempts, &t.FirstDeliveredNS,
		&t.ExecutionDeadNS, &t.QueueExpiresNS, &t.AbortRequested, &t.ResultJSON,
		&t.CreatedNS, &t.CompletedNS); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("scan pull task: %w", err)
	}
	t.State = PullTaskState(state)
	return &t, nil
}

// ---------------------------------------------------------------------------
// 入队(dispatcher 集成入口)
// ---------------------------------------------------------------------------

// EnqueueTask 把一条 remote-pull 任务落入队列并登记 agent_runs。
// queued+leased 共用该 Agent 的 max_concurrency+queue_capacity 容量。
func (s *AgentPullStore) EnqueueTask(
	ctx context.Context,
	runID string,
	agent *AgentConfig,
	payloadJSON string,
	queueTTL time.Duration,
) error {
	now := nowNS()
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks
		 WHERE agent_slug = $1 AND state IN ('queued','leased')`,
		agent.Slug).Scan(&count); err != nil {
		return fmt.Errorf("count pull queue for %s: %w", agent.Slug, err)
	}
	if count >= agent.MaxConcurrency+agent.QueueCapacity {
		return PullOverloadedError
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_pull_tasks
		   (run_id, agent_slug, agent_revision, executor_kind, payload_json, state,
		    execution_dead_ns, queue_expires_ns, created_ns, created_at)
		 VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$8,$9)`,
		runID, agent.Slug, agent.Revision, string(agent.ExecutorKind), payloadJSON,
		now+agent.Timeout().Nanoseconds(), now+queueTTL.Nanoseconds(),
		now, nowTimestamp()); err != nil {
		return fmt.Errorf("insert pull task: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_runs
		   (agent_slug, run_id, agent_revision, executor_kind, execution_mode,
		    status, request_json, created_at)
		 VALUES ($1,$2,$3,$4,'remote_pull','running',$5,$6)
		 ON CONFLICT (agent_slug, run_id) DO NOTHING`,
		agent.Slug, runID, agent.Revision, string(agent.ExecutorKind),
		payloadJSON, nowTimestamp()); err != nil {
		return fmt.Errorf("record pull run: %w", err)
	}
	return tx.Commit(ctx)
}

// GetTask 读取任务快照。
func (s *AgentPullStore) GetTask(ctx context.Context, runID string) (*PullTask, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+pullTaskColumns+` FROM agent_pull_tasks WHERE run_id = $1`, runID)
	return scanPullTask(row)
}

// WaitTerminal 阻塞等待任务进入终态(dispatcher 的有界同步等待)。
// 返回终态与结果 JSON;超时/取消返回 ctx.Err()。
func (s *AgentPullStore) WaitTerminal(ctx context.Context, runID string) (PullTaskState, string, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := s.GetTask(ctx, runID)
		if err != nil {
			return "", "", err
		}
		if pullTerminalStates[task.State] {
			return task.State, task.ResultJSON, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------------------
// claim / renew / complete
// ---------------------------------------------------------------------------

// ClaimOutcome 是 claim 的三种互斥结果。
type ClaimOutcome int

const (
	ClaimDelivered ClaimOutcome = iota
	ClaimEmpty
	ClaimRateLimited
)

// ClaimTask 用 SKIP LOCKED 弹出一条 queued 任务并建立租约(单条 UPDATE ... RETURNING)。
// capacity<=0 视为不限(仍受全局在途租约上限约束)。
func (s *AgentPullStore) ClaimTask(
	ctx context.Context,
	workerID string,
	slugs []string,
	capacity int,
	cfg *AgentAPIConfig,
) (*PullTask, ClaimOutcome, error) {
	now := nowNS()
	var outstanding int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased'`).Scan(&outstanding); err != nil {
		return nil, ClaimEmpty, fmt.Errorf("count outstanding leases: %w", err)
	}
	if outstanding >= cfg.MaxOutstandingLeases {
		return nil, ClaimRateLimited, nil
	}
	if capacity > 0 {
		var held int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased' AND lease_owner = $1`,
			workerID).Scan(&held); err != nil {
			return nil, ClaimEmpty, fmt.Errorf("count worker leases: %w", err)
		}
		if held >= capacity {
			return nil, ClaimRateLimited, nil
		}
	}
	var claimed RunID
	err := s.pool.QueryRow(ctx, `
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
		workerID, now+cfg.LeaseTTL().Nanoseconds(), now, slugs).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ClaimEmpty, nil
	}
	if err != nil {
		return nil, ClaimEmpty, fmt.Errorf("claim pull task: %w", err)
	}
	task, err := s.GetTask(ctx, claimed)
	if err != nil {
		return nil, ClaimEmpty, fmt.Errorf("reload claimed task: %w", err)
	}
	return task, ClaimDelivered, nil
}

// RunID 是 run 标识的类型别名,避免裸 string 散落。
type RunID = string

// WorkerMismatchLeases 返回服务端可见活跃租约数;多于 worker 自报在途数时
// 疑似双实例分叉(claim 409 判定依据,AGENT-PULL-API §1)。
func (s *AgentPullStore) WorkerMismatchLeases(ctx context.Context, workerID string, selfReportedInFlight int) (int, error) {
	var active int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state = 'leased' AND lease_owner = $1`,
		workerID).Scan(&active); err != nil {
		return 0, fmt.Errorf("count worker active leases: %w", err)
	}
	if active > selfReportedInFlight {
		return active, nil
	}
	return 0, nil
}

// RenewOutcome 是 renew 的五种互斥结果。
type RenewOutcome int

const (
	RenewOK RenewOutcome = iota
	RenewAbort
	RenewLeaseLost
	RenewDeadline
	RenewUnknown
)

// RenewLease 为活跃租约续期;语义见 AGENT-PULL-API §2。
func (s *AgentPullStore) RenewLease(
	ctx context.Context,
	runID string,
	workerID string,
	cfg *AgentAPIConfig,
) (RenewOutcome, error) {
	now := nowNS()
	tag, err := s.pool.Exec(ctx, `
		UPDATE agent_pull_tasks
		SET lease_expires_ns = $1
		WHERE run_id = $2 AND state = 'leased'
		  AND lease_owner = $3
		  AND lease_expires_ns > $4
		  AND execution_dead_ns > $4`,
		now+cfg.LeaseTTL().Nanoseconds(), runID, workerID, now)
	if err != nil {
		return RenewUnknown, fmt.Errorf("renew lease: %w", err)
	}
	if tag.RowsAffected() == 1 {
		task, err := s.GetTask(ctx, runID)
		if err != nil {
			return RenewUnknown, err
		}
		if task.AbortRequested {
			return RenewAbort, nil
		}
		return RenewOK, nil
	}
	task, err := s.GetTask(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenewUnknown, nil
	}
	if err != nil {
		return RenewUnknown, err
	}
	switch {
	case pullTerminalStates[task.State]:
		return RenewUnknown, nil
	case task.ExecutionDeadNS <= now:
		return RenewDeadline, nil
	default:
		return RenewLeaseLost, nil
	}
}

// RequestAbort 置取消位;下一轮 renew 以 abort:true 传达(§7)。
func (s *AgentPullStore) RequestAbort(ctx context.Context, runID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE agent_pull_tasks SET abort_requested = true WHERE run_id = $1`, runID)
	if err != nil {
		return fmt.Errorf("request abort: %w", err)
	}
	return nil
}

// CompleteOutcome 是 complete 的判定结果。
type CompleteOutcome struct {
	Accepted  bool
	RunStatus string // completed / failed / canceled / superseded
	Conflict  bool   // 同 runId 不同请求体
}

// CompleteTask 幂等回传;语义见 AGENT-PULL-API §3。
// 重放判定用请求体哈希;租约失效后到达的结果仅归档(superseded)。
func (s *AgentPullStore) CompleteTask(
	ctx context.Context,
	runID string,
	workerID string,
	status string, // succeeded | failed | aborted
	resultJSON string,
	errorCode string,
) (CompleteOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompleteOutcome{}, fmt.Errorf("begin complete tx: %w", err)
	}
	defer tx.Rollback(ctx)

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
		runID).Scan(&state, &owner, &expiresNS, &storedJSON, &createdNS, &completedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompleteOutcome{}, pgx.ErrNoRows
	}
	if err != nil {
		return CompleteOutcome{}, fmt.Errorf("load task for complete: %w", err)
	}

	// 幂等重放:曾由 worker 接受过结果(completed_by='worker')且请求体一致 → accepted;
	// 不一致 → RESULT_CONFLICT。reaper 判死(completed_by IS NULL)的终态任务,
	// 迟到结果一律仅归档为 superseded(AGENT-PULL-API §3)。
	if pullTerminalStates[PullTaskState(state)] {
		if completedBy != nil && *completedBy == "worker" {
			if storedJSON != nil && hashEqual(*storedJSON, resultJSON) {
				return CompleteOutcome{Accepted: true, RunStatus: state}, nil
			}
			return CompleteOutcome{Accepted: false, RunStatus: state, Conflict: true}, nil
		}
		if err := archiveSuperseded(tx, runID, resultJSON); err != nil {
			return CompleteOutcome{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteOutcome{}, err
		}
		return CompleteOutcome{Accepted: false, RunStatus: "superseded"}, nil
	}

	now := nowNS()
	leaseValid := state == "leased" && owner != nil && *owner == workerID &&
		expiresNS != nil && *expiresNS > now

	if !leaseValid {
		// 租约已被重派或他人持有:结果仅归档。
		if err := archiveSuperseded(tx, runID, resultJSON); err != nil {
			return CompleteOutcome{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteOutcome{}, err
		}
		return CompleteOutcome{Accepted: false, RunStatus: "superseded"}, nil
	}

	finalState := mapStatusToPullState(status)
	durationMS := (now - createdNS) / int64(time.Millisecond)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_pull_tasks SET
		  state = $2, result_json = $3, completed_ns = $4, completed_by = 'worker',
		  lease_owner = NULL, lease_expires_ns = NULL
		WHERE run_id = $1`,
		runID, string(finalState), resultJSON, now); err != nil {
		return CompleteOutcome{}, fmt.Errorf("finalize pull task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_runs SET
		  status = $2, result_json = $3, error_code = $4,
		  completed_at = $5, duration_ms = $6
		WHERE run_id = $1`,
		runID, runStatusForRuns(finalState), resultJSON, nullableString(errorCode),
		nowTimestamp(), durationMS); err != nil {
		return CompleteOutcome{}, fmt.Errorf("finalize pull run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteOutcome{}, err
	}
	return CompleteOutcome{Accepted: true, RunStatus: string(finalState)}, nil
}

// archiveSuperseded 把迟到结果归档进 agent_runs(status=superseded)。
func archiveSuperseded(tx pgx.Tx, runID, resultJSON string) error {
	_, err := tx.Exec(context.Background(), `
		INSERT INTO agent_runs (agent_slug, run_id, agent_revision, executor_kind,
		                        execution_mode, status, request_json, result_json,
		                        created_at, completed_at)
		SELECT agent_slug, run_id, agent_revision, executor_kind, 'remote_pull',
		       'superseded', payload_json, $2, $3, $3
		FROM agent_pull_tasks WHERE run_id = $1
		ON CONFLICT (agent_slug, run_id) DO UPDATE
		  SET status='superseded', result_json=$2, completed_at=$3`,
		runID, resultJSON, nowTimestamp())
	if err != nil {
		return fmt.Errorf("archive superseded result: %w", err)
	}
	return nil
}

func mapStatusToPullState(status string) PullTaskState {
	switch status {
	case "succeeded":
		return PullTaskCompleted
	case "aborted":
		return PullTaskCanceled
	default:
		return PullTaskFailed
	}
}

func runStatusForRuns(state PullTaskState) string {
	switch state {
	case PullTaskCompleted:
		return "completed"
	case PullTaskCanceled:
		return "canceled"
	default:
		return "failed"
	}
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func hashEqual(a, b string) bool {
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sumA[:]) == hex.EncodeToString(sumB[:])
}

// ---------------------------------------------------------------------------
// worker 心跳
// ---------------------------------------------------------------------------

// Heartbeat upsert worker 记录(首次心跳即注册)。
func (s *AgentPullStore) Heartbeat(
	ctx context.Context,
	workerID string,
	connectorID int64,
	inFlight int,
	capacity int,
	capabilities string,
) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_pull_workers
		  (worker_id, connector_id, in_flight, capacity, capabilities, last_heartbeat_ns, online)
		VALUES ($1,$2,$3,$4,$5,$6,true)
		ON CONFLICT (worker_id, connector_id) DO UPDATE SET
		  in_flight = $3, capacity = $4, capabilities = $5,
		  last_heartbeat_ns = $6, online = true`,
		workerID, connectorID, inFlight, capacity, capabilities, nowNS())
	if err != nil {
		return fmt.Errorf("upsert pull worker: %w", err)
	}
	return nil
}

// QueueDepth 返回各队列待领任务总数(heartbeat 回执)。
func (s *AgentPullStore) QueueDepth(ctx context.Context, slugs []string) (int, error) {
	var depth int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_pull_tasks WHERE state='queued' AND agent_slug = ANY($1)`,
		slugs).Scan(&depth); err != nil {
		return 0, fmt.Errorf("count queue depth: %w", err)
	}
	return depth, nil
}

// ListWorkers 供管理台展示连接器状态。
func (s *AgentPullStore) ListWorkers(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.worker_id, c.name, w.in_flight, w.capacity, w.capabilities,
		       w.last_heartbeat_ns, w.online
		FROM agent_pull_workers w
		JOIN agent_pull_connectors c ON c.id = w.connector_id
		ORDER BY c.name, w.worker_id`)
	if err != nil {
		return nil, fmt.Errorf("list pull workers: %w", err)
	}
	defer rows.Close()
	workers := []map[string]any{}
	for rows.Next() {
		var (
			workerID, name, capabilities string
			inFlight, capacity           int
			lastNS                       int64
			online                       bool
		)
		if err := rows.Scan(&workerID, &name, &inFlight, &capacity, &capabilities, &lastNS, &online); err != nil {
			return nil, err
		}
		workers = append(workers, map[string]any{
			"worker_id": workerID, "connector": name, "in_flight": inFlight,
			"capacity": capacity, "capabilities": capabilities,
			"last_heartbeat_ns": lastNS, "online": online,
		})
	}
	return workers, rows.Err()
}

// ---------------------------------------------------------------------------
// reaper:租约过期 / 队列 TTL / worker 离线
// ---------------------------------------------------------------------------

// SweepStats 是一轮回收的行数统计,供观测。
type SweepStats struct {
	Requeued       int64
	TerminalLease  int64
	TerminalQueue  int64
	WorkersOffline int64
}

// SweepExpired 执行一轮回收;语义见 AGENT-PULL-API §6。
func (s *AgentPullStore) SweepExpired(ctx context.Context, cfg *AgentAPIConfig) (SweepStats, error) {
	var stats SweepStats
	now := nowNS()

	// 1. 可重派的过期租约 → 回到队列。
	tag, err := s.pool.Exec(ctx, `
		UPDATE agent_pull_tasks SET state='queued', lease_owner=NULL, lease_expires_ns=NULL
		WHERE state='leased' AND lease_expires_ns < $1
		  AND attempts < $2 AND execution_dead_ns > $1 AND NOT abort_requested`,
		now, cfg.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("requeue expired leases: %w", err)
	}
	stats.Requeued = tag.RowsAffected()

	// 2. 不可重派的过期租约 → 终态(aborted / task_timeout / lease_expired)。
	tag, err = s.pool.Exec(ctx, `
		UPDATE agent_pull_tasks t SET
		  state = CASE WHEN t.abort_requested THEN 'canceled' ELSE 'failed' END,
		  completed_ns = $1,
		  result_json = json_build_object('error', json_build_object(
		    'code', CASE WHEN t.abort_requested THEN 'aborted'
		                 WHEN t.execution_dead_ns <= $1 THEN 'task_timeout'
		                 ELSE 'lease_expired' END))::text
		WHERE t.state='leased' AND t.lease_expires_ns < $1
		  AND (t.attempts >= $2 OR t.execution_dead_ns <= $1 OR t.abort_requested)`,
		now, cfg.MaxAttempts)
	if err != nil {
		return stats, fmt.Errorf("terminate expired leases: %w", err)
	}
	stats.TerminalLease = tag.RowsAffected()

	// 3. 队列 TTL 耗尽 → failed(queue_timeout)。
	tag, err = s.pool.Exec(ctx, `
		UPDATE agent_pull_tasks SET state='failed', completed_ns=$1,
		  result_json = json_build_object('error', json_build_object('code','queue_timeout'))::text
		WHERE state='queued' AND queue_expires_ns < $1`, now)
	if err != nil {
		return stats, fmt.Errorf("expire queued tasks: %w", err)
	}
	stats.TerminalQueue = tag.RowsAffected()

	// 4. agent_runs 同步终态(覆盖 2 与 3)。
	if stats.TerminalLease+stats.TerminalQueue > 0 {
		if _, err := s.pool.Exec(ctx, `
			UPDATE agent_runs r SET
			  status = t.state, completed_at = $1::text,
			  result_json = t.result_json,
			  error_code = NULLIF(t.result_json, '')::jsonb #>> '{error,code}',
			  duration_ms = ($2 - t.created_ns) / 1000000
			FROM agent_pull_tasks t
			WHERE t.run_id = r.run_id AND r.status = 'running'
			  AND t.state IN ('failed','canceled')`,
			nowTimestamp(), now); err != nil {
			return stats, fmt.Errorf("sync terminal runs: %w", err)
		}
	}

	// 5. worker 离线判定:超过 N 个心跳间隔未上报(间隔按 lease/3 近似)。
	threshold := int64(cfg.WorkerOfflineAfterMiss) * cfg.RenewInterval().Nanoseconds()
	tag, err = s.pool.Exec(ctx, `
		UPDATE agent_pull_workers SET online=false, capacity=0, in_flight=0
		WHERE online AND last_heartbeat_ns < $1`, now-threshold)
	if err != nil {
		return stats, fmt.Errorf("mark workers offline: %w", err)
	}
	stats.WorkersOffline = tag.RowsAffected()
	return stats, nil
}
