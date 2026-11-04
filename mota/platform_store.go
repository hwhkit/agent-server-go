package mota

// PostgreSQL 存储层:管理台数据(Agent / 运行记录 / 通道事件 / 会话 / 幂等)。
// 对应 Rust 侧 src/platform/store.rs。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// idempotencyStaleAfterSecs 是 processing 记录被判定为陈旧、可被接管的阈值。
const idempotencyStaleAfterSecs = 2 * 60 * 60

// periodicGCIntervalSecs 是 nonce / 会话过期清理的最小间隔。
const periodicGCIntervalSecs = 60

// ErrStaleProviderRevision 表示 Provider 在调用方读取后已被其他请求修改。
// errors.Is 可用于把它稳定映射为 HTTP 412。
var ErrStaleProviderRevision = errors.New("Provider revision changed; reload before retrying")

var ErrStaleAgentRevision = errors.New("Agent revision changed; reload before retrying")

// ErrAgentSlugExists provides a stable cross-replica mapping for the agents
// table's unique slug constraint.
var ErrAgentSlugExists = errors.New("Agent slug already exists")

// ErrLastEnabledAdmin is returned from the transactional account mutation
// fence when a change would leave the database without an enabled admin.
var ErrLastEnabledAdmin = errors.New("at least one enabled administrator is required")

// All account updates/deletes take the same PostgreSQL transaction advisory
// lock. The in-process mutex in AdminHandler only coordinates one replica;
// this lock preserves the invariant across every instance sharing the DB.
const accountMutationAdvisoryLockID int64 = 0x4d4f544141444d49

// agentColumns 是 agents 表的列顺序,decodeAgentRow 依赖该顺序。
const agentColumns = `id, slug, name, description, enabled, executor_kind, ` +
	`execution_mode, signing_secret_enc, signature_encoding, system_prompt, workspace_path, ` +
	`claude_model, allowed_tools_json, permission_mode, timeout_secs, ` +
	`max_concurrency, queue_capacity, revision, created_at, updated_at`

// AgentStore 是平台的 PostgreSQL 存储句柄。可安全并发使用。
type AgentStore struct {
	pool          *pgxpool.Pool
	cipher        *SecretCipher
	lastNonceGC   atomic.Int64
	lastSessionGC atomic.Int64
}

// AuditEvent describes the redacted security metadata that must commit in the
// same PostgreSQL transaction as a control-plane mutation. Details must never
// contain passwords, signing secrets, API keys, bearer tokens, or raw
// Authorization values; callers should record only facts such as revision,
// role, enabled state, and whether a secret was rotated.
type AuditEvent struct {
	Actor   string
	Action  string
	Target  string
	Details map[string]any
}

func validateMutationAudit(event AuditEvent, action, target string) error {
	if strings.TrimSpace(event.Actor) == "" {
		return errors.New("audit actor must not be empty")
	}
	if event.Action != action || event.Target != target {
		return fmt.Errorf("audit event must use action %q and target %q", action, target)
	}
	return nil
}

func cloneAuditEventWithFields(event *AuditEvent, fields map[string]any) *AuditEvent {
	if event == nil {
		return nil
	}
	cloned := &AuditEvent{
		Actor: event.Actor, Action: event.Action, Target: event.Target,
		Details: make(map[string]any, len(event.Details)+len(fields)),
	}
	for key, value := range event.Details {
		cloned.Details[key] = value
	}
	for key, value := range fields {
		cloned.Details[key] = value
	}
	return cloned
}

func insertAuditTx(ctx context.Context, tx pgx.Tx, event *AuditEvent) error {
	if event == nil {
		return nil
	}
	if strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Action) == "" ||
		strings.TrimSpace(event.Target) == "" {
		return errors.New("audit actor, action, and target must not be empty")
	}
	details := event.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit details for %s: %w", event.Action, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, details, created_at) `+
			`VALUES ($1, $2, $3, $4, $5)`,
		event.Actor, event.Action, event.Target, string(encoded), nowTimestamp()); err != nil {
		return fmt.Errorf("insert audit event %s: %w", event.Action, err)
	}
	return nil
}

// agentRunsSchemaSQL 是 agent_runs 的唯一 DDL 定义;Agent Pull 存储与平台
// 存储共用(两处都 IF NOT EXISTS,幂等)。
const agentRunsSchemaSQL = `CREATE TABLE IF NOT EXISTS agent_runs (
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
                PRIMARY KEY(agent_slug, run_id))`

// ConnectAgentStore 连接 PostgreSQL,并确保表结构存在。
func ConnectAgentStore(ctx context.Context, databaseURL string, cipher *SecretCipher) (*AgentStore, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL connection string for Agent platform storage: %w", err)
	}
	poolConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL for Agent platform storage: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL for Agent platform storage: %w", err)
	}
	store := &AgentStore{pool: pool, cipher: cipher}
	if err := store.initialize(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// Pool 暴露底层连接池,供 Push 幂等存储复用同一个库。
func (s *AgentStore) Pool() *pgxpool.Pool { return s.pool }

// Close 关闭连接池。
func (s *AgentStore) Close() error {
	s.pool.Close()
	return nil
}

// initialize 建表与补列。全部语句幂等,可重复执行。
func (s *AgentStore) initialize(ctx context.Context) error {
	statements := []string{
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
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS execution_mode TEXT NOT NULL DEFAULT 'gateway'`,
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
		agentRunsSchemaSQL,
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
		`CREATE TABLE IF NOT EXISTS channel_events (
                id            BIGSERIAL PRIMARY KEY,
                event_type    TEXT NOT NULL,
                agent_slug    TEXT,
                run_id        TEXT,
                url           TEXT NOT NULL,
                status_code   BIGINT,
                accepted      BOOLEAN,
                run_status    TEXT,
                pass          BOOLEAN,
                latency_ms    BIGINT,
                detail        TEXT,
                request_json  TEXT,
                result_json   TEXT,
                created_at    TEXT NOT NULL)`,
		`ALTER TABLE channel_events ADD COLUMN IF NOT EXISTS request_json TEXT`,
		`ALTER TABLE channel_events ADD COLUMN IF NOT EXISTS result_json TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_channel_events_type_created ON channel_events(event_type, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_events_slug ON channel_events(agent_slug, created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("initialize Agent platform schema: %w", err)
		}
	}
	return nil
}

// nowTimestamp 返回定宽的 RFC3339 UTC 时间戳。
//
// 定宽(始终 9 位小数 + "Z")保证 TEXT 列上的 ORDER BY created_at 与时间顺序一致;
// 变宽格式会让「无小数部分」的时间戳排到「有小数部分」之后。
func nowTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// parseTimestamp 解析存储的 RFC3339 时间戳。
func parseTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339, value)
}

// ---------------------------------------------------------------------------
// Agent CRUD
// ---------------------------------------------------------------------------

