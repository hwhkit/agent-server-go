package postgres

// 运行数据保留策略：周期性清扫超窗的历史数据，防止千级 Agent 常态运行
// 时 channel_events / audit_log 无界增长拖垮列表查询与表容量。
//
// 口径：
//   - keep <= 0 表示策略关闭（Sweep 直接返回 0），保守默认——审计与事件
//     的保留期限是合规决策，由部署方显式开启（PLATFORM__RETENTION_DAYS）；
//   - 过期 admin_sessions 无条件清扫（会话表按 expires_at 天然过期，
//     清它没有合规争议，只是登录表自洁）；
//   - push 幂等终态与智能体运行记录不在保留策略范围内（协议与审计依据）。

import (
	"context"
	"fmt"
	"time"
)

// RetentionStore 执行历史数据保留清扫。
type RetentionStore struct{ db *DB }

// NewRetentionStore 构建保留清扫存储。
func NewRetentionStore(db *DB) *RetentionStore { return &RetentionStore{db: db} }

// Sweep 执行一轮清扫：返回 (channelEvents, auditRows, sessions, error)。
// days 是保留窗口（天）；<=0 时只清过期会话。
func (s *RetentionStore) Sweep(ctx context.Context, days int) (events, audits, sessions int64, err error) {
	// 过期会话：无条件清（expires_at 是 BIGINT unix 秒，SessionStore 同构）。
	sessionCutoff := time.Now().Unix()
	tag, sessionErr := s.db.Pool.Exec(ctx,
		`DELETE FROM admin_sessions WHERE expires_at < $1`, sessionCutoff)
	if sessionErr != nil {
		return 0, 0, 0, fmt.Errorf("sweep expired sessions: %w", sessionErr)
	}
	sessions = tag.RowsAffected()

	if days <= 0 {
		return 0, 0, sessions, nil
	}
	eventCutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02T15:04:05.000000000Z07:00")
	tag, err = s.db.Pool.Exec(ctx,
		`DELETE FROM channel_events WHERE created_at < $1`, eventCutoff)
	if err != nil {
		return 0, 0, sessions, fmt.Errorf("sweep channel events: %w", err)
	}
	events = tag.RowsAffected()
	tag, err = s.db.Pool.Exec(ctx,
		`DELETE FROM audit_log WHERE created_at < $1`, eventCutoff)
	if err != nil {
		return 0, events, sessions, fmt.Errorf("sweep audit log: %w", err)
	}
	audits = tag.RowsAffected()
	return events, audits, sessions, nil
}
