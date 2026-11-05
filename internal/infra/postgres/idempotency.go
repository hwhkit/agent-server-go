package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/platform"
)

// PushIdempotencyStore 实现 platform.PushIdempotencyStore（push_idempotency 表）。
// SQL 语义自旧栈 idempotency.go 逐字移植，含两条关键防线：
//   - 行不存在时 FOR UPDATE 锁不到任何东西 → 插入必须 ON CONFLICT DO NOTHING，
//     并发 Begin 只有一个 New，后到者按 InProgress/Conflict 判定而不是撞唯一键 500；
//   - Push 签名不覆盖幂等键头 → run_id 唯一索引兜底，跨键同 run 判 Conflict。
type PushIdempotencyStore struct{ db *DB }

var _ platform.PushIdempotencyStore = (*PushIdempotencyStore)(nil)

// stateFinalizationTimeout 约束「记录终态 / 释放租约」的小事务：必须活过
// 请求取消，又不能把后台协程永远挂住。
const stateFinalizationTimeout = 5 * time.Second

func finalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), stateFinalizationTimeout)
}

// NewPushIdempotencyStore 构建存储并执行幂等 DDL 与旧库歧义检查。
func NewPushIdempotencyStore(ctx context.Context, db *DB) (*PushIdempotencyStore, error) {
	store := &PushIdempotencyStore{db: db}
	if err := store.initialize(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

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
		if _, err := s.db.Pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("initialize push idempotency schema: %w", err)
		}
	}
	// Push HMAC 覆盖请求体（含 body.runId）但不覆盖幂等键头。调用方不能靠
	// 只改幂等键让同一个已签名 run 执行两次；旧库存在歧义重复行时拒绝启动，
	// 而不是静默删除重复运行的证据。
	var duplicateRunID string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT run_id FROM push_idempotency GROUP BY run_id HAVING COUNT(*) > 1 LIMIT 1`).
		Scan(&duplicateRunID)
	switch {
	case err == nil:
		return fmt.Errorf("initialize push idempotency schema: duplicate run_id %q; "+
			"reconcile duplicate rows before starting", duplicateRunID)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("inspect push idempotency run_id uniqueness: %w", err)
	}
	if _, err := s.db.Pool.Exec(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_push_idempotency_run_id ON push_idempotency(run_id)`); err != nil {
		return fmt.Errorf("enforce push idempotency run_id uniqueness: %w", err)
	}
	// 终态行不做时间清理（签名无时间戳，删除会让被捕获的旧请求重新可执行）。
	return nil
}

