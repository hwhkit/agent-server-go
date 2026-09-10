package mota

// 跨进程 Push 幂等状态(PostgreSQL)。
// 对应 Rust 侧 src/mota/idempotency.rs。
//
// MOTA 是 at-least-once 投递;runId / 幂等键的状态不能只放进程内内存,
// 否则多 worker 或重启会重复执行有副作用的任务。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pushLeaseStaleAfterSecs 是 processing 租约被判定为陈旧、可被接管的阈值。
//
// 只有超过这个时长才允许抢占;**已完成的记录不会被抢占**,执行中的慢任务也不会 ——
// 抢早了就等于重复执行一次有副作用的任务。
const pushLeaseStaleAfterSecs = 15 * 60

// pushRecordRetentionSecs 是已完成记录的保留时长。
const pushRecordRetentionSecs = 7 * 24 * 60 * 60

// Push 幂等记录的三种状态。
const (
	pushStatusProcessing    = "processing"
	pushStatusSyncDone      = "sync_done"
	pushStatusAsyncAccepted = "async_accepted"
)

// PushDecisionKind 是幂等查询的结论。
type PushDecisionKind int

const (
	// PushNew 表示本请求拿到了新租约,可以执行。
	PushNew PushDecisionKind = iota
	// PushInProgress 表示同键请求正在执行中。
	PushInProgress
	// PushSyncCached 表示命中已完成的同步响应,直接回放。
	PushSyncCached
	// PushAsyncAccepted 表示命中已受理的异步请求,回放 202。
	PushAsyncAccepted
	// PushConflict 表示同一个键配了不同的 runId 或请求体。
	PushConflict
)

// PushDecision 是幂等查询结果。ResponseBody 仅在 PushSyncCached 时有意义。
type PushDecision struct {
	Kind         PushDecisionKind
	ResponseBody string
}

// errPushLeaseLost 表示提交终态时租约已经不属于本请求。
var errPushLeaseLost = errors.New("Push idempotency lease was lost")

// PushIdempotencyStore 是 Push 幂等状态存储。
//
// 生产必须用 PostgreSQL 后端;内存后端只给单元测试用 —— 它不跨进程,
// 起两个副本就会各自重复执行同一个任务。
type PushIdempotencyStore struct {
	pool *pgxpool.Pool

	// 以下两项只在内存后端使用。
	mu      sync.Mutex
	records map[string]*pushMemoryRecord
}

type pushMemoryRecord struct {
	runID        string
	requestHash  string
	status       string
	responseBody string
	hasResponse  bool
	updatedAt    int64
}

