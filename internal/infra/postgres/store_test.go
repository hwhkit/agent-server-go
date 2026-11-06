package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/position"
	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/postgres"
)

// 集成测试门禁：TEST_DATABASE_URL 未设置时跳过（与既有约定一致）。
// 推荐指向专用测试库，例如：
//
//	createdb ir_v2_test
//	TEST_DATABASE_URL='postgres://postgres:mota@127.0.0.1:5432/ir_v2_test' go test ./internal/infra/postgres
func setupDB(t *testing.T) *postgres.DB {
	t.Helper()
	// 优先使用 V2_TEST_DATABASE_URL：新栈测试与旧包（ir/）测试各自独占一个库，
	// 避免 go test 并行跑包时双方在 agent_pull_* 表上互相干扰。
	url := os.Getenv("V2_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("V2_TEST_DATABASE_URL / TEST_DATABASE_URL not set; skipping postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestMigrateAndSeedPositions(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	// Migrate/Seed 幂等：再跑一次不报错、不重复。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if err := db.Seed(ctx); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	store := postgres.NewPositionStore(db)
	all, err := store.List(ctx, true)
	if err != nil {
		t.Fatalf("list positions: %v", err)
	}
	byCode := map[string]position.Position{}
	for _, p := range all {
		byCode[p.Code] = p
	}
	for _, code := range append(position.BuiltinCodes, position.CodeCustom) {
		if _, ok := byCode[code]; !ok {
			t.Errorf("seeded position %s missing", code)
		}
	}
	if byCode[position.CodeFundamentalAnalyst].Name != "基本面分析师" {
		t.Errorf("builtin name = %q", byCode[position.CodeFundamentalAnalyst].Name)
	}
	if all[len(all)-1].Code != position.CodeCustom {
		t.Errorf("__CUSTOM__ must sort last, got %q", all[len(all)-1].Code)
	}
}

func TestPositionStoreCRUD(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store := postgres.NewPositionStore(db)

	p := &position.Position{
		Code: "TEST_POSITION_CRUD", Name: "测试岗位",
		Description: "集成测试", ContractRef: "custom:test-crud", Enabled: true,
	}
	rev, err := store.Upsert(ctx, p, nil)
	if err != nil || rev != 1 {
		t.Fatalf("create: rev=%d err=%v", rev, err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), p.Code) })

	p.Name = "测试岗位 v2"
	rev2, err := store.Upsert(ctx, p, &rev)
	if err != nil || rev2 != 2 {
		t.Fatalf("update with If-Match: rev=%d err=%v", rev2, err)
	}
	stale := rev
	if _, err := store.Upsert(ctx, p, &stale); !errors.Is(err, position.ErrRevisionConflict) {
		t.Errorf("stale If-Match = %v, want ErrRevisionConflict", err)
	}
	if _, err := store.Upsert(ctx, &position.Position{Code: "NO_SUCH_POS_X", Name: "n", ContractRef: "custom:x"}, &stale); !errors.Is(err, position.ErrNotFound) {
		t.Errorf("If-Match on missing = %v, want ErrNotFound", err)
	}

	got, err := store.GetByCode(ctx, "TEST_POSITION_CRUD")
	if err != nil || got.Name != "测试岗位 v2" || got.Revision != 2 {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := store.GetByCode(ctx, "NO_SUCH_POS_X"); !errors.Is(err, position.ErrNotFound) {
		t.Errorf("get missing = %v", err)
	}

	// __CUSTOM__ 保留岗位：拒绝删除。
	if err := store.Delete(ctx, position.CodeCustom); !errors.Is(err, position.ErrReserved) {
		t.Errorf("delete __CUSTOM__ = %v, want ErrReserved", err)
	}
	if err := store.Delete(ctx, p.Code); err != nil {
		t.Errorf("delete unstaffed position: %v", err)
	}
}

func newTestAgent(slug string) *agent.Agent {
	secret := []byte("0123456789abcdef-secret")
	return &agent.Agent{
		Slug: slug, Name: "测试智能体", Binding: exec.Binding{Kind: "codex", Mode: exec.ModeLocal},
		Positions: []string{"FUNDAMENTAL_ANALYST"}, SigningSecret: secret, Enabled: true,
	}
}

func TestAgentStoreRoundTripAndCAS(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store := postgres.NewAgentStore(db, cipher)

	a := newTestAgent("it-agent-roundtrip")
	rev, err := store.Upsert(ctx, a, nil)
	if err != nil || rev != 1 {
		t.Fatalf("create agent: rev=%d err=%v", rev, err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), a.Slug) })

	got, err := store.GetBySlug(ctx, "it-agent-roundtrip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.SigningSecret) != "0123456789abcdef-secret" {
		t.Error("signing secret must round-trip through AES-GCM")
	}
	if len(got.Positions) != 1 || got.Positions[0] != "FUNDAMENTAL_ANALYST" {
		t.Errorf("staffing = %v", got.Positions)
	}
	if got.Binding.Kind != "codex" || got.Binding.Mode != exec.ModeLocal {
		t.Errorf("binding = %+v", got.Binding)
	}
	if got.TimeoutSecs != agent.DefaultTimeoutSecs || got.MaxConcurrency != 1 {
		t.Errorf("defaults = %d/%d", got.TimeoutSecs, got.MaxConcurrency)
	}

	// If-Match CAS。
	got.Description = "更新描述"
	got.SigningSecret = nil // 更新不带密钥 = 保留既有密文
	rev2, err := store.Upsert(ctx, got, &rev)
	if err != nil || rev2 != 2 {
		t.Fatalf("update: rev=%d err=%v", rev2, err)
	}
	again, err := store.GetBySlug(ctx, got.Slug)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if string(again.SigningSecret) != "0123456789abcdef-secret" {
		t.Error("secret must be preserved when update omits it")
	}
	stale := rev
	if _, err := store.Upsert(ctx, again, &stale); !errors.Is(err, agent.ErrRevisionConflict) {
		t.Errorf("stale If-Match = %v", err)
	}

	// 注册表版本递增。
	v1, err := store.RegistryVersion(ctx)
	if err != nil {
		t.Fatalf("registry version: %v", err)
	}
	if _, err := store.Upsert(ctx, again, &rev2); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	v2, _ := store.RegistryVersion(ctx)
	if v2 <= v1 {
		t.Errorf("registry revision must increase: %d -> %d", v1, v2)
	}

	if err := store.Delete(ctx, "it-agent-roundtrip"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := store.GetBySlug(ctx, "it-agent-roundtrip"); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("get after delete = %v", err)
	}
}

