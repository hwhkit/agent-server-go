// Package run 定义运行：一次任务派发的完整生命周期记录。
//
// 单发运行与团队步骤运行共用同一实体：TeamRunID/TeamStepID 非空即团队
// 子运行，运行日志页据此做父子展开。
package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"ir-agent-server/internal/domain/exec"
)

// Status 是运行状态。状态机与既有系统对齐：
//
//	queued ──派发──▶ leased ──完成──▶ completed | failed | canceled
//	  ▲                 │
//	  └──租约过期重派────┘（attempts < 上限 且 未过墙钟 且 无取消）
type Status string

const (
	StatusQueued Status = "queued"
	StatusLeased Status = "leased"
	// StatusRunning 是引擎内联执行的进行中状态（A2A 与团队子运行使用，
	// 与存量 agent_runs.status 取值一致）。
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Terminal 报告状态是否为终态。
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// Run 是一次运行记录。
type Run struct {
	RunID     string
	AgentSlug string
	// AgentRevision 与 Executor 快照记录派发时刻的配置，避免智能体后续
	// 编辑改写历史语义。
	AgentRevision int64
	ExecutorKind  string
	ExecutionMode string
	PositionCode  string
	Status        Status
	// Attempt 是第几次派发（1 起）。
	Attempt int
	// TeamRunID / TeamStepID 非空表示团队子运行。
	TeamRunID  string
	TeamStepID string
	// Input/Output 是请求与结果信封原文；ExecutorOutput 是执行节点直出结果。
	Input          json.RawMessage
	Output         json.RawMessage
	ExecutorOutput json.RawMessage
	Usage          *exec.Usage
	DurationMS     *int64
	ErrorCode      string
	// ResponseCode 是动态 Agent HTTP 入口实际返回给调用方的状态码（审计用）。
	ResponseCode *int64
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

// ErrNotFound 是仓储未命中哨兵。
var ErrNotFound = errors.New("run not found")

// Outcome 是一次终态落定的载荷。
type Outcome struct {
	Status    Status
	Output    json.RawMessage
	Usage     *exec.Usage
	ErrorCode string
	Duration  *int64
}

// Filter 是运行日志查询条件。零值表示不过滤。
type Filter struct {
	AgentSlug string
	Position  string
	TeamRunID string
	Statuses  []Status
	Since     time.Time
	Until     time.Time
	// Before 是 keyset 游标（created_at < Before，严格小于）：深分页/导出
	// 用它替代 OFFSET——OFFSET 在大表上是 O(offset) 全扫。与 Until 的
	// 区别仅在边界比较（Until 为 <=）。
	Before         time.Time
	Limit          int
	Offset         int
	SingleRunsOnly bool // 仅单发运行（无团队关联）
}

// Repository 是运行仓储端口。方法面在 infra 迁移阶段按现有存储行为补齐；
// P1 先固定核心语义：Append 按 RunID 幂等，Complete 仅在非终态时落定。
type Repository interface {
	Append(ctx context.Context, r Run) error
	Get(ctx context.Context, runID string) (*Run, error)
	// Complete 把运行落为终态；已终态时返回当前记录且不覆盖（幂等）。
	Complete(ctx context.Context, runID string, outcome Outcome) (*Run, error)
	List(ctx context.Context, f Filter) (items []Run, total int, err error)
}

// Totals 是运行记录的聚合统计（仪表盘用）。
type Totals struct {
	Completed  int
	Failed     int
	Canceled   int
	InProgress int // running + leased
	Queued     int
	// Token 计量汇总（usage_json 落地值累加）。
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}
