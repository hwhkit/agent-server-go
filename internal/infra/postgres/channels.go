package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
)

// ListChannels 返回全部启用智能体的心跳通道凭据（slug + 解密后的签名密钥）。
// Push 形态心跳按通道独立发送 Bearer 密钥。
func (s *AgentStore) ListChannels(ctx context.Context) ([]agent.Channel, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT slug, signing_secret_enc FROM agents WHERE enabled = true ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list agent channels: %w", err)
	}
	defer rows.Close()
	var channels []agent.Channel
	for rows.Next() {
		var slug, encrypted string
		if err := rows.Scan(&slug, &encrypted); err != nil {
			return nil, err
		}
		secret, err := s.cipher.Decrypt(encrypted, slug)
		if err != nil {
			// 与旧栈一致：单个通道解密失败即整体失败（主密钥不匹配是
			// 部署级错误，静默跳过会让平台把智能体判离线而无告警）。
			return nil, fmt.Errorf("decrypt channel secret for %s", slug)
		}
		channels = append(channels, agent.Channel{Slug: slug, SigningSecret: secret})
	}
	return channels, rows.Err()
}

// CachedChannelLister 是心跳通道的读缓存装饰器（app/worker.ChannelLister
// 同形）：心跳 tick（5s 级）不再每轮全表解密。失效条件为「注册表版本变化
// 或 TTL 过期」二者任一——版本在智能体创建/编辑/删除/换钥时递增，换钥
// 即使在 TTL 窗口内也能立即生效；版本探测失败时降级供旧缓存（心跳循环
// 不因一次版本查询抖动而整体失败）。
type CachedChannelLister struct {
	store *AgentStore
	ttl   time.Duration

	mu        sync.Mutex
	cached    []agent.Channel
	fetchedAt time.Time
	version   int64
}

// NewCachedChannelLister 组装通道缓存（ttl <= 0 视作 10s）。
func NewCachedChannelLister(store *AgentStore, ttl time.Duration) *CachedChannelLister {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &CachedChannelLister{store: store, ttl: ttl}
}

// ListChannels 实现 app/worker.ChannelLister。
func (c *CachedChannelLister) ListChannels(ctx context.Context) ([]agent.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	version, versionErr := c.store.RegistryVersion(ctx)
	fresh := time.Since(c.fetchedAt) < c.ttl
	if fresh && c.cached != nil {
		if versionErr != nil || version == c.version {
			// 版本查询失败：降级返回缓存（心跳可用性优先）。
			return c.cached, nil
		}
	}
	channels, err := c.store.ListChannels(ctx)
	if err != nil {
		if c.cached != nil {
			// 重读失败也降级供旧缓存（版本已变化时下一 tick 会再试）。
			return c.cached, nil
		}
		return nil, err
	}
	c.cached = channels
	c.fetchedAt = time.Now()
	if versionErr == nil {
		c.version = version
	}
	return channels, nil
}

// ChannelEventStore 是通道事件存储（channel_events 表）。
type ChannelEventStore struct{ db *DB }

// NewChannelEventStore 构建通道事件存储。
func NewChannelEventStore(db *DB) *ChannelEventStore { return &ChannelEventStore{db: db} }

// RecordChannelEvent 写入一条通道事件（callback / heartbeat / 自检）。
func (s *ChannelEventStore) RecordChannelEvent(ctx context.Context, event audit.ChannelEvent) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO channel_events
		 (event_type, agent_slug, run_id, url, status_code, accepted, run_status,
		  pass, latency_ms, detail, request_json, result_json, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		event.EventType, event.AgentSlug, event.RunID, event.URL, event.StatusCode,
		event.Accepted, event.RunStatus, event.Pass, event.LatencyMs, event.Detail,
		event.Input, event.Output, nowTS())
	if err != nil {
		return fmt.Errorf("record channel event: %w", err)
	}
	return nil
}

// ListChannelEvents 查询通道事件，按 eventType / slug 过滤，时间倒序。
// detail/请求/响应文本做展示截断（与旧栈管理台口径一致）。
func (s *ChannelEventStore) ListChannelEvents(ctx context.Context, eventType, slug string, limit int) ([]audit.ChannelEvent, error) {
	if limit < 1 {
		limit = 25
	}
	if limit > 50 {
		limit = 50
	}
	const selectEvents = `SELECT id, event_type, agent_slug, run_id, url, status_code, accepted,
		run_status, pass, latency_ms, LEFT(detail, 2048), created_at FROM channel_events`

	var (
		rows pgx.Rows
		err  error
	)
	switch {
	case eventType != "" && slug != "":
		rows, err = s.db.Pool.Query(ctx, selectEvents+
			` WHERE event_type = $1 AND agent_slug = $2 ORDER BY created_at DESC LIMIT $3`,
			eventType, slug, limit)
	case eventType != "":
		rows, err = s.db.Pool.Query(ctx, selectEvents+
			` WHERE event_type = $1 ORDER BY created_at DESC LIMIT $2`, eventType, limit)
	case slug != "":
		rows, err = s.db.Pool.Query(ctx, selectEvents+
			` WHERE agent_slug = $1 ORDER BY created_at DESC LIMIT $2`, slug, limit)
	default:
		rows, err = s.db.Pool.Query(ctx, selectEvents+` ORDER BY created_at DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list channel events: %w", err)
	}
	defer rows.Close()

	out := make([]audit.ChannelEvent, 0, limit)
	for rows.Next() {
		var (
			event     audit.ChannelEvent
			createdAt string
		)
		if err := rows.Scan(&event.ID, &event.EventType, &event.AgentSlug, &event.RunID,
			&event.URL, &event.StatusCode, &event.Accepted, &event.RunStatus,
			&event.Pass, &event.LatencyMs, &event.Detail, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTS(createdAt)
		out = append(out, event)
	}
	return out, rows.Err()
}

// RemotePullSlugs 返回全部启用的 remote-pull 智能体 slug（agent-api 通配
// scope 的展开来源；有序便于确定性展开）。
func (s *AgentStore) RemotePullSlugs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT slug FROM agents WHERE enabled AND execution_mode = 'remote_pull' ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}
