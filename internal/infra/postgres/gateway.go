package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/platform"
)

// GatewayStore 是动态 Agent 入口的网关状态存储：签名 nonce 防重放、
// 幂等租约状态机、运行登记与终态落库（与幂等迁移同事务）。
// SQL 语义自旧栈 platform_store.go 逐字移植。
type GatewayStore struct {
	db *DB
	// lastNonceGC 是周期性 nonce 清理的上次执行时刻（Unix 秒）。
	lastNonceGC atomic.Int64
}

// NewGatewayStore 构建网关存储。
func NewGatewayStore(db *DB) *GatewayStore { return &GatewayStore{db: db} }

// nonceGCIntervalSecs 是 nonce 表周期性清理间隔。
const nonceGCIntervalSecs = 10 * 60

// ConsumeRequestNonce 记录一个签名 nonce。返回 true 表示首次使用；
// false 表示重放。顺带做周期性过期清理（CAS 认领，单实例执行）。
func (s *GatewayStore) ConsumeRequestNonce(ctx context.Context, agentSlug, nonce string, expiresAt int64) (bool, error) {
	now := time.Now().Unix()
	shouldGC := claimPeriodicGC(&s.lastNonceGC, now)

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if shouldGC {
		if _, err := tx.Exec(ctx, `DELETE FROM request_nonces WHERE expires_at <= $1`, now); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO request_nonces (agent_slug, nonce, expires_at)
		 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		agentSlug, nonce, expiresAt)
	if err != nil {
		return false, err
	}
	inserted := tag.RowsAffected() == 1
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return inserted, nil
}

func claimPeriodicGC(lastRun *atomic.Int64, now int64) bool {
	previous := lastRun.Load()
	if now-previous < nonceGCIntervalSecs {
		return false
	}
	return lastRun.CompareAndSwap(previous, now)
}

// BeginIdempotency 申请幂等租约。
//
// enforceRunID 表示调用方显式给了 runId：缓存/在途记录的 runId 与之不符
// 即冲突，陈旧接管时还要清掉未完成的旧运行行（运行表刻意不保存请求体，
// 已完成的运行不能安全重放）。
func (s *GatewayStore) BeginIdempotency(
	ctx context.Context,
	agentSlug, key, requestHash, runID string,
	enforceRunID bool,
) (platform.GatewayDecision, error) {
	newLeaseID := randomToken(18)
	now := time.Now().UTC()
	nowText := nowTS()

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return platform.GatewayDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`INSERT INTO idempotency_records
		 (agent_slug, key, request_hash, status, lease_id, run_id, updated_at)
		 VALUES ($1, $2, $3, 'processing', $4, $5, $6) ON CONFLICT DO NOTHING`,
		agentSlug, key, requestHash, newLeaseID, runID, nowText)
	if err != nil {
		return platform.GatewayDecision{}, err
	}
	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return platform.GatewayDecision{}, err
		}
		return platform.GatewayDecision{Kind: platform.GatewayNew, LeaseID: newLeaseID, RunID: runID}, nil
	}

	var storedHash, status, updatedAt string
	var response, storedRunID *string
	err = tx.QueryRow(ctx,
		`SELECT request_hash, status, response_body, updated_at, run_id
		 FROM idempotency_records WHERE agent_slug = $1 AND key = $2 FOR UPDATE`, agentSlug, key).
		Scan(&storedHash, &status, &response, &updatedAt, &storedRunID)
	if err != nil {
		return platform.GatewayDecision{}, err
	}

	decision, err := s.resolveIdempotency(ctx, tx, idempotencyLookup{
		agentSlug: agentSlug, key: key, requestHash: requestHash, runID: runID,
		enforceRunID: enforceRunID, newLeaseID: newLeaseID,
		now: now, nowText: nowText,
		storedHash: storedHash, status: status, response: response,
		updatedAt: updatedAt, storedRunID: storedRunID,
	})
	if err != nil {
		return platform.GatewayDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return platform.GatewayDecision{}, err
	}
	return decision, nil
}

