package connector

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// 远端任务队列（agent-api 的 claim → renew → complete 生命周期）
// ---------------------------------------------------------------------------

// TaskState 是队列任务状态。
type TaskState string

const (
	TaskQueued     TaskState = "queued"
	TaskLeased     TaskState = "leased"
	TaskCompleted  TaskState = "completed"
	TaskFailed     TaskState = "failed"
	TaskCanceled   TaskState = "canceled"
	TaskSuperseded TaskState = "superseded"
)

// Terminal 报告状态是否不可再变更。
func (s TaskState) Terminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskCanceled, TaskSuperseded:
		return true
	}
	return false
}

// Task 是一条队列任务的完整快照。
type Task struct {
	RunID         string
	AgentSlug     string
	AgentRevision int64
	ExecutorKind  string
	Payload       []byte
	State         TaskState
	LeaseOwner    string
	// LeaseExpiresAt 零值表示无租约。
	LeaseExpiresAt time.Time
	Attempts       int
	// FirstDeliveredAt 是首次派发时刻（executionDeadline 的基准）。
	FirstDeliveredAt  time.Time
	ExecutionDeadline time.Time
	QueueExpiresAt    time.Time
	AbortRequested    bool
	Result            []byte
	CreatedAt         time.Time
	CompletedAt       time.Time
}

// NewTask 是一次入队请求。AdmissionLimit 是该智能体的准入上限
// （MaxConcurrency + QueueCapacity）；队列中 queued+leased 达到上限时
// 入队被拒（ErrQueueFull）。
type NewTask struct {
	RunID         string
	AgentSlug     string
	AgentRevision int64
	ExecutorKind  string
	Payload       []byte
	// ExecutionTimeout 决定 ExecutionDeadline = 入队时刻 + ExecutionTimeout。
	ExecutionTimeout time.Duration
	QueueTTL         time.Duration
	AdmissionLimit   int
}

// ClaimPolicy 是认领侧的服务端策略。
type ClaimPolicy struct {
	LeaseTTL             time.Duration
	MaxOutstandingLeases int
}

// ClaimRequest 是一次认领请求。Capacity ≤ 0 表示 worker 不限并发
// （仍受 MaxOutstandingLeases 全局闸约束）。
type ClaimRequest struct {
	WorkerID string
	Slugs    []string
	Capacity int
	Policy   ClaimPolicy
}

// ClaimOutcome 是 claim 的三种互斥结果。
type ClaimOutcome int

const (
	// ClaimDelivered 认领成功，Task 有效。
	ClaimDelivered ClaimOutcome = iota
	// ClaimEmpty 窗口内无任务（真空闲或 scope 不交）。
	ClaimEmpty
	// ClaimRateLimited 并发已满（全局或 worker capacity）。
	ClaimRateLimited
)

// RenewOutcome 是 renew 的五种互斥结果（AGENT-PULL-API §2）。
type RenewOutcome int

const (
	RenewOK RenewOutcome = iota
	// RenewAbort 平台已请求取消，连接器应停止执行与续租。
	RenewAbort
	// RenewLeaseLost 租约已过期/被重派，结果不会被接受。
	RenewLeaseLost
	// RenewDeadline 超过执行墙钟。
	RenewDeadline
	// RenewUnknown runId 未知或已终态。
	RenewUnknown
)

// CompleteRequest 是一次结果回传。Status ∈ succeeded | failed | aborted。
type CompleteRequest struct {
	RunID     string
	WorkerID  string
	Status    string
	Result    []byte
	ErrorCode string
}

// CompleteOutcome 是 complete 的判定结果（AGENT-PULL-API §3）。
type CompleteOutcome struct {
	// Accepted 只有租约有效（或幂等重放一致）时为 true。
	Accepted bool
	// RunStatus 是任务落定的状态（completed/failed/canceled/superseded）。
	RunStatus string
	// Conflict 表示同 runId 曾以不同请求体完成（RESULT_CONFLICT，以首次为准）。
	Conflict bool
}