// storedAgent 是 agents 表一行的原始映射(签名密钥仍为密文)。
type storedAgent struct {
	id                int64
	slug              string
	name              string
	description       string
	enabled           bool
	executorKind      string
	executionMode     string
	signingSecretEnc  string
	signatureEncoding string
	systemPrompt      string
	workspacePath     string
	claudeModel       *string
	allowedToolsJSON  string
	permissionMode    string
	timeoutSecs       int64
	maxConcurrency    int64
	queueCapacity     int64
	revision          int64
	createdAt         string
	updatedAt         string
}

// scanAgentRow 按 agentColumns 的顺序读取一行。
func scanAgentRow(row pgx.Row) (*storedAgent, error) {
	stored := &storedAgent{}
	err := row.Scan(
		&stored.id, &stored.slug, &stored.name, &stored.description, &stored.enabled,
		&stored.executorKind, &stored.executionMode, &stored.signingSecretEnc,
		&stored.signatureEncoding, &stored.systemPrompt, &stored.workspacePath,
		&stored.claudeModel, &stored.allowedToolsJSON, &stored.permissionMode,
		&stored.timeoutSecs, &stored.maxConcurrency, &stored.queueCapacity,
		&stored.revision, &stored.createdAt, &stored.updatedAt,
	)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// decodeAgent 解密签名密钥并把枚举列解析为强类型。
func decodeAgent(stored *storedAgent, cipher *SecretCipher) (*AgentConfig, error) {
	signingSecret, err := cipher.Decrypt(stored.signingSecretEnc, stored.slug)
	if err != nil {
		return nil, err
	}
	executorKind, err := ParseExecutorKind(stored.executorKind)
	if err != nil {
		return nil, err
	}
	executionMode, err := ParseExecutionMode(stored.executionMode)
	if err != nil {
		return nil, err
	}
	var allowedTools []string
	// 解析失败时退化为空列表,与 Rust 侧 unwrap_or_default 一致。
	_ = json.Unmarshal([]byte(stored.allowedToolsJSON), &allowedTools)
	return &AgentConfig{
		ID:                stored.id,
		Slug:              stored.slug,
		Name:              stored.name,
		Description:       stored.description,
		Enabled:           stored.enabled,
		ExecutorKind:      executorKind,
		ExecutionMode:     executionMode,
		SigningSecret:     signingSecret,
		SignatureEncoding: stored.signatureEncoding,
		SystemPrompt:      stored.systemPrompt,
		WorkspacePath:     stored.workspacePath,
		ClaudeModel:       stored.claudeModel,
		AllowedTools:      allowedTools,
		PermissionMode:    stored.permissionMode,
		TimeoutSecs:       uint64(stored.timeoutSecs),
		MaxConcurrency:    int(stored.maxConcurrency),
		QueueCapacity:     int(stored.queueCapacity),
		Revision:          stored.revision,
		CreatedAt:         stored.createdAt,
		UpdatedAt:         stored.updatedAt,
	}, nil
}

// ListAgents 按创建时间倒序返回全部 Agent:新建的排在前。
func (s *AgentStore) ListAgents(ctx context.Context) ([]*AgentConfig, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+agentColumns+" FROM agents ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []*AgentConfig
	for rows.Next() {
		stored, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		agent, err := decodeAgent(stored, s.cipher)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// GetAgent 按 slug 取一个 Agent;不存在返回 (nil, nil)。
func (s *AgentStore) GetAgent(ctx context.Context, slug string) (*AgentConfig, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE slug = $1", slug)
	stored, err := scanAgentRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return decodeAgent(stored, s.cipher)
}

// CreateAgent 插入一个新 Agent,revision 从 1 开始。
func (s *AgentStore) CreateAgent(ctx context.Context, input *AgentInput) (*AgentConfig, error) {
	return s.createAgent(ctx, input, nil)
}

// CreateAgentAudited atomically inserts an Agent and its audit event.
func (s *AgentStore) CreateAgentAudited(
	ctx context.Context, input *AgentInput, audit AuditEvent,
) (*AgentConfig, error) {
	if err := validateMutationAudit(audit, "agent.create", input.Slug); err != nil {
		return nil, err
	}
	return s.createAgent(ctx, input, &audit)
}

func (s *AgentStore) createAgent(
	ctx context.Context, input *AgentInput, audit *AuditEvent,
) (*AgentConfig, error) {
	if input.SigningSecret == nil {
		return nil, errors.New("signing_secret is required")
	}
	encrypted, err := s.cipher.Encrypt(*input.SigningSecret, input.Slug)
	if err != nil {
		return nil, err
	}
	tools, err := json.Marshal(orEmptySlice(input.AllowedTools))
	if err != nil {
		return nil, err
	}
	now := nowTimestamp()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row := tx.QueryRow(ctx,
		`INSERT INTO agents (slug, name, description, enabled, executor_kind, `+
			`execution_mode, signing_secret_enc, signature_encoding, system_prompt, `+
			`workspace_path, claude_model, allowed_tools_json, permission_mode, `+
			`timeout_secs, max_concurrency, queue_capacity, revision, created_at, updated_at) `+
			`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, 1, $17, $17) `+
			`RETURNING `+agentColumns,
		input.Slug, input.Name, input.Description, input.EnabledOrDefault(),
		string(input.ExecutorKind), string(input.ExecutionMode), encrypted,
		input.SignatureEncoding, input.SystemPrompt, input.WorkspacePath,
		input.ClaudeModel, string(tools), input.PermissionMode,
		int64(input.TimeoutSecsOrDefault()), int64(input.MaxConcurrencyOrDefault()),
		int64(input.QueueCapacityOrDefault()), now,
	)
	stored, err := scanAgentRow(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrAgentSlugExists
		}
		return nil, err
	}
	agent, err := decodeAgent(stored, s.cipher)
	if err != nil {
		return nil, err
	}
	if err := bumpAgentRegistryRevision(ctx, tx); err != nil {
		return nil, err
	}
	audit = cloneAuditEventWithFields(audit, map[string]any{
		"revision": agent.Revision, "enabled": agent.Enabled,
		"executorKind": agent.ExecutorKind,
	})
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return agent, nil
}

// UpdateAgent 整体更新一个 Agent 并把 revision +1。input.SigningSecret 为 nil 时
// 保留原密钥。Agent 不存在时返回 (nil, nil)。
func (s *AgentStore) UpdateAgent(ctx context.Context, slug string, input *AgentInput) (*AgentConfig, error) {
	return s.updateAgentIfRevision(ctx, slug, input, nil, nil)
}

func (s *AgentStore) UpdateAgentIfRevision(
	ctx context.Context, slug string, input *AgentInput, expectedRevision *int64,
) (*AgentConfig, error) {
	return s.updateAgentIfRevision(ctx, slug, input, expectedRevision, nil)
}

// UpdateAgentIfRevisionAudited atomically updates an Agent and appends its
// audit event. A stale revision or audit failure rolls back both writes.
func (s *AgentStore) UpdateAgentIfRevisionAudited(
	ctx context.Context,
	slug string,
	input *AgentInput,
	expectedRevision *int64,
	audit AuditEvent,
) (*AgentConfig, error) {
	if err := validateMutationAudit(audit, "agent.update", slug); err != nil {
		return nil, err
	}
	return s.updateAgentIfRevision(ctx, slug, input, expectedRevision, &audit)
}

func (s *AgentStore) updateAgentIfRevision(
	ctx context.Context,
	slug string,
	input *AgentInput,
	expectedRevision *int64,
	audit *AuditEvent,
) (*AgentConfig, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentSecret string
	var revision int64
	err = tx.QueryRow(ctx,
		"SELECT signing_secret_enc, revision FROM agents WHERE slug = $1 FOR UPDATE", slug).
		Scan(&currentSecret, &revision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if expectedRevision != nil && revision != *expectedRevision {
		return nil, ErrStaleAgentRevision
	}

	encrypted := currentSecret
	if input.SigningSecret != nil {
		encrypted, err = s.cipher.Encrypt(*input.SigningSecret, slug)
		if err != nil {
			return nil, err
		}
	}
	tools, err := json.Marshal(orEmptySlice(input.AllowedTools))
	if err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx,
		`UPDATE agents SET name = $2, description = $3, enabled = $4, `+
			`executor_kind = $5, execution_mode = $6, signing_secret_enc = $7, `+
			`signature_encoding = $8, system_prompt = $9, workspace_path = $10, `+
			`claude_model = $11, allowed_tools_json = $12, permission_mode = $13, `+
			`timeout_secs = $14, max_concurrency = $15, queue_capacity = $16, `+
			`revision = $17, updated_at = $18 WHERE slug = $1 RETURNING `+agentColumns,
		slug, input.Name, input.Description, input.EnabledOrDefault(),
		string(input.ExecutorKind), string(input.ExecutionMode), encrypted,
		input.SignatureEncoding, input.SystemPrompt, input.WorkspacePath,
		input.ClaudeModel, string(tools), input.PermissionMode,
		int64(input.TimeoutSecsOrDefault()), int64(input.MaxConcurrencyOrDefault()),
		int64(input.QueueCapacityOrDefault()), revision+1, nowTimestamp(),
	)
	stored, err := scanAgentRow(row)
	if err != nil {
		return nil, err
	}
	agent, err := decodeAgent(stored, s.cipher)
	if err != nil {
		return nil, err
	}
	if err := bumpAgentRegistryRevision(ctx, tx); err != nil {
		return nil, err
	}
	audit = cloneAuditEventWithFields(audit, map[string]any{
		"revision": agent.Revision, "enabled": agent.Enabled,
		"executorKind": agent.ExecutorKind,
	})
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return agent, nil
}

// DeleteAgent 删除一个 Agent;返回是否确实删掉了一行。
func (s *AgentStore) DeleteAgent(ctx context.Context, slug string) (bool, error) {
	return s.deleteAgent(ctx, slug, nil)
}

// DeleteAgentAudited atomically deletes an Agent and appends its audit event.
func (s *AgentStore) DeleteAgentAudited(
	ctx context.Context, slug string, audit AuditEvent,
) (bool, error) {
	if err := validateMutationAudit(audit, "agent.delete", slug); err != nil {
		return false, err
	}
	return s.deleteAgent(ctx, slug, &audit)
}

func (s *AgentStore) deleteAgent(
	ctx context.Context, slug string, audit *AuditEvent,
) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "DELETE FROM agents WHERE slug = $1", slug)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := bumpAgentRegistryRevision(ctx, tx); err != nil {
		return false, err
	}
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func bumpAgentRegistryRevision(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ('agent_registry_revision', '1') `+
			`ON CONFLICT (key) DO UPDATE SET value = `+
			`(platform_settings.value::bigint + 1)::text`)
	return err
}

func (s *AgentStore) AgentRegistryRevision(ctx context.Context) (int64, error) {
	var text string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key = 'agent_registry_revision'`).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	revision, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse Agent registry revision: %w", err)
	}
	return revision, nil
}

