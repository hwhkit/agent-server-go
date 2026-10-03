package mota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 端到端脚手架
// ---------------------------------------------------------------------------

const adminTestPassword = "correct horse battery staple"

// adminFixture 是一套带真实 PG 存储的管理台测试环境。
type adminFixture struct {
	state   *PlatformState
	handler *AdminHandler
	// session 是已登录会话(newAdminFixture 会自动登录一次)。
	session *IssuedSession
}

func TestDecodeAdminJSONRejectsUnknownTrailingAndOversizedInput(t *testing.T) {
	for _, body := range []string{
		`{"username":"ok","unknown":true}`,
		`{"username":"ok"} {"username":"second"}`,
		`{"username":"` + strings.Repeat("x", 100) + `"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var payload struct {
			Username string `json:"username"`
		}
		if err := decodeAdminJSON(request, 64, &payload); err == nil {
			t.Errorf("body %q must be rejected", body)
		}
	}
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	requireTestDatabase(t)

	cfg := testPlatformConfig(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg.WorkspaceRoot = root
	cfg.AdminUsername = "admin"
	cfg.AdminPassword = adminTestPassword

	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })

	fixture := &adminFixture{state: state, handler: &AdminHandler{State: state}}
	fixture.session = fixture.mustLogin(t)
	return fixture
}

// call 发一次管理台请求。带 session 时自动附上 Cookie 与 CSRF 头。
func (f *adminFixture) call(
	t *testing.T,
	method, target string,
	body string,
	authenticated bool,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if authenticated && f.session != nil {
		request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: f.session.Token})
		request.Header.Set("X-CSRF-Token", f.session.CSRFToken)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// callWithoutCSRF 带会话 Cookie 但不带 CSRF 头。
func (f *adminFixture) callWithoutCSRF(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: f.session.Token})
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *adminFixture) callAs(t *testing.T, session *IssuedSession, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: session.Token})
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *adminFixture) loginAs(t *testing.T, username, password string) *IssuedSession {
	t.Helper()
	body := jsonString(map[string]any{"username": username, "password": password})
	recorder := f.call(t, http.MethodPost, "/api/admin/login", body, false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login %s status=%d body=%s", username, recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Data sessionData `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	token := ""
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("login did not set session cookie")
	}
	return &IssuedSession{Token: token, Username: decoded.Data.Username, Role: decoded.Data.Role, CSRFToken: decoded.Data.CSRFToken, ExpiresAt: decoded.Data.ExpiresAt}
}

