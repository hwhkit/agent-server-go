package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ir-agent-server/internal/domain/connector"
)

// TokenStore 实现 connector.TokenRepository（agent_pull_connectors 表）。
// 明文令牌不落库；Hash 由调用方用 crypto.TokenHash 预先填充。
type TokenStore struct{ db *DB }

// NewTokenStore 构建令牌仓储。
func NewTokenStore(db *DB) *TokenStore { return &TokenStore{db: db} }

var _ connector.TokenRepository = (*TokenStore)(nil)

// Issue 按名字 upsert 令牌：同名再签发即轮换（旧哈希立即失效、状态回
// active、宽限清空），与旧栈 IssueConnector 语义一致。
func (s *TokenStore) Issue(ctx context.Context, t *connector.Token) error {
	if t.Hash == "" {
		return errors.New("token hash is required")
	}
	var id int64
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO agent_pull_connectors (name, token_hash, scopes, status, grace_until_ns, expires_ns, created_at)
		 VALUES ($1, $2, $3, 'active', $4, $5, $6)
		 ON CONFLICT (name) DO UPDATE SET
		   token_hash = $2, scopes = $3, status = 'active',
		   grace_until_ns = NULL, revoked_ns = NULL
		 RETURNING id`,
		t.Name, t.Hash, strings.Join(t.Scopes, ","), nsPtr(t.GraceUntil), nsPtr(t.ExpiresAt), nowTS()).
		Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("connector token hash collision: %w", err)
		}
		return fmt.Errorf("issue connector token: %w", err)
	}
	t.ID = strconv.FormatInt(id, 10)
	return nil
}

// GetByName 按名字取令牌记录（管理台按名字操作）。
func (s *TokenStore) GetByName(ctx context.Context, name string) (*connector.Token, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT id, name, token_hash, scopes, status, grace_until_ns, expires_ns, revoked_ns, created_at
		 FROM agent_pull_connectors WHERE name = $1`, name)
	t, err := scanToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connector.ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// RevokeByName 按名字立即吊销（无宽限）。
func (s *TokenStore) RevokeByName(ctx context.Context, name string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_pull_connectors SET status = 'revoked', revoked_ns = $2 WHERE name = $1`,
		name, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("revoke connector by name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return connector.ErrTokenNotFound
	}
	return nil
}

// GetByHash 按令牌哈希取记录。
func (s *TokenStore) GetByHash(ctx context.Context, hash string) (*connector.Token, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT id, name, token_hash, scopes, status, grace_until_ns, expires_ns, revoked_ns, created_at
		 FROM agent_pull_connectors WHERE token_hash = $1`, hash)
	t, err := scanToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connector.ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// List 返回全部令牌记录（不含明文）。
func (s *TokenStore) List(ctx context.Context) ([]connector.Token, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, name, token_hash, scopes, status, grace_until_ns, expires_ns, revoked_ns, created_at
		 FROM agent_pull_connectors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list connector tokens: %w", err)
	}
	defer rows.Close()
	out := make([]connector.Token, 0, 4)
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Revoke 吊销令牌，可附轮换宽限期终点。
func (s *TokenStore) Revoke(ctx context.Context, id string, graceUntil *time.Time) error {
	idNum, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return connector.ErrTokenNotFound
	}
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_pull_connectors SET status = 'revoked', revoked_ns = $2, grace_until_ns = $3
		 WHERE id = $1`, idNum, time.Now().UnixNano(), nsPtr(graceUntil))
	if err != nil {
		return fmt.Errorf("revoke connector token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return connector.ErrTokenNotFound
	}
	return nil
}

func scanToken(row pgx.Row) (*connector.Token, error) {
	var (
		t         connector.Token
		id        int64
		scopes    string
		status    string
		graceNs   *int64
		expiresNs *int64
		revokedNs *int64
		createdAt string
	)
	err := row.Scan(&id, &t.Name, &t.Hash, &scopes, &status, &graceNs, &expiresNs, &revokedNs, &createdAt)
	if err != nil {
		return nil, err
	}
	t.ID = strconv.FormatInt(id, 10)
	if scopes != "" {
		t.Scopes = strings.Split(scopes, ",")
	}
	created := parseTS(createdAt)
	t.CreatedAt = created
	t.GraceUntil = timeFromNS(graceNs)
	t.ExpiresAt = timeFromNS(expiresNs)
	if status != "active" {
		// 旧栈写入的 revoked/rotated 行没有 revoked_ns：回落到创建时间，
		// Active() 语义保持「已吊销且过宽限期即失效」。
		if revoked := timeFromNS(revokedNs); revoked != nil {
			t.RevokedAt = revoked
		} else {
			t.RevokedAt = &created
		}
	}
	return &t, nil
}

// WorkerStore 实现 connector.WorkerRegistry（agent_pull_workers 表）。
type WorkerStore struct{ db *DB }

// NewWorkerStore 构建 worker 注册仓储。
func NewWorkerStore(db *DB) *WorkerStore { return &WorkerStore{db: db} }

var _ connector.WorkerRegistry = (*WorkerStore)(nil)

// Beat 上报心跳：按 (worker_id, connector_id) upsert 并置为在线。
func (s *WorkerStore) Beat(ctx context.Context, w connector.Worker) error {
	connectorID, err := strconv.ParseInt(w.TokenID, 10, 64)
	if err != nil {
		return fmt.Errorf("worker %s: invalid token id", w.WorkerID)
	}
	// capabilities 列沿用旧栈编码 "slug1,slug2|version"（新旧双栈共表期间的
	// 数据格式契约；P6 删除旧栈后可迁移为 JSON）。
	capabilities := strings.Join(w.Slugs, ",") + "|" + w.Version
	interval := w.BeatIntervalSeconds
	if interval <= 0 {
		interval = connector.DefaultHeartbeatSeconds
	}
	_, err = s.db.Pool.Exec(ctx,
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
			connectorID int64
			caps        string
			lastBeat    int64
		)
		if err := rows.Scan(&w.WorkerID, &connectorID, &w.InFlight, &w.Capacity, &caps, &lastBeat); err != nil {
			return nil, err
		}
		w.TokenID = strconv.FormatInt(connectorID, 10)
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
