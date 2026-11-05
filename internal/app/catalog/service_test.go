package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/position"
	"ir-agent-server/internal/domain/team"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

type fakePositions struct {
	mu    sync.Mutex
	items map[string]*position.Position
}

func newFakePositions(codes ...string) *fakePositions {
	store := &fakePositions{items: map[string]*position.Position{}}
	for _, code := range codes {
		contractRef := "custom:" + code
		if position.IsBuiltin(code) {
			contractRef = code // 内置岗位契约引用固定为岗位 Code
		}
		store.items[code] = &position.Position{Code: code, Name: code, ContractRef: contractRef, Enabled: true, Revision: 1}
	}
	return store
}

func (f *fakePositions) GetByCode(_ context.Context, code string) (*position.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.items[code]
	if !ok {
		return nil, position.ErrNotFound
	}
	out := *p
	return &out, nil
}

func (f *fakePositions) List(context.Context, bool) ([]position.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]position.Position, 0, len(f.items))
	for _, p := range f.items {
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakePositions) Upsert(_ context.Context, p *position.Position, ifMatch *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.items[p.Code]; ok {
		if ifMatch != nil && *ifMatch != existing.Revision {
			return 0, position.ErrRevisionConflict
		}
		p.Revision = existing.Revision + 1
	} else {
		if ifMatch != nil {
			return 0, position.ErrNotFound
		}
		p.Revision = 1
	}
	f.items[p.Code] = p
	return p.Revision, nil
}

func (f *fakePositions) SetEnabled(_ context.Context, code string, enabled bool, _ *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.items[code]
	if !ok {
		return 0, position.ErrNotFound
	}
	p.Enabled = enabled
	p.Revision++
	return p.Revision, nil
}

func (f *fakePositions) Delete(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[code]; !ok {
		return position.ErrNotFound
	}
	delete(f.items, code)
	return nil
}

type fakeAgents struct {
	mu    sync.Mutex
	items map[string]*agent.Agent
}

func newFakeAgents() *fakeAgents { return &fakeAgents{items: map[string]*agent.Agent{}} }

func (f *fakeAgents) GetBySlug(_ context.Context, slug string) (*agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.items[slug]
	if !ok {
		return nil, agent.ErrNotFound
	}
	out := *a
	return &out, nil
}

func (f *fakeAgents) List(context.Context, bool) ([]agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.Agent, 0, len(f.items))
	for _, a := range f.items {
		out = append(out, *a)
	}
	return out, nil
}

func (f *fakeAgents) Search(_ context.Context, q agent.ListQuery) ([]agent.Agent, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	out := make([]agent.Agent, 0, len(f.items))
	for _, a := range f.items {
		if q.Query != "" && !strings.Contains(a.Slug, q.Query) && !strings.Contains(a.Name, q.Query) {
			continue
		}
		out = append(out, *a)
	}
	total := int64(len(out))
	if q.Offset < len(out) {
		out = out[q.Offset:]
		if len(out) > limit {
			out = out[:limit]
		}
	} else {
		out = nil
	}
	return out, total, nil
}

func (f *fakeAgents) Upsert(_ context.Context, a *agent.Agent, ifMatch *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.items[a.Slug]; ok {
		if ifMatch != nil && *ifMatch != existing.Revision {
			return 0, agent.ErrRevisionConflict
		}
		a.Revision = existing.Revision + 1
	} else {
		if ifMatch != nil {
			return 0, agent.ErrNotFound
		}
		a.Revision = 1
	}
	f.items[a.Slug] = a
	return a.Revision, nil
}

func (f *fakeAgents) Delete(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[slug]; !ok {
		return agent.ErrNotFound
	}
	delete(f.items, slug)
	return nil
}

func (f *fakeAgents) RegistryVersion(context.Context) (int64, error) { return 0, nil }

type fakeTeams struct {
	mu    sync.Mutex
	items map[string]*team.Team
}

func newFakeTeams() *fakeTeams { return &fakeTeams{items: map[string]*team.Team{}} }

func (f *fakeTeams) Get(_ context.Context, id string) (*team.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.items[id]
	if !ok {
		return nil, team.ErrNotFound
	}
	out := *t
	return &out, nil
}

func (f *fakeTeams) List(context.Context, bool) ([]team.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]team.Team, 0, len(f.items))
	for _, t := range f.items {
		out = append(out, *t)
	}
	return out, nil
}

