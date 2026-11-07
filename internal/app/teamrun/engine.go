// Package teamrun 实现团队编排引擎：把 TeamRun 的 DAG 状态机推进到终态。
//
// 设计要点（蓝图 §3.3）：
//   - 全部状态落库（team_runs.steps_json），引擎进程崩溃后由恢复扫描从库中
//     重建推进，不在内存中持有独占状态；
//   - 步骤派发产生普通子 Run（agent_runs，携带 team_run_id/team_step_id），
//     执行复用 app/dispatch（本机/托管/网关/远端队列对引擎透明）；
//   - DAG 求值是 domain/team.Evaluate 纯函数，引擎只做「求值 → 落库(CAS) →
//     派发 → 折叠结果」的循环；
//   - 步骤语义 at-least-once：孤儿步骤按 Retry.MaxAttempts 重派，耗尽落
//     failed(engine_lost)，副作用幂等是执行方责任。
package teamrun

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
)

// ErrTeamBusy 表示团队并行运行数已达上限。
var ErrTeamBusy = errors.New("team concurrent run limit reached")

// ErrTeamUnavailable 表示团队不存在或未启用。
var ErrTeamUnavailable = errors.New("team is not available")

// Dispatcher 是任务分发端口（app/dispatch 的 *Service 结构上满足）。
type Dispatcher interface {
	Execute(ctx context.Context, agentCode string, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// Aborter 是远端子运行的取消通道（connector.TaskQueue 结构性满足；可为 nil）。
type Aborter interface {
	RequestAbort(ctx context.Context, runID string) error
}

// Engine 是编排引擎。
type Engine struct {
	teams      team.Repository
	teamRuns   team.TeamRunRepository
	agentRuns  run.Repository
	agents     agent.Repository
	dispatcher Dispatcher
	aborter    Aborter

	// reconcileInterval 是恢复扫描节奏。
	reconcileInterval time.Duration

	mu         sync.Mutex
	cancels    map[string]context.CancelFunc // childRunID → 步骤执行取消
	inflight   map[string]bool               // teamRunID+"\x00"+stepID → 在飞
	background sync.WaitGroup
}

// Deps 是 Engine 的依赖注入。
type Deps struct {
	Teams      team.Repository
	TeamRuns   team.TeamRunRepository
	AgentRuns  run.Repository
	Agents     agent.Repository
	Dispatcher Dispatcher
	Aborter    Aborter // 可 nil（无远端步骤取消通道）
	// ReconcileInterval 缺省 15s。
	ReconcileInterval time.Duration
}

// NewEngine 组装编排引擎。
func NewEngine(deps Deps) *Engine {
	interval := deps.ReconcileInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &Engine{
		teams: deps.Teams, teamRuns: deps.TeamRuns, agentRuns: deps.AgentRuns,
		agents: deps.Agents, dispatcher: deps.Dispatcher, aborter: deps.Aborter,
		reconcileInterval: interval,
		cancels:           map[string]context.CancelFunc{},
		inflight:          map[string]bool{},
	}
}

// Wait 等待全部在飞步骤协程结束（优雅停机 / 测试同步）。
func (e *Engine) Wait() { e.background.Wait() }

// ---------------------------------------------------------------------------
// 提交
// ---------------------------------------------------------------------------

// Submission 是一次团队运行提交。
type Submission struct {
	TeamID         string
	Input          json.RawMessage
	IdempotencyKey string
	// Actor 是提交者（审计）。
	Actor string
}

// Submit 幂等提交团队运行：同 IdempotencyKey 重放返回既有运行。
// 成功创建后立即异步推进一轮（恢复循环兜底）。
// ErrNotRetryable 表示目标团队运行尚未落定（重试只针对终态）。
var ErrNotRetryable = errors.New("team run is not in a terminal state")

// Retry 以新幂等键重放一次已落定的团队运行（B10）：沿用运行时固化的
// 团队快照 ID 与原始输入——与「重新提交」的区别是无需再填表单，且
// 重试链通过幂等键前缀可追溯（orig-key/retry-*）。
func (e *Engine) Retry(ctx context.Context, id, actor string) (*team.TeamRun, error) {
	origin, err := e.teamRuns.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !origin.Status.Terminal() {
		return nil, ErrNotRetryable
	}
	return e.Submit(ctx, Submission{
		TeamID: origin.Snapshot.ID,
		Input:  origin.Input,
		IdempotencyKey: fmt.Sprintf("%s/retry/%s", origin.IdempotencyKey,
			strings.ReplaceAll(strconv.FormatInt(time.Now().UnixNano(), 36), "-", "")),
		Actor: actor,
	})
}

func (e *Engine) Submit(ctx context.Context, submission Submission) (*team.TeamRun, error) {
	definition, err := e.teams.Get(ctx, submission.TeamID)
	if err != nil {
		if errors.Is(err, team.ErrNotFound) {
			return nil, fmt.Errorf("%w: team %s not found", ErrTeamUnavailable, submission.TeamID)
		}
		return nil, err
	}
	if !definition.Enabled {
		return nil, fmt.Errorf("%w: team %s is disabled", ErrTeamUnavailable, definition.ID)
	}
	if strings.TrimSpace(submission.IdempotencyKey) == "" {
		return nil, errors.New("idempotency key is required")
	}

	// 并行上限：只数未落定的。
	active, err := e.teamRuns.ListActive(ctx, 512)
	if err != nil {
		return nil, err
	}
	inFlight := 0
	for _, item := range active {
		if item.TeamID == definition.ID {
			inFlight++
		}
	}
	if inFlight >= definition.MaxConcurrentRuns {
		return nil, fmt.Errorf("%w: %d/%d", ErrTeamBusy, inFlight, definition.MaxConcurrentRuns)
	}

	tr := &team.TeamRun{
		ID: "teamrun_" + randomToken(12), TeamID: definition.ID,
		TeamRevision: definition.Revision, Snapshot: *definition,
		IdempotencyKey: submission.IdempotencyKey,
		Status:         team.TeamRunQueued,
		Steps:          map[string]team.StepRun{},
		Input:          submission.Input,
	}
	for _, step := range definition.Steps {
		tr.Steps[step.ID] = team.StepRun{StepID: step.ID, State: team.StepPending}
	}
	created, err := e.teamRuns.Create(ctx, tr)
	if err != nil {
		return nil, fmt.Errorf("create team run: %w", err)
	}
	if !created {
		existing, err := e.teamRuns.GetByIdempotencyKey(ctx, submission.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		return existing, nil
	}
	slog.Info("team run submitted", "team_run", tr.ID, "team", tr.TeamID,
		"steps", len(tr.Steps), "actor", submission.Actor)
	e.spawn(context.WithoutCancel(ctx), tr.ID)
	return tr, nil
}

// Get 按 ID 读取团队运行（管理台查询入口）。
func (e *Engine) Get(ctx context.Context, teamRunID string) (*team.TeamRun, error) {
	return e.teamRuns.Get(ctx, teamRunID)
}

// List 分页列出团队运行（新→旧）；teamID 为空返回全部。
func (e *Engine) List(ctx context.Context, teamID string, limit, offset int) ([]team.TeamRun, int, error) {
	return e.teamRuns.List(ctx, teamID, limit, offset)
}

// Cancel 请求取消团队运行：未派发步骤置 canceled，在飞步骤经 ctx/远端
// abort 通道传播；已落定则无操作。
func (e *Engine) Cancel(ctx context.Context, teamRunID string) error {
	tr, err := e.teamRuns.Get(ctx, teamRunID)
	if err != nil {
		return err
	}
	if tr.Status.Terminal() {
		return nil
	}
	// 在飞步骤：取消执行 ctx + 远端 abort。
	e.mu.Lock()
	for stepID, step := range tr.Steps {
		if step.State == team.StepDispatched || step.State == team.StepRunning {
			if cancel, ok := e.cancels[step.ChildRunID]; ok {
				cancel()
			}
			if e.aborter != nil && step.ChildRunID != "" {
				if err := e.aborter.RequestAbort(ctx, step.ChildRunID); err != nil {
					slog.Warn("team cancel: remote abort failed",
						"team_run", teamRunID, "step", stepID, "err", err)
				}
			}
		}
	}
	e.mu.Unlock()

	steps := cloneSteps(tr.Steps)
	cas := map[string]team.StepState{}
	now := time.Now().UTC()
	for id, step := range steps {
		if step.State == team.StepPending || step.State == team.StepDispatched ||
			step.State == team.StepRunning {
			cas[id] = step.State
			step.State = team.StepCanceled
			step.EndedAt = &now
			steps[id] = step
		}
	}
	ok, err := e.teamRuns.SaveSteps(ctx, teamRunID, steps, team.TeamRunCanceled, cas)
	if err != nil {
		return err
	}
	if !ok {
		// 并发推进/已终态：重读确认。
		current, err := e.teamRuns.Get(ctx, teamRunID)
		if err != nil {
			return err
		}
		if !current.Status.Terminal() {
			return errors.New("cancel lost the state race; retry")
		}
	}
	slog.Info("team run canceled", "team_run", teamRunID)
	return nil
}

// ---------------------------------------------------------------------------
// 推进循环
// ---------------------------------------------------------------------------

// Advance 把一个团队运行推进到「无可立即执行的动作」为止：折叠已完成
// 子运行、应用跳过、派发就绪步骤、落定终态。幂等，可重复调用（恢复扫描
// 与步骤完成回调共用）。
func (e *Engine) Advance(ctx context.Context, teamRunID string) error {
	for iteration := 0; iteration < 128; iteration++ {
		tr, err := e.teamRuns.Get(ctx, teamRunID)
		if err != nil {
			return err
		}
		if tr.Status.Terminal() {
			return nil
		}
		folded, err := e.foldChildOutcomes(ctx, tr)
		if err != nil {
			return err
		}
		if folded {
			// 折叠改变了状态：重读后重新求值。
			continue
		}
		progressed, err := e.advanceOnce(ctx, tr)
		if err != nil {
			return err
		}
		if !progressed {
			return nil
		}
	}
	slog.Warn("team run advance hit the iteration cap", "team_run", teamRunID)
	return nil
}

// advanceOnce 执行一轮求值-落库-派发。返回是否产生了状态变化。
func (e *Engine) advanceOnce(ctx context.Context, tr *team.TeamRun) (bool, error) {
	evaluation := tr.Snapshot.Evaluate(tr.States())

	// fail_fast：立即落定团队失败。
	if evaluation.FailFastStep != "" {
		failed := cloneSteps(tr.Steps)
		step := failed[evaluation.FailFastStep]
		message := "step failed under fail_fast"
		if step.Error != nil && step.Error.Message != "" {
			message = step.Error.Message
		}
		cas := map[string]team.StepState{evaluation.FailFastStep: team.StepFailed}
		tr.Error = &team.StepError{Code: "step_failed", Message: message}
		return e.save(ctx, tr.ID, failed, team.TeamRunFailed, cas)
	}

	steps := cloneSteps(tr.Steps)
	cas := map[string]team.StepState{}
	changed := false

	// 跳过传播。
	for _, stepID := range evaluation.Skip {
		current := steps[stepID]
		if current.State != team.StepPending {
			continue
		}
		cas[stepID] = team.StepPending
		current.State = team.StepSkipped
		now := time.Now().UTC()
		current.EndedAt = &now
		steps[stepID] = current
		changed = true
	}

	// 终态落定。
	if evaluation.Settled {
		status := team.TeamRunCompleted
		ok, err := e.save(ctx, tr.ID, steps, status, cas)
		if ok && err == nil {
			slog.Info("team run completed", "team_run", tr.ID)
		}
		return ok, err
	}

	// 派发就绪步骤。
	var ready []team.Step
	readyIDs := map[string]bool{}
	for _, stepID := range evaluation.Ready {
		readyIDs[stepID] = true
	}
	for _, step := range tr.Snapshot.Steps {
		if readyIDs[step.ID] {
			ready = append(ready, step)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })

	dispatched := false
	for _, step := range ready {
		agentSlug, resolveErr := e.resolveTarget(ctx, step.Target)
		current := steps[step.ID]
		if resolveErr != nil {
			// 无可用智能体：步骤立即失败（走其 OnFailure 策略）。
			cas[step.ID] = team.StepPending
			now := time.Now().UTC()
			current.State = team.StepFailed
			current.EndedAt = &now
			current.Error = &team.StepError{Code: "no_agent_available", Message: resolveErr.Error()}
			steps[step.ID] = current
			changed = true
			continue
		}
		childRunID := "run_" + randomToken(12)
		cas[step.ID] = team.StepPending
		now := time.Now().UTC()
		current.State = team.StepDispatched
		current.Attempts++
		current.ChildRunID = childRunID
		current.StartedAt = &now
		steps[step.ID] = current
		dispatched = true

		// 子运行登记（幂等 Append）。
		timeout := e.stepTimeout(step, agentSlug)
		if err := e.agentRuns.Append(ctx, run.Run{
			RunID: childRunID, AgentSlug: agentSlug, Status: run.StatusRunning,
			Attempt: current.Attempts, TeamRunID: tr.ID, TeamStepID: step.ID,
			PositionCode: step.Target.PositionCode,
		}); err != nil {
			slog.Error("append team step run failed", "team_run", tr.ID,
				"step", step.ID, "err", err)
		}
		e.spawnStep(tr, step, agentSlug, childRunID, timeout)
	}

	if dispatched || changed {
		status := tr.Status
		if status == team.TeamRunQueued {
			status = team.TeamRunRunning
		}
		return e.save(ctx, tr.ID, steps, status, cas)
	}
	return false, nil
}

// save 以 CAS 落库步骤状态；失败（并发推进）返回 false 由调用方重读。
func (e *Engine) save(ctx context.Context, id string, steps map[string]team.StepRun,
	status team.TeamRunStatus, cas map[string]team.StepState) (bool, error) {
	ok, err := e.teamRuns.SaveSteps(ctx, id, steps, status, cas)
	if err != nil {
		return false, err
	}
	if !ok {
		slog.Info("team run state moved under us; reloading", "team_run", id)
	}
	return ok, nil
}

// foldChildOutcomes 把已终态的子运行折叠进步骤状态（恢复扫描与推进共用；
// 正常路径由步骤协程自己折叠，这里是兜底）。
func (e *Engine) foldChildOutcomes(ctx context.Context, tr *team.TeamRun) (bool, error) {
	steps := cloneSteps(tr.Steps)
	cas := map[string]team.StepState{}
	changed := false
	for id, step := range steps {
		if step.State != team.StepDispatched && step.State != team.StepRunning {
			continue
		}
		if step.ChildRunID == "" || e.isStepInflight(tr.ID, id) {
			continue
		}
		child, err := e.agentRuns.Get(ctx, step.ChildRunID)
		if err != nil || !child.Status.Terminal() {
			continue
		}
		cas[id] = step.State
		now := time.Now().UTC()
		step.EndedAt = &now
		switch child.Status {
		case run.StatusCompleted:
			step.State = team.StepSucceeded
			step.OutputRef = "run:" + child.RunID
		case run.StatusCanceled:
			step.State = team.StepCanceled
		default:
			step.State = team.StepFailed
			step.Error = &team.StepError{Code: orDefault(child.ErrorCode, "step_failed"),
				Message: "step run finished as " + string(child.Status)}
		}
		steps[id] = step
		changed = true
	}
	if !changed {
		return false, nil
	}
	ok, err := e.save(ctx, tr.ID, steps, tr.Status, cas)
	return ok, err
}

// ---------------------------------------------------------------------------
// 步骤执行
// ---------------------------------------------------------------------------

func (e *Engine) spawnStep(tr *team.TeamRun, step team.Step, agentSlug, childRunID string, timeout time.Duration) {
	input := e.renderStepInput(tr, step)
	stepCtx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[childRunID] = cancel
	e.inflight[tr.ID+"\x00"+step.ID] = true
	e.mu.Unlock()

	e.background.Add(1)
	go func() {
		defer e.background.Done()
		defer cancel()
		defer func() {
			e.mu.Lock()
			delete(e.cancels, childRunID)
			delete(e.inflight, tr.ID+"\x00"+step.ID)
			e.mu.Unlock()
			if recovered := recover(); recovered != nil {
				slog.Error("team step goroutine panicked",
					"team_run", tr.ID, "step", step.ID, "child_run", childRunID)
			}
			// 结果已折叠或异常退出：推进一轮让 DAG 继续（脱离步骤 ctx，
			// 取消/超时后仍要落定团队状态）。
			if err := e.Advance(context.Background(), tr.ID); err != nil {
				slog.Warn("team advance after step failed", "team_run", tr.ID, "err", err)
			}
		}()
		e.executeStep(stepCtx, tr, step, agentSlug, childRunID, input, timeout)
	}()
}

// executeStep 执行一个步骤：分发 → 子运行落终态 → 步骤状态折叠（CAS）。
func (e *Engine) executeStep(ctx context.Context, tr *team.TeamRun, step team.Step,
	agentSlug, childRunID string, input json.RawMessage, timeout time.Duration) {
	runID := childRunID
	var parsed any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &parsed)
	}
	task := &platform.TaskContext{
		RunID:        &runID,
		TaskBrief:    parsed,
		Params:       parsed,
		PositionType: nonEmptyOrNil(step.Target.PositionCode),
	}
	started := time.Now()
	output, err := e.dispatcher.Execute(ctx, agentSlug, task, parsed, timeout)
	duration := time.Since(started).Milliseconds()

	outcome := run.Outcome{Duration: &duration}
	var outputRef string
	switch {
	case err == nil && output != nil:
		outcome.Status = run.StatusCompleted
		envelope, marshalErr := json.Marshal(output.ToPayload())
		if marshalErr == nil {
			outcome.Output = envelope
			outputRef = "run:" + childRunID
		}
		if output.Usage != nil {
			outcome.Usage = &exec.Usage{
				InputTokens:  int64(output.Usage.PromptTokens),
				OutputTokens: int64(output.Usage.CompletionTokens),
				TotalTokens:  int64(output.Usage.PromptTokens + output.Usage.CompletionTokens),
			}
		}
	case ctx.Err() != nil:
		outcome.Status = run.StatusCanceled
		outcome.ErrorCode = "canceled"
	default:
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = "executor_failed"
		var coded interface{ ErrorCode() string }
		if errors.As(err, &coded) {
			outcome.ErrorCode = coded.ErrorCode()
		}
	}
	if _, completeErr := e.agentRuns.Complete(ctx, childRunID, outcome); completeErr != nil {
		slog.Error("complete team step run failed", "child_run", childRunID, "err", completeErr)
	}

	// 折叠步骤状态（CAS：dispatched → 终态）。团队运行已被取消/落定时
	// SaveSteps 返回 false，结果仍保留在子运行里供审计。
	state := team.StepSucceeded
	var stepErr *team.StepError
	if outcome.Status == run.StatusCanceled {
		state = team.StepCanceled
	} else if outcome.Status == run.StatusFailed {
		state = team.StepFailed
		code := orDefault(outcome.ErrorCode, "step_failed")
		stepErr = &team.StepError{Code: code, Message: "step execution failed"}
	}
	now := time.Now().UTC()
	updated := team.StepRun{
		StepID: step.ID, State: state, Attempts: 0, ChildRunID: childRunID,
		OutputRef: outputRef, Error: stepErr, EndedAt: &now,
	}
	bg := context.WithoutCancel(ctx)
	current, err := e.teamRuns.Get(bg, tr.ID)
	if err != nil {
		slog.Error("reload team run after step failed", "team_run", tr.ID, "err", err)
		return
	}
	steps := cloneSteps(current.Steps)
	existing := steps[step.ID]
	updated.Attempts = existing.Attempts
	if existing.StartedAt != nil {
		updated.StartedAt = existing.StartedAt
	}
	steps[step.ID] = updated
	if _, err := e.teamRuns.SaveSteps(bg, tr.ID, steps, current.Status,
		map[string]team.StepState{step.ID: existing.State}); err != nil {
		slog.Error("fold team step state failed", "team_run", tr.ID, "step", step.ID, "err", err)
	}
}

// renderStepInput 渲染步骤输入：上游输出经 OutputRef 从子运行回读。
func (e *Engine) renderStepInput(tr *team.TeamRun, step team.Step) json.RawMessage {
	outputs := map[string]json.RawMessage{}
	for id, prior := range tr.Steps {
		if prior.State == team.StepSucceeded && strings.HasPrefix(prior.OutputRef, "run:") {
			child, err := e.agentRuns.Get(context.Background(), strings.TrimPrefix(prior.OutputRef, "run:"))
			if err == nil && len(child.Output) > 0 {
				outputs[id] = child.Output
			}
		}
	}
	rendered, err := Render(step.InputTemplate, RenderContext{TeamInput: tr.Input, StepOutputs: outputs})
	if err != nil {
		slog.Warn("render step input failed; falling back to team input",
			"team_run", tr.ID, "step", step.ID, "err", err)
		return tr.Input
	}
	return rendered
}

// resolveTarget 把步骤目标解析为智能体 slug。岗位目标在聘任该岗位的启用
// 智能体中按 slug 字典序取第一个（确定性；容量由目标智能体队列约束）。
func (e *Engine) resolveTarget(ctx context.Context, target team.StepTarget) (string, error) {
	if target.AgentSlug != "" {
		agentRecord, err := e.agents.GetBySlug(ctx, target.AgentSlug)
		if err != nil {
			return "", fmt.Errorf("agent %s unavailable: %w", target.AgentSlug, err)
		}
		if !agentRecord.Enabled {
			return "", fmt.Errorf("agent %s is disabled", target.AgentSlug)
		}
		return agentRecord.Slug, nil
	}
	all, err := e.agents.List(ctx, false)
	if err != nil {
		return "", err
	}
	candidates := make([]string, 0, 2)
	for _, item := range all {
		if item.ServesPosition(target.PositionCode) {
			candidates = append(candidates, item.Slug)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no enabled agent staffs position %s", target.PositionCode)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

// stepTimeout 计算步骤墙钟：步骤覆盖 > 目标智能体配置 > 领域缺省。
func (e *Engine) stepTimeout(step team.Step, agentSlug string) time.Duration {
	if step.TimeoutSecs != nil && *step.TimeoutSecs > 0 {
		return time.Duration(*step.TimeoutSecs) * time.Second
	}
	agentRecord, err := e.agents.GetBySlug(context.Background(), agentSlug)
	if err == nil && agentRecord.TimeoutSecs > 0 {
		return time.Duration(agentRecord.TimeoutSecs) * time.Second
	}
	return time.Duration(agent.DefaultTimeoutSecs) * time.Second
}

func (e *Engine) isStepInflight(teamRunID, stepID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inflight[teamRunID+"\x00"+stepID]
}

// spawn 后台推进一轮（提交后立即调度，不阻塞调用方）。
func (e *Engine) spawn(ctx context.Context, teamRunID string) {
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("team advance goroutine panicked", "team_run", teamRunID)
			}
		}()
		if err := e.Advance(ctx, teamRunID); err != nil {
			slog.Warn("initial team advance failed", "team_run", teamRunID, "err", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// 恢复
// ---------------------------------------------------------------------------

// Recover 扫描未落定的团队运行：折叠已终态子运行、重派孤儿步骤
// （在飞集合里没有、子运行也没在推进的 dispatched/running 步骤）、继续 DAG。
func (e *Engine) Recover(ctx context.Context) error {
	active, err := e.teamRuns.ListActive(ctx, 256)
	if err != nil {
		return err
	}
	for _, tr := range active {
		if err := e.recoverOne(ctx, &tr); err != nil {
			slog.Warn("team run recovery failed", "team_run", tr.ID, "err", err)
		}
	}
	return nil
}

func (e *Engine) recoverOne(ctx context.Context, tr *team.TeamRun) error {
	steps := cloneSteps(tr.Steps)
	cas := map[string]team.StepState{}
	changed := false
	for id, step := range steps {
		if step.State != team.StepDispatched && step.State != team.StepRunning {
			continue
		}
		if e.isStepInflight(tr.ID, id) {
			continue // 本进程正在执行
		}
		// 孤儿步骤：子运行终态则折叠，否则按重试预算重派或判死。
		if step.ChildRunID != "" {
			child, err := e.agentRuns.Get(ctx, step.ChildRunID)
			if err == nil && child.Status.Terminal() {
				continue // foldChildOutcomes 会处理
			}
		}
		maxAttempts := 1
		for _, definition := range tr.Snapshot.Steps {
			if definition.ID == id {
				maxAttempts = definition.Retry.MaxAttempts
			}
		}
		cas[id] = step.State
		if step.Attempts < maxAttempts {
			step.State = team.StepPending // 回到待派发，Advance 会重派
			step.ChildRunID = ""
		} else {
			now := time.Now().UTC()
			step.State = team.StepFailed
			step.EndedAt = &now
			step.Error = &team.StepError{Code: "engine_lost",
				Message: "step execution was lost with the engine and retries are exhausted"}
		}
		steps[id] = step
		changed = true
	}
	if changed {
		if _, err := e.save(ctx, tr.ID, steps, tr.Status, cas); err != nil {
			return err
		}
	}
	return e.Advance(ctx, tr.ID)
}

// Run 是恢复循环：按 reconcileInterval 扫描未落定运行直至 ctx 取消。
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Recover(ctx); err != nil {
				slog.Warn("team run reconcile failed", "err", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func cloneSteps(steps map[string]team.StepRun) map[string]team.StepRun {
	out := make(map[string]team.StepRun, len(steps))
	for id, step := range steps {
		out[id] = step
	}
	return out
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nonEmptyOrNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
