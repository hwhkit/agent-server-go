package team

import (
	"context"
	"encoding/json"
	"time"
)

// TeamRunStatus 是团队运行状态。
type TeamRunStatus string

const (
	TeamRunQueued    TeamRunStatus = "queued"
	TeamRunRunning   TeamRunStatus = "running"
	TeamRunCompleted TeamRunStatus = "completed"
	TeamRunFailed    TeamRunStatus = "failed"
	TeamRunCanceled  TeamRunStatus = "canceled"
)

// Terminal 报告团队运行是否已落定。
func (s TeamRunStatus) Terminal() bool {
	switch s {
	case TeamRunCompleted, TeamRunFailed, TeamRunCanceled:
		return true
	}
	return false
}

// StepRun 是团队运行中单个步骤的执行态。ChildRunID 关联派发出的子运行，
// 运行日志页据此做父子展开。
type StepRun struct {
	StepID     string    `json:"step_id"`
	State      StepState `json:"state"`
	Attempts   int       `json:"attempts"`
	ChildRunID string    `json:"child_run_id,omitempty"`
	// OutputRef 是步骤输出的定位符：≤4MiB 的输出内联存储在团队运行记录中，
	// 超限时存 runs 表并以 run:<id> 形式引用（InputTemplate 渲染时解引用）。
	OutputRef string     `json:"output_ref,omitempty"`
	Error     *StepError `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// StepError 是步骤失败信息。Message 不得包含凭据或供应商原始响应。
type StepError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TeamRun 是一次团队运行。全部状态落库，进程崩溃后由引擎从库中恢复推进
// （不在内存中持有任何独占状态）。
type TeamRun struct {
	ID     string
	TeamID string
	// TeamRevision 与 Snapshot 固化提交时刻的团队定义；运行中的团队运行
	// 不受团队后续编辑影响（对齐智能体快照语义）。
	TeamRevision int64
	Snapshot     Team
	// IdempotencyKey 保证提交幂等：同键重放返回既有 TeamRun。
	IdempotencyKey string
	Status         TeamRunStatus
	Steps          map[string]StepRun
	// Input 是团队级输入（步骤模板可引用 {{team.input...}}）。
	Input       json.RawMessage
	Error       *StepError
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// StepState 视图：把 Steps 映射为 Evaluate 需要的状态表。
func (tr *TeamRun) States() map[string]StepState {
	states := make(map[string]StepState, len(tr.Steps))
	for id, sr := range tr.Steps {
		states[id] = sr.State
	}
	return states
}

// TeamRunRepository 是团队运行仓储端口。方法面在编排引擎落地时按事务
// 语义细化；核心不变量：
//   - Create 按 (idempotency_key) 幂等；
//   - SaveSteps 以状态前置条件做 CAS（防止双引擎实例重复推进同一步骤）。
type TeamRunRepository interface {
	Create(ctx context.Context, tr *TeamRun) (created bool, err error)
	Get(ctx context.Context, id string) (*TeamRun, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*TeamRun, error)
	// ListActive 返回未落定的团队运行，供引擎恢复扫描。
	ListActive(ctx context.Context, limit int) ([]TeamRun, error)
	SaveSteps(ctx context.Context, id string, steps map[string]StepRun, status TeamRunStatus, cas map[string]StepState) (bool, error)
	List(ctx context.Context, teamID string, limit, offset int) (items []TeamRun, total int, err error)
}
