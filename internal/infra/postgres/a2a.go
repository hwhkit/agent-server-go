package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A2ATaskStore 是 A2A 任务存储（复用 agent_runs 表，不新建任务表）：
//
//   - Task.ID = agent_runs.run_id，主键 (agent_slug, run_id) 天然保证同一
//     智能体下 task id 唯一；
//   - 完成态的 A2A Task 快照写进 result_json，读取时直接复原；
//   - contextId 在任务执行中也要能读到，因此随请求信封写进 request_json；
//   - A2A 运行自动出现在管理台运行记录里，并沿用 agent_revision 快照。
//
// 方法签名与 app/a2a 的 TaskStore 端口结构性一致（原始参数形态，
// 避免 infra 反向依赖 app 类型）。
type A2ATaskStore struct{ db *DB }

// NewA2ATaskStore 构建 A2A 任务存储。
func NewA2ATaskStore(db *DB) *A2ATaskStore { return &A2ATaskStore{db: db} }

// NewA2ATaskStoreFromPool 在既有连接池上构建（迁移期与旧栈共库共池）。
func NewA2ATaskStoreFromPool(pool *pgxpool.Pool) *A2ATaskStore {
	return &A2ATaskStore{db: &DB{Pool: pool}}
}

// Start 登记新任务，初始状态 running（对外映射成 TASK_STATE_WORKING）。
// created=false 表示 task id 已存在（A2A task id 由服务端生成，冲突说明
// 重放或碰撞）。
func (s *A2ATaskStore) Start(ctx context.Context, agentSlug, taskID string,
	agentRevision int64, executorKind, executionMode string, requestJSON []byte) (bool, error) {
	var requestText *string
	if requestJSON != nil {
		text := string(requestJSON)
		requestText = &text
	}
	tag, err := s.db.Pool.Exec(ctx,
		`INSERT INTO agent_runs
		 (agent_slug, run_id, agent_revision, executor_kind, execution_mode, status, request_json, created_at)
		 VALUES ($1, $2, $3, $4, $5, 'running', $6, $7) ON CONFLICT DO NOTHING`,
		agentSlug, taskID, agentRevision, executorKind, executionMode, requestText, nowTS())
	if err != nil {
		return false, fmt.Errorf("start a2a task: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Finish 写入终态与完整 Task 快照。只允许推进 running 状态的记录
// （过期请求不能改写已被接管/落定的运行）。A2A 的错误在 JSON-RPC 信封里，
// HTTP 层始终 200，response_code 固定记录 200。
func (s *A2ATaskStore) Finish(ctx context.Context, agentSlug, taskID, status string,
	durationMs int64, errorCode *string, resultJSON []byte, executorOutput *string) error {
	ctx, cancel := finalizationContext(ctx)
	defer cancel()

	var resultText *string
	if resultJSON != nil {
		text := string(resultJSON)
		resultText = &text
	}
	var responseCode int64 = 200
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_runs SET status = $3, duration_ms = $4, error_code = $5,
		 response_code = $6, result_json = $7, executor_result_json = $8, completed_at = $9
		 WHERE agent_slug = $1 AND run_id = $2 AND status = 'running'`,
		agentSlug, taskID, status, durationMs, errorCode, responseCode,
		resultText, executorOutput, nowTS())
	if err != nil {
		return fmt.Errorf("finish a2a task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("run is no longer in the running state")
	}
	return nil
}

// Get 读取任务记录。payload 语义：执行中是请求信封，终态是 Task 快照
// （result_json 优先，回退 request_json）。未找到返回 (nil, false, nil)。
func (s *A2ATaskStore) Get(ctx context.Context, agentSlug, taskID string) (status string, payload []byte, found bool, err error) {
	var resultJSON, requestJSON *string
	err = s.db.Pool.QueryRow(ctx,
		`SELECT status, result_json, request_json FROM agent_runs
		 WHERE agent_slug = $1 AND run_id = $2`, agentSlug, taskID).
		Scan(&status, &resultJSON, &requestJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("get a2a task: %w", err)
	}
	if resultJSON != nil {
		payload = []byte(*resultJSON)
	} else if requestJSON != nil {
		payload = []byte(*requestJSON)
	}
	return status, payload, true, nil
}
