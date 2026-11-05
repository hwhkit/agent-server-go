package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/audit"
)

// AuditStore 实现 audit.Repository（audit_log 表）。
// 事务性写入（业务变更与审计同事务）由 app 层通过 WithTx 组合完成；
// P4 迁移应用层时补充 Tx 形态入口，此处先提供池级实现。
type AuditStore struct{ db *DB }

// NewAuditStore 构建审计仓储。
func NewAuditStore(db *DB) *AuditStore { return &AuditStore{db: db} }

var _ audit.Repository = (*AuditStore)(nil)

// Append 批量写入审计事件。CreatedAt 为零值时使用当前时间。
func (s *AuditStore) Append(ctx context.Context, events ...audit.Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, event := range events {
		details := "{}"
		if len(event.Detail) > 0 {
			details = string(event.Detail)
		}
		createdAt := nowTS()
		if !event.CreatedAt.IsZero() {
			createdAt = formatTS(event.CreatedAt)
		}
		batch.Queue(
			`INSERT INTO audit_log (actor, action, target, details, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			event.Actor, string(event.Action), event.Target, details, createdAt)
	}
	if err := s.db.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("append audit events: %w", err)
	}
	return nil
}

// List 按条件查询审计事件（新→旧）。调用方必须先经
// audit.Query.Authorize 收口权限（非 admin 只能查本人）。
func (s *AuditStore) List(ctx context.Context, q audit.Query) ([]audit.Event, int, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	conditions := []string{}
	args := []any{}
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, len(args)))
	}
	if q.Actor != "" {
		add(`actor = $%d`, q.Actor)
	}
	if q.Action != "" {
		add(`action = $%d`, string(q.Action))
	}
	if !q.Since.IsZero() {
		add(`created_at >= $%d`, formatTS(q.Since))
	}
	if !q.Until.IsZero() {
		add(`created_at <= $%d`, formatTS(q.Until))
	}
	where := ``
	if len(conditions) > 0 {
		where = ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	var total int
	if err := s.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}
	args = append(args, limit, q.Offset)
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, actor, action, target, details, created_at FROM audit_log`+where+
			fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	out := make([]audit.Event, 0, limit)
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *event)
	}
	return out, total, rows.Err()
}

// Get 按 ID 取审计事件。
func (s *AuditStore) Get(ctx context.Context, id int64) (*audit.Event, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT id, actor, action, target, details, created_at FROM audit_log WHERE id = $1`, id)
	event, err := scanAudit(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, audit.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return event, nil
}

func scanAudit(row pgx.Row) (*audit.Event, error) {
	var (
		event     audit.Event
		action    string
		details   string
		createdAt string
	)
	err := row.Scan(&event.ID, &event.Actor, &action, &event.Target, &details, &createdAt)
	if err != nil {
		return nil, err
	}
	event.Action = audit.Action(action)
	if details != "" && json.Valid([]byte(details)) {
		event.Detail = json.RawMessage(details)
	}
	event.CreatedAt = parseTS(createdAt)
	return &event, nil
}