// ListAgentSecrets 列出所有启用 Agent 的 (slug, 明文通道密钥),供 Push 心跳按
// Agent 发送。
func (s *AgentStore) ListAgentSecrets(ctx context.Context) ([][2]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT slug, signing_secret_enc FROM agents WHERE enabled = true")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var secrets [][2]string
	for rows.Next() {
		var slug, encrypted string
		if err := rows.Scan(&slug, &encrypted); err != nil {
			return nil, err
		}
		secret, err := s.cipher.Decrypt(encrypted, slug)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, [2]string{slug, secret})
	}
	return secrets, rows.Err()
}

func orEmptySlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// ---------------------------------------------------------------------------
// 重放保护 / 会话 / 审计
// ---------------------------------------------------------------------------

// claimPeriodicGC 做节流:距上次清理不足 periodicGCIntervalSecs 秒时返回 false。
// CAS 保证并发下只有一个调用者真正执行清理。
func claimPeriodicGC(lastRun *atomic.Int64, now int64) bool {
	previous := lastRun.Load()
	if now-previous < periodicGCIntervalSecs {
		return false
	}
	return lastRun.CompareAndSwap(previous, now)
}

// ConsumeRequestNonce 记录一个签名 nonce。返回 true 表示首次使用;false 表示重放。
func (s *AgentStore) ConsumeRequestNonce(ctx context.Context, agentSlug, nonce string, expiresAt int64) (bool, error) {
	now := time.Now().Unix()
	shouldGC := claimPeriodicGC(&s.lastNonceGC, now)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if shouldGC {
		if _, err := tx.Exec(ctx, "DELETE FROM request_nonces WHERE expires_at <= $1", now); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO request_nonces (agent_slug, nonce, expires_at) `+
			`VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		agentSlug, nonce, expiresAt)
	if err != nil {
		return false, err
	}
	inserted := tag.RowsAffected() == 1
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return inserted, nil
}