func TestPositionDeleteBlockedWhileStaffed(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	positions := postgres.NewPositionStore(db)
	cipher, _ := crypto.NewSecretCipher("it-shared-master")
	agents := postgres.NewAgentStore(db, cipher)

	p := &position.Position{Code: "TEST_STAFFED_POS", Name: "被聘任岗位", ContractRef: "custom:staffed", Enabled: true}
	if _, err := positions.Upsert(ctx, p, nil); err != nil {
		t.Fatalf("create position: %v", err)
	}
	t.Cleanup(func() { _ = positions.Delete(context.Background(), p.Code) })

	a := newTestAgent("it-agent-staffed")
	a.Positions = []string{"TEST_STAFFED_POS"}
	if _, err := agents.Upsert(ctx, a, nil); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = agents.Delete(context.Background(), a.Slug) })

	if err := positions.Delete(ctx, "TEST_STAFFED_POS"); !errors.Is(err, position.ErrInUse) {
		t.Errorf("delete staffed position = %v, want ErrInUse", err)
	}
}

func TestTeamAndTeamRunLifecycle(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	teams := postgres.NewTeamStore(db)
	runs := postgres.NewTeamRunStore(db)

	dag := &team.Team{
		ID: "it-team", Name: "研究流水线", Enabled: true,
		Steps: []team.Step{
			{ID: "fundamental", Target: team.StepTarget{PositionCode: "FUNDAMENTAL_ANALYST"}},
			{ID: "synthesize", Target: team.StepTarget{PositionCode: "STRATEGIST"}, DependsOn: []string{"fundamental"}},
		},
	}
	rev, err := teams.Upsert(ctx, dag, nil)
	if err != nil || rev != 1 {
		t.Fatalf("create team: rev=%d err=%v", rev, err)
	}
	t.Cleanup(func() { _ = teams.Delete(context.Background(), "it-team") })

	got, err := teams.Get(ctx, "it-team")
	if err != nil {
		t.Fatalf("get team: %v", err)
	}
	if len(got.Steps) != 2 || got.Steps[1].DependsOn[0] != "fundamental" {
		t.Errorf("steps round-trip = %+v", got.Steps)
	}
	if got.MaxConcurrentRuns != team.DefaultMaxConcurrentRuns {
		t.Errorf("max concurrent default = %d", got.MaxConcurrentRuns)
	}

	// 环依赖被领域校验拒绝，不落库。
	cyclic := &team.Team{ID: "it-team-cycle", Name: "环", Steps: []team.Step{
		{ID: "a", Target: team.StepTarget{AgentSlug: "x"}, DependsOn: []string{"b"}},
		{ID: "b", Target: team.StepTarget{AgentSlug: "x"}, DependsOn: []string{"a"}},
	}}
	if _, err := teams.Upsert(ctx, cyclic, nil); err == nil {
		t.Error("cyclic team must be rejected")
	}

	// TeamRun 幂等提交（ID/键带运行后缀，保证共享测试库上可重复执行）。
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	runID := "it-teamrun-" + suffix
	tr := &team.TeamRun{
		ID: runID, TeamID: "it-team", TeamRevision: rev, Snapshot: *got,
		IdempotencyKey: "it-submit-key-" + suffix, Status: team.TeamRunQueued,
		Steps: map[string]team.StepRun{
			"fundamental": {StepID: "fundamental", State: team.StepPending},
			"synthesize":  {StepID: "synthesize", State: team.StepPending},
		},
		Input: json.RawMessage(`{"topic":"test"}`),
	}
	created, err := runs.Create(ctx, tr)
	if err != nil || !created {
		t.Fatalf("create team run: created=%v err=%v", created, err)
	}
	again, err := runs.Create(ctx, tr)
	if err != nil || again {
		t.Fatalf("replay must not create: created=%v err=%v", again, err)
	}
	byKey, err := runs.GetByIdempotencyKey(ctx, tr.IdempotencyKey)
	if err != nil || byKey.ID != runID {
		t.Fatalf("get by key: %+v err=%v", byKey, err)
	}

	active, err := runs.ListActive(ctx, 100)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	found := false
	for _, item := range active {
		if item.ID == runID {
			found = true
		}
	}
	if !found {
		t.Error("queued team run must appear in ListActive")
	}

	// CAS 推进：期望状态匹配则成功。
	updated := map[string]team.StepRun{
		"fundamental": {StepID: "fundamental", State: team.StepRunning, Attempts: 1, ChildRunID: "run-child-1"},
		"synthesize":  {StepID: "synthesize", State: team.StepPending},
	}
	ok, err := runs.SaveSteps(ctx, runID, updated, team.TeamRunRunning,
		map[string]team.StepState{"fundamental": team.StepPending})
	if err != nil || !ok {
		t.Fatalf("cas advance: ok=%v err=%v", ok, err)
	}
	// 过期期望状态 → CAS 拒绝（fundamental 已是 running，期望仍是 pending）。
	ok, err = runs.SaveSteps(ctx, runID, updated, team.TeamRunRunning,
		map[string]team.StepState{"fundamental": team.StepPending})
	if err != nil {
		t.Fatalf("stale cas err: %v", err)
	}
	if ok {
		t.Error("stale CAS expectation must not apply")
	}
	// 终态落定后拒绝再写。
	final := map[string]team.StepRun{
		"fundamental": {StepID: "fundamental", State: team.StepSucceeded},
		"synthesize":  {StepID: "synthesize", State: team.StepCanceled},
	}
	ok, err = runs.SaveSteps(ctx, runID, final, team.TeamRunCanceled,
		map[string]team.StepState{"fundamental": team.StepRunning, "synthesize": team.StepPending})
	if err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	ok, err = runs.SaveSteps(ctx, runID, final, team.TeamRunCompleted, nil)
	if err != nil {
		t.Fatalf("post-terminal write err: %v", err)
	}
	if ok {
		t.Error("terminal team run must reject further writes")
	}
}

