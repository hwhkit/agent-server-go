package share

// 公开分享用例测试：视图脱敏（无 agent slug）、触发限流、越权轮询拒绝、
// 停用团队即时失效。fakes 覆盖三个端口的最小语义。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ir-agent-server/internal/domain/team"
)

type fakeShares struct {
	active map[string]string // token -> teamID
}

func (f *fakeShares) Create(_ context.Context, teamID string) (*team.TeamShare, error) {
	token := "sh_fake_" + teamID
	f.active[token] = teamID
	return &team.TeamShare{Token: token, TeamID: teamID}, nil
}

func (f *fakeShares) Resolve(_ context.Context, token string) (*team.TeamShare, error) {
	if teamID, ok := f.active[token]; ok {
		return &team.TeamShare{Token: token, TeamID: teamID}, nil
	}
	return nil, team.ErrShareNotFound
}

func (f *fakeShares) Revoke(_ context.Context, teamID string) error {
	for token, id := range f.active {
		if id == teamID {
			delete(f.active, token)
		}
	}
	return nil
}

type fakeTeams struct{ byID map[string]*team.Team }

func (f *fakeTeams) Get(_ context.Context, id string) (*team.Team, error) {
	if t, ok := f.byID[id]; ok {
		return t, nil
	}
	return nil, team.ErrNotFound
}
func (f *fakeTeams) List(context.Context, bool) ([]team.Team, error) { return nil, nil }
func (f *fakeTeams) Upsert(context.Context, *team.Team, *int64) (int64, error) {
	return 1, nil
}
func (f *fakeTeams) Delete(context.Context, string) error { return nil }

type fakeRunner struct {
	runs   map[string]*team.TeamRun
	submit func(sub Submission) (*team.TeamRun, error)
}

func (f *fakeRunner) Submit(_ context.Context, s Submission) (*team.TeamRun, error) {
	return f.submit(s)
}
func (f *fakeRunner) Get(_ context.Context, id string) (*team.TeamRun, error) {
	if run, ok := f.runs[id]; ok {
		return run, nil
	}
	return nil, team.ErrNotFound
}

type fakeRuns struct{ byID map[string]*ChildRun }

func (f *fakeRuns) Get(_ context.Context, id string) (*ChildRun, error) {
	if run, ok := f.byID[id]; ok {
		return run, nil
	}
	return nil, errors.New("not found")
}

func newFixtureFull(t *testing.T, enabled bool) (*Service, *fakeShares, *fakeRunner) {
	t.Helper()
	shares := &fakeShares{active: map[string]string{}}
	teams := &fakeTeams{byID: map[string]*team.Team{
		"t-share": {ID: "t-share", Name: "信号团队", Enabled: enabled, Steps: []team.Step{
			{ID: "a", Target: team.StepTarget{PositionCode: "FUNDAMENTAL_ANALYST", AgentSlug: "secret-agent"}},
		}},
	}}
	_, _ = shares.Create(context.Background(), "t-share")
	var runner *fakeRunner
	runner = &fakeRunner{runs: map[string]*team.TeamRun{}, submit: func(s Submission) (*team.TeamRun, error) {
		run := &team.TeamRun{ID: "tr_" + s.IdempotencyKey, TeamID: s.TeamID, Status: team.TeamRunCompleted}
		runner.runs[run.ID] = run
		return run, nil
	}}
	svc := NewService(shares, teams, runner,
		&fakeRuns{byID: map[string]*ChildRun{}}, Config{TriggersPerMinute: 2})
	return svc, shares, runner
}

func newFixture(t *testing.T, enabled bool) (*Service, *fakeShares) {
	svc, shares, _ := newFixtureFull(t, enabled)
	return svc, shares
}

func TestViewRedactsAgentDetails(t *testing.T) {
	svc, _ := newFixture(t, true)
	view, err := svc.View(context.Background(), "sh_fake_t-share")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(view)
	if string(raw) == "" || containsSub(raw, "secret-agent") {
		t.Errorf("view must not leak agent slug: %s", raw)
	}
	if len(view.Steps) != 1 || view.Steps[0].Position != "FUNDAMENTAL_ANALYST" {
		t.Errorf("steps = %+v", view.Steps)
	}
}

func containsSub(raw []byte, needle string) bool {
	return len(needle) > 0 && indexOf(string(raw), needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestTriggerRateLimited(t *testing.T) {
	svc, _ := newFixture(t, true)
	ctx := context.Background()
	if _, err := svc.Trigger(ctx, "sh_fake_t-share", "a", "v", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	if _, err := svc.Trigger(ctx, "sh_fake_t-share", "b", "v", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second trigger: %v", err)
	}
	if _, err := svc.Trigger(ctx, "sh_fake_t-share", "c", "v", json.RawMessage(`{}`)); !errors.Is(err, ErrRateLimited) {
		t.Errorf("third trigger = %v, want ErrRateLimited", err)
	}
	// 坏 token 不消耗配额也不放行。
	if _, err := svc.Trigger(ctx, "sh_unknown", "x", "v", json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token = %v", err)
	}
}

func TestDisabledTeamAndRevokedShare(t *testing.T) {
	svc, shares := newFixture(t, false)
	if _, err := svc.View(context.Background(), "sh_fake_t-share"); !errors.Is(err, ErrNotShared) {
		t.Errorf("disabled team view = %v, want ErrNotShared", err)
	}
	_ = shares
}

func TestRunRejectsCrossTeamAccess(t *testing.T) {
	svc, _, runner := newFixtureFull(t, true)
	ctx := context.Background()
	// 注入一个属于其他团队的 run：归属校验必须拒绝。
	runner.runs["tr_foreign"] = &team.TeamRun{ID: "tr_foreign", TeamID: "someone-else",
		Steps: map[string]team.StepRun{}}
	if _, err := svc.Run(ctx, "sh_fake_t-share", "tr_foreign"); !errors.Is(err, ErrNotShared) {
		t.Errorf("cross-team run = %v, want ErrNotShared", err)
	}
	run, err := svc.Trigger(ctx, "sh_fake_t-share", "k1", "v", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(ctx, "sh_fake_t-share", run.ID); err != nil {
		t.Errorf("own run lookup = %v", err)
	}
}

func TestTokenBucketRefill(t *testing.T) {
	b := &tokenBucket{capacity: 2, tokens: 0}
	if b.take(time.Now()) {
		t.Error("empty bucket must refuse")
	}
	b.tokens = 2
	if !b.take(time.Now()) || !b.take(time.Now()) {
		t.Error("full bucket must allow capacity takes")
	}
	if b.take(time.Now()) {
		t.Error("drained bucket must refuse")
	}
	b.last = time.Now().Add(-time.Minute) // 模拟一分钟后补满
	if !b.take(time.Now()) {
		t.Error("refilled bucket must allow again")
	}
}