// EnsureBootstrapAdmin 把旧版单管理员配置迁移为数据库账号；已存在账号绝不
// 覆盖密码、角色或启停状态。
func (s *AgentStore) EnsureBootstrapAdmin(ctx context.Context, username, passwordHash string) error {
	now := nowTimestamp()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_users (username, password_hash, role, enabled, created_at, updated_at) `+
			`VALUES ($1, $2, 'admin', true, $3, $3) ON CONFLICT (username) DO NOTHING`,
		username, passwordHash, now)
	return err
}

func (s *AgentStore) GetUserAuth(ctx context.Context, username string) (*UserAuthRecord, error) {
	var record UserAuthRecord
	var role string
	err := s.pool.QueryRow(ctx,
		"SELECT username, password_hash, role, enabled FROM admin_users WHERE username = $1",
		username).Scan(&record.Username, &record.PasswordHash, &role, &record.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parsed, ok := ParseUserRole(role)
	if !ok {
		return nil, fmt.Errorf("invalid stored user role: %s", role)
	}
	record.Role = parsed
	return &record, nil
}

func (s *AgentStore) GetUser(ctx context.Context, username string) (*UserView, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT username, role, enabled, created_at, updated_at FROM admin_users WHERE username = $1",
		username)
	view, err := scanUserView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return view, err
}

func (s *AgentStore) ListUsers(ctx context.Context) ([]UserView, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT username, role, enabled, created_at, updated_at FROM admin_users ORDER BY created_at ASC, username ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []UserView{}
	for rows.Next() {
		view, err := scanUserView(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *view)
	}
	return users, rows.Err()
}

func (s *AgentStore) CreateUser(ctx context.Context, username, passwordHash string, role UserRole) (bool, error) {
	return s.createUser(ctx, username, passwordHash, role, nil)
}

// CreateUserAudited atomically creates an account and appends its audit event.
func (s *AgentStore) CreateUserAudited(
	ctx context.Context,
	username, passwordHash string,
	role UserRole,
	audit AuditEvent,
) (bool, error) {
	if err := validateMutationAudit(audit, "user.create", username); err != nil {
		return false, err
	}
	return s.createUser(ctx, username, passwordHash, role, &audit)
}

func (s *AgentStore) createUser(
	ctx context.Context,
	username, passwordHash string,
	role UserRole,
	audit *AuditEvent,
) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := nowTimestamp()
	tag, err := tx.Exec(ctx,
		`INSERT INTO admin_users (username, password_hash, role, enabled, created_at, updated_at) `+
			`VALUES ($1, $2, $3, true, $4, $4) ON CONFLICT (username) DO NOTHING`,
		username, passwordHash, string(role), now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *AgentStore) UpdateUser(ctx context.Context, username string, passwordHash *string, role UserRole, enabled bool) (*UserView, error) {
	return s.updateUser(ctx, username, passwordHash, role, enabled, nil)
}

// UpdateUserAudited atomically updates an account, revokes affected sessions,
// and appends its audit event.
func (s *AgentStore) UpdateUserAudited(
	ctx context.Context,
	username string,
	passwordHash *string,
	role UserRole,
	enabled bool,
	audit AuditEvent,
) (*UserView, error) {
	if err := validateMutationAudit(audit, "user.update", username); err != nil {
		return nil, err
	}
	return s.updateUser(ctx, username, passwordHash, role, enabled, &audit)
}

func (s *AgentStore) updateUser(
	ctx context.Context,
	username string,
	passwordHash *string,
	role UserRole,
	enabled bool,
	audit *AuditEvent,
) (*UserView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", accountMutationAdvisoryLockID); err != nil {
		return nil, err
	}
	var currentRole string
	var currentEnabled bool
	if err := tx.QueryRow(ctx,
		"SELECT role, enabled FROM admin_users WHERE username = $1 FOR UPDATE", username).
		Scan(&currentRole, &currentEnabled); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if currentRole == string(UserRoleAdmin) && currentEnabled &&
		(role != UserRoleAdmin || !enabled) {
		var count int64
		if err := tx.QueryRow(ctx,
			"SELECT COUNT(*) FROM admin_users WHERE role = 'admin' AND enabled = true").
			Scan(&count); err != nil {
			return nil, err
		}
		if count <= 1 {
			return nil, ErrLastEnabledAdmin
		}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE admin_users SET password_hash = COALESCE($2, password_hash), `+
			`role = $3, enabled = $4, updated_at = $5 WHERE username = $1`,
		username, passwordHash, string(role), enabled, nowTimestamp())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	if passwordHash != nil || !enabled {
		if _, err := tx.Exec(ctx, "DELETE FROM admin_sessions WHERE username = $1", username); err != nil {
			return nil, err
		}
	}
	view, err := scanUserView(tx.QueryRow(ctx,
		"SELECT username, role, enabled, created_at, updated_at FROM admin_users WHERE username = $1",
		username))
	if err != nil {
		return nil, err
	}
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

func (s *AgentStore) DeleteUser(ctx context.Context, username string) (bool, error) {
	return s.deleteUser(ctx, username, nil)
}

// DeleteUserAudited atomically deletes an account and appends its audit event.
func (s *AgentStore) DeleteUserAudited(
	ctx context.Context, username string, audit AuditEvent,
) (bool, error) {
	if err := validateMutationAudit(audit, "user.delete", username); err != nil {
		return false, err
	}
	return s.deleteUser(ctx, username, &audit)
}

func (s *AgentStore) deleteUser(
	ctx context.Context, username string, audit *AuditEvent,
) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", accountMutationAdvisoryLockID); err != nil {
		return false, err
	}
	var currentRole string
	var currentEnabled bool
	if err := tx.QueryRow(ctx,
		"SELECT role, enabled FROM admin_users WHERE username = $1 FOR UPDATE", username).
		Scan(&currentRole, &currentEnabled); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if currentRole == string(UserRoleAdmin) && currentEnabled {
		var count int64
		if err := tx.QueryRow(ctx,
			"SELECT COUNT(*) FROM admin_users WHERE role = 'admin' AND enabled = true").
			Scan(&count); err != nil {
			return false, err
		}
		if count <= 1 {
			return false, ErrLastEnabledAdmin
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM admin_sessions WHERE username = $1", username); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "DELETE FROM admin_users WHERE username = $1", username)
	if err != nil {
		return false, err
	}
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *AgentStore) EnabledAdminCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM admin_users WHERE role = 'admin' AND enabled = true").Scan(&count)
	return count, err
}

func scanUserView(row pgx.Row) (*UserView, error) {
	var view UserView
	var role string
	if err := row.Scan(&view.Username, &role, &view.Enabled, &view.CreatedAt, &view.UpdatedAt); err != nil {
		return nil, err
	}
	parsed, ok := ParseUserRole(role)
	if !ok {
		return nil, fmt.Errorf("invalid stored user role: %s", role)
	}
	view.Role = parsed
	return &view, nil
}

