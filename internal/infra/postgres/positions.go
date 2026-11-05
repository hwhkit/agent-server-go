package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/position"
)

// PositionStore 实现 position.Repository。
type PositionStore struct{ db *DB }

// NewPositionStore 构建岗位仓储。
func NewPositionStore(db *DB) *PositionStore { return &PositionStore{db: db} }

var _ position.Repository = (*PositionStore)(nil)

const positionColumns = `code, name, description, contract_ref, output_schema, enabled, revision, created_at, updated_at`

func scanPosition(row pgx.Row) (*position.Position, error) {
	var p position.Position
	var schema *string
	err := row.Scan(&p.Code, &p.Name, &p.Description, &p.ContractRef, &schema,
		&p.Enabled, &p.Revision, new(string), new(string))
	if err != nil {
		return nil, err
	}
	if schema != nil {
		p.OutputSchema = json.RawMessage(*schema)
	}
	return &p, nil
}

// GetByCode 按 Code 取岗位。
func (s *PositionStore) GetByCode(ctx context.Context, code string) (*position.Position, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+positionColumns+` FROM job_positions WHERE code = $1`, code)
	p, err := scanPosition(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, position.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get position: %w", err)
	}
	return p, nil
}

// List 返回岗位，内置在前（按 seed 顺序），自定义按 Code 排序随后。
func (s *PositionStore) List(ctx context.Context, includeDisabled bool) ([]position.Position, error) {
	query := `SELECT ` + positionColumns + ` FROM job_positions`
	if !includeDisabled {
		query += ` WHERE enabled`
	}
	query += ` ORDER BY (code = '__CUSTOM__'), code`
	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()
	out := make([]position.Position, 0, 8)
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, fmt.Errorf("list positions scan: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Upsert 创建或更新岗位，事务内递增 revision。ifMatchRevision 非空且不匹配
// 时返回 position.ErrRevisionConflict；对不存在的岗位指定 ifMatch 返回
// position.ErrNotFound。
func (s *PositionStore) Upsert(ctx context.Context, p *position.Position, ifMatchRevision *int64) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	schema := (*string)(nil)
	if len(p.OutputSchema) > 0 {
		text := string(p.OutputSchema)
		schema = &text
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM job_positions WHERE code = $1 FOR UPDATE`, p.Code).Scan(&current)
	now := nowTS()
	var newRevision int64
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if ifMatchRevision != nil {
			return 0, position.ErrNotFound
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO job_positions (code, name, description, contract_ref, output_schema, enabled, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7) RETURNING revision`,
			p.Code, p.Name, p.Description, p.ContractRef, schema, p.Enabled, now).Scan(&newRevision)
	case err != nil:
		return 0, fmt.Errorf("lock position: %w", err)
	default:
		if ifMatchRevision != nil && *ifMatchRevision != current {
			return 0, position.ErrRevisionConflict
		}
		err = tx.QueryRow(ctx,
			`UPDATE job_positions SET name = $2, description = $3, contract_ref = $4, output_schema = $5,
			 enabled = $6, revision = $7, updated_at = $8
			 WHERE code = $1 RETURNING revision`,
			p.Code, p.Name, p.Description, p.ContractRef, schema, p.Enabled, current+1, now).Scan(&newRevision)
	}
	if err != nil {
		return 0, fmt.Errorf("upsert position: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	p.Revision = newRevision
	return newRevision, nil
}

// SetEnabled 切换岗位启用状态（CAS 语义同 Upsert）。
func (s *PositionStore) SetEnabled(ctx context.Context, code string, enabled bool, ifMatchRevision *int64) (int64, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM job_positions WHERE code = $1 FOR UPDATE`, code).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, position.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lock position: %w", err)
	}
	if ifMatchRevision != nil && *ifMatchRevision != current {
		return 0, position.ErrRevisionConflict
	}
	var newRevision int64
	if err := tx.QueryRow(ctx,
		`UPDATE job_positions SET enabled = $2, revision = $3, updated_at = $4 WHERE code = $1 RETURNING revision`,
		code, enabled, current+1, nowTS()).Scan(&newRevision); err != nil {
		return 0, fmt.Errorf("set position enabled: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newRevision, nil
}

// Delete 删除岗位。保留岗位拒绝删除；仍被智能体聘任时返回 position.ErrInUse。
func (s *PositionStore) Delete(ctx context.Context, code string) error {
	if code == position.CodeCustom {
		return fmt.Errorf("%w: %s cannot be deleted", position.ErrReserved, code)
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var used int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM agent_positions WHERE position_code = $1`, code).Scan(&used); err != nil {
		return fmt.Errorf("check staffing: %w", err)
	}
	if used > 0 {
		return position.ErrInUse
	}
	tag, err := tx.Exec(ctx, `DELETE FROM job_positions WHERE code = $1`, code)
	if err != nil {
		return fmt.Errorf("delete position: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return position.ErrNotFound
	}
	return tx.Commit(ctx)
}
