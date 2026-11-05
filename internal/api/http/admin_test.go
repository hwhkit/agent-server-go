package http_test

// 管理 API v2 端到端集成测试（真 PostgreSQL，门禁同 infra/postgres 测试：
// V2_TEST_DATABASE_URL / TEST_DATABASE_URL）。覆盖：登录/会话/CSRF/角色门禁、
// 岗位/智能体/团队 CRUD 与 If-Match、团队运行提交与推进、dashboard、
// 连接器令牌签发与吊销。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	apihttp "ir-agent-server/internal/api/http"
	"ir-agent-server/internal/app/admin"
	"ir-agent-server/internal/app/agentapi"
	"ir-agent-server/internal/app/catalog"
	"ir-agent-server/internal/app/dashboard"
	"ir-agent-server/internal/app/teamrun"
	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/domain/platform"
	infrabackend "ir-agent-server/internal/infra/backend"
	"ir-agent-server/internal/infra/config"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/postgres"
)

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

type dbAccounts struct{ store *postgres.AccountStore }

func (d dbAccounts) Lookup(ctx context.Context, username string) (*account.Account, error) {
	user, err := d.store.Get(ctx, username)
	if err == account.ErrNotFound {
		return nil, nil
	}
	return user, err
}

type stubTeamDispatcher struct{}

func (stubTeamDispatcher) Execute(context.Context, string, *platform.TaskContext,
	any, time.Duration) (*platform.ProcessedOutput, error) {
	return &platform.ProcessedOutput{Summary: "team-step-done"}, nil
}

type adminFixture struct {
	server *httptest.Server
	client *http.Client
	csrf   string
	suffix string
	base   string
	db     *postgres.DB
	events *postgres.ChannelEventStore
}