// CreateSession 新建或替换一条管理会话。
func (s *AgentStore) CreateSession(ctx context.Context, tokenHash, username, csrfToken string, expiresAt int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, username, csrf_token, expires_at, created_at) `+
			`VALUES ($1, $2, $3, $4, $5) `+
			`ON CONFLICT (token_hash) DO UPDATE SET `+
			`  username = EXCLUDED.username, csrf_token = EXCLUDED.csrf_token, `+
			`  expires_at = EXCLUDED.expires_at, created_at = EXCLUDED.created_at`,
		tokenHash, username, csrfToken, expiresAt, nowTimestamp())
	return err
}

// GetSession 取一条未过期的管理会话;不存在或已过期返回 (nil, nil)。
func (s *AgentStore) GetSession(ctx context.Context, tokenHash string) (*SessionRecord, error) {
	now := time.Now().Unix()
	if claimPeriodicGC(&s.lastSessionGC, now) {
		if _, err := s.pool.Exec(ctx, "DELETE FROM admin_sessions WHERE expires_at <= $1", now); err != nil {
			return nil, err
		}
	}
	record := &SessionRecord{}
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT s.username, u.role, s.csrf_token, s.expires_at FROM admin_sessions s `+
			`JOIN admin_users u ON u.username = s.username `+
			`WHERE s.token_hash = $1 AND s.expires_at > $2 AND u.enabled = true`, tokenHash, now).
		Scan(&record.Username, &role, &record.CSRFToken, &record.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	parsed, ok := ParseUserRole(role)
	if !ok {
		return nil, fmt.Errorf("invalid stored user role: %s", role)
	}
	record.Role = parsed
	return record, nil
}

// DeleteSession 注销一条管理会话。
func (s *AgentStore) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM admin_sessions WHERE token_hash = $1", tokenHash)
	return err
}

// RecordAudit 追加一条审计日志。
func (s *AgentStore) RecordAudit(ctx context.Context, actor, action, target string, details any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, details, created_at) `+
			`VALUES ($1, $2, $3, $4, $5)`,
		actor, action, target, string(encoded), nowTimestamp())
	return err
}

// ListAuditLogs 按时间倒序读取最近审计。actor 非空时只返回该用户自己的记录。
func (s *AgentStore) ListAuditLogs(ctx context.Context, limit int, actor string) ([]AuditLogView, error) {
	limit = clampInt(limit, 1, 200)
	const columns = "id, actor, action, target, details, created_at"
	var rows pgx.Rows
	var err error
	if actor != "" {
		rows, err = s.pool.Query(ctx, "SELECT "+columns+" FROM audit_log WHERE actor = $1 ORDER BY created_at DESC, id DESC LIMIT $2", actor, limit)
	} else {
		rows, err = s.pool.Query(ctx, "SELECT "+columns+" FROM audit_log ORDER BY created_at DESC, id DESC LIMIT $1", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []AuditLogView{}
	for rows.Next() {
		var view AuditLogView
		var details string
		if err := rows.Scan(&view.ID, &view.Actor, &view.Action, &view.Target, &details, &view.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(details), &view.Details); err != nil {
			view.Details = details
		}
		logs = append(logs, view)
	}
	return logs, rows.Err()
}

// GetExecutorProvider 读取智能体中心 Provider 配置。整份 JSON 使用平台主密钥
// 加密存储，避免 API key 与 MCP Authorization 以明文落库。
func (s *AgentStore) GetExecutorProvider(ctx context.Context, providerID string) (map[string]any, error) {
	key := "executor_provider:" + providerID
	var encrypted string
	err := s.pool.QueryRow(ctx, "SELECT value FROM platform_settings WHERE key = $1", key).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plaintext, err := s.cipher.Decrypt(encrypted, key)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(plaintext), &value); err != nil {
		return nil, fmt.Errorf("decode stored executor provider %s: %w", providerID, err)
	}
	return value, nil
}

// PutExecutorProvider 加密并保存一份 Provider 配置。
func (s *AgentStore) PutExecutorProvider(ctx context.Context, providerID string, value map[string]any) error {
	_, err := s.PutExecutorProviderGroupIfRevision(ctx, providerID,
		map[string]map[string]any{providerID: value}, nil)
	return err
}

// PutExecutorProviderGroupIfRevision 在一个事务里保存共享字段的 Provider 别名组。
// primaryID 的 revision 用于乐观并发控制；同组每个别名的 revision 都会递增，
// 因而更新任一入口都会让其他入口已经打开的编辑表单失效。
func (s *AgentStore) PutExecutorProviderGroupIfRevision(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
) (int64, error) {
	return s.putExecutorProviderGroupIfRevision(ctx, primaryID, values, expectedRevision, nil)
}

// PutExecutorProviderGroupIfRevisionAudited atomically saves a Provider alias
// group and appends its redacted audit event. CAS or audit failures leave the
// encrypted Provider values and every revision unchanged.
func (s *AgentStore) PutExecutorProviderGroupIfRevisionAudited(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
	audit AuditEvent,
) (int64, error) {
	if err := validateMutationAudit(audit, "provider.update", primaryID); err != nil {
		return 0, err
	}
	return s.putExecutorProviderGroupIfRevision(
		ctx, primaryID, values, expectedRevision, &audit)
}

func (s *AgentStore) putExecutorProviderGroupIfRevision(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
	audit *AuditEvent,
) (int64, error) {
	if _, ok := values[primaryID]; !ok || len(values) == 0 {
		return 0, errors.New("grouped Provider save must include its primary Provider")
	}
	type encryptedProvider struct {
		id, key, value string
	}
	encryptedValues := make([]encryptedProvider, 0, len(values))
	for id, value := range values {
		key := "executor_provider:" + id
		plaintext, err := json.Marshal(value)
		if err != nil {
			return 0, err
		}
		encrypted, err := s.cipher.Encrypt(string(plaintext), key)
		if err != nil {
			return 0, err
		}
		encryptedValues = append(encryptedValues, encryptedProvider{id: id, key: key, value: encrypted})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ('executor_provider_revision', '0') `+
			`ON CONFLICT DO NOTHING`); err != nil {
		return 0, err
	}
	var globalText string
	if err := tx.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key = 'executor_provider_revision' FOR UPDATE`).Scan(&globalText); err != nil {
		return 0, err
	}
	globalRevision, err := strconv.ParseInt(globalText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse global executor provider revision: %w", err)
	}
	primaryRevision, err := executorProviderItemRevision(ctx, tx, primaryID)
	if err != nil {
		return 0, err
	}
	if expectedRevision != nil && *expectedRevision != primaryRevision {
		return 0, ErrStaleProviderRevision
	}

	for _, item := range encryptedValues {
		revision := primaryRevision
		if item.id != primaryID {
			revision, err = executorProviderItemRevision(ctx, tx, item.id)
			if err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO platform_settings (key, value) VALUES ($1, $2) `+
				`ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, item.key, item.value); err != nil {
			return 0, err
		}
		revisionKey := "executor_provider_item_revision:" + item.id
		if _, err := tx.Exec(ctx,
			`INSERT INTO platform_settings (key, value) VALUES ($1, $2) `+
				`ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
			revisionKey, strconv.FormatInt(revision+1, 10)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE platform_settings SET value = $1 WHERE key = 'executor_provider_revision'`,
		strconv.FormatInt(globalRevision+1, 10)); err != nil {
		return 0, err
	}
	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return primaryRevision + 1, nil
}

func executorProviderItemRevision(ctx context.Context, tx pgx.Tx, providerID string) (int64, error) {
	key := "executor_provider_item_revision:" + providerID
	var text string
	err := tx.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key = $1`, key).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	revision, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse executor provider revision: %w", err)
	}
	return revision, nil
}

