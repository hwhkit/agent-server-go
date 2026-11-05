// Package postgres 实现全部领域仓储端口（domain/*/Repository）。
//
// 约定与既有系统一致：
//   - 时间戳列是定宽 RFC3339 UTC TEXT（保证 ORDER BY 与时间序一致）；
//   - DDL 全部幂等（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS），启动即迁移；
//   - 新表只增不改不删，存量库可直接升级（见 doc/ARCHITECTURE-V2.md §4）。
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ir-agent-server/internal/domain/position"
)

// DB 持有连接池并负责 schema 迁移与 seed。
type DB struct {
	Pool *pgxpool.Pool
}

// Open 连接 PostgreSQL、执行迁移与 seed。
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL connection string: %w", err)
	}
	poolConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	db := &DB{Pool: pool}
	if err := db.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := db.Seed(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭连接池。
func (db *DB) Close() { db.Pool.Close() }

// schemaStatements 是幂等 DDL 全集。
//
// P2 范围覆盖新仓储使用的表；push 幂等表、nonce 表、channel_events 与
// agent_pull_tasks 的迁移随 P4（应用层）补充进本列表——存量库上这些表
// 已由旧栈建好，新栈切换前不会缺表。
func schemaStatements() []string {
	statements := []string{
		// ---- 存量表（与旧栈 DDL 一致，保证全新库可独立启动）----
		`CREATE TABLE IF NOT EXISTS agents (
			id                  BIGSERIAL PRIMARY KEY,
			slug                TEXT NOT NULL UNIQUE,
			name                TEXT NOT NULL,
			description         TEXT NOT NULL DEFAULT '',
			enabled             BOOLEAN NOT NULL DEFAULT true,
			executor_kind       TEXT NOT NULL,
			execution_mode      TEXT NOT NULL DEFAULT 'gateway',
			signing_secret_enc  TEXT NOT NULL,
			signature_encoding  TEXT NOT NULL DEFAULT 'hex',
			system_prompt       TEXT NOT NULL DEFAULT '',
			workspace_path      TEXT NOT NULL DEFAULT '.',
			claude_model        TEXT,
			allowed_tools_json  TEXT NOT NULL DEFAULT '[]',
			permission_mode     TEXT NOT NULL DEFAULT 'dontAsk',
			timeout_secs        BIGINT NOT NULL DEFAULT 600,
			max_concurrency     BIGINT NOT NULL DEFAULT 1,
			queue_capacity      BIGINT NOT NULL DEFAULT 10,
			revision            BIGINT NOT NULL DEFAULT 1,
			created_at          TEXT NOT NULL,
			updated_at          TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS admin_users (
			username      TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL CHECK (role IN ('admin', 'user')),
			enabled       BOOLEAN NOT NULL DEFAULT true,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS admin_sessions (
			token_hash   TEXT PRIMARY KEY,
			username     TEXT NOT NULL,
			csrf_token   TEXT NOT NULL,
			expires_at   BIGINT NOT NULL,
			created_at   TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_runs (
			agent_slug       TEXT NOT NULL,
			run_id           TEXT NOT NULL,
			agent_revision   BIGINT NOT NULL,
			executor_kind    TEXT,
			execution_mode   TEXT,
			status           TEXT NOT NULL,
			duration_ms      BIGINT,
			error_code       TEXT,
			response_code    BIGINT,
			request_json     TEXT,
			result_json      TEXT,
			executor_result_json TEXT,
			created_at       TEXT NOT NULL,
			completed_at     TEXT,
			PRIMARY KEY(agent_slug, run_id))`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_created ON agent_runs(created_at DESC)`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS response_code BIGINT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS executor_kind TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS execution_mode TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS executor_result_json TEXT`,
		`CREATE TABLE IF NOT EXISTS idempotency_records (
			agent_slug     TEXT NOT NULL,
			key            TEXT NOT NULL,
			request_hash   TEXT NOT NULL,
			status         TEXT NOT NULL,
			response_body  TEXT,
			lease_id       TEXT,
			run_id         TEXT,
			updated_at     TEXT NOT NULL,
			PRIMARY KEY(agent_slug, key))`,
		`CREATE INDEX IF NOT EXISTS idx_idempotency_updated ON idempotency_records(updated_at)`,
		`CREATE TABLE IF NOT EXISTS request_nonces (
			agent_slug  TEXT NOT NULL,
			nonce       TEXT NOT NULL,
			expires_at  BIGINT NOT NULL,
			PRIMARY KEY(agent_slug, nonce))`,
		`CREATE INDEX IF NOT EXISTS idx_request_nonces_expiry ON request_nonces(expires_at)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id          BIGSERIAL PRIMARY KEY,
			actor       TEXT NOT NULL,
			action      TEXT NOT NULL,
			target      TEXT NOT NULL,
			details     TEXT NOT NULL,
			created_at  TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS platform_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_pull_tasks (
			run_id              TEXT PRIMARY KEY,
			agent_slug          TEXT NOT NULL,
			agent_revision      BIGINT NOT NULL,
			executor_kind       TEXT NOT NULL DEFAULT '',
			payload_json        TEXT NOT NULL,
			state               TEXT NOT NULL,
			lease_owner         TEXT,
			lease_expires_ns    BIGINT,
			attempts            INT NOT NULL DEFAULT 0,
			first_delivered_ns  BIGINT,
			execution_dead_ns   BIGINT,
			queue_expires_ns    BIGINT NOT NULL,
			abort_requested     BOOLEAN NOT NULL DEFAULT false,
			result_json         TEXT,
			completed_by        TEXT,
			created_ns          BIGINT NOT NULL,
			created_at          TEXT NOT NULL,
			completed_ns        BIGINT)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_pop ON agent_pull_tasks(agent_slug, created_ns) WHERE state = 'queued'`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_lease_expiry ON agent_pull_tasks(lease_expires_ns) WHERE state = 'leased'`,
		`CREATE INDEX IF NOT EXISTS idx_agent_pull_tasks_queue_expiry ON agent_pull_tasks(queue_expires_ns) WHERE state = 'queued'`,
		`CREATE TABLE IF NOT EXISTS agent_pull_workers (
			worker_id         TEXT NOT NULL,
			connector_id      BIGINT NOT NULL,
			in_flight         INT NOT NULL DEFAULT 0,
			capacity          INT NOT NULL DEFAULT 0,
			capabilities      TEXT NOT NULL DEFAULT '',
			last_heartbeat_ns BIGINT NOT NULL,
			online            BOOLEAN NOT NULL DEFAULT true,
			PRIMARY KEY(worker_id, connector_id))`,
		`CREATE TABLE IF NOT EXISTS agent_pull_connectors (
			id             BIGSERIAL PRIMARY KEY,
			name           TEXT NOT NULL UNIQUE,
			token_hash     TEXT NOT NULL UNIQUE,
			scopes         TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','rotated')),
			grace_until_ns BIGINT,
			created_at     TEXT NOT NULL)`,

		// ---- v2 新增（只增不改）----
		`CREATE TABLE IF NOT EXISTS job_positions (
			code          TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			description   TEXT NOT NULL DEFAULT '',
			contract_ref  TEXT NOT NULL,
			output_schema TEXT,
			enabled       BOOLEAN NOT NULL DEFAULT true,
			revision      BIGINT NOT NULL DEFAULT 1,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_positions (
			agent_slug    TEXT NOT NULL,
			position_code TEXT NOT NULL,
			PRIMARY KEY (agent_slug, position_code))`,
		`CREATE INDEX IF NOT EXISTS idx_agent_positions_code ON agent_positions(position_code)`,
		`CREATE TABLE IF NOT EXISTS teams (
			id                  TEXT PRIMARY KEY,
			name                TEXT NOT NULL,
			description         TEXT NOT NULL DEFAULT '',
			steps_json          TEXT NOT NULL,
			max_concurrent_runs INT NOT NULL DEFAULT 4,
			enabled             BOOLEAN NOT NULL DEFAULT true,
			revision            BIGINT NOT NULL DEFAULT 1,
			created_at          TEXT NOT NULL,
			updated_at          TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS team_runs (
			id              TEXT PRIMARY KEY,
			team_id         TEXT NOT NULL,
			team_revision   BIGINT NOT NULL,
			snapshot_json   TEXT NOT NULL,
			idempotency_key TEXT NOT NULL UNIQUE,
			status          TEXT NOT NULL,
			steps_json      TEXT NOT NULL,
			input_json      TEXT,
			error_json      TEXT,
			created_at      TEXT NOT NULL,
			completed_at    TEXT)`,
		`CREATE INDEX IF NOT EXISTS idx_team_runs_active ON team_runs(status) WHERE status IN ('queued','running')`,
		`CREATE INDEX IF NOT EXISTS idx_team_runs_team ON team_runs(team_id, created_at DESC)`,
		// 运行日志父子展开与岗位筛选（对旧行全部为 NULL，语义=单发运行）。
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS team_run_id TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS team_step_id TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS position_code TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 1`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS usage_json TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_team ON agent_runs(team_run_id) WHERE team_run_id IS NOT NULL`,
		// 连接器令牌生命周期补列（旧栈忽略这些列，共存安全）。
		`ALTER TABLE agent_pull_connectors ADD COLUMN IF NOT EXISTS expires_ns BIGINT`,
		`ALTER TABLE agent_pull_connectors ADD COLUMN IF NOT EXISTS revoked_ns BIGINT`,
		// 全局注册表版本行（多实例收敛轮询用）。
		`INSERT INTO platform_settings (key, value) VALUES ('agent_registry_revision', '1') ON CONFLICT (key) DO NOTHING`,
	}
	return append(statements, channelEventsStatements()...)
}

// Migrate 执行幂等 DDL。
func (db *DB) Migrate(ctx context.Context) error {
	for _, statement := range schemaStatements() {
		if _, err := db.Pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
	}
	return nil
}

// builtinPositionSeed 是内置岗位（名称与既有 roleContracts 标签一致）。
var builtinPositionSeed = []struct{ Code, Name, Description string }{
	{position.CodeFundamentalAnalyst, "基本面分析师", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodeTechnicalAnalyst, "技术分析师", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodeStrategist, "策略分析师", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodeRiskManager, "风控经理", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodeExecutionTrader, "执行交易员", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodePortfolioManager, "组合经理", "内置岗位；角色契约由 golden 快照守护"},
	{position.CodeCustom, "自定义岗位", "迁移兜底岗位：未命中内置角色的智能体聘任于此"},
}

// Seed 写入内置岗位并回填存量智能体的聘任关系。幂等：已存在的行不覆盖
// （用户后续对名称/描述的编辑得以保留），已聘任的智能体不重复回填。
//
// 回填规则与旧栈 resolveRoleContract 语义一致：system_prompt 命中预置
// 角色 id → 对应岗位；空 → 基本面分析师（旧栈缺省即第一个角色）；
// 其余（自定义提示词）→ __CUSTOM__。
func (db *DB) Seed(ctx context.Context) error {
	now := nowTS()
	for _, seed := range builtinPositionSeed {
		_, err := db.Pool.Exec(ctx,
			`INSERT INTO job_positions (code, name, description, contract_ref, enabled, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $1, true, 1, $4, $4)
			 ON CONFLICT (code) DO NOTHING`,
			seed.Code, seed.Name, seed.Description, now)
		if err != nil {
			return fmt.Errorf("seed position %s: %w", seed.Code, err)
		}
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO agent_positions (agent_slug, position_code)
		SELECT a.slug, CASE lower(btrim(a.system_prompt))
			WHEN '' THEN 'FUNDAMENTAL_ANALYST'
			WHEN 'fundamental_analyst' THEN 'FUNDAMENTAL_ANALYST'
			WHEN 'technical_analyst' THEN 'TECHNICAL_ANALYST'
			WHEN 'strategist' THEN 'STRATEGIST'
			WHEN 'risk_manager' THEN 'RISK_MANAGER'
			WHEN 'execution_trader' THEN 'EXECUTION_TRADER'
			WHEN 'portfolio_manager' THEN 'PORTFOLIO_MANAGER'
			ELSE '__CUSTOM__' END
		FROM agents a
		WHERE NOT EXISTS (SELECT 1 FROM agent_positions ap WHERE ap.agent_slug = a.slug)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return fmt.Errorf("backfill agent staffing: %w", err)
	}
	return nil
}

// nowTS 返回定宽 RFC3339 UTC 时间戳。定宽（始终 9 位小数 + "Z"）保证
// TEXT 列上的 ORDER BY 与时间顺序一致。
func nowTS() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// formatTS 把时间格式化为存储形态。
func formatTS(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// parseTS 解析存储的 RFC3339 时间戳；解析失败返回零值（展示层容错）。
func parseTS(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// nsPtr / timeFromNS 处理可空纳秒时间戳列。
func nsPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	ns := t.UnixNano()
	return &ns
}

func timeFromNS(ns *int64) *time.Time {
	if ns == nil {
		return nil
	}
	t := time.Unix(0, *ns).UTC()
	return &t
}

// channelEventsStatements 是通道事件表的幂等 DDL（与旧栈一致）。
func channelEventsStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS channel_events (
			id          BIGSERIAL PRIMARY KEY,
			event_type  TEXT NOT NULL,
			agent_slug  TEXT,
			run_id      TEXT,
			url         TEXT NOT NULL DEFAULT '',
			status_code BIGINT,
			accepted    BOOLEAN,
			run_status  TEXT,
			pass        BOOLEAN,
			latency_ms  BIGINT,
			detail      TEXT,
			created_at  TEXT NOT NULL)`,
		`ALTER TABLE channel_events ADD COLUMN IF NOT EXISTS request_json TEXT`,
		`ALTER TABLE channel_events ADD COLUMN IF NOT EXISTS result_json TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_channel_events_type_created ON channel_events(event_type, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_events_slug ON channel_events(agent_slug, created_at DESC)`,
	}
}

// Ping 探活（仪表盘/就绪探针）。
func (db *DB) Ping(ctx context.Context) error { return db.Pool.Ping(ctx) }