// ConnectPushIdempotencyStore 连接 PostgreSQL 并确保表结构存在。
func ConnectPushIdempotencyStore(ctx context.Context, databaseURL string) (*PushIdempotencyStore, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL connection string for Push idempotency: %w", err)
	}
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL for Push idempotency: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL for Push idempotency: %w", err)
	}
	store, err := NewPushIdempotencyStoreFromPool(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// NewPushIdempotencyStoreFromPool 复用已有连接池,避免同一进程为同一 PostgreSQL
// 建立两组连接。
func NewPushIdempotencyStoreFromPool(ctx context.Context, pool *pgxpool.Pool) (*PushIdempotencyStore, error) {
	store := &PushIdempotencyStore{pool: pool}
	if err := store.initialize(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// NewInMemoryPushIdempotencyStore 是进程内测试后端。
//
// 生产代码只能用 PostgreSQL 后端 —— 内存状态不跨进程,重启或多副本都会重复执行。
func NewInMemoryPushIdempotencyStore() *PushIdempotencyStore {
	return &PushIdempotencyStore{records: make(map[string]*pushMemoryRecord)}
}

// IsInMemory 报告当前是否为内存后端。
func (s *PushIdempotencyStore) IsInMemory() bool { return s.pool == nil }

func (s *PushIdempotencyStore) initialize(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS push_idempotency (
			key            TEXT PRIMARY KEY,
			run_id         TEXT NOT NULL,
			request_hash   TEXT NOT NULL,
			status         TEXT NOT NULL,
			response_body  TEXT,
			updated_at     BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_push_idempotency_updated ON push_idempotency(updated_at)`,
	}
	for _, statement := range statements {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("initialize Push idempotency schema: %w", err)
		}
	}
	// 只删七天前**已完成**的记录;执行中的租约不自动清理,避免慢任务被重复执行。
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM push_idempotency WHERE status != $1 AND updated_at < $2`,
		pushStatusProcessing, time.Now().Unix()-pushRecordRetentionSecs); err != nil {
		return fmt.Errorf("prune Push idempotency records: %w", err)
	}
	return nil
}

// Begin 申请幂等租约。
//
// 决策优先级(顺序有意义):
//  1. runId 或请求体哈希与既有记录不符 → Conflict(同一个键不能改内容重投);
//  2. 已完成的同步响应 → SyncCached 回放;
//  3. 已受理的异步请求 → AsyncAccepted 回放 202;
//  4. processing 且已陈旧(> 15 分钟)→ 接管,返回 New;
//  5. processing 且不陈旧 → InProgress;
//  6. 无记录 → 插入 processing,返回 New。
func (s *PushIdempotencyStore) Begin(ctx context.Context, key, runID, requestHash string) (*PushDecision, error) {
	if s.IsInMemory() {
		return s.beginInMemory(key, runID, requestHash), nil
	}

	// 整个读改写必须在一个事务里,否则两个并发请求会同时判成 New。
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var storedRunID, storedHash, status string
	var responseBody *string
	var updatedAt int64
	err = tx.QueryRow(ctx,
		`SELECT run_id, request_hash, status, response_body, updated_at `+
			`FROM push_idempotency WHERE key = $1 FOR UPDATE`, key,
	).Scan(&storedRunID, &storedHash, &status, &responseBody, &updatedAt)

	var decision *PushDecision
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// 行不存在时 FOR UPDATE 锁不到任何东西,两个并发请求会同时走到这里。
		// 所以插入必须 ON CONFLICT DO NOTHING:抢到的那个是 New,没抢到的
		// (RowsAffected == 0)说明别人正在执行,按 InProgress 处理 ——
		// 直接裸插会让后到的请求撞唯一键约束,把并发退化成 500。
		tag, err := tx.Exec(ctx,
			`INSERT INTO push_idempotency (key, run_id, request_hash, status, response_body, updated_at) `+
				`VALUES ($1, $2, $3, $4, NULL, $5) ON CONFLICT (key) DO NOTHING`,
			key, runID, requestHash, pushStatusProcessing, time.Now().Unix())
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			decision = &PushDecision{Kind: PushNew}
		} else {
			decision = &PushDecision{Kind: PushInProgress}
		}
	case err != nil:
		return nil, err
	case storedRunID != runID || storedHash != requestHash:
		decision = &PushDecision{Kind: PushConflict}
	case status == pushStatusSyncDone:
		if responseBody == nil {
			// 标记为已完成却没有响应体是不自洽的:当成冲突而不是回放空响应。
			decision = &PushDecision{Kind: PushConflict}
		} else {
			decision = &PushDecision{Kind: PushSyncCached, ResponseBody: *responseBody}
		}
	case status == pushStatusAsyncAccepted:
		decision = &PushDecision{Kind: PushAsyncAccepted}
	case updatedAt <= time.Now().Unix()-pushLeaseStaleAfterSecs:
		if _, err := tx.Exec(ctx,
			`UPDATE push_idempotency SET status = $2, response_body = NULL, updated_at = $3 `+
				`WHERE key = $1`,
			key, pushStatusProcessing, time.Now().Unix()); err != nil {
			return nil, err
		}
		decision = &PushDecision{Kind: PushNew}
	default:
		decision = &PushDecision{Kind: PushInProgress}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return decision, nil
}

// beginInMemory 是内存后端的 Begin。
func (s *PushIdempotencyStore) beginInMemory(key, runID, requestHash string) *PushDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	record, exists := s.records[key]
	switch {
	case !exists:
		s.records[key] = &pushMemoryRecord{
			runID: runID, requestHash: requestHash,
			status: pushStatusProcessing, updatedAt: now,
		}
		return &PushDecision{Kind: PushNew}
	case record.runID != runID || record.requestHash != requestHash:
		return &PushDecision{Kind: PushConflict}
	case record.status == pushStatusSyncDone:
		if !record.hasResponse {
			return &PushDecision{Kind: PushConflict}
		}
		return &PushDecision{Kind: PushSyncCached, ResponseBody: record.responseBody}
	case record.status == pushStatusAsyncAccepted:
		return &PushDecision{Kind: PushAsyncAccepted}
	case record.updatedAt <= now-pushLeaseStaleAfterSecs:
		record.status = pushStatusProcessing
		record.responseBody = ""
		record.hasResponse = false
		record.updatedAt = now
		return &PushDecision{Kind: PushNew}
	default:
		return &PushDecision{Kind: PushInProgress}
	}
}

// CompleteSync 记录同步响应,供后续同键请求回放。
func (s *PushIdempotencyStore) CompleteSync(ctx context.Context, key, runID, requestHash, responseBody string) error {
	return s.update(ctx, key, runID, requestHash, pushStatusSyncDone, &responseBody)
}

// AcceptAsync 记录异步受理,供后续同键请求回放 202。
func (s *PushIdempotencyStore) AcceptAsync(ctx context.Context, key, runID, requestHash string) error {
	return s.update(ctx, key, runID, requestHash, pushStatusAsyncAccepted, nil)
}

// Release 释放本请求持有的 processing 租约。
//
// 只删自己的、且仍处于 processing 的记录 —— 不能把别人已完成的结果删掉。
func (s *PushIdempotencyStore) Release(ctx context.Context, key, runID, requestHash string) error {
	if s.IsInMemory() {
		s.mu.Lock()
		defer s.mu.Unlock()
		record, exists := s.records[key]
		if exists && record.runID == runID && record.requestHash == requestHash &&
			record.status == pushStatusProcessing {
			delete(s.records, key)
		}
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM push_idempotency `+
			`WHERE key = $1 AND run_id = $2 AND request_hash = $3 AND status = $4`,
		key, runID, requestHash, pushStatusProcessing)
	return err
}

// update 把记录推进到终态。租约已不属于本请求时报错。
func (s *PushIdempotencyStore) update(
	ctx context.Context,
	key, runID, requestHash, status string,
	responseBody *string,
) error {
	if s.IsInMemory() {
		s.mu.Lock()
		defer s.mu.Unlock()
		record, exists := s.records[key]
		if !exists || record.runID != runID || record.requestHash != requestHash {
			return errPushLeaseLost
		}
		record.status = status
		record.hasResponse = responseBody != nil
		record.responseBody = ""
		if responseBody != nil {
			record.responseBody = *responseBody
		}
		record.updatedAt = time.Now().Unix()
		return nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE push_idempotency SET status = $4, response_body = $5, updated_at = $6 `+
			`WHERE key = $1 AND run_id = $2 AND request_hash = $3`,
		key, runID, requestHash, status, responseBody, time.Now().Unix())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errPushLeaseLost
	}
	return nil
}

// Close 释放连接池(仅在本存储独占连接池时调用)。
func (s *PushIdempotencyStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
