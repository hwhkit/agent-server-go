package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/infra/crypto"
)

// AgentStore 实现 agent.Repository。签名密钥以 AES-256-GCM 加密存储
// （AAD = slug，密文格式与旧栈逐字节兼容，存量行可直接解密）。
type AgentStore struct {
	db     *DB
	cipher *crypto.SecretCipher
}

// NewAgentStore 构建智能体仓储。
func NewAgentStore(db *DB, cipher *crypto.SecretCipher) *AgentStore {
	return &AgentStore{db: db, cipher: cipher}
}

var _ agent.Repository = (*AgentStore)(nil)

const agentColumns = `id, slug, name, description, enabled, executor_kind, execution_mode,
	signing_secret_enc, signature_encoding, system_prompt, workspace_path, claude_model,
	allowed_tools_json, permission_mode, timeout_secs, max_concurrency, queue_capacity,
	revision, created_at, updated_at`

// agentSelectBody 是列清单 + 聘任岗位子查询（不含 FROM，供 Search 追加
// COUNT(*) OVER() 总数列）。
const agentSelectBody = `SELECT ` + agentColumns + `,
	COALESCE((SELECT array_agg(position_code ORDER BY position_code)
		FROM agent_positions ap WHERE ap.agent_slug = a.slug), '{}')`

const agentSelect = agentSelectBody + `
	FROM agents a`

// scanner 同时满足 pgx.Row 与 pgx.Rows 的扫描面。
type scanner interface{ Scan(dest ...any) error }

// scanAgentInto 扫描一行智能体。decrypt=true（执行路径，GetBySlug）解密
// 签名密钥；false（管理列表路径）不解密，仅置 SecretConfigured——单个
// 坏密文只影响自身标记，不再阻塞整页浏览。count 非 nil 时额外扫描一行
// 窗口总数列（Search 的 COUNT(*) OVER()）。
func (s *AgentStore) scanAgentInto(row scanner, decrypt bool, count *int64) (*agent.Agent, error) {
	var (
		a         agent.Agent
		secretEnc string
		kind      string
		mode      string
		model     *string
		toolsJSON string
		positions []string
		createdAt string
		updatedAt string
		timeout   int64
		conc      int64
		queue     int64
	)
	err := row.Scan(&a.ID, &a.Slug, &a.Name, &a.Description, &a.Enabled, &kind, &mode,
		&secretEnc, &a.SignatureEncoding, &a.SystemPrompt, &a.WorkspacePath, &model,
		&toolsJSON, &a.PermissionMode, &timeout, &conc, &queue,
		&a.Revision, &createdAt, &updatedAt, &positions)
	if err != nil {
		return nil, err
	}
	a.Binding = exec.Binding{Kind: exec.Kind(kind), Mode: exec.Mode(mode)}
	a.Model = model
	if toolsJSON != "" {
		var tools []string
		if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
			return nil, fmt.Errorf("decode allowed_tools: %w", err)
		}
		a.AllowedTools = tools
	}
	a.Positions = positions
	a.TimeoutSecs = uint64(timeout)
	a.MaxConcurrency = int(conc)
	a.QueueCapacity = int(queue)
	a.CreatedAt = parseTS(createdAt)
	a.UpdatedAt = parseTS(updatedAt)
	a.SecretConfigured = secretEnc != ""
	if !decrypt {
		return &a, nil
	}
	secret, err := s.cipher.Decrypt(secretEnc, a.Slug)
	if err != nil {
		// 密文损坏或主密钥不匹配：不回显任何密文细节。
		return nil, fmt.Errorf("decrypt signing secret for agent %s", a.Slug)
	}
	a.SigningSecret = []byte(secret)
	return &a, nil
}