// SweepPolicy 是后台清扫策略。
type SweepPolicy struct {
	// MaxAttempts 是重派上限（attempts 达到后过期租约落终态）。
	MaxAttempts int
	// WorkerOfflineAfterMiss × RenewInterval 是 worker 离线判定窗口。
	WorkerOfflineAfterMiss int
	RenewInterval          time.Duration
}

// SweepStats 是一次清扫的战果。
type SweepStats struct {
	Requeued       int64
	TerminalLease  int64
	TerminalQueue  int64
	WorkersOffline int64
}

// Errors.
var (
	// ErrQueueFull 表示智能体队列（queued+leased）已满，对应 agent_overloaded。
	ErrQueueFull = errors.New("pull queue is full")
	// ErrTaskNotFound 表示 runId 未知。
	ErrTaskNotFound = errors.New("pull task not found")
)

// TaskQueue 是远端任务队列端口。交付语义 at-least-once：租约过期重派与
// 原执行者迟到的 complete 并存，副作用幂等是连接器责任。
type TaskQueue interface {
	// Enqueue 入队并同时登记运行记录；超出准入上限返回 ErrQueueFull。
	Enqueue(ctx context.Context, task NewTask) error
	// Get 读取任务快照；未知 runId 返回 ErrTaskNotFound。
	Get(ctx context.Context, runID string) (*Task, error)
	// WaitTerminal 阻塞轮询直至终态；ctx 超时/取消返回 ctx.Err()
	// （任务仍在远端执行，结果照常落库）。
	WaitTerminal(ctx context.Context, runID string) (TaskState, []byte, error)
	// Claim 用 SKIP LOCKED 弹出一条 queued 任务并建立租约。
	Claim(ctx context.Context, req ClaimRequest) (*Task, ClaimOutcome, error)
	// WorkerLeaseCount 返回该 worker 当前持有的活跃租约数
	// （WORKER_LEASE_ACTIVE 409 判定：服务端可见租约多于自报在途数
	// 即疑似双实例）。
	WorkerLeaseCount(ctx context.Context, workerID string) (int, error)
	// Renew 为活跃租约续期；租约必须仍由该 worker 持有且未过期、未过墙钟。
	Renew(ctx context.Context, runID, workerID string, leaseTTL time.Duration) (RenewOutcome, error)
	// RequestAbort 置取消位；下一轮 renew 以 RenewAbort 传达。
	RequestAbort(ctx context.Context, runID string) error
	// Complete 幂等回传结果；租约失效后到达的结果仅归档为 superseded。
	Complete(ctx context.Context, req CompleteRequest) (CompleteOutcome, error)
	// QueueDepth 返回指定 slug 集合的 queued 任务数。
	QueueDepth(ctx context.Context, slugs []string) (int, error)
	// Sweep 执行后台清扫：过期租约重派/落终态、队列 TTL、worker 离线判定。
	Sweep(ctx context.Context, policy SweepPolicy) (SweepStats, error)
	// RecordDirectRun 幂等登记一次机器侧直接提交的运行（定时任务/本地触发，
	// 不经租约）：直接落 agent_runs 终态；同 (slug, runId) 重放返回既有状态。
	// slug 必须对应已注册智能体（ErrUnknownAgent）。
	RecordDirectRun(ctx context.Context, run DirectRun) (string, error)
}

// ErrUnknownAgent 表示直接运行提交指向未注册的智能体。
var ErrUnknownAgent = errors.New("unknown agent")

// DirectRun 是一次机器侧直接提交的运行（POST /agent-api/v1/runs）。
// Result 是与 complete 同构的规范化结果文档。
type DirectRun struct {
	Slug       string
	RunID      string // 空 = 由服务层生成
	Status     string // succeeded | failed
	Input      []byte // 调用输入（cron 参数等），可空
	Result     []byte
	ErrorCode  string
	DurationMs int64
}

// QueueTotals 是队列的在途统计（仪表盘用）。
type QueueTotals struct {
	Queued int
	Leased int
}
