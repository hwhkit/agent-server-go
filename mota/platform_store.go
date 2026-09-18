package mota

// PostgreSQL 存储层:管理台数据(Agent / 运行记录 / 通道事件 / 会话 / 幂等)。
// 对应 Rust 侧 src/platform/store.rs。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// idempotencyStaleAfterSecs 是 processing 记录被判定为陈旧、可被接管的阈值。
const idempotencyStaleAfterSecs = 2 * 60 * 60

// periodicGCIntervalSecs 是 nonce / 会话过期清理的最小间隔。
const periodicGCIntervalSecs = 60

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
	row := s.pool.QueryRow(ctx,
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
		return nil, err
	}
	return decodeAgent(stored, s.cipher)
}

// UpdateAgent 整体更新一个 Agent 并把 revision +1。input.SigningSecret 为 nil 时
// 保留原密钥。Agent 不存在时返回 (nil, nil)。
func (s *AgentStore) UpdateAgent(ctx context.Context, slug string, input *AgentInput) (*AgentConfig, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentSecret string
	var revision int64
	err = tx.QueryRow(ctx, "SELECT signing_secret_enc, revision FROM agents WHERE slug = $1", slug).
		Scan(&currentSecret, &revision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
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
	_, err = tx.Exec(ctx,
		`UPDATE agents SET name = $2, description = $3, enabled = $4, `+
			`executor_kind = $5, execution_mode = $6, signing_secret_enc = $7, `+
			`signature_encoding = $8, system_prompt = $9, workspace_path = $10, `+
			`claude_model = $11, allowed_tools_json = $12, permission_mode = $13, `+
			`timeout_secs = $14, max_concurrency = $15, queue_capacity = $16, `+
			`revision = $17, updated_at = $18 WHERE slug = $1`,
		slug, input.Name, input.Description, input.EnabledOrDefault(),
		string(input.ExecutorKind), string(input.ExecutionMode), encrypted,
		input.SignatureEncoding, input.SystemPrompt, input.WorkspacePath,
		input.ClaudeModel, string(tools), input.PermissionMode,
		int64(input.TimeoutSecsOrDefault()), int64(input.MaxConcurrencyOrDefault()),
		int64(input.QueueCapacityOrDefault()), revision+1, nowTimestamp(),
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetAgent(ctx, slug)
}

// DeleteAgent 删除一个 Agent;返回是否确实删掉了一行。
func (s *AgentStore) DeleteAgent(ctx context.Context, slug string) (bool, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM agents WHERE slug = $1", slug)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
	err := s.pool.QueryRow(ctx,
		`SELECT username, csrf_token, expires_at FROM admin_sessions `+
			`WHERE token_hash = $1 AND expires_at > $2`, tokenHash, now).
		Scan(&record.Username, &record.CSRFToken, &record.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
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
	key := "executor_provider:" + providerID
	plaintext, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encrypted, err := s.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1, $2) `+
			`ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, encrypted)
	return err
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

// ListRuns 按时间倒序列出运行记录。slug 非空时只列该 Agent。limit 收敛到 1..200。
func (s *AgentStore) ListRuns(ctx context.Context, limit int, slug string) ([]RunView, error) {
	limit = clampInt(limit, 1, 200)
	const selectRuns = `SELECT run_id, agent_slug, agent_revision, executor_kind, execution_mode, ` +
		`status, duration_ms, error_code, response_code, request_json, result_json, ` +
		`executor_result_json, created_at, completed_at FROM agent_runs`

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

// ListChannelEvents 查询通道事件,按 eventType / slug 过滤,时间倒序。
func (s *AgentStore) ListChannelEvents(ctx context.Context, eventType, slug string, limit int) ([]ChannelEventView, error) {
	limit = clampInt(limit, 1, 200)
	const selectEvents = `SELECT event_type, agent_slug, run_id, url, status_code, accepted, ` +
		`run_status, pass, latency_ms, detail, request_json, result_json, created_at FROM channel_events`

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
			`FROM idempotency_records WHERE agent_slug = $1 AND key = $2`, agentSlug, key).
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
		`SELECT status FROM agent_runs WHERE agent_slug = $1 AND run_id = $2`,
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