// idempotencyLookup 汇总已存在记录的判定输入。
type idempotencyLookup struct {
	agentSlug    string
	key          string
	requestHash  string
	runID        string
	enforceRunID bool
	newLeaseID   string
	now          time.Time
	nowText      string
	storedHash   string
	status       string
	response     *string
	updatedAt    string
	storedRunID  *string
}

func (s *GatewayStore) resolveIdempotency(ctx context.Context, tx pgx.Tx, in idempotencyLookup) (platform.GatewayDecision, error) {
	if in.storedHash != in.requestHash {
		return platform.GatewayDecision{Kind: platform.GatewayConflict}, nil
	}

	// 迁移前遗留的缓存行无法关联到真实运行。不要在回放时伪造一个「原始」
	// run ID：作废旧缓存，在新租约下重新执行一次。
	if in.status == "completed" && in.storedRunID == nil {
		tag, err := tx.Exec(ctx,
			`UPDATE idempotency_records SET status = 'processing',
			 response_body = NULL, lease_id = $3, run_id = $4, updated_at = $5
			 WHERE agent_slug = $1 AND key = $2`,
			in.agentSlug, in.key, in.newLeaseID, in.runID, in.nowText)
		if err != nil {
			return platform.GatewayDecision{}, err
		}
		if tag.RowsAffected() != 1 {
			return platform.GatewayDecision{}, errors.New("failed to migrate legacy idempotency record")
		}
		return platform.GatewayDecision{Kind: platform.GatewayNew, LeaseID: in.newLeaseID, RunID: in.runID}, nil
	}

	missingRunID := in.storedRunID == nil
	effectiveRunID := in.runID
	if in.storedRunID != nil {
		effectiveRunID = *in.storedRunID
	}
	if missingRunID {
		if _, err := tx.Exec(ctx,
			`UPDATE idempotency_records SET run_id = $3 WHERE agent_slug = $1 AND key = $2`,
			in.agentSlug, in.key, effectiveRunID); err != nil {
			return platform.GatewayDecision{}, err
		}
	}

	if in.enforceRunID && effectiveRunID != in.runID {
		return platform.GatewayDecision{Kind: platform.GatewayConflict}, nil
	}
	if in.status == "completed" {
		if in.response != nil {
			return platform.GatewayDecision{
				Kind: platform.GatewayCached, ResponseBody: *in.response, RunID: effectiveRunID,
			}, nil
		}
		return platform.GatewayDecision{Kind: platform.GatewayInProgress}, nil
	}

	// 无法解析 updated_at 时按陈旧处理，避免坏数据永久占住租约。
	processingIsStale := false
	if in.status == "processing" {
		updated := parseTS(in.updatedAt)
		if updated.IsZero() {
			processingIsStale = true
		} else {
			processingIsStale = updated.Unix() < in.now.Unix()-platform.GatewayIdempotencyStaleAfter
		}
	}
	if !processingIsStale {
		return platform.GatewayDecision{Kind: platform.GatewayInProgress}, nil
	}

	var runStatus *string
	var scanned string
	err := tx.QueryRow(ctx,
		`SELECT status FROM agent_runs WHERE agent_slug = $1 AND run_id = $2 FOR UPDATE`,
		in.agentSlug, effectiveRunID).Scan(&scanned)
	switch {
	case err == nil:
		runStatus = &scanned
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return platform.GatewayDecision{}, err
	}

	// 已完成的运行不能安全重放：运行表刻意不保存请求体。
	if in.enforceRunID && runStatus != nil && *runStatus == "completed" {
		return platform.GatewayDecision{Kind: platform.GatewayConflict}, nil
	}

	takeoverRunID := effectiveRunID
	if runStatus != nil {
		if in.enforceRunID {
			tag, err := tx.Exec(ctx,
				`DELETE FROM agent_runs WHERE agent_slug = $1
				 AND run_id = $2 AND status != 'completed'`,
				in.agentSlug, effectiveRunID)
			if err != nil {
				return platform.GatewayDecision{}, err
			}
			if tag.RowsAffected() != 1 {
				return platform.GatewayDecision{}, errors.New("failed to reset stale explicit run")
			}
		} else {
			if _, err := tx.Exec(ctx,
				`UPDATE agent_runs SET status = 'failed',
				 error_code = 'stale_idempotency_lease',
				 completed_at = $3 WHERE agent_slug = $1
				 AND run_id = $2 AND status = 'running'`,
				in.agentSlug, effectiveRunID, in.nowText); err != nil {
				return platform.GatewayDecision{}, err
			}
			takeoverRunID = in.runID
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE idempotency_records SET status = 'processing',
		 response_body = NULL, lease_id = $3, run_id = $4,
		 updated_at = $5 WHERE agent_slug = $1 AND key = $2`,
		in.agentSlug, in.key, in.newLeaseID, takeoverRunID, in.nowText)
	if err != nil {
		return platform.GatewayDecision{}, err
	}
	if tag.RowsAffected() != 1 {
		return platform.GatewayDecision{}, errors.New("failed to take over stale idempotency lease")
	}
	return platform.GatewayDecision{Kind: platform.GatewayNew, LeaseID: in.newLeaseID, RunID: takeoverRunID}, nil
}

// ReleaseIdempotency 释放本请求持有的租约，使同键请求可以重试。
func (s *GatewayStore) ReleaseIdempotency(ctx context.Context, agentSlug, key, leaseID string) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM idempotency_records WHERE agent_slug = $1 AND key = $2
		 AND lease_id = $3 AND status = 'processing'`,
		agentSlug, key, leaseID)
	return err
}

// StartRun 登记一次运行开始。run_id 已存在时返回 false（调用方据此判冲突）。
func (s *GatewayStore) StartRun(
	ctx context.Context,
	agentSlug, runID string,
	revision int64,
	executorKind, executionMode string,
	requestJSON *string,
) (bool, error) {
	tag, err := s.db.Pool.Exec(ctx,
		`INSERT INTO agent_runs
		 (agent_slug, run_id, agent_revision, executor_kind, execution_mode, status, request_json, created_at)
		 VALUES ($1, $2, $3, $4, $5, 'running', $6, $7) ON CONFLICT DO NOTHING`,
		agentSlug, runID, revision, executorKind, executionMode, requestJSON, nowTS())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinalizeRun 在一个事务里写入运行终态与对应的幂等状态迁移。lease 检查
// 保证已被更新请求接管的记录不会被过期请求改写。
func (s *GatewayStore) FinalizeRun(ctx context.Context, finalization platform.GatewayRunFinalization) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE agent_runs SET status = $3, duration_ms = $4, error_code = $5,
		 response_code = $6, result_json = $7, executor_result_json = $8, completed_at = $9
		 WHERE agent_slug = $1 AND run_id = $2 AND status = 'running'`,
		finalization.AgentSlug, finalization.RunID, finalization.Status,
		finalization.DurationMs, finalization.ErrorCode, finalization.ResponseCode,
		finalization.ResultJSON, finalization.ExecutorResultJSON, nowTS())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("run is no longer in the running state")
	}

	if transition := finalization.Idempotency; transition != nil {
		switch transition.Kind {
		case platform.GatewayFinalizeComplete:
			tag, err := tx.Exec(ctx,
				`UPDATE idempotency_records SET status = 'completed',
				 response_body = $4, updated_at = $5
				 WHERE agent_slug = $1 AND key = $2 AND lease_id = $3
				 AND status = 'processing'`,
				finalization.AgentSlug, transition.Key, transition.LeaseID,
				transition.ResponseBody, nowTS())
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("idempotency lease is no longer owned by this request")
			}
		case platform.GatewayFinalizeRelease:
			tag, err := tx.Exec(ctx,
				`DELETE FROM idempotency_records WHERE agent_slug = $1
				 AND key = $2 AND lease_id = $3 AND status = 'processing'`,
				finalization.AgentSlug, transition.Key, transition.LeaseID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("idempotency lease is no longer owned by this request")
			}
		}
	}
	return tx.Commit(ctx)
}

// randomToken 返回 n 字节随机数据的 base64url（无填充）编码（租约 ID 用）。
func randomToken(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