// Begin 申请幂等租约。整个读改写在一个事务里，否则两个并发请求会同时判成 New。
func (s *PushIdempotencyStore) Begin(ctx context.Context, key, runID, requestHash string) (*platform.IdempotencyDecision, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var storedRunID, storedHash, status string
	var responseBody *string
	var updatedAt int64
	err = tx.QueryRow(ctx,
		`SELECT run_id, request_hash, status, response_body, updated_at
		 FROM push_idempotency WHERE key = $1 FOR UPDATE`, key,
	).Scan(&storedRunID, &storedHash, &status, &responseBody, &updatedAt)

	var decision *platform.IdempotencyDecision
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		tag, insertErr := tx.Exec(ctx,
			`INSERT INTO push_idempotency (key, run_id, request_hash, status, response_body, updated_at)
			 VALUES ($1, $2, $3, $4, NULL, $5) ON CONFLICT DO NOTHING`,
			key, runID, requestHash, platform.IdempotencyProcessing, time.Now().Unix())
		if insertErr != nil {
			return nil, insertErr
		}
		if tag.RowsAffected() == 1 {
			decision = &platform.IdempotencyDecision{Kind: platform.IdemNew}
		} else {
			// ON CONFLICT 可能命中主键，也可能命中 run_id 唯一防线。
			// 同一已签名 run 换键重投是冲突，绝不是第二次执行。
			var existingKey, existingHash string
			lookupErr := tx.QueryRow(ctx,
				`SELECT key, request_hash FROM push_idempotency WHERE run_id = $1`, runID).
				Scan(&existingKey, &existingHash)
			switch {
			case lookupErr == nil && (existingKey != key || existingHash != requestHash):
				decision = &platform.IdempotencyDecision{Kind: platform.IdemConflict}
			case lookupErr == nil:
				decision = &platform.IdempotencyDecision{Kind: platform.IdemInProgress}
			case errors.Is(lookupErr, pgx.ErrNoRows):
				// 主键冲突也可能是既有键对应不同的 runId。
				var existingRunID string
				keyErr := tx.QueryRow(ctx,
					`SELECT run_id FROM push_idempotency WHERE key = $1`, key).Scan(&existingRunID)
				if keyErr != nil {
					if errors.Is(keyErr, pgx.ErrNoRows) {
						return nil, errors.New("push idempotency conflict disappeared during claim")
					}
					return nil, keyErr
				}
				decision = &platform.IdempotencyDecision{Kind: platform.IdemConflict}
			default:
				return nil, lookupErr
			}
		}
	case err != nil:
		return nil, err
	case storedRunID != runID || storedHash != requestHash:
		decision = &platform.IdempotencyDecision{Kind: platform.IdemConflict}
	case status == platform.IdempotencySyncDone:
		if responseBody == nil {
			// 标记为已完成却没有响应体是不自洽的：当成冲突而不是回放空响应。
			decision = &platform.IdempotencyDecision{Kind: platform.IdemConflict}
		} else {
			decision = &platform.IdempotencyDecision{Kind: platform.IdemSyncCached, ResponseBody: *responseBody}
		}
	case status == platform.IdempotencySyncFailed:
		body := ""
		if responseBody != nil {
			body = *responseBody
		}
		decision = &platform.IdempotencyDecision{Kind: platform.IdemSyncFailed, ResponseBody: body}
	case status == platform.IdempotencyAsyncAccepted:
		decision = &platform.IdempotencyDecision{Kind: platform.IdemAsyncAccepted}
	case updatedAt <= time.Now().Unix()-int64(platform.PushLeaseStaleAfter/time.Second):
		if _, takeoverErr := tx.Exec(ctx,
			`UPDATE push_idempotency SET status = $2, response_body = NULL, updated_at = $3
			 WHERE key = $1`,
			key, platform.IdempotencyProcessing, time.Now().Unix()); takeoverErr != nil {
			return nil, takeoverErr
		}
		decision = &platform.IdempotencyDecision{Kind: platform.IdemNew}
	default:
		decision = &platform.IdempotencyDecision{Kind: platform.IdemInProgress}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return decision, nil
}

// CompleteSync 记录同步成功响应。
func (s *PushIdempotencyStore) CompleteSync(ctx context.Context, key, runID, requestHash, responseBody string) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()
	return s.update(ctx, key, runID, requestHash, platform.IdempotencySyncDone, &responseBody)
}

// FailSync 记录执行后终态失败；与 Release 不同，刻意阻止重试再次执行同一任务。
func (s *PushIdempotencyStore) FailSync(ctx context.Context, key, runID, requestHash, reason string) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()
	return s.update(ctx, key, runID, requestHash, platform.IdempotencySyncFailed, &reason)
}

// AcceptAsync 记录异步受理。
func (s *PushIdempotencyStore) AcceptAsync(ctx context.Context, key, runID, requestHash string) error {
	return s.update(ctx, key, runID, requestHash, platform.IdempotencyAsyncAccepted, nil)
}

// Release 释放本请求持有的 processing 租约（只删自己的、仍在 processing 的记录）。
func (s *PushIdempotencyStore) Release(ctx context.Context, key, runID, requestHash string) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM push_idempotency
		 WHERE key = $1 AND run_id = $2 AND request_hash = $3 AND status = $4`,
		key, runID, requestHash, platform.IdempotencyProcessing)
	return err
}

func (s *PushIdempotencyStore) update(ctx context.Context, key, runID, requestHash, status string, responseBody *string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE push_idempotency SET status = $4, response_body = $5, updated_at = $6
		 WHERE key = $1 AND run_id = $2 AND request_hash = $3`,
		key, runID, requestHash, status, responseBody, time.Now().Unix())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return platform.ErrPushLeaseLost
	}
	return nil
}
