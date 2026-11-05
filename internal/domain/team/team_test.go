package team

import (
	"testing"
)

func testTeam(t *testing.T, steps ...Step) *Team {
	t.Helper()
	team := &Team{ID: "t1", Name: "研究团队", Steps: steps}
	if err := team.Validate(); err != nil {
		t.Fatalf("validate fixture team: %v", err)
	}
	return team
}

func step(id string, deps ...string) Step {
	return Step{
		ID:        id,
		Target:    StepTarget{PositionCode: "FUNDAMENTAL_ANALYST"},
		DependsOn: deps,
	}
}

func TestValidateRejectsCycles(t *testing.T) {
	t.Parallel()
	team := &Team{ID: "t", Name: "n", Steps: []Step{step("a", "b"), step("b", "a")}}
	if err := team.Validate(); err == nil {
		t.Fatal("cycle must be rejected")
	}
}

func TestValidateRejectsUnknownDependencyAndDupID(t *testing.T) {
	t.Parallel()
	if err := (&Team{ID: "t", Name: "n", Steps: []Step{step("a", "ghost")}}).Validate(); err == nil {
		t.Error("unknown dependency must be rejected")
	}
	dup := []Step{step("a"), step("a")}
	if err := (&Team{ID: "t", Name: "n", Steps: dup}).Validate(); err == nil {
		t.Error("duplicate step id must be rejected")
	}
}

func TestValidateRejectsBadTargets(t *testing.T) {
	t.Parallel()
	both := Step{ID: "a", Target: StepTarget{PositionCode: "P", AgentSlug: "s"}}
	if err := (&Team{ID: "t", Name: "n", Steps: []Step{both}}).Validate(); err == nil {
		t.Error("target with both position and agent must be rejected")
	}
	none := Step{ID: "a"}
	if err := (&Team{ID: "t", Name: "n", Steps: []Step{none}}).Validate(); err == nil {
		t.Error("target with neither position nor agent must be rejected")
	}
}

func TestValidateAppliesDefaults(t *testing.T) {
	t.Parallel()
	team := &Team{ID: "t", Name: "n", Steps: []Step{step("a")}}
	if err := team.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if team.MaxConcurrentRuns != DefaultMaxConcurrentRuns {
		t.Errorf("max concurrent runs default = %d", team.MaxConcurrentRuns)
	}
	if team.Steps[0].Retry.MaxAttempts != 1 {
		t.Errorf("retry default = %d", team.Steps[0].Retry.MaxAttempts)
	}
	if team.Steps[0].OnFailure != FailFast {
		t.Errorf("on_failure default = %q", team.Steps[0].OnFailure)
	}
}

func TestTopologicalOrderDeterministic(t *testing.T) {
	t.Parallel()
	team := testTeam(t, step("c", "a", "b"), step("a"), step("b", "a"))
	order, err := team.TopologicalOrder()
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	got := []string{order[0].ID, order[1].ID, order[2].ID}
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestEvaluateLinearProgression(t *testing.T) {
	t.Parallel()
	team := testTeam(t, step("a"), step("b", "a"))

	ev := team.Evaluate(map[string]StepState{})
	if len(ev.Ready) != 1 || ev.Ready[0] != "a" || ev.Verdict != VerdictRunning {
		t.Fatalf("initial evaluation = %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"a": StepSucceeded})
	if len(ev.Ready) != 1 || ev.Ready[0] != "b" {
		t.Fatalf("after a succeeded = %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"a": StepSucceeded, "b": StepRunning})
	if len(ev.Ready) != 0 || ev.Settled {
		t.Fatalf("while b running = %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"a": StepSucceeded, "b": StepSucceeded})
	if !ev.Settled || ev.Verdict != VerdictCompleted {
		t.Fatalf("all done = %+v", ev)
	}
}

func TestEvaluateFailFastStopsEverything(t *testing.T) {
	t.Parallel()
	team := testTeam(t, step("a"), step("b", "a"))
	ev := team.Evaluate(map[string]StepState{"a": StepFailed})
	if ev.Verdict != VerdictFailed || ev.FailFastStep != "a" {
		t.Fatalf("fail_fast evaluation = %+v", ev)
	}
	if len(ev.Ready) != 0 {
		t.Fatalf("fail_fast must not dispatch more steps: %+v", ev)
	}
}

func TestEvaluateContinueExemptsDownstream(t *testing.T) {
	t.Parallel()
	a := step("a")
	a.OnFailure = Continue
	b := step("b", "a")
	team := testTeam(t, a, b)

	ev := team.Evaluate(map[string]StepState{"a": StepFailed})
	if len(ev.Ready) != 1 || ev.Ready[0] != "b" || ev.Verdict != VerdictRunning {
		t.Fatalf("continue mode must dispatch downstream: %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"a": StepFailed, "b": StepSucceeded})
	if !ev.Settled || ev.Verdict != VerdictCompleted {
		t.Fatalf("continue mode terminal = %+v", ev)
	}
}

func TestEvaluateSkipBranchPropagates(t *testing.T) {
	t.Parallel()
	a := step("a")
	a.OnFailure = SkipBranch
	team := testTeam(t, a, step("b", "a"), step("c", "b"), step("d"))

	ev := team.Evaluate(map[string]StepState{"a": StepFailed})
	if len(ev.Skip) != 1 || ev.Skip[0] != "b" {
		t.Fatalf("first hop skip = %+v", ev)
	}
	if len(ev.Ready) != 1 || ev.Ready[0] != "d" {
		t.Fatalf("unrelated branch must still dispatch: %+v", ev)
	}

	// 引擎迭代到不动点：b 跳过后 c 也应跳过；d 成功则团队 completed。
	ev = team.Evaluate(map[string]StepState{"a": StepFailed, "b": StepSkipped, "d": StepSucceeded})
	if len(ev.Skip) != 1 || ev.Skip[0] != "c" {
		t.Fatalf("transitive skip = %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"a": StepFailed, "b": StepSkipped, "c": StepSkipped, "d": StepSucceeded})
	if !ev.Settled || ev.Verdict != VerdictCompleted {
		t.Fatalf("skip_branch terminal = %+v", ev)
	}
}

func TestEvaluateCanceledUpstreamSkipsDownstream(t *testing.T) {
	t.Parallel()
	team := testTeam(t, step("a"), step("b", "a"))
	ev := team.Evaluate(map[string]StepState{"a": StepCanceled})
	if len(ev.Skip) != 1 || ev.Skip[0] != "b" {
		t.Fatalf("canceled upstream must skip downstream: %+v", ev)
	}
}

func TestEvaluateDiamondJoin(t *testing.T) {
	t.Parallel()
	team := testTeam(t, step("root"), step("left", "root"), step("right", "root"), step("join", "left", "right"))

	ev := team.Evaluate(map[string]StepState{"root": StepSucceeded})
	if len(ev.Ready) != 2 || ev.Ready[0] != "left" || ev.Ready[1] != "right" {
		t.Fatalf("parallel fan-out = %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"root": StepSucceeded, "left": StepSucceeded, "right": StepRunning})
	if len(ev.Ready) != 0 {
		t.Fatalf("join must wait for all deps: %+v", ev)
	}

	ev = team.Evaluate(map[string]StepState{"root": StepSucceeded, "left": StepSucceeded, "right": StepSucceeded})
	if len(ev.Ready) != 1 || ev.Ready[0] != "join" {
		t.Fatalf("join ready = %+v", ev)
	}
}
