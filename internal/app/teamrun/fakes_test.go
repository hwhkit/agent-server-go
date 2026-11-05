package teamrun

// 编排引擎测试假件：四个领域仓储的内存实现（语义与 infra/postgres 对齐：
// SaveSteps 的 CAS 与终态守卫、Create 的幂等键、Append/Complete 的幂等）。

import (
	"context"
	"errors"
	"sync"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
)

// ---------------------------------------------------------------------------
// team.Repository
// ---------------------------------------------------------------------------

type fakeTeamRepo struct {
	mu    sync.Mutex
	teams map[string]*team.Team
}

func newFakeTeamRepo(teams ...*team.Team) *fakeTeamRepo {
	repo := &fakeTeamRepo{teams: map[string]*team.Team{}}
	for _, t := range teams {
		repo.teams[t.ID] = t
	}
	return repo
}

func (f *fakeTeamRepo) Get(_ context.Context, id string) (*team.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.teams[id]; ok {
		copy := *t
		return &copy, nil
	}
	return nil, team.ErrNotFound
}

func (f *fakeTeamRepo) List(context.Context, bool) ([]team.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]team.Team, 0, len(f.teams))
	for _, t := range f.teams {
		out = append(out, *t)
	}
	return out, nil
}

func (f *fakeTeamRepo) Upsert(_ context.Context, t *team.Team, ifMatch *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.teams[t.ID]; ok {
		if ifMatch != nil && *ifMatch != existing.Revision {
			return 0, team.ErrRevisionConflict
		}
		t.Revision = existing.Revision + 1
	} else {
		if ifMatch != nil {
			return 0, team.ErrNotFound
		}
		t.Revision = 1
	}
	f.teams[t.ID] = t
	return t.Revision, nil
}

func (f *fakeTeamRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.teams[id]; !ok {
		return team.ErrNotFound
	}
	delete(f.teams, id)
	return nil
}

// ---------------------------------------------------------------------------
// team.TeamRunRepository
// ---------------------------------------------------------------------------

type fakeTeamRunRepo struct {
	mu       sync.Mutex
	runs     map[string]*team.TeamRun
	byKey    map[string]string
	saveHook func(tr *team.TeamRun) // 观察点
}

func newFakeTeamRunRepo() *fakeTeamRunRepo {
	return &fakeTeamRunRepo{runs: map[string]*team.TeamRun{}, byKey: map[string]string{}}
}

func (f *fakeTeamRunRepo) Create(_ context.Context, tr *team.TeamRun) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.byKey[tr.IdempotencyKey]; exists {
		return false, nil
	}
	stored := *tr
	stored.Steps = cloneSteps(tr.Steps)
	stored.CreatedAt = time.Now().UTC()
	f.runs[tr.ID] = &stored
	f.byKey[tr.IdempotencyKey] = tr.ID
	return true, nil
}

func (f *fakeTeamRunRepo) Get(_ context.Context, id string) (*team.TeamRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.runs[id]
	if !ok {
		return nil, team.ErrNotFound
	}
	out := *tr
	out.Steps = cloneSteps(tr.Steps)
	return &out, nil
}

func (f *fakeTeamRunRepo) GetByIdempotencyKey(_ context.Context, key string) (*team.TeamRun, error) {
	f.mu.Lock()
	id, ok := f.byKey[key]
	f.mu.Unlock()
	if !ok {
		return nil, team.ErrNotFound
	}
	return f.Get(context.Background(), id)
}

func (f *fakeTeamRunRepo) ListActive(_ context.Context, limit int) ([]team.TeamRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]team.TeamRun, 0)
	for _, tr := range f.runs {
		if !tr.Status.Terminal() {
			out = append(out, *tr)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeTeamRunRepo) SaveSteps(_ context.Context, id string, steps map[string]team.StepRun,
	status team.TeamRunStatus, cas map[string]team.StepState) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.runs[id]
	if !ok {
		return false, team.ErrNotFound
	}
	if tr.Status.Terminal() {
		return false, nil
	}
	for stepID, want := range cas {
		if tr.Steps[stepID].State != want {
			return false, nil
		}
	}
	tr.Steps = cloneSteps(steps)
	tr.Status = status
	if status.Terminal() && tr.CompletedAt == nil {
		now := time.Now().UTC()
		tr.CompletedAt = &now
	}
	if f.saveHook != nil {
		f.saveHook(tr)
	}
	return true, nil
}

func (f *fakeTeamRunRepo) List(_ context.Context, teamID string, limit, offset int) ([]team.TeamRun, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]team.TeamRun, 0)
	for _, tr := range f.runs {
		if teamID == "" || tr.TeamID == teamID {
			out = append(out, *tr)
		}
	}
	return out, len(out), nil
}

// ---------------------------------------------------------------------------
// run.Repository
// ---------------------------------------------------------------------------

type fakeRunRepo struct {
	mu   sync.Mutex
	runs map[string]*run.Run
}

func newFakeRunRepo() *fakeRunRepo { return &fakeRunRepo{runs: map[string]*run.Run{}} }

func (f *fakeRunRepo) Append(_ context.Context, r run.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.runs[r.RunID]; exists {
		return nil
	}
	if r.Status == "" {
		r.Status = run.StatusQueued
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	stored := r
	f.runs[r.RunID] = &stored
	return nil
}

func (f *fakeRunRepo) Get(_ context.Context, runID string) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return nil, run.ErrNotFound
	}
	out := *r
	return &out, nil
}

