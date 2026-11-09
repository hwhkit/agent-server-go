package postgres

// team_shares 表存储：团队分享链接（公开子页面凭证）。
//
// 活跃唯一性由 partial unique index 保证（team_id WHERE revoked_at IS NULL）；
// Create 在事务内「撤销旧活跃 + 插入新令牌」完成轮换，无两步窗口。

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/team"
)

// TeamShareStore 实现 team.ShareRepository。
type TeamShareStore struct{ db *DB }

var _ team.ShareRepository = (*TeamShareStore)(nil)

// NewTeamShareStore 构建分享存储。
func NewTeamShareStore(db *DB) *TeamShareStore { return &TeamShareStore{db: db} }

const teamSharesDDL = `
CREATE TABLE IF NOT EXISTS team_shares (
	token      TEXT PRIMARY KEY,
	team_id    TEXT NOT NULL,
	created_at TEXT NOT NULL,
	revoked_at TEXT)`

// Create 为 teamID 轮换分享令牌：事务内撤销既有活跃分享并插入新令牌。
func (s *TeamShareStore) Create(ctx context.Context, teamID string) (*team.TeamShare, error) {
	token, err := team.NewShareToken()
	if err != nil {
		return nil, err
	}
	now := nowTS()
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE team_shares SET revoked_at = $2 WHERE team_id = $1 AND revoked_at IS NULL`,
		teamID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO team_shares (token, team_id, created_at) VALUES ($1, $2, $3)`,
		token, teamID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &team.TeamShare{Token: token, TeamID: teamID}, nil
}

// Resolve 按 token 取活跃分享；未命中或已撤销统一 ErrShareNotFound
// （对外不区分「不存在」与「已失效」）。
func (s *TeamShareStore) Resolve(ctx context.Context, token string) (*team.TeamShare, error) {
	var share team.TeamShare
	var createdAt string
	var revokedAt *string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT token, team_id, created_at, revoked_at FROM team_shares
		 WHERE token = $1 AND revoked_at IS NULL`, token).
		Scan(&share.Token, &share.TeamID, &createdAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, team.ErrShareNotFound
	}
	if err != nil {
		return nil, err
	}
	share.CreatedAt = parseTS(createdAt)
	return &share, nil
}

// Revoke 撤销 teamID 的活跃分享（no-op 安全）。
func (s *TeamShareStore) Revoke(ctx context.Context, teamID string) error {
	_, err := s.db.Pool.Exec(ctx,
		`UPDATE team_shares SET revoked_at = $2 WHERE team_id = $1 AND revoked_at IS NULL`,
		teamID, nowTS())
	return err
}