// ListExecutorProviderRevisions 返回每个 Provider 当前的乐观锁版本。
func (s *AgentStore) ListExecutorProviderRevisions(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM platform_settings WHERE key LIKE 'executor_provider_item_revision:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := map[string]int64{}
	const prefix = "executor_provider_item_revision:"
	for rows.Next() {
		var key, text string
		if err := rows.Scan(&key, &text); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		revision, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse executor provider revision: %w", err)
		}
		revisions[strings.TrimPrefix(key, prefix)] = revision
	}
	return revisions, rows.Err()
}

// ExecutorProviderRevision 返回任一 Provider 变更都会递增的全局版本。
func (s *AgentStore) ExecutorProviderRevision(ctx context.Context) (int64, error) {
	var text string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key = 'executor_provider_revision'`).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	revision, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse global executor provider revision: %w", err)
	}
	return revision, nil
}

// ---------------------------------------------------------------------------
// 运行记录
// ---------------------------------------------------------------------------

// RunFinalization 是一次运行的终态写入参数。
type RunFinalization struct {
	AgentSlug          string
	RunID              string
	Status             string
	DurationMs         int64
	ErrorCode          *string
	ResponseCode       *int64
	ResultJSON         *string
	ExecutorResultJSON *string
	Idempotency        *IdempotencyFinalization
}

// StartRun 登记一次运行开始。run_id 已存在时返回 false(调用方据此判冲突)。
func (s *AgentStore) StartRun(
	ctx context.Context,
	agentSlug, runID string,
	revision int64,
	executorKind, executionMode string,
	requestJSON *string,
) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO agent_runs `+
			`(agent_slug, run_id, agent_revision, executor_kind, execution_mode, status, request_json, created_at) `+
			`VALUES ($1, $2, $3, $4, $5, 'running', $6, $7) ON CONFLICT DO NOTHING`,
		agentSlug, runID, revision, executorKind, executionMode, requestJSON, nowTimestamp())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinalizeRun 在一个事务里写入运行终态与对应的幂等状态迁移。lease 检查保证已被
