package platform

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Push 幂等（域端口）
// ---------------------------------------------------------------------------

// 幂等记录状态。
const (
	IdempotencyProcessing    = "processing"
	IdempotencySyncDone      = "sync_done"
	IdempotencySyncFailed    = "sync_failed"
	IdempotencyAsyncAccepted = "async_accepted"
)

// PushLeaseStaleAfter 是 processing 租约的陈旧阈值：超过后新请求可接管。
const PushLeaseStaleAfter = 15 * time.Minute

// IdempotencyDecisionKind 是 Begin 的决策种类。
type IdempotencyDecisionKind int

const (
	// IdemNew 拿到了新租约，调用方负责执行并最终 Complete/Fail/Release。
	IdemNew IdempotencyDecisionKind = iota
	// IdemInProgress 同键请求正在处理中（409）。
	IdemInProgress
	// IdemSyncCached 已完成的同步响应，回放 ResponseBody（200）。
	IdemSyncCached
	// IdemSyncFailed 已执行但结果无法序列化的终态失败，回放原因（502）。
	// 刻意阻止重试再次触发任务副作用。
	IdemSyncFailed
	// IdemAsyncAccepted 已受理的异步请求，回放 202。
	IdemAsyncAccepted
	// IdemConflict 同键不同 runId/请求体，或同 runId 不同键（409）。
	IdemConflict
)

// IdempotencyDecision 是一次 Begin 决策。
type IdempotencyDecision struct {
	Kind IdempotencyDecisionKind
	// ResponseBody 仅 SyncCached（响应信封）与 SyncFailed（失败原因）携带。
	ResponseBody string
}

// ErrPushLeaseLost 表示幂等租约已不属于本请求（被接管或已推进终态）。
var ErrPushLeaseLost = errors.New("push idempotency lease lost")

// PushIdempotencyStore 是 Push 幂等存储端口。
//
// Push 签名只覆盖请求体（含 body.runId），不覆盖幂等键头——调用方不能
// 通过只改幂等键让同一个已签名 run 执行两次，实现必须以 run_id 唯一约束
// 兜底（跨键同 run 判 Conflict）。
//
// 终态行不做时间清理：Push 签名没有时间戳，删除旧记录会让被捕获的旧
// 请求重新可执行。未来的保留策略必须先补充紧凑的持久 run-id 墓碑。
type PushIdempotencyStore interface {
	// Begin 申请幂等租约。决策优先级（顺序有意义）：
	//  1. runId 或请求体哈希与既有记录不符 → Conflict；
	//  2. sync_done → SyncCached（回放响应体；无响应体是不自洽状态，判 Conflict）；
	//  3. sync_failed → SyncFailed；
	//  4. async_accepted → AsyncAccepted（回放 202）；
	//  5. processing 且已陈旧（> PushLeaseStaleAfter）→ 接管，判 New；
	//  6. processing 且不陈旧 → InProgress；
	//  7. 无记录 → 插入 processing，判 New。
	Begin(ctx context.Context, key, runID, requestHash string) (*IdempotencyDecision, error)
	// CompleteSync 记录同步成功响应（后续同键请求回放）。
	CompleteSync(ctx context.Context, key, runID, requestHash, responseBody string) error
	// FailSync 记录执行后终态失败（阻止重试再次执行有副作用的任务）。
	FailSync(ctx context.Context, key, runID, requestHash, reason string) error
	// AcceptAsync 记录异步受理（后续同键请求回放 202）。
	AcceptAsync(ctx context.Context, key, runID, requestHash string) error
	// Release 释放本请求持有的 processing 租约；只删自己的、仍在
	// processing 的记录，不能动别人已完成的结果。
	Release(ctx context.Context, key, runID, requestHash string) error
}
