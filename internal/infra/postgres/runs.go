package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/run"
)

// RunStore 实现 run.Repository，落在存量 agent_runs 表（v2 新增
// team_run_id / team_step_id / position_code / attempts / usage_json 列）。
type RunStore struct{ db *DB }

// NewRunStore 构建运行仓储。
func NewRunStore(db *DB) *RunStore { return &RunStore{db: db} }

var _ run.Repository = (*RunStore)(nil)

const runColumns = `agent_slug, run_id, agent_revision, executor_kind, execution_mode,
	position_code, status, attempts, team_run_id, team_step_id, request_json, result_json,
	executor_result_json, usage_json, duration_ms, error_code, response_code, created_at, completed_at`

// runColumnsLean 是列表查询的瘦列集：不含 request/result/executor 三个
// 大文本列（单列可达 MiB 级）。千级 Agent 的运行表上，列表页每行拖
// MiB 级 blob 是首要读放大；全量载荷走 Get（详情端点按 runID 精确取）。
// Input/Output/ExecutorOutput 在列表结果中保持 nil（API 层不出现这三个键）。
const runColumnsLean = `agent_slug, run_id, agent_revision, executor_kind, execution_mode,
	position_code, status, attempts, team_run_id, team_step_id, usage_json,
	duration_ms, error_code, response_code, created_at, completed_at`

// scanRunLean 是 scanRun 的瘦列变体（大文本三列不扫，字段保持 nil）。
func scanRunLean(row pgx.Row) (*run.Run, error) {
	var (
		r           run.Run
		kind        *string
		mode        *string
		posCode     *string
		status      string
		attempts    int
		teamRunID   *string
		teamStepID  *string
		usageJSON   *string
		errorCode   *string
		createdAt   string
		completedAt *string
	)
	err := row.Scan(&r.AgentSlug, &r.RunID, &r.AgentRevision, &kind, &mode, &posCode,
		&status, &attempts, &teamRunID, &teamStepID, &usageJSON, &r.DurationMS,
		&errorCode, &r.ResponseCode, &createdAt, &completedAt)
	if err != nil {
		return nil, err
	}
	r.Status = run.Status(status)
	r.Attempt = attempts
	applyRunNulls(&r, kind, mode, posCode, teamRunID, teamStepID, usageJSON,
		errorCode, createdAt, completedAt)
	return &r, nil
}

func scanRun(row pgx.Row) (*run.Run, error) {
	var (
		r            run.Run
		kind         *string
		mode         *string
		posCode      *string
		status       string
		attempts     int
		teamRunID    *string
		teamStepID   *string
		requestJSON  *string
		resultJSON   *string
		executorJSON *string
		usageJSON    *string
		errorCode    *string
		createdAt    string
		completedAt  *string
	)
	err := row.Scan(&r.AgentSlug, &r.RunID, &r.AgentRevision, &kind, &mode, &posCode,
		&status, &attempts, &teamRunID, &teamStepID, &requestJSON, &resultJSON,
		&executorJSON, &usageJSON, &r.DurationMS, &errorCode, &r.ResponseCode,
		&createdAt, &completedAt)
	if err != nil {
		return nil, err
	}
	r.Status = run.Status(status)
	r.Attempt = attempts
	for _, pair := range []struct {
		src *string
		dst *string
	}{
		{kind, &r.ExecutorKind}, {mode, &r.ExecutionMode}, {posCode, &r.PositionCode},
		{teamRunID, &r.TeamRunID}, {teamStepID, &r.TeamStepID}, {errorCode, &r.ErrorCode},
	} {
		if pair.src != nil {
			*pair.dst = *pair.src
		}
	}
	for _, pair := range []struct {
		src *string
		dst *json.RawMessage
	}{
		{requestJSON, &r.Input}, {resultJSON, &r.Output}, {executorJSON, &r.ExecutorOutput},
	} {
		if pair.src != nil {
			*pair.dst = json.RawMessage(*pair.src)
		}
	}
	if usageJSON != nil && *usageJSON != "" {
		var usage exec.Usage
		if err := json.Unmarshal([]byte(*usageJSON), &usage); err == nil {
			r.Usage = &usage
		}
	}
	r.CreatedAt = parseTS(createdAt)
	if completedAt != nil {
		parsed := parseTS(*completedAt)
		r.CompletedAt = &parsed
	}
	return &r, nil
}