func setupAdmin(t *testing.T) *adminFixture {
	t.Helper()
	url := os.Getenv("V2_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no test database configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(db.Close)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	accounts := postgres.NewAccountStore(db)
	adminHash, err := crypto.HashPassword("admin-pass-" + suffix)
	if err != nil {
		t.Fatal(err)
	}
	userHash, err := crypto.HashPassword("user-pass-" + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Create(ctx, &account.Account{
		Username: "admin-" + suffix, Role: account.RoleAdmin,
		PasswordHash: adminHash, Enabled: true,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := accounts.Create(ctx, &account.Account{
		Username: "user-" + suffix, Role: account.RoleUser,
		PasswordHash: userHash, Enabled: true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM admin_sessions WHERE username IN ($1, $2)`,
			"admin-"+suffix, "user-"+suffix)
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM admin_users WHERE username IN ($1, $2)`,
			"admin-"+suffix, "user-"+suffix)
	})

	auth, err := admin.NewAuth(admin.Config{
		DummyPasswordHash: adminHash, SessionTTL: time.Hour,
		Verify: crypto.VerifyPassword,
	}, dbAccounts{store: accounts}, postgres.NewSessionStore(db))
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}

	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatal(err)
	}
	agentStore := postgres.NewAgentStore(db, cipher)
	catalogService := catalog.NewService(catalog.Deps{
		Positions: postgres.NewPositionStore(db),
		Agents:    agentStore,
		Teams:     postgres.NewTeamStore(db),
		Audit:     postgres.NewAuditStore(db),
	})
	teamRunStore := postgres.NewTeamRunStore(db)
	engine := teamrun.NewEngine(teamrun.Deps{
		Teams: postgres.NewTeamStore(db), TeamRuns: teamRunStore,
		AgentRuns: postgres.NewRunStore(db), Agents: agentStore,
		Dispatcher: stubTeamDispatcher{},
	})
	t.Cleanup(engine.Wait)
	dash := dashboard.NewService(dashboard.Deps{
		Version: "test", Health: db, Runs: postgres.NewRunStore(db),
		Queue: postgres.NewTaskQueueStore(db), Workers: postgres.NewWorkerStore(db),
		TeamRuns: teamRunStore,
	})
	connectors := agentapi.NewService(agentapi.Deps{
		Config:  agentapi.Config{LeaseSeconds: 90, RenewInterval: 30 * time.Second, HeartbeatIntervalSeconds: 60, ClaimMaxWait: time.Second, MaxOutstandingLeases: 64, MaxAttempts: 3, QueueTTL: time.Hour},
		Queue:   postgres.NewTaskQueueStore(db),
		Tokens:  postgres.NewTokenStore(db),
		Workers: postgres.NewWorkerStore(db),
	})

	channelEvents := postgres.NewChannelEventStore(db)
	backendStore := infrabackend.NewStore(db.Pool, cipher)
	providerConfig, providerRevision, err := infrabackend.LoadInitial(ctx,
		backendStore, &config.PlatformConfig{})
	if err != nil {
		t.Fatalf("load providers: %v", err)
	}
	providers := infrabackend.NewManager(backendStore, providerConfig, providerRevision, nil)

	router := apihttp.AdminRouter(apihttp.AdminDeps{
		Auth: auth, Catalog: catalogService, Dashboard: dash, Engine: engine,
		Connectors: connectors, Runs: postgres.NewRunStore(db),
		Audits: postgres.NewAuditStore(db), CookieTTL: time.Hour,
		Accounts: admin.NewAccountService(accounts, crypto.HashPassword,
			postgres.NewAuditStore(db)),
		ChannelEvents: channelEvents,
		Providers:     providers,
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	fixture := &adminFixture{server: server, client: client, suffix: suffix,
		base: server.URL + "/api/admin/v2", db: db, events: channelEvents}
	fixture.login(t, "admin-"+suffix, "admin-pass-"+suffix)
	return fixture
}

func (f *adminFixture) login(t *testing.T, username, password string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := f.client.Post(f.base+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var envelope struct {
		Data struct {
			CSRFToken string `json:"csrfToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	f.csrf = envelope.Data.CSRFToken
}

func (f *adminFixture) do(t *testing.T, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, f.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if f.csrf != "" {
		request.Header.Set("X-CSRF-Token", f.csrf)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	resp, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	if resp.StatusCode != http.StatusNoContent {
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
	}
	return resp.StatusCode, decoded
}

func dataOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no data object: %v", envelope)
	}
	return data
}

// ---------------------------------------------------------------------------
// 认证与门禁
// ---------------------------------------------------------------------------

func TestAdminAuthGates(t *testing.T) {
	fixture := setupAdmin(t)

	// 未登录读取 → 401。
	anon := &http.Client{}
	resp, err := anon.Get(fixture.base + "/positions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("anonymous status = %d", resp.StatusCode)
	}

	// 错误口令 → 401。
	body, _ := json.Marshal(map[string]string{"username": "admin-" + fixture.suffix, "password": "wrong"})
	resp, err = anon.Post(fixture.base+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("bad login status = %d", resp.StatusCode)
	}

	// 写操作缺 CSRF → 403。
	saved := fixture.csrf
	fixture.csrf = ""
	status, envelope := fixture.do(t, "POST", "/positions",
		map[string]any{"code": "X_NO_CSRF", "name": "x", "contract_ref": "custom:x"}, nil)
	if status != 403 {
		t.Errorf("no-csrf status = %d %v", status, envelope)
	}
	fixture.csrf = saved

	// 普通用户：读可以，写 403。
	userFixture := &adminFixture{server: fixture.server, suffix: fixture.suffix, base: fixture.base}
	jar, _ := cookiejar.New(nil)
	userFixture.client = &http.Client{Jar: jar}
	userFixture.login(t, "user-"+fixture.suffix, "user-pass-"+fixture.suffix)
	status, _ = userFixture.do(t, "GET", "/positions", nil, nil)
	if status != 200 {
		t.Errorf("user read status = %d", status)
	}
	status, envelope = userFixture.do(t, "POST", "/positions",
		map[string]any{"code": "X_USER_WRITE", "name": "x", "contract_ref": "custom:x"}, nil)
	if status != 403 {
		t.Errorf("user write status = %d %v", status, envelope)
	}
	// 审计日志对普通用户 403。
	status, _ = userFixture.do(t, "GET", "/audits", nil, nil)
	if status != 403 {
		t.Errorf("user audits status = %d", status)
	}
}

// ---------------------------------------------------------------------------
// 目录 CRUD
// ---------------------------------------------------------------------------

func TestAdminCatalogCRUD(t *testing.T) {
	fixture := setupAdmin(t)

	// 内置岗位已 seed。
	status, envelope := fixture.do(t, "GET", "/positions", nil, nil)
	if status != 200 {
		t.Fatalf("list positions = %d", status)
	}
	list := envelope["data"].([]any)
	if len(list) < 7 {
		t.Errorf("seeded positions = %d", len(list))
	}

	// 创建 → If-Match 冲突 → 删除。
	code := "IT_POS_" + strings.ToUpper(fixture.suffix)[:8]
	status, envelope = fixture.do(t, "POST", "/positions",
		map[string]any{"code": code, "name": "集成岗位", "contract_ref": "custom:it", "enabled": true}, nil)
	if status != 200 || dataOf(t, envelope)["revision"] != float64(1) {
		t.Fatalf("create position = %d %v", status, envelope)
	}
	status, envelope = fixture.do(t, "PUT", "/positions/"+code,
		map[string]any{"code": code, "name": "改名", "contract_ref": "custom:it"},
		map[string]string{"If-Match": "99"})
	if status != 409 {
		t.Errorf("stale if-match = %d %v", status, envelope)
	}
	status, _ = fixture.do(t, "PUT", "/positions/"+code,
		map[string]any{"code": code, "name": "改名", "contract_ref": "custom:it"},
		map[string]string{"If-Match": "1"})
	if status != 200 {
		t.Errorf("fresh if-match = %d", status)
	}

	// 智能体：创建（带密钥）→ 视图不回显密钥 → 聘任校验。
	slug := "it-agent-" + fixture.suffix
	status, envelope = fixture.do(t, "POST", "/agents", map[string]any{
		"slug": slug, "name": "集成台", "executor_kind": "pi", "execution_mode": "local",
		"positions": []string{code}, "signing_secret": "0123456789abcdef-it-secret",
		"enabled": true,
	}, nil)
	if status != 200 {
		t.Fatalf("create agent = %d %v", status, envelope)
	}
	status, envelope = fixture.do(t, "GET", "/agents/"+slug, nil, nil)
	view := dataOf(t, envelope)
	if view["signing_secret_configured"] != true {
		t.Errorf("agent view = %v", view)
	}
	if raw, _ := json.Marshal(view); bytes.Contains(raw, []byte("it-secret")) {
		t.Error("agent view must never echo the secret")
	}
	status, envelope = fixture.do(t, "POST", "/agents", map[string]any{
		"slug": "ghost-" + fixture.suffix, "name": "幽灵", "executor_kind": "pi",
		"execution_mode": "local", "positions": []string{"NO_SUCH_POS"},
		"signing_secret": "0123456789abcdef", "enabled": true,
	}, nil)
	if status != 422 {
		t.Errorf("unknown position staffing = %d %v", status, envelope)
	}

	// 岗位被聘任时不可删。
	status, envelope = fixture.do(t, "DELETE", "/positions/"+code, nil, nil)
	if status != 409 {
		t.Errorf("delete staffed position = %d %v", status, envelope)
	}

	// 团队 + 团队运行全链路。
	teamID := "it-team-" + fixture.suffix
	status, envelope = fixture.do(t, "POST", "/teams", map[string]any{
		"id": teamID, "name": "集成流水线", "enabled": true,
		"steps": []map[string]any{
			{"id": "step-a", "target": map[string]string{"agent_slug": slug}},
		},
	}, nil)
	if status != 200 {
		t.Fatalf("create team = %d %v", status, envelope)
	}
	status, envelope = fixture.do(t, "POST", "/team-runs", map[string]any{
		"team_id": teamID, "input": map[string]any{"topic": "it"},
		"idempotency_key": "it-key-" + fixture.suffix,
	}, nil)
	if status != 202 {
		t.Fatalf("submit team run = %d %v", status, envelope)
	}
	runID := dataOf(t, envelope)["id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, envelope = fixture.do(t, "GET", "/team-runs/"+runID, nil, nil)
		if status == 200 && dataOf(t, envelope)["status"] == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data := dataOf(t, envelope)
	if data["status"] != "completed" {
		t.Fatalf("team run did not complete: %v", data)
	}
	// 幂等重放。
	status, replay := fixture.do(t, "POST", "/team-runs", map[string]any{
		"team_id": teamID, "idempotency_key": "it-key-" + fixture.suffix,
	}, nil)
	if status != 202 || dataOf(t, replay)["id"] != runID {
		t.Errorf("replay = %d %v", status, replay)
	}
	// 已终态运行的取消是 no-op。
	if status, _ := fixture.do(t, "POST", "/team-runs/"+runID+"/cancel", nil, nil); status != 204 {
		t.Errorf("cancel completed = %d", status)
	}
	// 子运行父子关联可见。
	status, envelope = fixture.do(t, "GET", "/runs?team_run_id="+runID, nil, nil)
	runsData := dataOf(t, envelope)
	items, _ := runsData["items"].([]any)
	if status != 200 || len(items) != 1 {
		t.Errorf("child runs = %d %v", status, runsData)
	}

	// dashboard。
	status, envelope = fixture.do(t, "GET", "/dashboard", nil, nil)
	snapshot := dataOf(t, envelope)
	if status != 200 || snapshot["databaseOk"] != true || snapshot["version"] != "test" {
		t.Errorf("dashboard = %d %v", status, snapshot)
	}

	// 连接器令牌：签发（明文一次）→ 列表 → 吊销。
	connectorName := "it-conn-" + fixture.suffix
	status, envelope = fixture.do(t, "POST", "/agent-connectors",
		map[string]any{"name": connectorName, "scopes": []string{slug}}, nil)
	if status != 201 {
		t.Fatalf("issue connector = %d %v", status, envelope)
	}
	token, _ := dataOf(t, envelope)["token"].(string)
	if !strings.HasPrefix(token, "act_") {
		t.Errorf("token = %q", token)
	}
	status, envelope = fixture.do(t, "GET", "/agent-connectors", nil, nil)
	found := false
	for _, item := range envelope["data"].([]any) {
		entry := item.(map[string]any)
		if entry["name"] == connectorName {
			found = true
			if raw, _ := json.Marshal(entry); bytes.Contains(raw, []byte(token)) {
				t.Error("connector list must not echo tokens")
			}
		}
	}
	if !found {
		t.Error("issued connector missing from list")
	}
	if status, _ := fixture.do(t, "DELETE", "/agent-connectors/"+connectorName, nil, nil); status != 204 {
		t.Errorf("revoke = %d", status)
	}

	// 审计事件已落库（agent.created 可查）。
	status, envelope = fixture.do(t, "GET", "/audits?action=agent.created&limit=5", nil, nil)
	if status != 200 {
		t.Errorf("audits = %d", status)
	}
	auditData := dataOf(t, envelope)
	if total, _ := auditData["total"].(float64); total < 1 {
		t.Errorf("agent.created audits = %v", auditData)
	}

	// 清理团队与智能体、岗位（顺序：先解聘）。
	if status, _ := fixture.do(t, "DELETE", "/teams/"+teamID, nil, nil); status != 204 {
		t.Errorf("delete team = %d", status)
	}
	if status, _ := fixture.do(t, "DELETE", "/agents/"+slug, nil, nil); status != 204 {
		t.Errorf("delete agent = %d", status)
	}
	if status, _ := fixture.do(t, "DELETE", "/positions/"+code, nil, nil); status != 204 {
		t.Errorf("delete position = %d", status)
	}
}