func (f *fakeRunRepo) Complete(_ context.Context, runID string, outcome run.Outcome) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return nil, run.ErrNotFound
	}
	if !r.Status.Terminal() {
		r.Status = outcome.Status
		if len(outcome.Output) > 0 {
			r.Output = outcome.Output
		}
		r.Usage = outcome.Usage
		r.ErrorCode = outcome.ErrorCode
		now := time.Now().UTC()
		r.CompletedAt = &now
	}
	out := *r
	return &out, nil
}

func (f *fakeRunRepo) List(context.Context, run.Filter) ([]run.Run, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]run.Run, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, *r)
	}
	return out, len(out), nil
}

// ---------------------------------------------------------------------------
// agent.Repository
// ---------------------------------------------------------------------------

type fakeAgentRepo struct {
	mu     sync.Mutex
	agents map[string]*agent.Agent
}

func newFakeAgentRepo(agents ...*agent.Agent) *fakeAgentRepo {
	repo := &fakeAgentRepo{agents: map[string]*agent.Agent{}}
	for _, a := range agents {
		repo.agents[a.Slug] = a
	}
	return repo
}

func (f *fakeAgentRepo) GetBySlug(_ context.Context, slug string) (*agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[slug]
	if !ok {
		return nil, agent.ErrNotFound
	}
	out := *a
	return &out, nil
}

func (f *fakeAgentRepo) List(_ context.Context, includeDisabled bool) ([]agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.Agent, 0, len(f.agents))
	for _, a := range f.agents {
		if includeDisabled || a.Enabled {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeAgentRepo) Upsert(_ context.Context, a *agent.Agent, _ *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.Revision++
	f.agents[a.Slug] = a
	return a.Revision, nil
}

func (f *fakeAgentRepo) Delete(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.agents, slug)
	return nil
}

func (f *fakeAgentRepo) RegistryVersion(context.Context) (int64, error) { return 0, nil }

// ---------------------------------------------------------------------------
// Dispatcher
// ---------------------------------------------------------------------------

type fakeDispatcher struct {
	mu       sync.Mutex
	behavior map[string]func(ctx context.Context) (*platform.ProcessedOutput, error)
	calls    []string
	tasks    map[string]*platform.TaskContext
	outputs  map[string]*platform.ProcessedOutput
	errs     map[string]error
	blocked  chan string
	release  chan struct{}
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		behavior: map[string]func(context.Context) (*platform.ProcessedOutput, error){},
		tasks:    map[string]*platform.TaskContext{},
		outputs:  map[string]*platform.ProcessedOutput{},
		errs:     map[string]error{},
		blocked:  make(chan string, 16),
		release:  make(chan struct{}),
	}
}

func (f *fakeDispatcher) on(slug string, output *platform.ProcessedOutput, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[slug] = output
	f.errs[slug] = err
}

func (f *fakeDispatcher) blockOn(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behavior[slug] = func(ctx context.Context) (*platform.ProcessedOutput, error) {
		f.blocked <- slug
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		output, err := f.outputs[slug], f.errs[slug]
		if output == nil && err == nil {
			output = &platform.ProcessedOutput{Summary: "done by " + slug}
		}
		return output, err
	}
}

func (f *fakeDispatcher) Execute(ctx context.Context, agentCode string, task *platform.TaskContext,
	_ any, _ time.Duration) (*platform.ProcessedOutput, error) {
	f.mu.Lock()
	f.calls = append(f.calls, agentCode)
	f.tasks[agentCode] = task
	behavior := f.behavior[agentCode]
	output, err := f.outputs[agentCode], f.errs[agentCode]
	f.mu.Unlock()
	if behavior != nil {
		return behavior(ctx)
	}
	if err != nil {
		return nil, err
	}
	if output == nil {
		output = &platform.ProcessedOutput{Summary: "done by " + agentCode}
	}
	return output, nil
}

func (f *fakeDispatcher) taskFor(slug string) *platform.TaskContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[slug]
}

func (f *fakeDispatcher) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---------------------------------------------------------------------------
// 组装
// ---------------------------------------------------------------------------

type engineFixture struct {
	engine     *Engine
	teams      *fakeTeamRepo
	teamRuns   *fakeTeamRunRepo
	runs       *fakeRunRepo
	agents     *fakeAgentRepo
	dispatcher *fakeDispatcher
}

func testAgent(slug string, positions ...string) *agent.Agent {
	return &agent.Agent{
		Slug: slug, Name: slug, Enabled: true,
		Binding:        exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		Positions:      positions,
		TimeoutSecs:    60,
		MaxConcurrency: 2, QueueCapacity: 2, Revision: 1,
	}
}

func testTeam(id string, steps ...team.Step) *team.Team {
	t := &team.Team{ID: id, Name: id, Enabled: true, Steps: steps}
	if err := t.Validate(); err != nil {
		panic(err)
	}
	return t
}

func newEngineFixture(teams *fakeTeamRepo, agents *fakeAgentRepo) *engineFixture {
	fixture := &engineFixture{
		teams: teams, teamRuns: newFakeTeamRunRepo(),
		runs: newFakeRunRepo(), agents: agents,
		dispatcher: newFakeDispatcher(),
	}
	fixture.engine = NewEngine(Deps{
		Teams: teams, TeamRuns: fixture.teamRuns, AgentRuns: fixture.runs,
		Agents: agents, Dispatcher: fixture.dispatcher,
		ReconcileInterval: time.Hour,
	})
	return fixture
}

var errFake = errors.New("fake failure")