// mustLogin 走 HTTP 登录并解出会话。
func (f *adminFixture) mustLogin(t *testing.T) *IssuedSession {
	t.Helper()
	body := `{"username":"admin","password":` + jsonString(adminTestPassword) + `}`
	recorder := f.call(t, http.MethodPost, "/api/admin/login", body, false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Data sessionData `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("login response is not JSON: %v", err)
	}
	token := ""
	for _, raw := range recorder.Result().Cookies() {
		if raw.Name == SessionCookieName {
			token = raw.Value
		}
	}
	if token == "" {
		t.Fatalf("login did not set %s", SessionCookieName)
	}
	return &IssuedSession{
		Token:     token,
		Username:  decoded.Data.Username,
		Role:      decoded.Data.Role,
		CSRFToken: decoded.Data.CSRFToken,
		ExpiresAt: decoded.Data.ExpiresAt,
	}
}

// wantAdminError 断言错误信封的状态与码,返回 message。
func wantAdminError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, status, recorder.Body.String())
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not an error envelope: %v (body: %s)", err, recorder.Body.String())
	}
	if decoded.Error.Code != code {
		t.Fatalf("code = %q, want %q (body: %s)", decoded.Error.Code, code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != adminNoStore {
		t.Errorf("Cache-Control = %q, want %q", got, adminNoStore)
	}
	return decoded.Error.Message
}

// adminData 解出 {"data": ...} 里的原始 JSON。
func adminData(t *testing.T, recorder *httptest.ResponseRecorder, status int) json.RawMessage {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, status, recorder.Body.String())
	}
	var decoded struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not a data envelope: %v (body: %s)", err, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != adminNoStore {
		t.Errorf("Cache-Control = %q, want %q", got, adminNoStore)
	}
	return decoded.Data
}

// agentPayload 生成一份合法的 Agent 配置请求体。
func agentPayload(slug string, overrides map[string]any) string {
	payload := map[string]any{
		"slug": slug, "name": "admin test agent",
		"executor_kind": "rust", "execution_mode": "local",
		"signing_secret":     "admin-test-secret-0123456789",
		"signature_encoding": "hex", "workspace_path": "agents",
		"permission_mode": "dontAsk", "timeout_secs": 30,
		"max_concurrency": 2, "queue_capacity": 4,
	}
	for key, value := range overrides {
		if value == nil {
			delete(payload, key)
			continue
		}
		payload[key] = value
	}
	return jsonString(payload)
}

// ---------------------------------------------------------------------------
// /admin 页面
// ---------------------------------------------------------------------------

func TestAdminPageSecurityHeaders(t *testing.T) {
	fixture := newAdminFixture(t)
	for _, path := range []string{"/admin", "/admin/"} {
		recorder := fixture.call(t, http.MethodGet, path, "", false)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, recorder.Code)
		}
		headers := map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           adminNoStore,
			"Content-Security-Policy": adminContentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
		}
		for name, want := range headers {
			if got := recorder.Header().Get(name); got != want {
				t.Errorf("%s %s = %q, want %q", path, name, got, want)
			}
		}
		// 页面必须是与 rs 同一份 web/admin.html。
		for _, marker := range []string{
			`id="agent-execution-mode"`,
			`<option value="pi">Pi</option>`,
			`<option value="claude_code">Claude Code</option>`,
			"channel-events",
		} {
			if !strings.Contains(recorder.Body.String(), marker) {
				t.Errorf("%s is missing %s", path, marker)
			}
		}
	}
	// 非 GET 方法。
	recorder := fixture.call(t, http.MethodPost, "/admin", "", false)
	wantAdminError(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

// CSP 必须锁死取值来源:图片/字体/对象全禁,base-uri 与 frame-ancestors 也锁死。
func TestAdminContentSecurityPolicyIsRestrictive(t *testing.T) {
	t.Parallel()
	for _, directive := range []string{
		"default-src 'self'", "img-src 'none'", "font-src 'none'", "object-src 'none'",
		"base-uri 'none'", "frame-ancestors 'none'", "form-action 'self'", "connect-src 'self'",
	} {
		if !strings.Contains(adminContentSecurityPolicy, directive) {
			t.Errorf("CSP is missing %q", directive)
		}
	}
}

// ---------------------------------------------------------------------------
// 路由与方法
// ---------------------------------------------------------------------------

func TestAdminRoutingRejectsUnknownPathsAndMethods(t *testing.T) {
	fixture := newAdminFixture(t)

	recorder := fixture.call(t, http.MethodGet, "/api/admin/nope", "", true)
	wantAdminError(t, recorder, http.StatusNotFound, "not_found")

	// 每个端点只接受它自己的方法。
	cases := []struct{ method, target string }{
		{http.MethodGet, "/api/admin/login"},
		{http.MethodGet, "/api/admin/logout"},
		{http.MethodPost, "/api/admin/session"},
		{http.MethodPost, "/api/admin/runs"},
		{http.MethodPost, "/api/admin/channel-events"},
		{http.MethodPut, "/api/admin/agents"},
		{http.MethodPatch, "/api/admin/agents/some-slug"},
	}
	for _, item := range cases {
		recorder := fixture.call(t, item.method, item.target, "{}", true)
		wantAdminError(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	// 空 slug 落到 not_found 而不是 agents 集合路由。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/agents/", "", true)
	wantAdminError(t, recorder, http.StatusNotFound, "not_found")
}

// ---------------------------------------------------------------------------
// 登录 / 会话 / 登出
// ---------------------------------------------------------------------------

func TestAdminLoginSetsSessionCookieAndAudits(t *testing.T) {
	fixture := newAdminFixture(t)
	// newAdminFixture 已经登录过一次;这里再验一次响应细节。
	body := `{"username":"  admin  ","password":` + jsonString(adminTestPassword) + `}`
	recorder := fixture.call(t, http.MethodPost, "/api/admin/login", body, false)
	raw := adminData(t, recorder, http.StatusOK)
	var data sessionData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data is not sessionData: %v", err)
	}
	if data.Username != "admin" || data.Role != UserRoleAdmin {
		t.Errorf("username = %q", data.Username)
	}
	if data.CSRFToken == "" || data.ExpiresAt == 0 {
		t.Errorf("data = %+v", data)
	}
	// Cookie 属性。
	var cookie *http.Cookie
	for _, candidate := range recorder.Result().Cookies() {
		if candidate.Name == SessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("login must set the session cookie")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie = %+v, want HttpOnly + SameSite=Strict + Path=/", cookie)
	}
	if cookie.Secure {
		t.Error("Secure must be off when secure_cookies is false")
	}
	// 审计:登录成功要留痕。
	if !hasAuditAction(t, fixture.state, "admin.login") {
		t.Error("a successful login must be audited")
	}
}

func TestAdminLoginRejectsMalformedAndOversizedCredentials(t *testing.T) {
	fixture := newAdminFixture(t)

	// 请求体不是 JSON。
	recorder := fixture.call(t, http.MethodPost, "/api/admin/login", `{not json`, false)
	message := wantAdminError(t, recorder, http.StatusBadRequest, "invalid_request")
	if message != "登录请求格式不正确" {
		t.Errorf("message = %q", message)
	}

	// 超长用户名/口令直接按凭证错误拒掉(不进 argon2)。
	long := strings.Repeat("x", 600)
	for _, body := range []string{
		`{"username":` + jsonString(strings.Repeat("u", 121)) + `,"password":"p"}`,
		`{"username":"admin","password":` + jsonString(long) + `}`,
	} {
		recorder := fixture.call(t, http.MethodPost, "/api/admin/login", body, false)
		message := wantAdminError(t, recorder, http.StatusBadRequest, "invalid_credentials")
		if message != "用户名或密码不正确" {
			t.Errorf("message = %q", message)
		}
	}
}

func TestAdminLoginWrongCredentialsAreAudited(t *testing.T) {
	fixture := newAdminFixture(t)
	body := `{"username":"admin","password":"definitely-wrong"}`
	recorder := fixture.call(t, http.MethodPost, "/api/admin/login", body, false)
	message := wantAdminError(t, recorder, http.StatusUnauthorized, "invalid_credentials")
	if message != "用户名或密码不正确" {
		t.Errorf("message = %q", message)
	}
	if !hasAuditAction(t, fixture.state, "admin.login_failed") {
		t.Error("a credential failure must be audited")
	}
}

// 限流响应带 Retry-After,并且**不写审计**(避免廉价洪泛变成每请求一条 PG 写)。
func TestAdminLoginRateLimitedCarriesRetryAfter(t *testing.T) {
	fixture := newAdminFixture(t)
	// 手动把并发闸占满,构造确定性的限流。
	held := 0
	for fixture.state.Auth.loginSlots.TryAcquire() {
		held++
	}
	defer func() {
		for i := 0; i < held; i++ {
			fixture.state.Auth.loginSlots.Release()
		}
	}()

	before := countAuditAction(t, fixture.state, "admin.login_failed")
	body := `{"username":"admin","password":"whatever"}`
	recorder := fixture.call(t, http.MethodPost, "/api/admin/login", body, false)
	message := wantAdminError(t, recorder, http.StatusTooManyRequests, "login_rate_limited")
	if message != "登录尝试过于频繁，请稍后重试" {
		t.Errorf("message = %q", message)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if after := countAuditAction(t, fixture.state, "admin.login_failed"); after != before {
		t.Errorf("rate limiting must not write an audit row (%d => %d)", before, after)
	}
}

func TestAdminSessionEndpoint(t *testing.T) {
	fixture := newAdminFixture(t)

	// 无 Cookie。
	recorder := fixture.call(t, http.MethodGet, "/api/admin/session", "", false)
	message := wantAdminError(t, recorder, http.StatusUnauthorized, "authentication_required")
	if message != "管理会话不存在或已过期" {
		t.Errorf("message = %q", message)
	}

	// 有 Cookie(session 端点不要求 CSRF)。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/session", "", true)
	raw := adminData(t, recorder, http.StatusOK)
	var data sessionData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.Username != "admin" || data.Role != UserRoleAdmin || data.CSRFToken != fixture.session.CSRFToken {
		t.Errorf("data = %+v", data)
	}
}

func TestAdminLogoutRequiresCSRFAndClearsSession(t *testing.T) {
	fixture := newAdminFixture(t)

	// 缺 CSRF 头 → 403。
	recorder := fixture.callWithoutCSRF(t, http.MethodPost, "/api/admin/logout", "")
	message := wantAdminError(t, recorder, http.StatusForbidden, "invalid_csrf_token")
	if message != "CSRF token 缺失或无效" {
		t.Errorf("message = %q", message)
	}

	// 带 CSRF → 成功并清 Cookie。
	recorder = fixture.call(t, http.MethodPost, "/api/admin/logout", "", true)
	raw := adminData(t, recorder, http.StatusOK)
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["loggedOut"] != true {
		t.Errorf("data = %#v", payload)
	}
	cleared := false
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout must clear the session cookie")
	}
	if !hasAuditAction(t, fixture.state, "admin.logout") {
		t.Error("logout must be audited")
	}

	// 会话已失效。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/session", "", true)
	wantAdminError(t, recorder, http.StatusUnauthorized, "authentication_required")
}

func TestAccountManagementRolesRevocationAndAuditScope(t *testing.T) {
	fixture := newAdminFixture(t)
	username := uniqueSlug("console-user")
	password := "initial-user-password-2026"
	newPassword := "rotated-user-password-2026"

	create := fixture.call(t, http.MethodPost, "/api/admin/users",
		jsonString(map[string]any{"username": username, "password": password, "role": "user"}), true)
	raw := adminData(t, create, http.StatusCreated)
	var created UserView
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.Username != username || created.Role != UserRoleUser || !created.Enabled {
		t.Fatalf("created user = %+v", created)
	}
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteUser(context.Background(), username) })

	duplicate := fixture.call(t, http.MethodPost, "/api/admin/users",
		jsonString(map[string]any{"username": username, "password": password, "role": "user"}), true)
	wantAdminError(t, duplicate, http.StatusConflict, "username_exists")

	userSession := fixture.loginAs(t, username, password)
	if userSession.Role != UserRoleUser {
		t.Fatalf("session role = %q", userSession.Role)
	}
	// 普通用户保留控制台只读能力，但 Provider 与 Agent 控制面写操作必须由
	// 后端强制拒绝，不能只依赖前端隐藏按钮。
	for _, target := range []string{
		"/api/admin/providers", "/api/admin/agents", "/api/admin/runs", "/api/admin/channel-events",
	} {
		response := fixture.callAs(t, userSession, http.MethodGet, target, "")
		adminData(t, response, http.StatusOK)
	}
	providerResponse := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/providers", "")
	providerRaw := adminData(t, providerResponse, http.StatusOK)
	var userProviders []ProviderView
	if err := json.Unmarshal(providerRaw, &userProviders); err != nil {
		t.Fatal(err)
	}
	for _, provider := range userProviders {
		if provider.Editable || len(provider.Config) != 0 {
			t.Fatalf("standard user received editable Provider details: %+v", provider)
		}
	}
	for _, item := range []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPut, "/api/admin/providers/gateway", `{}`},
		{http.MethodPost, "/api/admin/agents", agentPayload(uniqueSlug("readonly"), nil)},
		{http.MethodPut, "/api/admin/agents/not-present", agentPayload("not-present", nil)},
		{http.MethodDelete, "/api/admin/agents/not-present", ""},
	} {
		response := fixture.callAs(t, userSession, item.method, item.target, item.body)
		wantAdminError(t, response, http.StatusForbidden, "admin_required")
	}
	forbidden := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/users", "")
	wantAdminError(t, forbidden, http.StatusForbidden, "admin_required")
	promote := fixture.call(t, http.MethodPut, "/api/admin/users/"+username,
		jsonString(map[string]any{"password": nil, "role": "admin", "enabled": true}), true)
	adminData(t, promote, http.StatusOK)
	promotedAccess := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/users", "")
	adminData(t, promotedAccess, http.StatusOK)
	demote := fixture.call(t, http.MethodPut, "/api/admin/users/"+username,
		jsonString(map[string]any{"password": nil, "role": "user", "enabled": true}), true)
	adminData(t, demote, http.StatusOK)
	demotedAccess := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/users", "")
	wantAdminError(t, demotedAccess, http.StatusForbidden, "admin_required")

	if err := fixture.state.Store.RecordAudit(context.Background(), username,
		"agent.update", "user-owned-audit", map[string]any{"revision": 2}); err != nil {
		t.Fatal(err)
	}
	userAuditResponse := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/audit-logs?limit=100", "")
	userAuditRaw := adminData(t, userAuditResponse, http.StatusOK)
	var userAudits []AuditLogView
	if err := json.Unmarshal(userAuditRaw, &userAudits); err != nil {
		t.Fatal(err)
	}
	if len(userAudits) == 0 {
		t.Fatal("user audit list is empty")
	}
	for _, entry := range userAudits {
		if entry.Actor != username {
			t.Fatalf("user audit leaked actor %q", entry.Actor)
		}
	}

	adminAuditResponse := fixture.call(t, http.MethodGet, "/api/admin/audit-logs?limit=100", "", true)
	adminAuditRaw := adminData(t, adminAuditResponse, http.StatusOK)
	var adminAudits []AuditLogView
	if err := json.Unmarshal(adminAuditRaw, &adminAudits); err != nil {
		t.Fatal(err)
	}
	var sawAdmin, sawUser bool
	for _, entry := range adminAudits {
		sawAdmin = sawAdmin || entry.Actor == "admin"
		sawUser = sawUser || entry.Actor == username
	}
	if !sawAdmin || !sawUser {
		t.Fatalf("admin audit scope incomplete: admin=%t user=%t", sawAdmin, sawUser)
	}

	rotate := fixture.call(t, http.MethodPut, "/api/admin/users/"+username,
		jsonString(map[string]any{"password": newPassword, "role": "user", "enabled": true}), true)
	adminData(t, rotate, http.StatusOK)
	stale := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/session", "")
	wantAdminError(t, stale, http.StatusUnauthorized, "authentication_required")
	userSession = fixture.loginAs(t, username, newPassword)

	disable := fixture.call(t, http.MethodPut, "/api/admin/users/"+username,
		jsonString(map[string]any{"password": nil, "role": "user", "enabled": false}), true)
	adminData(t, disable, http.StatusOK)
	disabledSession := fixture.callAs(t, userSession, http.MethodGet, "/api/admin/session", "")
	wantAdminError(t, disabledSession, http.StatusUnauthorized, "authentication_required")

	reenable := fixture.call(t, http.MethodPut, "/api/admin/users/"+username,
		jsonString(map[string]any{"password": nil, "role": "user", "enabled": true}), true)
	adminData(t, reenable, http.StatusOK)
	_ = fixture.loginAs(t, username, newPassword)

	deleted := fixture.call(t, http.MethodDelete, "/api/admin/users/"+username, "", true)
	adminData(t, deleted, http.StatusOK)
	if got, err := fixture.state.Store.GetUser(context.Background(), username); err != nil || got != nil {
		t.Fatalf("deleted user = %#v, %v", got, err)
	}
}

func TestAccountManagementProtectsCurrentAdminAndValidatesInput(t *testing.T) {
	fixture := newAdminFixture(t)

	selfDemote := fixture.call(t, http.MethodPut, "/api/admin/users/admin",
		jsonString(map[string]any{"password": nil, "role": "user", "enabled": true}), true)
	wantAdminError(t, selfDemote, http.StatusUnprocessableEntity, "cannot_demote_self")
	selfDelete := fixture.call(t, http.MethodDelete, "/api/admin/users/admin", "", true)
	wantAdminError(t, selfDelete, http.StatusUnprocessableEntity, "cannot_delete_self")

	invalidUsername := fixture.call(t, http.MethodPost, "/api/admin/users",
		jsonString(map[string]any{"username": "bad user", "password": "valid-password-2026", "role": "user"}), true)
	wantAdminError(t, invalidUsername, http.StatusUnprocessableEntity, "invalid_username")
	shortPassword := fixture.call(t, http.MethodPost, "/api/admin/users",
		jsonString(map[string]any{"username": "short-password", "password": "too-short", "role": "user"}), true)
	wantAdminError(t, shortPassword, http.StatusUnprocessableEntity, "invalid_password")
	invalidRole := fixture.call(t, http.MethodPost, "/api/admin/users",
		jsonString(map[string]any{"username": "invalid-role", "password": "valid-password-2026", "role": "owner"}), true)
	wantAdminError(t, invalidRole, http.StatusBadRequest, "invalid_user")
}

func TestDirectLoginSourceTrustsForwardingHeadersOnlyFromConfiguredProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/admin/login", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "198.51.100.20, 10.0.0.4")
	if got := directLoginSource(request, nil); got != "10.0.0.5" {
		t.Fatalf("untrusted proxy source = %q", got)
	}
	trusted := []string{"10.0.0.0/8"}
	if got := directLoginSource(request, trusted); got != "198.51.100.20" {
		t.Fatalf("trusted proxy source = %q, want client address", got)
	}
	request.Header.Set("X-Forwarded-For", "forged-value")
	if got := directLoginSource(request, trusted); got != "10.0.0.5" {
		t.Fatalf("malformed forwarding chain must fall back to peer, got %q", got)
	}
}

// hasAuditAction / countAuditAction 直接查审计表。
func countAuditAction(t *testing.T, state *PlatformState, action string) int {
	t.Helper()
	var count int
	if err := state.Store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}

func hasAuditAction(t *testing.T, state *PlatformState, action string) bool {
	t.Helper()
	return countAuditAction(t, state, action) > 0
}

// ---------------------------------------------------------------------------
// 鉴权门禁:所有数据端点都要会话,写操作还要 CSRF
// ---------------------------------------------------------------------------

func TestAdminEndpointsRequireSession(t *testing.T) {
	fixture := newAdminFixture(t)
	cases := []struct{ method, target, body string }{
		{http.MethodGet, "/api/admin/agents", ""},
		{http.MethodPost, "/api/admin/agents", agentPayload("x-slug", nil)},
		{http.MethodGet, "/api/admin/agents/x-slug", ""},
		{http.MethodPut, "/api/admin/agents/x-slug", agentPayload("x-slug", nil)},
		{http.MethodDelete, "/api/admin/agents/x-slug", ""},
		{http.MethodGet, "/api/admin/runs", ""},
		{http.MethodGet, "/api/admin/channel-events", ""},
		{http.MethodPost, "/api/admin/logout", ""},
	}
	for _, item := range cases {
		recorder := fixture.call(t, item.method, item.target, item.body, false)
		wantAdminError(t, recorder, http.StatusUnauthorized, "authentication_required")
	}
}

func TestAdminWriteEndpointsRequireCSRF(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("csrf")
	cases := []struct{ method, target, body string }{
		{http.MethodPost, "/api/admin/agents", agentPayload(slug, nil)},
		{http.MethodPut, "/api/admin/agents/" + slug, agentPayload(slug, nil)},
		{http.MethodDelete, "/api/admin/agents/" + slug, ""},
		{http.MethodPost, "/api/admin/logout", ""},
	}
	for _, item := range cases {
		recorder := fixture.callWithoutCSRF(t, item.method, item.target, item.body)
		wantAdminError(t, recorder, http.StatusForbidden, "invalid_csrf_token")
	}
	// 读端点不要求 CSRF。
	for _, target := range []string{
		"/api/admin/agents", "/api/admin/runs", "/api/admin/channel-events", "/api/admin/session",
	} {
		recorder := fixture.callWithoutCSRF(t, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 without CSRF", target, recorder.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Agent CRUD
// ---------------------------------------------------------------------------

func TestAdminAgentCRUDLifecycle(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("crud")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), slug) })

	// 创建 → 201。
	recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", agentPayload(slug, nil), true)
	raw := adminData(t, recorder, http.StatusCreated)
	var view AgentView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("data is not an AgentView: %v", err)
	}
	if view.Slug != slug || view.Revision != 1 {
		t.Errorf("view = %+v", view)
	}
	// **密钥绝不回显。**
	if strings.Contains(recorder.Body.String(), "admin-test-secret") {
		t.Error("the signing secret must never be echoed")
	}
	if !view.SigningSecretConfigured {
		t.Error("signing_secret_configured must be true")
	}
	// Registry 已应用。
	if fixture.state.Registry.Get(slug) == nil {
		t.Error("a created Agent must be applied to the Registry")
	}
	if !hasAuditAction(t, fixture.state, "agent.create") {
		t.Error("agent.create must be audited")
	}

	// 重复 slug → 409。
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", agentPayload(slug, nil), true)
	message := wantAdminError(t, recorder, http.StatusConflict, "slug_exists")
	if message != "该 slug 已被其他 Agent 使用" {
		t.Errorf("message = %q", message)
	}

	// 读取单个。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/agents/"+slug, "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Slug != slug {
		t.Errorf("slug = %q", view.Slug)
	}

	// 列表里能找到。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/agents", "", true)
	raw = adminData(t, recorder, http.StatusOK)
	var views []AgentView
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range views {
		if item.Slug == slug {
			found = true
		}
	}
	if !found {
		t.Error("the created Agent must appear in the list")
	}

	// 更新(不带密钥)→ revision+1 且 secretRotated=false。
	updated := agentPayload(slug, map[string]any{"name": "renamed", "signing_secret": nil})
	recorder = fixture.call(t, http.MethodPut, "/api/admin/agents/"+slug, updated, true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "renamed" || view.Revision != 2 {
		t.Errorf("view = %+v, want renamed at revision 2", view)
	}
	if got := auditDetail(t, fixture.state, "agent.update", "secretRotated"); got != false {
		t.Errorf("secretRotated = %#v, want false", got)
	}

	// 更新(带新密钥)→ secretRotated=true。
	rotated := agentPayload(slug, map[string]any{"signing_secret": "rotated-secret-0123456789"})
	recorder = fixture.call(t, http.MethodPut, "/api/admin/agents/"+slug, rotated, true)
	adminData(t, recorder, http.StatusOK)
	if got := auditDetail(t, fixture.state, "agent.update", "secretRotated"); got != true {
		t.Errorf("secretRotated = %#v, want true", got)
	}

	// 删除 → 200 且 Registry 移除。
	recorder = fixture.call(t, http.MethodDelete, "/api/admin/agents/"+slug, "", true)
	raw = adminData(t, recorder, http.StatusOK)
	var deleted map[string]any
	if err := json.Unmarshal(raw, &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted["deleted"] != true {
		t.Errorf("data = %#v", deleted)
	}
	if fixture.state.Registry.Get(slug) != nil {
		t.Error("a deleted Agent must leave the Registry")
	}
	if !hasAuditAction(t, fixture.state, "agent.delete") {
		t.Error("agent.delete must be audited")
	}

	// 再删/再读 → 404。
	recorder = fixture.call(t, http.MethodDelete, "/api/admin/agents/"+slug, "", true)
	wantAdminError(t, recorder, http.StatusNotFound, "agent_not_found")
	recorder = fixture.call(t, http.MethodGet, "/api/admin/agents/"+slug, "", true)
	message = wantAdminError(t, recorder, http.StatusNotFound, "agent_not_found")
	if message != "Agent 不存在" {
		t.Errorf("message = %q", message)
	}
	recorder = fixture.call(t, http.MethodPut, "/api/admin/agents/"+slug, agentPayload(slug, nil), true)
	wantAdminError(t, recorder, http.StatusNotFound, "agent_not_found")
}

// 路径里的 slug 优先于请求体里的 slug。
func TestAdminUpdateUsesPathSlug(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("path")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), slug) })
	adminData(t, fixture.call(t, http.MethodPost, "/api/admin/agents",
		agentPayload(slug, nil), true), http.StatusCreated)

	// 请求体写成别的 slug,应当被忽略。
	body := agentPayload("some-other-slug", map[string]any{"name": "by path", "signing_secret": nil})
	recorder := fixture.call(t, http.MethodPut, "/api/admin/agents/"+slug, body, true)
	raw := adminData(t, recorder, http.StatusOK)
	var view AgentView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Slug != slug || view.Name != "by path" {
		t.Errorf("view = %+v, want the path slug to win", view)
	}
}

func TestAdminAgentAuditFailureRollsBackMutationAndRegistry(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("audit-rollback")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), slug) })

	createdRaw := adminData(t, fixture.call(t, http.MethodPost, "/api/admin/agents",
		agentPayload(slug, nil), true), http.StatusCreated)
	var created AgentView
	if err := json.Unmarshal(createdRaw, &created); err != nil {
		t.Fatal(err)
	}

	// NOT VALID avoids scanning unrelated historical rows but still rejects a
	// new audit INSERT for this exact target. The Agent UPDATE happens first in
	// the same transaction, so this exercises a genuine PostgreSQL rollback.
	constraint := fmt.Sprintf("reject_agent_audit_%d", uniqueSeq.Add(1))
	if _, err := fixture.state.Store.pool.Exec(context.Background(), fmt.Sprintf(
		`ALTER TABLE audit_log ADD CONSTRAINT %s CHECK (target <> '%s') NOT VALID`,
		constraint, slug)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = fixture.state.Store.pool.Exec(context.Background(),
			"ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS "+constraint)
	}()

	update := agentPayload(slug, map[string]any{
		"name": "must roll back", "signing_secret": nil,
	})
	recorder := fixture.call(t, http.MethodPut, "/api/admin/agents/"+slug, update, true)
	wantAdminError(t, recorder, http.StatusInternalServerError, "storage_unavailable")

	stored, err := fixture.state.Store.GetAgent(context.Background(), slug)
	if err != nil || stored == nil {
		t.Fatalf("get Agent after audit failure: %v", err)
	}
	if stored.Name != created.Name || stored.Revision != created.Revision {
		t.Fatalf("database mutation survived audit rollback: %+v", stored)
	}
	runtime := fixture.state.Registry.Get(slug)
	if runtime == nil || runtime.Config.Name != created.Name ||
		runtime.Config.Revision != created.Revision {
		t.Fatalf("Registry changed after rolled-back mutation: %+v", runtime)
	}
	var updateAudits int
	if err := fixture.state.Store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'agent.update' AND target = $1`, slug).
		Scan(&updateAudits); err != nil {
		t.Fatal(err)
	}
	if updateAudits != 0 {
		t.Fatalf("rolled-back Agent update wrote %d audit rows", updateAudits)
	}
}

// JSON 解析失败是 400;业务校验失败是 422。
func TestAdminAgentConfigErrorsAreClassified(t *testing.T) {
	fixture := newAdminFixture(t)

	recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", `{not json`, true)
	message := wantAdminError(t, recorder, http.StatusBadRequest, "invalid_agent_config")
	if !strings.HasPrefix(message, "Agent 配置格式不正确：") {
		t.Errorf("message = %q", message)
	}

	// 类型不符也是 400(解码失败)。
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", `{"slug":123}`, true)
	wantAdminError(t, recorder, http.StatusBadRequest, "invalid_agent_config")

	// 结构正确但内容非法 → 422。
	for _, body := range []string{
		agentPayload("", nil),
		agentPayload("Invalid--Slug", nil),
		agentPayload(uniqueSlug("nosecret"), map[string]any{"signing_secret": nil}),
		agentPayload(uniqueSlug("badenc"), map[string]any{"signature_encoding": "rot13"}),
		agentPayload(uniqueSlug("badkind"), map[string]any{"executor_kind": "telepathy"}),
	} {
		recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
		wantAdminError(t, recorder, http.StatusUnprocessableEntity, "invalid_agent_config")
	}
}

// auditDetail 读最近一条指定 action 的审计明细字段。
func auditDetail(t *testing.T, state *PlatformState, action, key string) any {
	t.Helper()
	var raw string
	if err := state.Store.pool.QueryRow(context.Background(),
		`SELECT details FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
		action).Scan(&raw); err != nil {
		t.Fatalf("read audit details: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("audit details is not JSON: %v (%s)", err, raw)
	}
	return decoded[key]
}

// ---------------------------------------------------------------------------
// workspace 校验
// ---------------------------------------------------------------------------

// 只有「本机 CLI 模式」才要求 workspace 真实存在;网关模式与进程内处理器不校验。
func TestAdminValidateWorkspaceOnlyForLocalCLI(t *testing.T) {
	fixture := newAdminFixture(t)
	configured := clonePlatformConfig(fixture.state.ConfigSnapshot())
	configured.Gateway.BaseURL = "https://gateway.example.com"
	configured.LocalAgents.PiBinary = executableFile(t, "pi")
	fixture.state.ApplyConfig(configured)

	// 本机 Pi + 不存在的目录 → 422 invalid_workspace。
	body := agentPayload(uniqueSlug("badws"), map[string]any{
		"executor_kind": "pi", "execution_mode": "local", "workspace_path": "agents/missing",
	})
	recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	message := wantAdminError(t, recorder, http.StatusUnprocessableEntity, "invalid_workspace")
	if message != "本地执行器工作区不存在或不可访问" {
		t.Errorf("message = %q", message)
	}

	// 网关模式同样的路径 → 放行(不 spawn 子进程,不需要目录存在)。
	gatewaySlug := uniqueSlug("gwws")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), gatewaySlug) })
	body = agentPayload(gatewaySlug, map[string]any{
		"executor_kind": "pi", "execution_mode": "gateway", "workspace_path": "agents/missing",
	})
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	adminData(t, recorder, http.StatusCreated)

	// 进程内处理器(rust)也放行。
	rustSlug := uniqueSlug("rustws")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), rustSlug) })
	body = agentPayload(rustSlug, map[string]any{
		"executor_kind": "rust", "execution_mode": "local", "workspace_path": "agents/missing",
	})
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	adminData(t, recorder, http.StatusCreated)

	// 本机 Pi + 存在的目录 → 放行。
	okSlug := uniqueSlug("okws")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), okSlug) })
	body = agentPayload(okSlug, map[string]any{
		"executor_kind": "pi", "execution_mode": "local", "workspace_path": "agents",
	})
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	adminData(t, recorder, http.StatusCreated)
}

// 符号链接逃出根目录必须被拒。
func TestAdminValidateWorkspaceRejectsEscape(t *testing.T) {
	fixture := newAdminFixture(t)
	outside := t.TempDir()
	link := filepath.Join(fixture.state.Config.WorkspaceRoot, "agents", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	body := agentPayload(uniqueSlug("escape"), map[string]any{
		"executor_kind": "pi", "execution_mode": "local", "workspace_path": "agents/escape",
	})
	recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	message := wantAdminError(t, recorder, http.StatusUnprocessableEntity, "invalid_workspace")
	if message != "本地执行器工作区必须位于配置的根目录内" {
		t.Errorf("message = %q", message)
	}
}

// Claude 本机执行器未启用时不许配置。
func TestAdminRejectsClaudeLocalWhenDisabled(t *testing.T) {
	fixture := newAdminFixture(t)
	if fixture.state.ConfigSnapshot().ClaudeEnabled {
		t.Fatal("this test assumes claude is disabled by default")
	}
	body := agentPayload(uniqueSlug("claude"), map[string]any{
		"executor_kind": "claude_code", "execution_mode": "local", "workspace_path": "agents",
	})
	recorder := fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	message := wantAdminError(t, recorder, http.StatusUnprocessableEntity, "claude_executor_disabled")
	if message != "Claude Code 本地执行器尚未在服务端显式启用" {
		t.Errorf("message = %q", message)
	}

	// 启用后放行。
	configured := clonePlatformConfig(fixture.state.ConfigSnapshot())
	configured.ClaudeEnabled = true
	configured.ClaudeBinary = executableFile(t, "claude")
	configured.Gateway.BaseURL = "https://gateway.example.com"
	fixture.state.ApplyConfig(configured)
	slug := uniqueSlug("claudeok")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), slug) })
	body = agentPayload(slug, map[string]any{
		"executor_kind": "claude_code", "execution_mode": "local", "workspace_path": "agents",
	})
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	adminData(t, recorder, http.StatusCreated)

	// 网关模式的 Claude 不受该开关限制。
	configured = clonePlatformConfig(fixture.state.ConfigSnapshot())
	configured.ClaudeEnabled = false
	fixture.state.ApplyConfig(configured)
	gwSlug := uniqueSlug("claudegw")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), gwSlug) })
	body = agentPayload(gwSlug, map[string]any{
		"executor_kind": "claude_code", "execution_mode": "gateway", "workspace_path": "agents",
	})
	recorder = fixture.call(t, http.MethodPost, "/api/admin/agents", body, true)
	adminData(t, recorder, http.StatusCreated)
}

// ---------------------------------------------------------------------------
// 运行记录与通道事件
// ---------------------------------------------------------------------------

func TestAdminListRunsFiltersBySlug(t *testing.T) {
	fixture := newAdminFixture(t)
	slugA, slugB := uniqueSlug("runs-a"), uniqueSlug("runs-b")
	runA, runB := unique("run-a"), unique("run-b")
	for _, item := range []struct{ slug, runID string }{{slugA, runA}, {slugB, runB}} {
		started, err := fixture.state.Store.StartRun(context.Background(),
			item.slug, item.runID, 1, "rust", "local", nil)
		if err != nil || !started {
			t.Fatalf("start run %s: (%v, %v)", item.runID, started, err)
		}
	}

	// 不带 slug → 两条都在。
	recorder := fixture.call(t, http.MethodGet, "/api/admin/runs", "", true)
	raw := adminData(t, recorder, http.StatusOK)
	var runs []RunView
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatal(err)
	}
	if !containsRun(runs, runA) || !containsRun(runs, runB) {
		t.Error("an unfiltered listing must include both runs")
	}

	// 带 slug → 只剩对应那条。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/runs?slug="+slugA, "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatal(err)
	}
	if !containsRun(runs, runA) {
		t.Error("the filtered listing must include its own run")
	}
	if containsRun(runs, runB) {
		t.Error("the filtered listing must exclude other Agents' runs")
	}
	for _, run := range runs {
		if run.AgentSlug != slugA {
			t.Errorf("slug filter leaked %q", run.AgentSlug)
		}
	}

	// limit 生效;非法 limit 退回默认值而不是报错。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/runs?limit=1", "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("limit=1 returned %d runs", len(runs))
	}
	for _, query := range []string{"?limit=abc", "?limit=", "?limit=0", "?limit=-5"} {
		recorder := fixture.call(t, http.MethodGet, "/api/admin/runs"+query, "", true)
		adminData(t, recorder, http.StatusOK)
	}
}

func containsRun(runs []RunView, runID string) bool {
	for _, run := range runs {
		if run.RunID == runID {
			return true
		}
	}
	return false
}

func TestAdminListChannelEventsFiltersByTypeAndSlug(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("events")
	runID := unique("run")
	other := uniqueSlug("events-other")

	for _, item := range []struct {
		eventType, agentSlug string
	}{
		{"callback", slug},
		{"heartbeat", slug},
		{"callback", other},
	} {
		agentSlug, eventRun := item.agentSlug, runID
		if err := fixture.state.Store.RecordChannelEvent(context.Background(), ChannelEventRecord{
			EventType: item.eventType, AgentSlug: &agentSlug, RunID: &eventRun,
			URL: "https://cb.example/x",
		}); err != nil {
			t.Fatalf("record channel event: %v", err)
		}
	}

	// 按 type 过滤。
	recorder := fixture.call(t, http.MethodGet, "/api/admin/channel-events?type=heartbeat", "", true)
	raw := adminData(t, recorder, http.StatusOK)
	var events []ChannelEventView
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType != "heartbeat" {
			t.Errorf("type filter leaked %q", event.EventType)
		}
	}

	// 按 slug 过滤。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/channel-events?slug="+slug, "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected the Agent's own events")
	}
	for _, event := range events {
		if event.AgentSlug == nil || *event.AgentSlug != slug {
			t.Errorf("slug filter leaked %v", event.AgentSlug)
		}
	}

	// type + slug 同时生效。
	recorder = fixture.call(t, http.MethodGet,
		"/api/admin/channel-events?type=callback&slug="+slug, "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType != "callback" || event.AgentSlug == nil || *event.AgentSlug != slug {
			t.Errorf("combined filter leaked %+v", event)
		}
	}

	// 不带过滤器时也要能列出来(参数名是 type,不是 event_type)。
	recorder = fixture.call(t, http.MethodGet, "/api/admin/channel-events", "", true)
	raw = adminData(t, recorder, http.StatusOK)
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Error("an unfiltered listing must return events")
	}
}

// ---------------------------------------------------------------------------
// 变更不被请求取消撕开
// ---------------------------------------------------------------------------

// 请求上下文已取消时,Agent 变更仍必须完整落库并应用到 Registry。
//
// rs 侧靠 detached task 保证这一点;Go 侧靠 mutationContext 剥掉取消信号。
// 若不剥,写库会中途失败,留下「库里有、Registry 没有」或反之的半截状态。
func TestAdminMutationsSurviveRequestCancellation(t *testing.T) {
	fixture := newAdminFixture(t)
	slug := uniqueSlug("cancelled")
	t.Cleanup(func() { _, _ = fixture.state.Store.DeleteAgent(context.Background(), slug) })

	// 创建:请求上下文预先取消。
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/agents",
		strings.NewReader(agentPayload(slug, nil))).WithContext(cancelledCtx)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: fixture.session.Token})
	request.Header.Set("X-CSRF-Token", fixture.session.CSRFToken)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)

	// 鉴权本身走的是请求上下文,取消后会失败 —— 那是可接受的(还没落任何库)。
	// 但一旦通过鉴权进入变更,就必须整体成功。
	switch recorder.Code {
	case http.StatusCreated:
		if fixture.state.Registry.Get(slug) == nil {
			t.Error("a created Agent must reach the Registry even when the request was cancelled")
		}
		stored, err := fixture.state.Store.GetAgent(context.Background(), slug)
		if err != nil || stored == nil {
			t.Errorf("a created Agent must be persisted: (%v, %v)", stored, err)
		}
	case http.StatusInternalServerError, http.StatusUnauthorized:
		// 鉴权阶段就被取消挡住:库与 Registry 都不该出现半截状态。
		stored, _ := fixture.state.Store.GetAgent(context.Background(), slug)
		inRegistry := fixture.state.Registry.Get(slug) != nil
		if (stored != nil) != inRegistry {
			t.Errorf("store (%v) and Registry (%v) disagree after a cancelled request",
				stored != nil, inRegistry)
		}
	default:
		t.Fatalf("unexpected status %d: %s", recorder.Code, recorder.Body.String())
	}
}

// mutationContext 必须保留请求携带的值,同时丢弃取消信号。
func TestMutationContextDropsCancellationKeepsValues(t *testing.T) {
	t.Parallel()
	type key struct{}
	base, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))
	request := httptest.NewRequest(http.MethodPost, "/api/admin/agents", nil).WithContext(base)
	derived, cancelDerived := mutationContext(request)
	defer cancelDerived()

	cancel()
	if derived.Err() != nil {
		t.Errorf("derived context was cancelled: %v", derived.Err())
	}
	if derived.Value(key{}) != "kept" {
		t.Errorf("request values were lost: %#v", derived.Value(key{}))
	}
	deadline, ok := derived.Deadline()
	if !ok {
		t.Fatal("mutation context must have a finite deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > mutationContextTimeout {
		t.Fatalf("mutation deadline remaining = %v, want (0, %v]", remaining, mutationContextTimeout)
	}
}