func (f *fakeTeams) Upsert(_ context.Context, t *team.Team, ifMatch *int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.items[t.ID]; ok {
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
	f.items[t.ID] = t
	return t.Revision, nil
}

func (f *fakeTeams) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return team.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeAudit) Append(_ context.Context, events ...audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
	return nil
}

func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, event := range f.events {
		out = append(out, string(event.Action))
	}
	return out
}

func newTestService() (*Service, *fakePositions, *fakeAgents, *fakeTeams, *fakeAudit) {
	positions := newFakePositions("FUNDAMENTAL_ANALYST", "STRATEGIST")
	agents := newFakeAgents()
	teams := newFakeTeams()
	recorder := &fakeAudit{}
	service := NewService(Deps{Positions: positions, Agents: agents, Teams: teams, Audit: recorder})
	return service, positions, agents, teams, recorder
}

// ---------------------------------------------------------------------------
// 岗位
// ---------------------------------------------------------------------------

func TestPositionLifecycleAndProtections(t *testing.T) {
	t.Parallel()
	service, _, _, _, recorder := newTestService()
	ctx := context.Background()

	// 创建自定义岗位。
	custom := &position.Position{Code: "macro_analyst", Name: "宏观分析师", ContractRef: "custom:macro", Enabled: true}
	revision, err := service.SavePosition(ctx, custom, nil, "admin")
	if err != nil || revision != 1 {
		t.Fatalf("create = %d/%v", revision, err)
	}
	// If-Match 冲突传播。
	stale := int64(99)
	if _, err := service.SavePosition(ctx, custom, &stale, "admin"); err != position.ErrRevisionConflict {
		t.Errorf("stale save = %v", err)
	}
	// 内置岗位由系统 seed：目录里不存在时不接受经 API「创建」。
	emptyService := NewService(Deps{Positions: newFakePositions(), Agents: newFakeAgents(), Teams: newFakeTeams()})
	builtin := &position.Position{Code: position.CodeFundamentalAnalyst, Name: "基本面", ContractRef: position.CodeFundamentalAnalyst}
	if _, err := emptyService.SavePosition(ctx, builtin, nil, "admin"); err == nil || !strings.Contains(err.Error(), "seeded by the system") {
		t.Errorf("builtin create = %v", err)
	}
	// 已 seed 的内置岗位可以更新（If-Match 语义照常）。
	existing, err := service.GetPosition(ctx, position.CodeFundamentalAnalyst)
	if err != nil {
		t.Fatalf("get builtin: %v", err)
	}
	existing.Description = "内置岗位描述微调"
	if _, err := service.SavePosition(ctx, existing, &existing.Revision, "admin"); err != nil {
		t.Errorf("builtin update = %v", err)
	}
	// __CUSTOM__ 保留岗位不可经 API 创建。
	reserved := &position.Position{Code: position.CodeCustom, Name: "x"}
	if _, err := service.SavePosition(ctx, reserved, nil, "admin"); err == nil {
		t.Error("reserved position create must fail")
	}
	// 内置岗位不可删除；自定义可删。
	if err := service.DeletePosition(ctx, position.CodeFundamentalAnalyst, "admin"); err == nil {
		t.Error("builtin delete must fail")
	}
	if err := service.DeletePosition(ctx, "macro_analyst", "admin"); err != nil {
		t.Errorf("custom delete = %v", err)
	}
	actions := recorder.actions()
	wantPrefix := []string{"position.created", "position.deleted"}
	if len(actions) < 2 || actions[0] != wantPrefix[0] || actions[len(actions)-1] != wantPrefix[1] {
		t.Errorf("audit actions = %v", actions)
	}
}

// ---------------------------------------------------------------------------
// 智能体
// ---------------------------------------------------------------------------

