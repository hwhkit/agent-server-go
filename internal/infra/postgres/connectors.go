package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"ir-agent-server/internal/domain/connector"
)

// WorkerStore 实现 connector.WorkerRegistry（agent_pull_workers 表）。
type WorkerStore struct{ db *DB }

// NewWorkerStore 构建 worker 注册仓储。
func NewWorkerStore(db *DB) *WorkerStore { return &WorkerStore{db: db} }

var _ connector.WorkerRegistry = (*WorkerStore)(nil)

// Beat 上报心跳：按 (worker_id, connector_id) upsert 并置为在线。
func (s *WorkerStore) Beat(ctx context.Context, w connector.Worker) error {
	// TokenID 直接存字符串：act_ 令牌 ID 与 agent-key 主体（"agent:<slug>"）
	// 共用一列（connector_id 已迁移为 TEXT）。
	connectorID := w.TokenID
	// capabilities 列沿用旧栈编码 "slug1,slug2|version"（新旧双栈共表期间的
	// 数据格式契约；P6 删除旧栈后可迁移为 JSON）。
	capabilities := strings.Join(w.Slugs, ",") + "|" + w.Version
	interval := w.BeatIntervalSeconds
	if interval <= 0 {
		interval = connector.DefaultHeartbeatSeconds
	}
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO agent_pull_workers (worker_id, connector_id, in_flight, capacity, capabilities, last_heartbeat_ns, online)
		 VALUES ($1, $2, $3, $4, $5, $6, true)
		 ON CONFLICT (worker_id, connector_id) DO UPDATE SET
		   in_flight = $3, capacity = $4, capabilities = $5, last_heartbeat_ns = $6, online = true`,
		w.WorkerID, connectorID, w.InFlight, w.Capacity, string(capabilities), time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("record worker heartbeat: %w", err)
	}
	return nil
}

// ListOnline 返回标记在线的 worker（是否真正过期由域方法 Offline 判定）。
func (s *WorkerStore) ListOnline(ctx context.Context) ([]connector.Worker, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT worker_id, connector_id, in_flight, capacity, capabilities, last_heartbeat_ns
		 FROM agent_pull_workers WHERE online ORDER BY worker_id`)
	if err != nil {
		return nil, fmt.Errorf("list online workers: %w", err)
	}
	defer rows.Close()
	out := make([]connector.Worker, 0, 4)
	for rows.Next() {
		var (
			w           connector.Worker
			connectorID string
			caps        string
			lastBeat    int64
		)
		if err := rows.Scan(&w.WorkerID, &connectorID, &w.InFlight, &w.Capacity, &caps, &lastBeat); err != nil {
			return nil, err
		}
		w.TokenID = connectorID
		w.LastBeatAt = time.Unix(0, lastBeat).UTC()
		if caps != "" {
			slugsPart, version, _ := strings.Cut(caps, "|")
			if slugsPart != "" {
				w.Slugs = strings.Split(slugsPart, ",")
			}
			w.Version = version
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkOfflineStale 把超过 3 个默认心跳间隔未上报的 worker 置为离线，
// 返回受影响行数。容量记账清零由读取方按 online 标志过滤完成。
func (s *WorkerStore) MarkOfflineStale(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-time.Duration(connector.DefaultHeartbeatSeconds*connector.OfflineAfterMissedBeats) * time.Second).UnixNano()
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_pull_workers SET online = false WHERE online AND last_heartbeat_ns < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mark stale workers offline: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// isUniqueViolation 报告 PostgreSQL 唯一约束错误（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
