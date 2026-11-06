package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"ir-agent-server/internal/domain/team"
)

// TeamStore 实现 team.Repository。
type TeamStore struct{ db *DB }

// NewTeamStore 构建团队仓储。
func NewTeamStore(db *DB) *TeamStore { return &TeamStore{db: db} }

var _ team.Repository = (*TeamStore)(nil)

func scanTeam(row pgx.Row) (*team.Team, error) {
	var (
		t         team.Team
		stepsJSON string
		createdAt string
		updatedAt string
	)
	err := row.Scan(&t.ID, &t.Name, &t.Description, &stepsJSON, &t.MaxConcurrentRuns,
		&t.Enabled, &t.Revision, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &t.Steps); err != nil {
		return nil, fmt.Errorf("decode team steps: %w", err)
	}
	t.CreatedAt = parseTS(createdAt)
	t.UpdatedAt = parseTS(updatedAt)
	return &t, nil
}

const teamColumns = `id, name, description, steps_json, max_concurrent_runs, enabled, revision, created_at, updated_at`

// Get 按 ID 取团队。
func (s *TeamStore) Get(ctx context.Context, id string) (*team.Team, error) {
	row := s.db.Pool.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams WHERE id = $1`, id)
	t, err := scanTeam(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, team.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// List 返回团队，按 ID 排序。
func (s *TeamStore) List(ctx context.Context, includeDisabled bool) ([]team.Team, error) {
	query := `SELECT ` + teamColumns + ` FROM teams`
	if !includeDisabled {
		query += ` WHERE enabled`
	}
	query += ` ORDER BY id`
	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	defer rows.Close()
	out := make([]team.Team, 0, 4)
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Upsert 创建或更新团队（revision CAS）。定义先过 Validate（含 DAG 无环），
// 非法定义不落库。
func (s *TeamStore) Upsert(ctx context.Context, t *team.Team, ifMatchRevision *int64) (int64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	stepsJSON, err := json.Marshal(t.Steps)
	if err != nil {
		return 0, fmt.Errorf("encode team steps: %w", err)
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM teams WHERE id = $1 FOR UPDATE`, t.ID).Scan(&current)
	creating := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !creating {
		return 0, fmt.Errorf("lock team: %w", err)
	}
	if creating && ifMatchRevision != nil {
		return 0, team.ErrNotFound
	}
	if !creating && ifMatchRevision != nil && *ifMatchRevision != current {
		return 0, team.ErrRevisionConflict
	}

	now := nowTS()
	var newRevision int64
	if creating {
		err = tx.QueryRow(ctx,
			`INSERT INTO teams (id, name, description, steps_json, max_concurrent_runs, enabled, revision, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,1,$7,$7) RETURNING revision`,
			t.ID, t.Name, t.Description, string(stepsJSON), t.MaxConcurrentRuns, t.Enabled, now).Scan(&newRevision)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE teams SET name=$2, description=$3, steps_json=$4, max_concurrent_runs=$5,
			 enabled=$6, revision=$7, updated_at=$8 WHERE id=$1 RETURNING revision`,
			t.ID, t.Name, t.Description, string(stepsJSON), t.MaxConcurrentRuns,
			t.Enabled, current+1, now).Scan(&newRevision)
	}
	if err != nil {
		return 0, fmt.Errorf("write team: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	t.Revision = newRevision
	return newRevision, nil
}

// Delete 删除团队定义。在途 TeamRun 持有定义快照，不受删除影响，可继续推进到终态。
func (s *TeamStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return team.ErrNotFound
	}
	return nil
}

// TeamRunStore 实现 team.TeamRunRepository。
type TeamRunStore struct{ db *DB }

// NewTeamRunStore 构建团队运行仓储。
func NewTeamRunStore(db *DB) *TeamRunStore { return &TeamRunStore{db: db} }

var _ team.TeamRunRepository = (*TeamRunStore)(nil)

const teamRunColumns = `id, team_id, team_revision, snapshot_json, idempotency_key, status,
	steps_json, input_json, error_json, created_at, completed_at`

func scanTeamRun(row pgx.Row) (*team.TeamRun, error) {
	var (
		tr          team.TeamRun
		snapshot    string
		status      string
		stepsJSON   string
		inputJSON   *string
		errorJSON   *string
		createdAt   string
		completedAt *string
	)
	err := row.Scan(&tr.ID, &tr.TeamID, &tr.TeamRevision, &snapshot, &tr.IdempotencyKey,
		&status, &stepsJSON, &inputJSON, &errorJSON, &createdAt, &completedAt)
	if err != nil {
		return nil, err
	}
	tr.Status = team.TeamRunStatus(status)
	if err := json.Unmarshal([]byte(snapshot), &tr.Snapshot); err != nil {
		return nil, fmt.Errorf("decode team snapshot: %w", err)
	}
	tr.Steps = map[string]team.StepRun{}
	if stepsJSON != "" {
		if err := json.Unmarshal([]byte(stepsJSON), &tr.Steps); err != nil {
			return nil, fmt.Errorf("decode team run steps: %w", err)
		}
	}
	if inputJSON != nil {
		tr.Input = json.RawMessage(*inputJSON)
	}
	if errorJSON != nil {
		var stepErr team.StepError
		if err := json.Unmarshal([]byte(*errorJSON), &stepErr); err == nil {
			tr.Error = &stepErr
		}
	}
	tr.CreatedAt = parseTS(createdAt)
	if completedAt != nil {
		parsed := parseTS(*completedAt)
		tr.CompletedAt = &parsed
	}
	return &tr, nil
}

// Create 幂等创建团队运行：同 idempotency_key 已存在时 created=false 且不覆盖。
func (s *TeamRunStore) Create(ctx context.Context, tr *team.TeamRun) (bool, error) {
	snapshot, err := json.Marshal(tr.Snapshot)
	if err != nil {
		return false, fmt.Errorf("encode team snapshot: %w", err)
	}
	stepsJSON, err := json.Marshal(tr.Steps)
	if err != nil {
		return false, fmt.Errorf("encode team run steps: %w", err)
	}
	var inputJSON *string
	if len(tr.Input) > 0 {
		text := string(tr.Input)
		inputJSON = &text
	}
	tag, err := s.db.Pool.Exec(ctx,
		`INSERT INTO team_runs (id, team_id, team_revision, snapshot_json, idempotency_key,
			status, steps_json, input_json, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		tr.ID, tr.TeamID, tr.TeamRevision, string(snapshot), tr.IdempotencyKey,
		string(tr.Status), string(stepsJSON), inputJSON, nowTS())
	if err != nil {
		return false, fmt.Errorf("create team run: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Get 按 ID 取团队运行。
func (s *TeamRunStore) Get(ctx context.Context, id string) (*team.TeamRun, error) {
	row := s.db.Pool.QueryRow(ctx, `SELECT `+teamRunColumns+` FROM team_runs WHERE id = $1`, id)
	tr, err := scanTeamRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, team.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// GetByIdempotencyKey 按幂等键取团队运行（提交重放路径）。
func (s *TeamRunStore) GetByIdempotencyKey(ctx context.Context, key string) (*team.TeamRun, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+teamRunColumns+` FROM team_runs WHERE idempotency_key = $1`, key)
	tr, err := scanTeamRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, team.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// ListActive 返回未落定的团队运行（引擎恢复扫描）。
func (s *TeamRunStore) ListActive(ctx context.Context, limit int) ([]team.TeamRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+teamRunColumns+` FROM team_runs
		 WHERE status IN ('queued','running') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list active team runs: %w", err)
	}
	defer rows.Close()
	out := make([]team.TeamRun, 0, limit)
	for rows.Next() {
		tr, err := scanTeamRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *tr)
	}
	return out, rows.Err()
}

// SaveSteps 以状态前置条件（CAS）推进步骤状态：行锁下校验 cas 中每个步骤的
// 当前状态与期望一致，任何一项不符即整体放弃（返回 false），防止双实例
// 重复推进同一步骤。已落定的团队运行拒绝再写。
// SaveSteps 是团队运行推进的唯一写路径（v0.2 迭代 A10 评估结论：保持
// 现状）。单行 FOR UPDATE + 全量 steps JSON 重编码，百步 DAG 每步完成
// 约 100KB 级重编码、全程 ~10MB 事务量——Postgres 无压力；并发推进由
// 行锁串行化 + CAS 失败触发引擎重折（正确性自洽）。真正的规模约束是
// steps_json 内联的步骤输出体积（≤4MiB/步），由 OutputRef 上限治理。
func (s *TeamRunStore) SaveSteps(ctx context.Context, id string, steps map[string]team.StepRun,
	status team.TeamRunStatus, cas map[string]team.StepState) (bool, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus string
	var stepsJSON string
	err = tx.QueryRow(ctx,
		`SELECT status, steps_json FROM team_runs WHERE id = $1 FOR UPDATE`, id).
		Scan(&currentStatus, &stepsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, team.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock team run: %w", err)
	}
	if team.TeamRunStatus(currentStatus).Terminal() {
		return false, nil
	}
	current := map[string]team.StepRun{}
	if stepsJSON != "" {
		if err := json.Unmarshal([]byte(stepsJSON), &current); err != nil {
			return false, fmt.Errorf("decode stored steps: %w", err)
		}
	}
	for stepID, want := range cas {
		if current[stepID].State != want {
			return false, nil
		}
	}
	encoded, err := json.Marshal(steps)
	if err != nil {
		return false, fmt.Errorf("encode steps: %w", err)
	}
	completedQuery := `UPDATE team_runs SET status = $2, steps_json = $3`
	args := []any{id, string(status), string(encoded)}
	if status.Terminal() {
		completedQuery += `, completed_at = $4`
		args = append(args, nowTS())
	}
	completedQuery += ` WHERE id = $1`
	if _, err := tx.Exec(ctx, completedQuery, args...); err != nil {
		return false, fmt.Errorf("save team run steps: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// List 返回某团队的运行记录（新→旧）与总数。teamID 为空返回全部。
func (s *TeamRunStore) List(ctx context.Context, teamID string, limit, offset int) ([]team.TeamRun, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := ``
	args := []any{limit, offset}
	if teamID != "" {
		where = ` WHERE team_id = $3`
		args = append(args, teamID)
	}
	var total int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM team_runs`+where, args[2:]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count team runs: %w", err)
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+teamRunColumns+` FROM team_runs`+where+` ORDER BY created_at DESC LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list team runs: %w", err)
	}
	defer rows.Close()
	out := make([]team.TeamRun, 0, limit)
	for rows.Next() {
		tr, err := scanTeamRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *tr)
	}
	return out, total, rows.Err()
}