// GetBySlug 按 slug 取智能体（含聘任岗位与解密后的签名密钥）。
func (s *AgentStore) GetBySlug(ctx context.Context, slug string) (*agent.Agent, error) {
	row := s.db.Pool.QueryRow(ctx, agentSelect+` WHERE a.slug = $1`, slug)
	a, err := s.scanAgentInto(row, true, nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, agent.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// List 返回智能体，按 slug 排序（元数据扫描，不解密签名密钥）。
func (s *AgentStore) List(ctx context.Context, includeDisabled bool) ([]agent.Agent, error) {
	query := agentSelect
	if !includeDisabled {
		query += ` WHERE a.enabled`
	}
	query += ` ORDER BY a.slug`
	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	out := make([]agent.Agent, 0, 8)
	for rows.Next() {
		a, err := s.scanAgentInto(rows, false, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Search 是千级智能体规模的分页/搜索列表：ILIKE 模糊匹配 slug/name，
// 窗口 COUNT 返回总数；元数据扫描不解密（坏密文行只影响自身标记）。
func (s *AgentStore) Search(ctx context.Context, q agent.ListQuery) ([]agent.Agent, int64, error) {
	where := ` WHERE TRUE`
	args := []any{}
	if !q.IncludeDisabled {
		where += ` AND a.enabled`
	}
	if needle := strings.TrimSpace(q.Query); needle != "" {
		args = append(args, "%"+needle+"%")
		where += fmt.Sprintf(` AND (a.slug ILIKE $%d OR a.name ILIKE $%d)`, len(args), len(args))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	query := agentSelectBody + `, COUNT(*) OVER() AS total` + where +
		fmt.Sprintf(` ORDER BY a.slug LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search agents: %w", err)
	}
	defer rows.Close()
	var total int64
	out := make([]agent.Agent, 0, 8)
	for rows.Next() {
		a, err := s.scanAgentInto(rows, false, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// Upsert 创建或更新智能体。语义与旧栈一致：
//   - 事务内 FOR UPDATE + revision CAS（If-Match）；
//   - a.SigningSecret 为空 = 保留既有密文（更新场景）；创建时必须提供；
//   - 聘任集合整组替换；
//   - 每次写操作递增全局注册表版本（多实例收敛）。
func (s *AgentStore) Upsert(ctx context.Context, a *agent.Agent, ifMatchRevision *int64) (int64, error) {
	tools, err := json.Marshal(orEmpty(a.AllowedTools))
	if err != nil {
		return 0, fmt.Errorf("encode allowed_tools: %w", err)
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentSecret string
	var currentRevision int64
	err = tx.QueryRow(ctx,
		`SELECT signing_secret_enc, revision FROM agents WHERE slug = $1 FOR UPDATE`, a.Slug).
		Scan(&currentSecret, &currentRevision)
	creating := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !creating {
		return 0, fmt.Errorf("lock agent: %w", err)
	}
	if creating {
		if ifMatchRevision != nil {
			return 0, agent.ErrNotFound
		}
		if err := a.Validate(true); err != nil {
			return 0, err
		}
	} else {
		if ifMatchRevision != nil && *ifMatchRevision != currentRevision {
			return 0, agent.ErrRevisionConflict
		}
		if err := a.Validate(false); err != nil {
			return 0, err
		}
	}

	secretEnc := currentSecret
	if len(a.SigningSecret) > 0 {
		if secretEnc, err = s.cipher.Encrypt(string(a.SigningSecret), a.Slug); err != nil {
			return 0, fmt.Errorf("encrypt signing secret: %w", err)
		}
	}

	now := nowTS()
	var newRevision int64
	if creating {
		err = tx.QueryRow(ctx,
			`INSERT INTO agents (slug, name, description, enabled, executor_kind, execution_mode,
				signing_secret_enc, signature_encoding, system_prompt, workspace_path, claude_model,
				allowed_tools_json, permission_mode, timeout_secs, max_concurrency, queue_capacity,
				revision, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,1,$17,$17)
			 RETURNING revision, id`,
			a.Slug, a.Name, a.Description, a.Enabled, string(a.Binding.Kind), string(a.Binding.Mode),
			secretEnc, a.SignatureEncoding, a.SystemPrompt, a.WorkspacePath, a.Model,
			string(tools), a.PermissionMode, int64(a.TimeoutSecs), int64(a.MaxConcurrency),
			int64(a.QueueCapacity), now).Scan(&newRevision, &a.ID)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE agents SET name=$2, description=$3, enabled=$4, executor_kind=$5, execution_mode=$6,
				signing_secret_enc=$7, signature_encoding=$8, system_prompt=$9, workspace_path=$10,
				claude_model=$11, allowed_tools_json=$12, permission_mode=$13, timeout_secs=$14,
				max_concurrency=$15, queue_capacity=$16, revision=$17, updated_at=$18
			 WHERE slug=$1 RETURNING revision`,
			a.Slug, a.Name, a.Description, a.Enabled, string(a.Binding.Kind), string(a.Binding.Mode),
			secretEnc, a.SignatureEncoding, a.SystemPrompt, a.WorkspacePath, a.Model,
			string(tools), a.PermissionMode, int64(a.TimeoutSecs), int64(a.MaxConcurrency),
			int64(a.QueueCapacity), currentRevision+1, now).Scan(&newRevision)
	}
	if err != nil {
		return 0, fmt.Errorf("write agent: %w", err)
	}
	if err := replaceStaffing(ctx, tx, a.Slug, a.Positions); err != nil {
		return 0, err
	}
	if err := bumpRegistryRevision(ctx, tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	a.Revision = newRevision
	a.SigningSecret = nil // 出参不回带明文密钥
	return newRevision, nil
}

// Delete 删除智能体及其聘任关系，并递增注册表版本。
func (s *AgentStore) Delete(ctx context.Context, slug string) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM agents WHERE slug = $1`, slug)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return agent.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_positions WHERE agent_slug = $1`, slug); err != nil {
		return fmt.Errorf("delete staffing: %w", err)
	}
	if err := bumpRegistryRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RegistryVersion 返回全局注册表版本（多实例收敛轮询）。
func (s *AgentStore) RegistryVersion(ctx context.Context) (int64, error) {
	var text string
	err := s.db.Pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key = 'agent_registry_revision'`).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read registry revision: %w", err)
	}
	version, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse registry revision: %w", err)
	}
	return version, nil
}

// replaceStaffing 整组替换聘任关系（事务内）。
func replaceStaffing(ctx context.Context, tx pgx.Tx, slug string, positions []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM agent_positions WHERE agent_slug = $1`, slug); err != nil {
		return fmt.Errorf("clear staffing: %w", err)
	}
	for _, code := range positions {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_positions (agent_slug, position_code) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			slug, code); err != nil {
			return fmt.Errorf("insert staffing %s: %w", code, err)
		}
	}
	return nil
}

// bumpRegistryRevision 递增全局注册表版本（与旧栈共用同一 key，切换期收敛一致）。
func bumpRegistryRevision(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ('agent_registry_revision', '2')
		 ON CONFLICT (key) DO UPDATE SET value = (platform_settings.value::bigint + 1)::text`)
	return err
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