func TestRunStoreAppendCompleteIdempotent(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store := postgres.NewRunStore(db)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	slug := "it-agent-" + suffix
	r := run.Run{
		RunID: "it-run-" + suffix, AgentSlug: slug, AgentRevision: 3,
		ExecutorKind: "codex", ExecutionMode: "local", PositionCode: "FUNDAMENTAL_ANALYST",
		Status: run.StatusQueued, Attempt: 1, Input: json.RawMessage(`{"taskBrief":{}}`),
	}
	if err := store.Append(ctx, r); err != nil {
		t.Fatalf("append: %v", err)
	}
	// 幂等重放不覆盖。
	r.Status = run.StatusCompleted
	if err := store.Append(ctx, r); err != nil {
		t.Fatalf("replay append: %v", err)
	}
	got, err := store.Get(ctx, r.RunID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != run.StatusQueued {
		t.Errorf("replay must not overwrite: status=%s", got.Status)
	}

	usage := exec.Usage{InputTokens: 100, OutputTokens: 50}
	duration := int64(1234)
	completed, err := store.Complete(ctx, r.RunID, run.Outcome{
		Status: run.StatusCompleted, Output: json.RawMessage(`{"summary":"ok"}`),
		Usage: &usage, Duration: &duration,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Status != run.StatusCompleted || completed.Usage.TotalTokens != 150 {
		t.Errorf("completed = %+v usage=%+v", completed.Status, completed.Usage)
	}
	// 迟到的第二次 complete 不覆盖（幂等）。
	late, err := store.Complete(ctx, r.RunID, run.Outcome{Status: run.StatusFailed, ErrorCode: "late"})
	if err != nil {
		t.Fatalf("late complete: %v", err)
	}
	if late.Status != run.StatusCompleted || late.ErrorCode == "late" {
		t.Errorf("terminal run must not be overwritten: %+v", late)
	}

	// 团队子运行 + 过滤。
	teamRunID := "it-teamrun-" + suffix
	teamRun := run.Run{
		RunID: "it-run-team-" + suffix, AgentSlug: slug, AgentRevision: 1,
		Status: run.StatusQueued, TeamRunID: teamRunID, TeamStepID: "fundamental",
	}
	if err := store.Append(ctx, teamRun); err != nil {
		t.Fatalf("append team child: %v", err)
	}
	single, total, err := store.List(ctx, run.Filter{AgentSlug: slug, SingleRunsOnly: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(single) != 1 || single[0].RunID != r.RunID {
		t.Errorf("single-runs filter = %d/%d", total, len(single))
	}
	children, _, err := store.List(ctx, run.Filter{TeamRunID: teamRunID})
	if err != nil || len(children) != 1 || children[0].TeamStepID != "fundamental" {
		t.Errorf("team filter = %+v err=%v", children, err)
	}
}

func TestAccountLastAdminInvariantAndSessions(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	accounts := postgres.NewAccountStore(db)
	sessions := postgres.NewSessionStore(db)

	hash, err := crypto.HashPassword("it-password-123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	adminAcct := &account.Account{Username: "it_admin_sole", Role: account.RoleAdmin, PasswordHash: hash, Enabled: true}
	if err := accounts.Create(ctx, adminAcct); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// 测试清理走直连 SQL：仓储的「最后一个 admin」不变量会拒绝删除测试账号，
	// 不清理会让共享测试库上的重复运行撞用户名冲突。
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM admin_sessions WHERE username IN ('it_admin_sole','it_user_1')`)
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM admin_users WHERE username IN ('it_admin_sole','it_user_1')`)
	})
	userAcct := &account.Account{Username: "it_user_1", Role: account.RoleUser, PasswordHash: hash, Enabled: true}
	if err := accounts.Create(ctx, userAcct); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = accounts.Delete(context.Background(), "it_user_1") })

	// 注意：库中可能还有其他启用 admin（共享测试库），因此先禁用其他情况不在
	// 本测试范围；这里验证「唯一 admin」路径需要独占库。用角色降级 + 禁用组合验证不变量触发即可：
	// 当 it_admin_sole 是仅有 admin 时，Update 降级/禁用与 Delete 都必须被拒。
	others, err := accounts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	soleAdmin := true
	for _, a := range others {
		if a.Username != "it_admin_sole" && a.Role == account.RoleAdmin && a.Enabled {
			soleAdmin = false
		}
	}
	if soleAdmin {
		demote := *adminAcct
		demote.Role = account.RoleUser
		if err := accounts.Update(ctx, &demote); !errors.Is(err, account.ErrLastAdmin) {
			t.Errorf("demote sole admin = %v, want ErrLastAdmin", err)
		}
		if err := accounts.Delete(ctx, "it_admin_sole"); !errors.Is(err, account.ErrLastAdmin) {
			t.Errorf("delete sole admin = %v, want ErrLastAdmin", err)
		}
	}

	// 会话：Put/Get（联查角色）、过期即无效、DeleteExpired 清理。
	session := account.Session{
		TokenHash: crypto.TokenHash("it-session-token"), CSRFToken: "csrf-1",
		Username: "it_user_1", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := sessions.Put(ctx, session); err != nil {
		t.Fatalf("put session: %v", err)
	}
	got, err := sessions.Get(ctx, session.TokenHash)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Role != account.RoleUser || got.CSRFToken != "csrf-1" {
		t.Errorf("session = %+v", got)
	}
	expired := account.Session{
		TokenHash: crypto.TokenHash("it-session-expired"), CSRFToken: "csrf-2",
		Username: "it_user_1", ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := sessions.Put(ctx, expired); err != nil {
		t.Fatalf("put expired: %v", err)
	}
	if _, err := sessions.Get(ctx, expired.TokenHash); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("expired session get = %v, want ErrNotFound", err)
	}
	deleted, err := sessions.DeleteExpired(ctx, time.Now())
	if err != nil || deleted < 1 {
		t.Errorf("delete expired = %d, %v", deleted, err)
	}
	if err := sessions.Delete(ctx, session.TokenHash); err != nil {
		t.Errorf("delete session: %v", err)
	}
}

func TestAuditStoreAppendListAuthorize(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	store := postgres.NewAuditStore(db)

	events := []audit.Event{
		{Actor: "it_actor", Action: "agent.created", Target: "it-agent", Detail: json.RawMessage(`{"slug":"it-agent"}`)},
		{Actor: "it_other", Action: "team.created", Target: "it-team"},
	}
	if err := store.Append(ctx, events...); err != nil {
		t.Fatalf("append: %v", err)
	}
	mine, total, err := store.List(ctx, audit.Query{Actor: "it_actor"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total < 1 || mine[0].Action != "agent.created" {
		t.Errorf("list mine = %d %+v", total, mine)
	}
	if mine[0].CreatedAt.IsZero() || mine[0].CreatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("created_at = %v", mine[0].CreatedAt)
	}

	// 权限收口：user 角色查询被强制限定为本人。
	q := audit.Query{}
	if err := q.Authorize(account.RoleUser, "it_actor"); err != nil {
		t.Fatalf("authorize user: %v", err)
	}
	if q.Actor != "it_actor" {
		t.Errorf("user query must be scoped to self, got %q", q.Actor)
	}
	adminQ := audit.Query{Actor: "someone-else"}
	if err := adminQ.Authorize(account.RoleAdmin, "root"); err != nil || adminQ.Actor != "someone-else" {
		t.Errorf("admin query must stay unscoped: %q %v", adminQ.Actor, err)
	}
	badRoleQ := audit.Query{}
	if err := badRoleQ.Authorize(account.Role("ghost"), "x"); !errors.Is(err, audit.ErrPermissionDenied) {
		t.Errorf("unknown role = %v, want ErrPermissionDenied", err)
	}
}

func TestConnectorTokensAndWorkers(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	tokens := postgres.NewTokenStore(db)
	workers := postgres.NewWorkerStore(db)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	plain := "act_" + crypto.RandomToken(24)
	token := &connector.Token{
		Name:   "it-connector-" + suffix,
		Hash:   crypto.TokenHash(plain),
		Scopes: []string{"it-agent-a", "it-agent-b"},
	}
	if err := tokens.Issue(ctx, token); err != nil {
		t.Fatalf("issue token: %v", err)
	}
	got, err := tokens.GetByHash(ctx, crypto.TokenHash(plain))
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if !got.Active(time.Now()) || len(got.Scopes) != 2 || !got.InScope("it-agent-a") || got.InScope("it-agent-z") {
		t.Errorf("token = %+v", got)
	}
	// 同名再签发 = 轮换：旧哈希立即失效。
	rotated := &connector.Token{Name: token.Name, Hash: crypto.TokenHash(plain + "rotated"), Scopes: []string{"it-agent-a"}}
	if err := tokens.Issue(ctx, rotated); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := tokens.GetByHash(ctx, crypto.TokenHash(plain)); !errors.Is(err, connector.ErrTokenNotFound) {
		t.Errorf("old hash after rotation = %v, want ErrTokenNotFound", err)
	}
	// 吊销后立即失效（无宽限期）。轮换已把行哈希换成新值，按新哈希回查。
	if err := tokens.Revoke(ctx, got.ID, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revoked, err := tokens.GetByHash(ctx, crypto.TokenHash(plain+"rotated"))
	if err != nil {
		t.Fatalf("get rotated hash after revoke: %v", err)
	}
	if revoked.Active(time.Now()) {
		t.Error("revoked token must be inactive")
	}
	// 宽限期内仍有效。
	grace := time.Now().Add(time.Hour)
	token2 := &connector.Token{Name: "it-connector2-" + suffix, Hash: crypto.TokenHash("act_second_" + suffix)}
	if err := tokens.Issue(ctx, token2); err != nil {
		t.Fatalf("issue token2: %v", err)
	}
	if err := tokens.Revoke(ctx, token2.ID, &grace); err != nil {
		t.Fatalf("revoke with grace: %v", err)
	}
	graced, _ := tokens.GetByHash(ctx, token2.Hash)
	if !graced.Active(time.Now()) {
		t.Error("token within grace window must stay active")
	}
	if !graced.InScope("anything") {
		t.Error("empty scopes must mean unrestricted")
	}

	// worker 心跳与离线标记。
	w := connector.Worker{
		WorkerID: "it-worker-1", TokenID: got.ID, Slugs: []string{"it-agent-a"},
		Version: "1.0.0", InFlight: 1, Capacity: 4,
	}
	if err := workers.Beat(ctx, w); err != nil {
		t.Fatalf("beat: %v", err)
	}
	online, err := workers.ListOnline(ctx)
	if err != nil {
		t.Fatalf("list online: %v", err)
	}
	found := false
	for _, item := range online {
		if item.WorkerID == "it-worker-1" {
			found = true
			if item.InFlight != 1 || item.Capacity != 4 || len(item.Slugs) != 1 {
				t.Errorf("worker = %+v", item)
			}
			if item.Offline(time.Now()) {
				t.Error("fresh heartbeat must be online")
			}
		}
	}
	if !found {
		t.Fatal("worker missing from online list")
	}
	stale, err := workers.MarkOfflineStale(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if stale < 1 {
		t.Errorf("stale marked = %d", stale)
	}
	after, _ := workers.ListOnline(ctx)
	for _, item := range after {
		if item.WorkerID == "it-worker-1" {
			t.Error("worker must be offline after stale sweep")
		}
	}
}

// TestAgentStoreSearch 覆盖千级规模列表路径：分页/搜索/总数窗口列、
// 列表不解密（SigningSecret 为空、SecretConfigured 标记）。
func TestAgentStoreSearch(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store := postgres.NewAgentStore(db, cipher)

	slugs := []string{"it-search-alpha", "it-search-beta", "other-suffix"}
	for _, slug := range slugs {
		a := newTestAgent(slug)
		if slug == "it-search-beta" {
			a.Name = "贝塔搜索体"
		}
		if _, err := store.Upsert(ctx, a, nil); err != nil {
			t.Fatalf("upsert %s: %v", slug, err)
		}
		t.Cleanup(func() { _ = store.Delete(context.Background(), slug) })
	}

	// 全量分页：total 覆盖窗口总数；列表不解密。
	items, total, err := store.Search(ctx, agent.ListQuery{Limit: 2, Offset: 0,
		IncludeDisabled: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total < 3 {
		t.Errorf("total = %d (window count must cover all matches)", total)
	}
	if len(items) != 2 {
		t.Errorf("page size = %d", len(items))
	}
	for _, item := range items {
		if len(item.SigningSecret) != 0 {
			t.Errorf("list must not carry decrypted secret for %s", item.Slug)
		}
		if !item.SecretConfigured {
			t.Errorf("secret flag missing for %s", item.Slug)
		}
	}

	// slug 前缀搜索。
	_, total, err = store.Search(ctx, agent.ListQuery{Query: "it-search",
		IncludeDisabled: true})
	if err != nil || total != 2 {
		t.Errorf("slug query total = %d err = %v", total, err)
	}

	// 名称模糊搜索（中文 ILIKE）。
	items, total, err = store.Search(ctx, agent.ListQuery{Query: "贝塔",
		IncludeDisabled: true})
	if err != nil || total != 1 || len(items) != 1 || items[0].Slug != "it-search-beta" {
		t.Errorf("name query = (%d, %v) items = %v", total, err, items)
	}

	// 偏移页与 empty 页。
	items, total, err = store.Search(ctx, agent.ListQuery{Query: "it-search",
		IncludeDisabled: true, Limit: 2, Offset: 2})
	if err != nil || len(items) != 0 || total != 2 {
		t.Errorf("offset page = (%d items, total %d, err %v)", len(items), total, err)
	}

	// 执行路径不受影响：GetBySlug 仍解密。
	got, err := store.GetBySlug(ctx, "it-search-alpha")
	if err != nil || string(got.SigningSecret) != "0123456789abcdef-secret" {
		t.Errorf("get-by-slug decrypt = (%q, %v)", got.SigningSecret, err)
	}
}