// applyRunNulls 把瘦列集的可空列落到实体（lean 扫描共享；usage 解析同源）。
func applyRunNulls(r *run.Run, kind, mode, posCode, teamRunID, teamStepID,
	usageJSON, errorCode *string, createdAt string, completedAt *string,
) {
	for _, pair := range []struct {
		src *string
		dst *string
	}{
		{kind, &r.ExecutorKind}, {mode, &r.ExecutionMode}, {posCode, &r.PositionCode},
		{teamRunID, &r.TeamRunID}, {teamStepID, &r.TeamStepID}, {errorCode, &r.ErrorCode},
	} {
		if pair.src != nil {
			*pair.dst = *pair.src
		}
	}
	if usageJSON != nil && *usageJSON != "" {
		var usage exec.Usage
		if err := json.Unmarshal([]byte(*usageJSON), &usage); err == nil {
			r.Usage = &usage
		}
	}
	r.CreatedAt = parseTS(createdAt)
	if completedAt != nil {
		parsed := parseTS(*completedAt)
		r.CompletedAt = &parsed
	}
}

// Append 幂等插入运行记录：同 (agent_slug, run_id) 已存在时静默忽略。
func (s *RunStore) Append(ctx context.Context, r run.Run) error {
	status := string(r.Status)
	if status == "" {
		status = string(run.StatusQueued)
	}
	var usageJSON *string
	if r.Usage != nil {
		encoded, err := json.Marshal(r.Usage)
		if err != nil {
			return fmt.Errorf("encode usage: %w", err)
		}
		text := string(encoded)
		usageJSON = &text
	}
	attempt := r.Attempt
	if attempt == 0 {
		attempt = 1
	}
	createdAt := formatTS(r.CreatedAt)
	if r.CreatedAt.IsZero() {
		createdAt = nowTS()
	}
	var completedAt *string
	if r.CompletedAt != nil {
		text := formatTS(*r.CompletedAt)
		completedAt = &text
	}
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO agent_runs (agent_slug, run_id, agent_revision, executor_kind, execution_mode,
			position_code, status, attempts, team_run_id, team_step_id, request_json, result_json,
			executor_result_json, usage_json, duration_ms, error_code, response_code, created_at, completed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		 ON CONFLICT (agent_slug, run_id) DO NOTHING`,
		r.AgentSlug, r.RunID, r.AgentRevision, nilIfEmpty(r.ExecutorKind), nilIfEmpty(r.ExecutionMode),
		nilIfEmpty(r.PositionCode), status, attempt, nilIfEmpty(r.TeamRunID), nilIfEmpty(r.TeamStepID),
		rawOrNil(r.Input), rawOrNil(r.Output), rawOrNil(r.ExecutorOutput), usageJSON,
		r.DurationMS, nilIfEmpty(r.ErrorCode), r.ResponseCode, createdAt, completedAt)
	if err != nil {
		return fmt.Errorf("append run: %w", err)
	}
	return nil
}

// Get 按 run_id 取运行（run_id 全局唯一；防御性取最新一条）。
func (s *RunStore) Get(ctx context.Context, runID string) (*run.Run, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM agent_runs WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1`, runID)
	r, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, run.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Complete 把运行落为终态。已终态时不覆盖，返回当前记录（幂等重放语义：
// 租约过期重派后原执行者迟到的 complete 只归档不生效由调用方比对状态判断）。
func (s *RunStore) Complete(ctx context.Context, runID string, outcome run.Outcome) (*run.Run, error) {
	if !outcome.Status.Terminal() {
		return nil, fmt.Errorf("complete requires terminal status, got %q", outcome.Status)
	}
	var usageJSON *string
	if outcome.Usage != nil {
		normalized := outcome.Usage.Normalize()
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("encode usage: %w", err)
		}
		text := string(encoded)
		usageJSON = &text
	}
	completedAt := nowTS()
	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE agent_runs SET status = $2,
			result_json = COALESCE($3, result_json),
			usage_json = COALESCE($4, usage_json),
			duration_ms = COALESCE($5, duration_ms),
			error_code = COALESCE($6, error_code),
			completed_at = $7
		 WHERE run_id = $1 AND status NOT IN ('completed','failed','canceled')`,
		runID, string(outcome.Status), rawOrNil(outcome.Output), usageJSON,
		outcome.Duration, nilIfEmpty(outcome.ErrorCode), completedAt); err != nil {
		return nil, fmt.Errorf("complete run: %w", err)
	}
	current, err := s.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	return current, nil
}

// List 按条件分页查询运行（新→旧）。
func (s *RunStore) List(ctx context.Context, f run.Filter) ([]run.Run, int, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	conditions := []string{}
	args := []any{}
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, len(args)))
	}
	if f.AgentSlug != "" {
		add(`agent_slug = $%d`, f.AgentSlug)
	}
	if f.Position != "" {
		add(`position_code = $%d`, f.Position)
	}
	if f.TeamRunID != "" {
		add(`team_run_id = $%d`, f.TeamRunID)
	}
	if f.SingleRunsOnly {
		conditions = append(conditions, `team_run_id IS NULL`)
	}
	if len(f.Statuses) > 0 {
		values := make([]string, 0, len(f.Statuses))
		for _, status := range f.Statuses {
			values = append(values, string(status))
		}
		add(`status = ANY($%d)`, values)
	}
	if !f.Since.IsZero() {
		add(`created_at >= $%d`, formatTS(f.Since))
	}
	if !f.Until.IsZero() {
		add(`created_at <= $%d`, formatTS(f.Until))
	}
	if !f.Before.IsZero() {
		add(`created_at < $%d`, formatTS(f.Before))
	}
	if f.ErrorCode != "" {
		add(`error_code = $%d`, f.ErrorCode)
	}
	where := ``
	if len(conditions) > 0 {
		where = ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	var total int
	if err := s.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count runs: %w", err)
	}
	args = append(args, limit, f.Offset)
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+runColumnsLean+` FROM agent_runs`+where+
			fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	out := make([]run.Run, 0, limit)
	for rows.Next() {
		r, err := scanRunLean(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

func nilIfEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func rawOrNil(raw json.RawMessage) *string {
	if len(raw) == 0 {
		return nil
	}
	text := string(raw)
	return &text
}

// RunTotals 聚合统计指定时刻之后的运行（仪表盘）。usage_json 由本栈写入，
// 键为 inputTokens/outputTokens/totalTokens；非法/缺失值按 0 计。
func (s *RunStore) RunTotals(ctx context.Context, since time.Time) (run.Totals, error) {
	var totals run.Totals
	err := s.db.Pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE status = 'completed'),
		  COUNT(*) FILTER (WHERE status = 'failed'),
		  COUNT(*) FILTER (WHERE status = 'canceled'),
		  COUNT(*) FILTER (WHERE status IN ('running','leased')),
		  COUNT(*) FILTER (WHERE status = 'queued'),
		  COALESCE(SUM((NULLIF(usage_json,'')::jsonb->>'inputTokens')::bigint), 0),
		  COALESCE(SUM((NULLIF(usage_json,'')::jsonb->>'outputTokens')::bigint), 0),
		  COALESCE(SUM((NULLIF(usage_json,'')::jsonb->>'totalTokens')::bigint), 0)
		FROM agent_runs WHERE created_at >= $1`, formatTS(since)).
		Scan(&totals.Completed, &totals.Failed, &totals.Canceled,
			&totals.InProgress, &totals.Queued,
			&totals.InputTokens, &totals.OutputTokens, &totals.TotalTokens)
	if err != nil {
		return totals, fmt.Errorf("run totals: %w", err)
	}
	return totals, nil
}