// 更新请求接管的记录不会被过期请求改写。
func (s *AgentStore) FinalizeRun(ctx context.Context, finalization RunFinalization) error {
	ctx, cancel := stateFinalizationContext(ctx)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Keep the lock order consistent with BeginIdempotency: idempotency row
	// first, run row second. Besides avoiding a stale-takeover race, this makes
	// an obsolete worker fail before it can mutate the run terminal state.
	if transition := finalization.Idempotency; transition != nil {
		var leaseID, status string
		err := tx.QueryRow(ctx,
			`SELECT lease_id, status FROM idempotency_records `+
				`WHERE agent_slug = $1 AND key = $2 FOR UPDATE`,
			finalization.AgentSlug, transition.Key).Scan(&leaseID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("idempotency lease is no longer owned by this request")
		}
		if err != nil {
			return err
		}
		if leaseID != transition.LeaseID || status != "processing" {
			return errors.New("idempotency lease is no longer owned by this request")
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE agent_runs SET status = $3, duration_ms = $4, error_code = $5, `+
			`response_code = $6, result_json = $7, executor_result_json = $8, completed_at = $9 `+
			`WHERE agent_slug = $1 AND run_id = $2 AND status = 'running'`,
		finalization.AgentSlug, finalization.RunID, finalization.Status,
		finalization.DurationMs, finalization.ErrorCode, finalization.ResponseCode,
		finalization.ResultJSON, finalization.ExecutorResultJSON, nowTimestamp())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("run is no longer in the running state")
	}

	if transition := finalization.Idempotency; transition != nil {
		switch transition.Kind {
		case IdempotencyFinalizeComplete:
			tag, err := tx.Exec(ctx,
				`UPDATE idempotency_records SET status = 'completed', `+
					`response_body = $4, updated_at = $5 `+
					`WHERE agent_slug = $1 AND key = $2 AND lease_id = $3 `+
					`AND status = 'processing'`,
				finalization.AgentSlug, transition.Key, transition.LeaseID,
				transition.ResponseBody, nowTimestamp())
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("idempotency lease is no longer owned by this request")
			}
		case IdempotencyFinalizeRelease:
			tag, err := tx.Exec(ctx,
				`DELETE FROM idempotency_records WHERE agent_slug = $1 `+
					`AND key = $2 AND lease_id = $3 AND status = 'processing'`,
				finalization.AgentSlug, transition.Key, transition.LeaseID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("idempotency lease is no longer owned by this request")
			}
		}
	}
	return tx.Commit(ctx)
}

// GetRun 按主键读取单次运行记录。A2A 的 GetTask 需要它:ListRuns 是面向管理台
// 的倒序列表,无法定位具体 run。不存在时返回 (nil, nil)。
func (s *AgentStore) GetRun(ctx context.Context, agentSlug, runID string) (*RunView, error) {
	const selectRun = `SELECT run_id, agent_slug, agent_revision, executor_kind, execution_mode, ` +
		`status, duration_ms, error_code, response_code, request_json, result_json, ` +
		`executor_result_json, created_at, completed_at FROM agent_runs ` +
		`WHERE agent_slug = $1 AND run_id = $2`

	var view RunView
	err := s.pool.QueryRow(ctx, selectRun, agentSlug, runID).Scan(
		&view.RunID, &view.AgentSlug, &view.AgentRevision, &view.ExecutorKind,
		&view.ExecutionMode, &view.Status, &view.DurationMs, &view.ErrorCode,
		&view.ResponseCode, &view.Input, &view.Output, &view.ExecutorOutput,
		&view.CreatedAt, &view.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &view, nil
}

// ListRuns 按时间倒序列出运行预览。slug 非空时只列该 Agent；大字段在 SQL 中
// 截断，limit 收敛到 1..50。
func (s *AgentStore) ListRuns(ctx context.Context, limit int, slug string) ([]RunView, error) {
	limit = clampInt(limit, 1, 50)
	const selectRuns = `SELECT run_id, agent_slug, agent_revision, executor_kind, execution_mode, ` +
		`status, duration_ms, error_code, response_code, LEFT(request_json, 4096), ` +
		`LEFT(result_json, 4096), LEFT(executor_result_json, 4096), created_at, completed_at ` +
		`FROM agent_runs`

	var rows pgx.Rows
	var err error
	if slug != "" {
		rows, err = s.pool.Query(ctx,
			selectRuns+" WHERE agent_slug = $1 ORDER BY created_at DESC LIMIT $2", slug, limit)
	} else {
		rows, err = s.pool.Query(ctx, selectRuns+" ORDER BY created_at DESC LIMIT $1", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	views := []RunView{}
	for rows.Next() {
		var view RunView
		if err := rows.Scan(
			&view.RunID, &view.AgentSlug, &view.AgentRevision, &view.ExecutorKind,
			&view.ExecutionMode, &view.Status, &view.DurationMs, &view.ErrorCode,
			&view.ResponseCode, &view.Input, &view.Output, &view.ExecutorOutput,
			&view.CreatedAt, &view.CompletedAt,
		); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// ---------------------------------------------------------------------------
// 通道事件
// ---------------------------------------------------------------------------

// ChannelEventRecord 是一次通道事件(callback / heartbeat)的写入参数。
type ChannelEventRecord struct {
	EventType  string
	AgentSlug  *string
	RunID      *string
	URL        string
	StatusCode *int64
	Accepted   *bool
	RunStatus  *string
	Pass       *bool
	LatencyMs  *int64
	Detail     *string
	Input      *string
	Output     *string
}

// RecordChannelEvent 记录一次通道事件及其完整请求/响应 JSON。
func (s *AgentStore) RecordChannelEvent(ctx context.Context, event ChannelEventRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO channel_events `+
			`(event_type, agent_slug, run_id, url, status_code, accepted, run_status, `+
			` pass, latency_ms, detail, request_json, result_json, created_at) `+
			`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		event.EventType, event.AgentSlug, event.RunID, event.URL, event.StatusCode,
		event.Accepted, event.RunStatus, event.Pass, event.LatencyMs, event.Detail,
		event.Input, event.Output, nowTimestamp())
	return err
}

// ListChannelEvents 查询通道事件预览，按 eventType / slug 过滤，时间倒序。
func (s *AgentStore) ListChannelEvents(ctx context.Context, eventType, slug string, limit int) ([]ChannelEventView, error) {
	limit = clampInt(limit, 1, 50)
	const selectEvents = `SELECT event_type, agent_slug, run_id, url, status_code, accepted, ` +
		`run_status, pass, latency_ms, LEFT(detail, 2048), LEFT(request_json, 4096), ` +
		`LEFT(result_json, 4096), created_at FROM channel_events`

	var rows pgx.Rows
	var err error
	switch {
	case eventType != "" && slug != "":
		rows, err = s.pool.Query(ctx, selectEvents+
			" WHERE event_type = $1 AND agent_slug = $2 ORDER BY created_at DESC LIMIT $3",
			eventType, slug, limit)
	case eventType != "":
		rows, err = s.pool.Query(ctx, selectEvents+
			" WHERE event_type = $1 ORDER BY created_at DESC LIMIT $2", eventType, limit)
	case slug != "":
		rows, err = s.pool.Query(ctx, selectEvents+
			" WHERE agent_slug = $1 ORDER BY created_at DESC LIMIT $2", slug, limit)
	default:
		rows, err = s.pool.Query(ctx, selectEvents+" ORDER BY created_at DESC LIMIT $1", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	views := []ChannelEventView{}
	for rows.Next() {
		var view ChannelEventView
		if err := rows.Scan(
			&view.EventType, &view.AgentSlug, &view.RunID, &view.URL, &view.StatusCode,
			&view.Accepted, &view.RunStatus, &view.Pass, &view.LatencyMs, &view.Detail,
			&view.Input, &view.Output, &view.CreatedAt,
		); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

// IdempotencyDecisionKind 是幂等判定结果的种类。
type IdempotencyDecisionKind int

const (
	// IdempotencyNew 本请求取得租约,应当真正执行。
	IdempotencyNew IdempotencyDecisionKind = iota
	// IdempotencyCached 已有完成结果,直接回放。
	IdempotencyCached
	// IdempotencyInProgress 同键请求正在执行中。
	IdempotencyInProgress
	// IdempotencyConflict 同键请求的元数据与本次不一致。
	IdempotencyConflict
)

// IdempotencyDecision 是 BeginIdempotency 的判定结果。
type IdempotencyDecision struct {
	Kind IdempotencyDecisionKind
	// LeaseID 仅 IdempotencyNew 时有值。
	LeaseID string
	// RunID 在 IdempotencyNew 与 IdempotencyCached 时有值。
	RunID string
	// ResponseBody 仅 IdempotencyCached 时有值。
	ResponseBody string
}

// IdempotencyFinalizationKind 是幂等记录的终态迁移种类。
type IdempotencyFinalizationKind int

const (
	// IdempotencyFinalizeComplete 把记录标记完成并缓存响应体。
	IdempotencyFinalizeComplete IdempotencyFinalizationKind = iota
	// IdempotencyFinalizeRelease 删除记录,允许后续请求重试。
	IdempotencyFinalizeRelease
)

// IdempotencyFinalization 描述随运行终态一起提交的幂等迁移。
type IdempotencyFinalization struct {
	Kind         IdempotencyFinalizationKind
	Key          string
	LeaseID      string
	ResponseBody string
}

// CompleteIdempotency 构造「完成并缓存响应」的迁移。
func CompleteIdempotency(key, leaseID, responseBody string) *IdempotencyFinalization {
	return &IdempotencyFinalization{
		Kind: IdempotencyFinalizeComplete, Key: key, LeaseID: leaseID, ResponseBody: responseBody,
	}
}

// ReleaseIdempotencyTransition 构造「释放租约」的迁移。
func ReleaseIdempotencyTransition(key, leaseID string) *IdempotencyFinalization {
	return &IdempotencyFinalization{Kind: IdempotencyFinalizeRelease, Key: key, LeaseID: leaseID}
}

// BeginIdempotency 为一次请求申请幂等租约。
//
// enforceRunID 为真(调用方显式指定了 runId)时,已存在记录的 runId 必须与本次
// 一致,否则判 Conflict。
func (s *AgentStore) BeginIdempotency(
	ctx context.Context,
	agentSlug, key, requestHash, runID string,
	enforceRunID bool,
) (IdempotencyDecision, error) {
	newLeaseID := randomToken(18)
	now := time.Now().UTC()
	nowText := nowTimestamp()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IdempotencyDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`INSERT INTO idempotency_records `+
			`(agent_slug, key, request_hash, status, lease_id, run_id, updated_at) `+
			`VALUES ($1, $2, $3, 'processing', $4, $5, $6) ON CONFLICT DO NOTHING`,
		agentSlug, key, requestHash, newLeaseID, runID, nowText)
	if err != nil {
		return IdempotencyDecision{}, err
	}
	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return IdempotencyDecision{}, err
		}
		return IdempotencyDecision{Kind: IdempotencyNew, LeaseID: newLeaseID, RunID: runID}, nil
	}

	var storedHash, status, updatedAt string
	var response, storedRunID *string
	err = tx.QueryRow(ctx,
		`SELECT request_hash, status, response_body, updated_at, run_id `+
			`FROM idempotency_records WHERE agent_slug = $1 AND key = $2 FOR UPDATE`, agentSlug, key).
		Scan(&storedHash, &status, &response, &updatedAt, &storedRunID)
	if err != nil {
		return IdempotencyDecision{}, err
	}

	decision, err := s.resolveIdempotency(ctx, tx, idempotencyLookup{
		agentSlug:    agentSlug,
		key:          key,
		requestHash:  requestHash,
		runID:        runID,
		enforceRunID: enforceRunID,
		newLeaseID:   newLeaseID,
		now:          now,
		nowText:      nowText,
		storedHash:   storedHash,
		status:       status,
		response:     response,
		updatedAt:    updatedAt,
		storedRunID:  storedRunID,
	})
	if err != nil {
		return IdempotencyDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IdempotencyDecision{}, err
	}
	return decision, nil
}

// idempotencyLookup 汇总已存在记录的判定输入。
type idempotencyLookup struct {
	agentSlug    string
	key          string
	requestHash  string
	runID        string
	enforceRunID bool
	newLeaseID   string
	now          time.Time
	nowText      string
	storedHash   string
	status       string
	response     *string
	updatedAt    string
	storedRunID  *string
}

// resolveIdempotency 处理「记录已存在」的全部分支。
func (s *AgentStore) resolveIdempotency(ctx context.Context, tx pgx.Tx, in idempotencyLookup) (IdempotencyDecision, error) {
	if in.storedHash != in.requestHash {
		return IdempotencyDecision{Kind: IdempotencyConflict}, nil
	}

	// 迁移前遗留的缓存行无法关联到真实运行。不要在回放时伪造一个「原始」
	// run ID:作废旧缓存,在新租约下重新执行一次。
	if in.status == "completed" && in.storedRunID == nil {
		tag, err := tx.Exec(ctx,
			`UPDATE idempotency_records SET status = 'processing', `+
				`response_body = NULL, lease_id = $3, run_id = $4, updated_at = $5 `+
				`WHERE agent_slug = $1 AND key = $2`,
			in.agentSlug, in.key, in.newLeaseID, in.runID, in.nowText)
		if err != nil {
			return IdempotencyDecision{}, err
		}
		if tag.RowsAffected() != 1 {
			return IdempotencyDecision{}, errors.New("failed to migrate legacy idempotency record")
		}
		return IdempotencyDecision{Kind: IdempotencyNew, LeaseID: in.newLeaseID, RunID: in.runID}, nil
	}

	missingRunID := in.storedRunID == nil
	effectiveRunID := in.runID
	if in.storedRunID != nil {
		effectiveRunID = *in.storedRunID
	}
	if missingRunID {
		if _, err := tx.Exec(ctx,
			`UPDATE idempotency_records SET run_id = $3 WHERE agent_slug = $1 AND key = $2`,
			in.agentSlug, in.key, effectiveRunID); err != nil {
			return IdempotencyDecision{}, err
		}
	}

	if in.enforceRunID && effectiveRunID != in.runID {
		return IdempotencyDecision{Kind: IdempotencyConflict}, nil
	}
	if in.status == "completed" {
		if in.response != nil {
			return IdempotencyDecision{
				Kind: IdempotencyCached, ResponseBody: *in.response, RunID: effectiveRunID,
			}, nil
		}
		return IdempotencyDecision{Kind: IdempotencyInProgress}, nil
	}

	// 无法解析 updated_at 时按陈旧处理,避免坏数据永久占住租约。
	processingIsStale := false
	if in.status == "processing" {
		updated, err := parseTimestamp(in.updatedAt)
		if err != nil {
			processingIsStale = true
		} else {
			processingIsStale = updated.Unix() < in.now.Unix()-idempotencyStaleAfterSecs
		}
	}
	if !processingIsStale {
		return IdempotencyDecision{Kind: IdempotencyInProgress}, nil
	}

	var runStatus *string
	var scanned string
	err := tx.QueryRow(ctx,
		`SELECT status FROM agent_runs WHERE agent_slug = $1 AND run_id = $2 FOR UPDATE`,
		in.agentSlug, effectiveRunID).Scan(&scanned)
	switch {
	case err == nil:
		runStatus = &scanned
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return IdempotencyDecision{}, err
	}

	// 已完成的运行不能安全重放:运行表刻意不保存请求体。
	if in.enforceRunID && runStatus != nil && *runStatus == "completed" {
		return IdempotencyDecision{Kind: IdempotencyConflict}, nil
	}

	takeoverRunID := effectiveRunID
	if runStatus != nil {
		if in.enforceRunID {
			tag, err := tx.Exec(ctx,
				`DELETE FROM agent_runs WHERE agent_slug = $1 `+
					`AND run_id = $2 AND status != 'completed'`,
				in.agentSlug, effectiveRunID)
			if err != nil {
				return IdempotencyDecision{}, err
			}
			if tag.RowsAffected() != 1 {
				return IdempotencyDecision{}, errors.New("failed to reset stale explicit run")
			}
		} else {
			if _, err := tx.Exec(ctx,
				`UPDATE agent_runs SET status = 'failed', `+
					`error_code = 'stale_idempotency_lease', `+
					`completed_at = $3 WHERE agent_slug = $1 `+
					`AND run_id = $2 AND status = 'running'`,
				in.agentSlug, effectiveRunID, in.nowText); err != nil {
				return IdempotencyDecision{}, err
			}
			takeoverRunID = in.runID
		}
	}

	tag, err := tx.Exec(ctx,
		`UPDATE idempotency_records SET status = 'processing', `+
			`response_body = NULL, lease_id = $3, run_id = $4, `+
			`updated_at = $5 WHERE agent_slug = $1 AND key = $2`,
		in.agentSlug, in.key, in.newLeaseID, takeoverRunID, in.nowText)
	if err != nil {
		return IdempotencyDecision{}, err
	}
	if tag.RowsAffected() != 1 {
		return IdempotencyDecision{}, errors.New("failed to take over stale idempotency lease")
	}
	return IdempotencyDecision{Kind: IdempotencyNew, LeaseID: in.newLeaseID, RunID: takeoverRunID}, nil
}

// ReleaseIdempotency 释放本请求持有的租约,使同键请求可以重试。
func (s *AgentStore) ReleaseIdempotency(ctx context.Context, agentSlug, key, leaseID string) error {
	ctx, cancel := stateFinalizationContext(ctx)
	defer cancel()
	_, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_records WHERE agent_slug = $1 AND key = $2 `+
			`AND lease_id = $3 AND status = 'processing'`,
		agentSlug, key, leaseID)
	return err
}

// IdempotencyRecordKey 生成幂等记录键。缓存的响应只属于产生它的那个 Agent 修订版。
func IdempotencyRecordKey(agentID, revision int64, idempotencyKey string) string {
	return fmt.Sprintf("%d:%d:%s", agentID, revision, idempotencyKey)
}
