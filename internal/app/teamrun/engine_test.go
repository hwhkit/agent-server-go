package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
)

func submitAndWait(t *testing.T, fixture *engineFixture, teamID, key string, input string) *team.TeamRun {
	t.Helper()
	tr, err := fixture.engine.Submit(context.Background(), Submission{
		TeamID: teamID, IdempotencyKey: key, Input: json.RawMessage(input), Actor: "tester",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	fixture.engine.Wait()
	final, err := fixture.teamRuns.Get(context.Background(), tr.ID)
	if err != nil {
		t.Fatalf("get team run: %v", err)
	}
	return final
}

func stepTarget(target string) team.StepTarget {
	if strings.HasPrefix(target, "agent:") {
		return team.StepTarget{AgentSlug: strings.TrimPrefix(target, "agent:")}
	}
	return team.StepTarget{PositionCode: target}
}

// ---------------------------------------------------------------------------
// 提交
// ---------------------------------------------------------------------------

func TestSubmitValidationAndIdempotency(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(testAgent("desk-1", "FUNDAMENTAL_ANALYST"))
	teams := newFakeTeamRepo(testTeam("t-linear",
		team.Step{ID: "a", Target: stepTarget("agent:desk-1")}))
	fixture := newEngineFixture(teams, agents)
	ctx := context.Background()

	if _, err := fixture.engine.Submit(ctx, Submission{TeamID: "ghost", IdempotencyKey: "k"}); err == nil {
		t.Error("unknown team must be rejected")
	}
	if _, err := fixture.engine.Submit(ctx, Submission{TeamID: "t-linear"}); err == nil {
		t.Error("missing idempotency key must be rejected")
	}

	first, err := fixture.engine.Submit(ctx, Submission{TeamID: "t-linear", IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	replay, err := fixture.engine.Submit(ctx, Submission{TeamID: "t-linear", IdempotencyKey: "key-1"})
	if err != nil || replay.ID != first.ID {
		t.Fatalf("idempotent replay = %+v err=%v", replay, err)
	}
	fixture.engine.Wait()
}

func TestSubmitConcurrencyCap(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(testAgent("desk-1", "FUNDAMENTAL_ANALYST"))
	blocked := testTeam("t-blocked", team.Step{ID: "a", Target: stepTarget("agent:desk-1")})
	blocked.MaxConcurrentRuns = 1
	fixture := newEngineFixture(newFakeTeamRepo(blocked), agents)
	fixture.dispatcher.on("desk-1", nil, nil)
	fixture.dispatcher.blockOn("desk-1")

	first, err := fixture.engine.Submit(context.Background(),
		Submission{TeamID: "t-blocked", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	// 等在飞步骤真正开始执行。
	select {
	case <-fixture.dispatcher.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("step never started")
	}
	if _, err := fixture.engine.Submit(context.Background(),
		Submission{TeamID: "t-blocked", IdempotencyKey: "k2"}); err == nil ||
		!strings.Contains(err.Error(), "concurrent run limit") {
		t.Errorf("second submit = %v, want ErrTeamBusy", err)
	}
	close(fixture.dispatcher.release)
	fixture.engine.Wait()
	final, _ := fixture.teamRuns.Get(context.Background(), first.ID)
	if final.Status != team.TeamRunCompleted {
		t.Errorf("final status = %s", final.Status)
	}
}

// ---------------------------------------------------------------------------
// DAG 推进
// ---------------------------------------------------------------------------

func TestLinearDAGWithInputRendering(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(
		testAgent("fund", "FUNDAMENTAL_ANALYST"),
		testAgent("strat", "STRATEGIST"),
	)
	teams := newFakeTeamRepo(testTeam("t-pipeline",
		team.Step{ID: "research", Target: stepTarget("FUNDAMENTAL_ANALYST")},
		team.Step{
			ID: "synthesize", Target: stepTarget("STRATEGIST"),
			DependsOn: []string{"research"},
			InputTemplate: json.RawMessage(
				`{"topic":"{{team.input}}","upstream":"{{steps.research.output}}"}`),
		},
	))
	fixture := newEngineFixture(teams, agents)

	final := submitAndWait(t, fixture, "t-pipeline", "k-linear", `{"symbol":"AAPL"}`)
	if final.Status != team.TeamRunCompleted {
		t.Fatalf("status = %s steps=%+v", final.Status, final.Steps)
	}
	calls := fixture.dispatcher.callLog()
	if len(calls) != 2 || calls[0] != "fund" || calls[1] != "strat" {
		t.Errorf("dispatch order = %v", calls)
	}
	// 子运行父子关联。
	var child *run.Run
	for _, step := range final.Steps {
		if step.StepID == "synthesize" {
			if step.State != team.StepSucceeded || step.ChildRunID == "" {
				t.Fatalf("synthesize step = %+v", step)
			}
			got, err := fixture.runs.Get(context.Background(), step.ChildRunID)
			if err != nil {
				t.Fatalf("child run: %v", err)
			}
			child = got
		}
	}
	if child.TeamRunID != final.ID || child.TeamStepID != "synthesize" || child.AgentSlug != "strat" {
		t.Errorf("child run linkage = %+v", child)
	}
	if child.Output == nil {
		t.Error("child run must persist the result envelope")
	}
	// 下游收到的任务输入必须包含渲染后的团队输入与上游输出。
	task := fixture.dispatcher.taskFor("strat")
	if task == nil {
		t.Fatal("strat task not recorded")
	}
	brief, ok := task.TaskBrief.(map[string]any)
	if !ok {
		t.Fatalf("task brief = %T", task.TaskBrief)
	}
	if topic, ok := brief["topic"].(map[string]any); !ok || topic["symbol"] != "AAPL" {
		t.Errorf("team input not rendered into brief: %v", brief["topic"])
	}
	if up, ok := brief["upstream"].(map[string]any); !ok || up["payload_info"] == nil {
		t.Errorf("upstream output not rendered into brief: %v", brief["upstream"])
	}
}

func TestPositionTargetWithoutStaffFailsStep(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(testAgent("fund", "FUNDAMENTAL_ANALYST"))
	teams := newFakeTeamRepo(testTeam("t-nostaff",
		team.Step{ID: "orphan", Target: stepTarget("RISK_MANAGER")}))
	fixture := newEngineFixture(teams, agents)

	final := submitAndWait(t, fixture, "t-nostaff", "k-nostaff", `{}`)
	if final.Status != team.TeamRunFailed {
		t.Fatalf("status = %s", final.Status)
	}
	step := final.Steps["orphan"]
	if step.State != team.StepFailed || step.Error == nil || step.Error.Code != "no_agent_available" {
		t.Errorf("step = %+v", step)
	}
}

// ---------------------------------------------------------------------------
// 失败策略
// ---------------------------------------------------------------------------

func TestFailurePolicies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		onFailure  team.FailureMode
		wantStatus team.TeamRunStatus
		wantC      team.StepState
	}{
		{"fail_fast 熔断", team.FailFast, team.TeamRunFailed, team.StepPending},
		{"continue 豁免下游", team.Continue, team.TeamRunCompleted, team.StepSucceeded},
		{"skip_branch 跳过下游", team.SkipBranch, team.TeamRunCompleted, team.StepSkipped},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agents := newFakeAgentRepo(
				testAgent("bad", "FUNDAMENTAL_ANALYST"),
				testAgent("good", "STRATEGIST"),
			)
			teams := newFakeTeamRepo(testTeam("t-policy",
				team.Step{ID: "a", Target: stepTarget("agent:bad"), OnFailure: tc.onFailure},
				team.Step{ID: "b", Target: stepTarget("agent:good"), DependsOn: []string{"a"}},
			))
			fixture := newEngineFixture(teams, agents)
			fixture.dispatcher.on("bad", nil, errFake)
			fixture.dispatcher.on("good", nil, nil)

			final := submitAndWait(t, fixture, "t-policy", "k-"+string(tc.onFailure), `{}`)
			if final.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (steps %+v)", final.Status, tc.wantStatus, final.Steps)
			}
			if got := final.Steps["b"].State; got != tc.wantC {
				t.Errorf("downstream step = %s, want %s", got, tc.wantC)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

func TestCancelPropagatesToInflightStep(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(
		testAgent("slow", "FUNDAMENTAL_ANALYST"),
		testAgent("never", "STRATEGIST"),
	)
	teams := newFakeTeamRepo(testTeam("t-cancel",
		team.Step{ID: "a", Target: stepTarget("agent:slow")},
		team.Step{ID: "b", Target: stepTarget("agent:never"), DependsOn: []string{"a"}},
	))
	fixture := newEngineFixture(teams, agents)
	fixture.dispatcher.blockOn("slow")

	tr, err := fixture.engine.Submit(context.Background(),
		Submission{TeamID: "t-cancel", IdempotencyKey: "k-cancel"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-fixture.dispatcher.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("step never started")
	}
	if err := fixture.engine.Cancel(context.Background(), tr.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(fixture.dispatcher.release)
	fixture.engine.Wait()

	final, _ := fixture.teamRuns.Get(context.Background(), tr.ID)
	if final.Status != team.TeamRunCanceled {
		t.Fatalf("status = %s", final.Status)
	}
	if final.Steps["b"].State != team.StepCanceled {
		t.Errorf("pending step must be canceled: %+v", final.Steps["b"])
	}
	if got := final.Steps["a"].State; got != team.StepCanceled && got != team.StepFailed {
		t.Errorf("in-flight step state = %s", got)
	}
	calls := fixture.dispatcher.callLog()
	for _, call := range calls {
		if call == "never" {
			t.Error("downstream must not dispatch after cancel")
		}
	}
}

// ---------------------------------------------------------------------------
// 崩溃恢复
// ---------------------------------------------------------------------------

func TestRecoverFoldsTerminalChildRun(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(testAgent("desk", "FUNDAMENTAL_ANALYST"))
	teams := newFakeTeamRepo(testTeam("t-recover",
		team.Step{ID: "a", Target: stepTarget("agent:desk")},
		team.Step{ID: "b", Target: stepTarget("agent:desk"), DependsOn: []string{"a"}},
	))
	fixture := newEngineFixture(teams, agents)
	ctx := context.Background()

	// 手工构造「引擎崩溃现场」：步骤 a 停在 dispatched，子运行已终态。
	tr := &team.TeamRun{
		ID: "teamrun_orphan", TeamID: "t-recover", TeamRevision: 1,
		Snapshot: *teams.teams["t-recover"], IdempotencyKey: "k-orphan",
		Status: team.TeamRunRunning,
		Steps: map[string]team.StepRun{
			"a": {StepID: "a", State: team.StepDispatched, Attempts: 1, ChildRunID: "run_child_a"},
			"b": {StepID: "b", State: team.StepPending},
		},
	}
	if _, err := fixture.teamRuns.Create(ctx, tr); err != nil {
		t.Fatalf("seed team run: %v", err)
	}
	if err := fixture.runs.Append(ctx, run.Run{RunID: "run_child_a", AgentSlug: "desk", Status: run.StatusRunning}); err != nil {
		t.Fatalf("seed child run: %v", err)
	}
	envelope := json.RawMessage(`{"payload_info":"recovered"}`)
	if _, err := fixture.runs.Complete(ctx, "run_child_a", run.Outcome{Status: run.StatusCompleted, Output: envelope}); err != nil {
		t.Fatalf("complete child: %v", err)
	}

	if err := fixture.engine.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	fixture.engine.Wait()
	final, _ := fixture.teamRuns.Get(ctx, tr.ID)
	if final.Status != team.TeamRunCompleted {
		t.Fatalf("status = %s steps=%+v", final.Status, final.Steps)
	}
	if final.Steps["a"].State != team.StepSucceeded || final.Steps["a"].OutputRef != "run:run_child_a" {
		t.Errorf("folded step = %+v", final.Steps["a"])
	}
}

func TestRecoverRedispatchesOrphanThenExhausts(t *testing.T) {
	t.Parallel()
	agents := newFakeAgentRepo(testAgent("desk", "FUNDAMENTAL_ANALYST"))
	teams := newFakeTeamRepo(testTeam("t-orphan",
		team.Step{ID: "a", Target: stepTarget("agent:desk"), Retry: team.RetryPolicy{MaxAttempts: 2}},
	))
	fixture := newEngineFixture(teams, agents)
	ctx := context.Background()

	// 孤儿步骤（子运行失踪），attempts=1 < 2 → 回到 pending 重派成功。
	tr := &team.TeamRun{
		ID: "teamrun_redo", TeamID: "t-orphan", TeamRevision: 1,
		Snapshot: *teams.teams["t-orphan"], IdempotencyKey: "k-redo",
		Status: team.TeamRunRunning,
		Steps: map[string]team.StepRun{
			"a": {StepID: "a", State: team.StepDispatched, Attempts: 1, ChildRunID: "run_lost"},
		},
	}
	if _, err := fixture.teamRuns.Create(ctx, tr); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := fixture.engine.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	fixture.engine.Wait()
	final, _ := fixture.teamRuns.Get(ctx, tr.ID)
	if final.Status != team.TeamRunCompleted || final.Steps["a"].Attempts != 2 {
		t.Fatalf("redispatch = %s %+v", final.Status, final.Steps["a"])
	}

	// attempts 已到上限的孤儿 → failed(engine_lost)，不再重派。
	tr2 := &team.TeamRun{
		ID: "teamrun_dead", TeamID: "t-orphan", TeamRevision: 1,
		Snapshot: *teams.teams["t-orphan"], IdempotencyKey: "k-dead",
		Status: team.TeamRunRunning,
		Steps: map[string]team.StepRun{
			"a": {StepID: "a", State: team.StepDispatched, Attempts: 2, ChildRunID: "run_lost_2"},
		},
	}
	if _, err := fixture.teamRuns.Create(ctx, tr2); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if err := fixture.engine.Recover(ctx); err != nil {
		t.Fatalf("recover 2: %v", err)
	}
	fixture.engine.Wait()
	dead, _ := fixture.teamRuns.Get(ctx, tr2.ID)
	if dead.Status != team.TeamRunFailed {
		t.Fatalf("exhausted status = %s", dead.Status)
	}
	if dead.Steps["a"].Error == nil || dead.Steps["a"].Error.Code != "engine_lost" {
		t.Errorf("exhausted step = %+v", dead.Steps["a"])
	}
}

// ---------------------------------------------------------------------------
// 渲染器
// ---------------------------------------------------------------------------

func TestRenderPlaceholders(t *testing.T) {
	t.Parallel()
	rc := RenderContext{
		TeamInput:   json.RawMessage(`{"symbol":"AAPL"}`),
		StepOutputs: map[string]json.RawMessage{"a": json.RawMessage(`{"summary":"ok","n":3}`)},
	}
	// 整串占位保留 JSON 类型。
	template := json.RawMessage(`{"brief":"{{team.input}}","up":"{{steps.a.output}}","note":"see {{steps.a.output}} now","keep":"{{steps.missing.output}}","unknown":"{{whatever}}"}`)
	out, err := Render(template, rc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("rendered not JSON: %v (%s)", err, out)
	}
	if brief, ok := parsed["brief"].(map[string]any); !ok || brief["symbol"] != "AAPL" {
		t.Errorf("team.input placeholder = %v", parsed["brief"])
	}
	if up, ok := parsed["up"].(map[string]any); !ok || up["n"] != float64(3) {
		t.Errorf("step output placeholder = %v", parsed["up"])
	}
	if note, _ := parsed["note"].(string); !strings.Contains(note, `"summary":"ok"`) {
		t.Errorf("embedded placeholder = %v", parsed["note"])
	}
	if parsed["keep"] != nil {
		t.Errorf("known-pattern placeholder with missing output must render null: %v", parsed["keep"])
	}
	if parsed["unknown"] != "{{whatever}}" {
		t.Errorf("unknown placeholder must be preserved: %v", parsed["unknown"])
	}
	// 空模板回退团队输入。
	fallback, err := Render(nil, rc)
	if err != nil || string(fallback) != `{"symbol":"AAPL"}` {
		t.Errorf("fallback = %s/%v", fallback, err)
	}
}

// TestRetryNotRetryable 锁定 B10 语义：未落定的团队运行重试必须拒绝
// （ErrNotRetryable），终态允许以新幂等键重放且键可追溯。
func TestRetryNotRetryable(t *testing.T) {
	agents := newFakeAgentRepo(testAgent("desk-1", "FUNDAMENTAL_ANALYST"))
	teams := newFakeTeamRepo(testTeam("t-retry",
		team.Step{ID: "a", Target: stepTarget("agent:desk-1")}))
	fixture := newEngineFixture(teams, agents)
	ctx := context.Background()

	sub, err := fixture.engine.Submit(ctx, Submission{
		TeamID: "t-retry", IdempotencyKey: "retry-orig",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := fixture.engine.Retry(ctx, sub.ID, "tester"); !errors.Is(err, ErrNotRetryable) {
		t.Errorf("retry before terminal = %v, want ErrNotRetryable", err)
	}
	fixture.engine.Wait()
	retried, err := fixture.engine.Retry(ctx, sub.ID, "tester")
	if err != nil {
		t.Fatalf("retry after terminal: %v", err)
	}
	if retried.ID == sub.ID {
		t.Error("retry must create a new team run")
	}
	if !strings.HasPrefix(retried.IdempotencyKey, sub.IdempotencyKey+"/retry/") {
		t.Errorf("retry idempotency key %q must trace to origin %q",
			retried.IdempotencyKey, sub.IdempotencyKey)
	}
}
