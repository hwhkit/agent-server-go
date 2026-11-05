package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/account"
)

// AccountStore 实现 account.Repository（admin_users 表）。
// 「系统始终保留至少一个可用管理员」不变量在事务内强制。
type AccountStore struct{ db *DB }

// NewAccountStore 构建账号仓储。
func NewAccountStore(db *DB) *AccountStore { return &AccountStore{db: db} }

var _ account.Repository = (*AccountStore)(nil)

func scanAccount(row pgx.Row) (*account.Account, error) {
	var (
		a         account.Account
		role      string
		createdAt string
		updatedAt string
	)
	err := row.Scan(&a.Username, &a.PasswordHash, &role, &a.Enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	a.Role = account.Role(role)
	a.CreatedAt = parseTS(createdAt)
	a.UpdatedAt = parseTS(updatedAt)
	return &a, nil
}

const accountColumns = `username, password_hash, role, enabled, created_at, updated_at`

// Get 按用户名取账号。
func (s *AccountStore) Get(ctx context.Context, username string) (*account.Account, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM admin_users WHERE username = $1`, username)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, account.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// List 返回全部账号，按用户名排序。
func (s *AccountStore) List(ctx context.Context) ([]account.Account, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+accountColumns+` FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	out := make([]account.Account, 0, 4)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Create 创建账号（用户名冲突返回错误）。
func (s *AccountStore) Create(ctx context.Context, a *account.Account) error {
	now := nowTS()
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO admin_users (username, password_hash, role, enabled, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$5)`,
		a.Username, a.PasswordHash, string(a.Role), a.Enabled, now)
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// Update 更新账号。若变更会移除最后一个可用管理员，返回 account.ErrLastAdmin。
func (s *AccountStore) Update(ctx context.Context, a *account.Account) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentRole string
	var currentEnabled bool
	err = tx.QueryRow(ctx,
		`SELECT role, enabled FROM admin_users WHERE username = $1 FOR UPDATE`, a.Username).
		Scan(&currentRole, &currentEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return account.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	losesAdmin := currentRole == string(account.RoleAdmin) && currentEnabled &&
		(a.Role != account.RoleAdmin || !a.Enabled)
	if losesAdmin {
		if err := ensureAnotherAdmin(ctx, tx, a.Username); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE admin_users SET password_hash = $2, role = $3, enabled = $4, updated_at = $5
		 WHERE username = $1`,
		a.Username, a.PasswordHash, string(a.Role), a.Enabled, nowTS()); err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	return tx.Commit(ctx)
}

// Delete 删除账号。删除最后一个可用管理员被拒绝。
func (s *AccountStore) Delete(ctx context.Context, username string) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var role string
	var enabled bool
	err = tx.QueryRow(ctx,
		`SELECT role, enabled FROM admin_users WHERE username = $1 FOR UPDATE`, username).
		Scan(&role, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return account.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	if role == string(account.RoleAdmin) && enabled {
		if err := ensureAnotherAdmin(ctx, tx, username); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_users WHERE username = $1`, username); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE username = $1`, username); err != nil {
		return fmt.Errorf("delete account sessions: %w", err)
	}
	return tx.Commit(ctx)
}

func ensureAnotherAdmin(ctx context.Context, tx pgx.Tx, excludeUsername string) error {
	var others int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM admin_users WHERE role = 'admin' AND enabled AND username <> $1`,
		excludeUsername).Scan(&others); err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if others == 0 {
		return account.ErrLastAdmin
	}
	return nil
}

// SessionStore 实现 account.SessionStore（admin_sessions 表）。
type SessionStore struct{ db *DB }

// NewSessionStore 构建会话仓储。
func NewSessionStore(db *DB) *SessionStore { return &SessionStore{db: db} }

var _ account.SessionStore = (*SessionStore)(nil)

// Put 写入或刷新会话。
func (s *SessionStore) Put(ctx context.Context, session account.Session) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, username, csrf_token, expires_at, created_at)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (token_hash) DO UPDATE SET expires_at = $4`,
		session.TokenHash, session.Username, session.CSRFToken,
		session.ExpiresAt.Unix(), nowTS())
	if err != nil {
		return fmt.Errorf("put session: %w", err)
	}
	return nil
}

// Get 按 token 哈希取有效会话（联查账号角色；账号被禁用或会话过期视为无效）。
func (s *SessionStore) Get(ctx context.Context, tokenHash string) (*account.Session, error) {
	var (
		session   account.Session
		role      string
		enabled   bool
		expiresAt int64
		createdAt string
	)
	err := s.db.Pool.QueryRow(ctx,
		`SELECT s.username, s.csrf_token, s.expires_at, s.created_at, u.role, u.enabled
		 FROM admin_sessions s JOIN admin_users u ON u.username = s.username
		 WHERE s.token_hash = $1`, tokenHash).
		Scan(&session.Username, &session.CSRFToken, &expiresAt, &createdAt, &role, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, account.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	session.TokenHash = tokenHash
	session.Role = account.Role(role)
	session.ExpiresAt = unixTime(expiresAt)
	session.CreatedAt = parseTS(createdAt)
	if !enabled || session.ExpiresAt.Before(nowUTC()) {
		return nil, account.ErrNotFound
	}
	return &session, nil
}

// Delete 删除会话（登出）。
func (s *SessionStore) Delete(ctx context.Context, tokenHash string) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpired 清理过期会话，返回删除数。
func (s *SessionStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM admin_sessions WHERE expires_at < $1`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// unixTime 把秒级 Unix 时间转为 UTC 时间。
func unixTime(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

// nowUTC 返回当前 UTC 时间。
func nowUTC() time.Time { return time.Now().UTC() }
