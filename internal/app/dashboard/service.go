// Package dashboard 实现总览页的聚合用例：服务健康、接入状态、今日运行
// 与 token 用量。全部数据来自既有仓储端口的只读聚合，容错口径是「单项
// 失败降级为零值 + 日志」，总览永远可渲染。
package dashboard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
)

// RunTotalsSource 提供运行聚合（infra/postgres 的 RunStore 结构性满足）。
type RunTotalsSource interface {
	RunTotals(ctx context.Context, since time.Time) (run.Totals, error)
}

// QueueTotalsSource 提供队列在途聚合（infra/postgres 的 TaskQueueStore 满足）。
type QueueTotalsSource interface {
	QueueTotals(ctx context.Context) (connector.QueueTotals, error)
}

// HealthSource 是存储探活端口（infra/postgres 的 DB 满足）。
type HealthSource interface {
	Ping(ctx context.Context) error
}

// Snapshot 是一次总览聚合结果（api 层直接序列化）。
type Snapshot struct {
	Version    string    `json:"version"`
	DatabaseOK bool      `json:"databaseOk"`
	ServerTime time.Time `json:"serverTime"`

	OnlineWorkers  int                   `json:"onlineWorkers"`
	WorkersTotal   int                   `json:"workersTotal"`
	Queue          connector.QueueTotals `json:"queue"`
	ActiveTeamRuns int                   `json:"activeTeamRuns"`
	// 活跃团队运行按状态细分；列表已在手，顺手统计，不额外查询。
	TeamRunsRunning int `json:"teamRunsRunning"`
	TeamRunsQueued  int `json:"teamRunsQueued"`

	RunsToday run.Totals `json:"runsToday"`
}

// Service 是总览聚合服务。Snapshot 带短 TTL 缓存：控制台多会话 30s 轮询
// 与 /metrics 抓取（15s 级）共享同一份聚合，DB 上的 COUNT(FILTER) 不随
// 消费者数量线性叠加。
type Service struct {
	version  string
	health   HealthSource
	runs     RunTotalsSource
	queue    QueueTotalsSource // 可为 nil（AGENT_API 未启用）
	workers  connector.WorkerRegistry
	teamRuns team.TeamRunRepository
	now      func() time.Time

	cacheMu       sync.Mutex
	cacheTTL      time.Duration
	cache         Snapshot
	cacheFetched  time.Time
	hasCacheValue bool
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Version  string
	Health   HealthSource
	Runs     RunTotalsSource
	Queue    QueueTotalsSource
	Workers  connector.WorkerRegistry
	TeamRuns team.TeamRunRepository
	// CacheTTL 是聚合缓存有效期；<=0 视作 2s。窗口内并发 Snapshot 共享
	// 一次查询结果（数据可见性延迟上限即 TTL，总览场景可接受）。
	CacheTTL time.Duration
}

// NewService 组装总览服务。
func NewService(deps Deps) *Service {
	ttl := deps.CacheTTL
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	return &Service{
		version: deps.Version, health: deps.Health, runs: deps.Runs,
		queue: deps.Queue, workers: deps.Workers, teamRuns: deps.TeamRuns,
		now:      func() time.Time { return time.Now().UTC() },
		cacheTTL: ttl,
	}
}

// Snapshot 聚合总览数据（带短 TTL 缓存）。单项来源失败不阻断整体
// （降级为零值并记日志）。
func (s *Service) Snapshot(ctx context.Context) Snapshot {
	s.cacheMu.Lock()
	if s.hasCacheValue && s.now().Sub(s.cacheFetched) < s.cacheTTL {
		cached := s.cache
		s.cacheMu.Unlock()
		return cached
	}
	s.cacheMu.Unlock()
	snapshot := s.uncachedSnapshot(ctx)
	s.cacheMu.Lock()
	s.cache = snapshot
	s.cacheFetched = s.now()
	s.hasCacheValue = true
	s.cacheMu.Unlock()
	return snapshot
}

// uncachedSnapshot 执行真实聚合（缓存未命中路径）。
func (s *Service) uncachedSnapshot(ctx context.Context) Snapshot {
	now := s.now()
	snapshot := Snapshot{Version: s.version, ServerTime: now}

	if s.health != nil {
		if err := s.health.Ping(ctx); err != nil {
			slog.Warn("dashboard: database ping failed", "err", err)
		} else {
			snapshot.DatabaseOK = true
		}
	}
	if s.runs != nil {
		// 「今日」按 UTC 当日零点起算。
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		totals, err := s.runs.RunTotals(ctx, midnight)
		if err != nil {
			slog.Warn("dashboard: run totals failed", "err", err)
		} else {
			snapshot.RunsToday = totals
		}
	}
	if s.queue != nil {
		totals, err := s.queue.QueueTotals(ctx)
		if err != nil {
			slog.Warn("dashboard: queue totals failed", "err", err)
		} else {
			snapshot.Queue = totals
		}
	}
	if s.workers != nil {
		workers, err := s.workers.ListOnline(ctx)
		if err != nil {
			slog.Warn("dashboard: list workers failed", "err", err)
		} else {
			online := 0
			for _, worker := range workers {
				if !worker.Offline(now) {
					online++
				}
			}
			snapshot.OnlineWorkers = online
			snapshot.WorkersTotal = len(workers)
		}
	}
	if s.teamRuns != nil {
		active, err := s.teamRuns.ListActive(ctx, 512)
		if err != nil {
			slog.Warn("dashboard: list active team runs failed", "err", err)
		} else {
			snapshot.ActiveTeamRuns = len(active)
			for _, teamRun := range active {
				switch teamRun.Status {
				case team.TeamRunRunning:
					snapshot.TeamRunsRunning++
				case team.TeamRunQueued:
					snapshot.TeamRunsQueued++
				}
			}
		}
	}
	return snapshot
}
