package backend

// Provider 配置的 PostgreSQL 存储（自旧栈 platform_store.go 逐字迁移）。
//
// 存储形态与旧栈完全一致（同一张 platform_settings 表，可混跑）：
//   - `executor_provider:<id>`            整份 JSON 用平台主密钥 AES-GCM 加密
//     （AAD = key），API key / MCP Authorization 不落明文；
//   - `executor_provider_item_revision:<id>` 每项乐观锁版本（CAS 比较基准）；
//   - `executor_provider_revision`        全局版本，任一 Provider 变更即递增，
//     供多副本轻量轮询收敛（见 Manager.RefreshLoop）。
//
// 组保存（gateway/deepseek_harness、codex_local/pi_local 共享运行字段）在
// 单事务里更新全组值与全组 revision，并同事务追加审计——CAS 或审计失败时
// 密文与所有 revision 都保持不变。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ir-agent-server/internal/infra/crypto"
)

// Store 是 Provider 配置仓储。
type Store struct {
	pool   *pgxpool.Pool
	cipher *crypto.SecretCipher
}

// NewStore 组装 Provider 仓储（cipher 与智能体注册表共用平台主密钥）。
func NewStore(pool *pgxpool.Pool, cipher *crypto.SecretCipher) *Store {
	return &Store{pool: pool, cipher: cipher}
}

// GetExecutorProvider 读取智能体中心 Provider 配置；无记录返回 (nil, nil)。
func (s *Store) GetExecutorProvider(ctx context.Context, providerID string) (map[string]any, error) {
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

// PutExecutorProviderGroupIfRevision 在一个事务里保存共享字段的 Provider
// 别名组。primaryID 的 revision 用于乐观并发控制；同组每个别名的 revision
// 都会递增，因而更新任一入口都会让其他入口已经打开的编辑表单失效。
func (s *Store) PutExecutorProviderGroupIfRevision(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
) (int64, error) {
	return s.putExecutorProviderGroupIfRevision(ctx, primaryID, values, expectedRevision, nil)
}

// PutExecutorProviderGroupIfRevisionAudited 原子保存 Provider 别名组并同事务
// 追加脱敏审计事件。CAS 或审计失败时加密值与所有 revision 保持不变。
func (s *Store) PutExecutorProviderGroupIfRevisionAudited(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
	event AuditEvent,
) (int64, error) {
	if err := validateMutationAudit(event, "provider.update", primaryID); err != nil {
		return 0, err
	}
	return s.putExecutorProviderGroupIfRevision(ctx, primaryID, values, expectedRevision, &event)
}

func (s *Store) putExecutorProviderGroupIfRevision(
	ctx context.Context,
	primaryID string,
	values map[string]map[string]any,
	expectedRevision *int64,
	event *AuditEvent,
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
	if err := insertAuditTx(ctx, tx, event); err != nil {
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
func (s *Store) ListExecutorProviderRevisions(ctx context.Context) (map[string]int64, error) {
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
func (s *Store) ExecutorProviderRevision(ctx context.Context) (int64, error) {
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

// validateMutationAudit 强制变更审计的 actor 非空且 action/target 与调用点
// 声明一致（防「挂羊头」审计）。
func validateMutationAudit(event AuditEvent, action, target string) error {
	if strings.TrimSpace(event.Actor) == "" {
		return errors.New("audit actor must not be empty")
	}
	if string(event.Action) != action || event.Target != target {
		return fmt.Errorf("audit event must use action %q and target %q", action, target)
	}
	return nil
}

// insertAuditTx 在既有事务里追加审计行；nil 事件跳过（非 Audited 入口）。
func insertAuditTx(ctx context.Context, tx pgx.Tx, event *AuditEvent) error {
	if event == nil {
		return nil
	}
	if strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(string(event.Action)) == "" ||
		strings.TrimSpace(event.Target) == "" {
		return errors.New("audit actor, action, and target must not be empty")
	}
	details := "{}"
	if len(event.Detail) > 0 {
		details = string(event.Detail)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, details, created_at) `+
			`VALUES ($1, $2, $3, $4, $5)`,
		event.Actor, string(event.Action), event.Target, details, auditNow()); err != nil {
		return fmt.Errorf("insert audit event %s: %w", event.Action, err)
	}
	return nil
}

// auditNow 返回定宽 RFC3339 UTC 时间戳（与 infra/postgres.nowTS 同格式，
// 保证 TEXT 列排序即时间序）。
func auditNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}