func TestAgentStaffingValidation(t *testing.T) {
	t.Parallel()
	service, _, _, _, _ := newTestService()
	ctx := context.Background()

	valid := &agent.Agent{
		Slug: "desk-1", Name: "研究台", Enabled: true,
		Binding:       exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		Positions:     []string{"FUNDAMENTAL_ANALYST"},
		SigningSecret: []byte("0123456789abcdef-secret"),
	}
	if _, err := service.SaveAgent(ctx, valid, nil, "admin"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// 聘任不存在的岗位被拒。
	ghost := &agent.Agent{
		Slug: "desk-2", Name: "幽灵台", Enabled: true,
		Binding:   exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		Positions: []string{"ASTROLOGER"}, SigningSecret: []byte("0123456789abcdef"),
	}
	if _, err := service.SaveAgent(ctx, ghost, nil, "admin"); !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("ghost staffing = %v", err)
	}
	// 更新走 If-Match。
	revision := valid.Revision
	valid.Description = "更新"
	if _, err := service.SaveAgent(ctx, valid, &revision, "admin"); err != nil {
		t.Errorf("if-match update: %v", err)
	}
	stale := revision
	if _, err := service.SaveAgent(ctx, valid, &stale, "admin"); err != agent.ErrRevisionConflict {
		t.Errorf("stale update = %v", err)
	}
	// 岗位反查。
	staffed, err := service.AgentsForPosition(ctx, "fundamental_analyst")
	if err != nil || len(staffed) != 1 || staffed[0].Slug != "desk-1" {
		t.Errorf("staffing lookup = %+v/%v", staffed, err)
	}
}

// ---------------------------------------------------------------------------
// 团队
// ---------------------------------------------------------------------------

func TestTeamTargetValidationAndAudit(t *testing.T) {
	t.Parallel()
	service, _, agents, _, recorder := newTestService()
	ctx := context.Background()
	if _, err := agents.Upsert(ctx, &agent.Agent{
		Slug: "desk-1", Name: "d", Enabled: true,
		Binding:   exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		Positions: []string{"FUNDAMENTAL_ANALYST"}, SigningSecret: []byte("0123456789abcdef"),
	}, nil); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// 步骤目标引用不存在的岗位/智能体被拒。
	badPosition := &team.Team{ID: "t1", Name: "t", Enabled: true, Steps: []team.Step{
		{ID: "a", Target: team.StepTarget{PositionCode: "ASTROLOGER"}},
	}}
	if _, err := service.SaveTeam(ctx, badPosition, nil, "admin"); !strings.Contains(err.Error(), "unknown position") {
		t.Errorf("bad position target = %v", err)
	}
	badAgent := &team.Team{ID: "t1", Name: "t", Enabled: true, Steps: []team.Step{
		{ID: "a", Target: team.StepTarget{AgentSlug: "ghost"}},
	}}
	if _, err := service.SaveTeam(ctx, badAgent, nil, "admin"); !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("bad agent target = %v", err)
	}
	// 环依赖被领域校验拒绝。
	cyclic := &team.Team{ID: "t1", Name: "t", Enabled: true, Steps: []team.Step{
		{ID: "a", Target: team.StepTarget{AgentSlug: "desk-1"}, DependsOn: []string{"b"}},
		{ID: "b", Target: team.StepTarget{AgentSlug: "desk-1"}, DependsOn: []string{"a"}},
	}}
	if _, err := service.SaveTeam(ctx, cyclic, nil, "admin"); err == nil {
		t.Error("cyclic team must be rejected")
	}
	// 合法创建 + 审计动作。
	good := &team.Team{ID: "t1", Name: "研究流水线", Enabled: true, Steps: []team.Step{
		{ID: "a", Target: team.StepTarget{PositionCode: "FUNDAMENTAL_ANALYST"}},
		{ID: "b", Target: team.StepTarget{AgentSlug: "desk-1"}, DependsOn: []string{"a"},
			InputTemplate: json.RawMessage(`{"up":"{{steps.a.output}}"}`)},
	}}
	revision, err := service.SaveTeam(ctx, good, nil, "admin")
	if err != nil || revision != 1 {
		t.Fatalf("create team = %d/%v", revision, err)
	}
	good.Description = "v2"
	if _, err := service.SaveTeam(ctx, good, &revision, "admin"); err != nil {
		t.Errorf("update team: %v", err)
	}
	if err := service.DeleteTeam(ctx, "t1", "admin"); err != nil {
		t.Errorf("delete team: %v", err)
	}
	actions := strings.Join(recorder.actions(), ",")
	for _, want := range []string{"team.created", "team.updated", "team.deleted"} {
		if !strings.Contains(actions, want) {
			t.Errorf("audit missing %s: %s", want, actions)
		}
	}
}
