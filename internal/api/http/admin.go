package http

// 管理 API v2：八模块 IA 的后端面（蓝图 §5/§6）。
//
// 线约定：数据信封 {"data": …}，错误信封 {"error":{"code","message"}}；
// 写操作要求管理员角色 + X-CSRF-Token；If-Match 头携带 revision 做乐观并发。
// 审计日志仅管理员可见（前端隐藏只是第二道保险，403 在这里强制）。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/admin"
	"ir-agent-server/internal/app/agentapi"
	"ir-agent-server/internal/app/backend"
	"ir-agent-server/internal/app/catalog"
	"ir-agent-server/internal/app/dashboard"
	"ir-agent-server/internal/app/teamrun"
	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/position"
	"ir-agent-server/internal/domain/run"
	"ir-agent-server/internal/domain/team"
)

// AdminCookieName 是管理台会话 Cookie 名。
const AdminCookieName = "ir_admin_session"

// AdminDeps 是管理路由的装配依赖。
type AdminDeps struct {
	Auth       *admin.Auth
	Catalog    *catalog.Service
	Dashboard  *dashboard.Service
	Engine     *teamrun.Engine
	Connectors *agentapi.Service
	Accounts   *admin.AccountService
	// Providers 管理执行后端（原「执行职能体」）；nil 时端点返回 503。
	Providers backend.Manager
	Runs      run.Repository
	Audits    audit.Repository
	// ChannelEvents 为 nil 时通道事件端点返回空列表。
	ChannelEvents ChannelEventLister
	// CookieSecure 生产必须为 true（HTTPS only）。
	CookieSecure bool
	CookieTTL    time.Duration
}

// AdminRouter 组装 /api/admin/v2 子路由与控制台页面挂载点。
func AdminRouter(deps AdminDeps) chi.Router {
	h := &adminHandler{deps: deps}
	router := chi.NewRouter()
	router.Route("/api/admin/v2", func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/logout", h.logout)
		r.Get("/session", h.session)

		r.Group(func(r chi.Router) {
			r.Use(h.requireSession)
			r.Get("/dashboard", h.dashboard)

			r.Get("/positions", h.listPositions)
			r.Get("/positions/{code}", h.getPosition)
			r.Get("/agents", h.listAgents)
			r.Get("/agents/{slug}", h.getAgent)
			r.Get("/teams", h.listTeams)
			r.Get("/teams/{id}", h.getTeam)
			r.Get("/team-runs", h.listTeamRuns)
			r.Get("/team-runs/{id}", h.getTeamRun)
			r.Get("/runs", h.listRuns)
			r.Get("/agent-connectors", h.listConnectors)
			r.Get("/providers", h.listProviders)
			r.Get("/audits", h.listAudits)
			r.Get("/channel-events", h.listChannelEvents)
			r.Get("/users", h.listUsers)

			// 写操作：管理员 + CSRF。
			r.Group(func(r chi.Router) {
				r.Use(h.requireAdminWrite)
				r.Post("/positions", h.savePosition)
				r.Put("/positions/{code}", h.savePosition)
				r.Delete("/positions/{code}", h.deletePosition)
				r.Post("/agents", h.saveAgent)
				r.Put("/agents/{slug}", h.saveAgent)
				r.Delete("/agents/{slug}", h.deleteAgent)
				r.Post("/teams", h.saveTeam)
				r.Put("/teams/{id}", h.saveTeam)
				r.Delete("/teams/{id}", h.deleteTeam)
				r.Post("/team-runs", h.submitTeamRun)
				r.Post("/team-runs/{id}/cancel", h.cancelTeamRun)
				r.Post("/agent-connectors", h.issueConnector)
				r.Delete("/agent-connectors/{name}", h.revokeConnector)
				r.Put("/providers/{id}", h.updateProvider)
				r.Post("/users", h.createUser)
				r.Put("/users/{username}", h.updateUser)
				r.Delete("/users/{username}", h.deleteUser)
			})
		})
	})
	return router
}

type adminHandler struct{ deps AdminDeps }

type sessionKey struct{}

// ---------------------------------------------------------------------------
// 认证端点
// ---------------------------------------------------------------------------

func (h *adminHandler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "login body must be valid JSON")
		return
	}
	outcome, err := h.deps.Auth.Login(r.Context(), body.Username, body.Password, peerSource(r))
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "login failed")
		return
	}
	switch outcome.Kind {
	case admin.LoginRateLimited:
		w.Header().Set("Retry-After", strconv.FormatInt(outcome.RetryAfterSecs, 10))
		writeAdminError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; retry later")
	case admin.LoginInvalid:
		writeAdminError(w, http.StatusUnauthorized, "invalid_credentials", "username or password is incorrect")
	default:
		h.setSessionCookie(w, outcome.Session)
		writeAdminData(w, http.StatusOK, map[string]any{
			"username":  outcome.Session.Username,
			"role":      string(outcome.Session.Role),
			"csrfToken": outcome.Session.CSRFToken,
			"expiresAt": outcome.Session.ExpiresAt,
		})
	}
}

func (h *adminHandler) logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := sessionCookie(r); ok {
		if err := h.deps.Auth.Logout(r.Context(), token); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "storage_error", "logout failed")
			return
		}
	}
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *adminHandler) session(w http.ResponseWriter, r *http.Request) {
	token, ok := sessionCookie(r)
	if !ok {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "missing session cookie")
		return
	}
	user, err := h.deps.Auth.Authenticate(r.Context(), token)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "session lookup failed")
		return
	}
	if user == nil {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "session is invalid or expired")
		return
	}
	// csrfToken 回发给会话持有者本人（与 login 一致）：刷新页面后 SPA 无需
	// 重新登录即可继续写操作。CSRF 防御语义不受影响——令牌只对同源脚本可读，
	// 跨站请求者读不到。
	writeAdminData(w, http.StatusOK, map[string]any{
		"username": user.Username, "role": string(user.Role),
		"csrfToken": user.CSRFToken,
	})
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

func (h *adminHandler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := sessionCookie(r)
		if !ok {
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "missing session cookie")
			return
		}
		user, err := h.deps.Auth.Authenticate(r.Context(), token)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "storage_error", "session lookup failed")
			return
		}
		if user == nil {
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "session is invalid or expired")
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAdminWrite 强制「写操作 = 管理员 + CSRF」。
func (h *adminHandler) requireAdminWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := sessionUserFrom(r)
		if user == nil || user.Role != account.RoleAdmin {
			writeAdminError(w, http.StatusForbidden, "forbidden", "admin role is required for writes")
			return
		}
		if !h.deps.Auth.VerifyCSRF(r.Header.Get("X-CSRF-Token"), user) {
			writeAdminError(w, http.StatusForbidden, "csrf_failed", "missing or invalid X-CSRF-Token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sessionUserFrom(r *http.Request) *admin.SessionUser {
	user, _ := r.Context().Value(sessionKey{}).(*admin.SessionUser)
	return user
}

func sessionCookie(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(AdminCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

func (h *adminHandler) setSessionCookie(w http.ResponseWriter, issued *admin.IssuedSession) {
	http.SetCookie(w, &http.Cookie{
		Name: AdminCookieName, Value: issued.Token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: h.deps.CookieSecure,
		MaxAge: int(h.deps.CookieTTL.Seconds()),
	})
}

func (h *adminHandler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: AdminCookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: h.deps.CookieSecure, MaxAge: -1,
	})
}

// peerSource 取直接对端地址（登录退避的来源维度）。反向代理场景的受信
// 转发来源解析在装配层显式配置后注入（P6c），这里不信任任何转发头。
func peerSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// dashboard / runs / audits
// ---------------------------------------------------------------------------

func (h *adminHandler) dashboard(w http.ResponseWriter, r *http.Request) {
	writeAdminData(w, http.StatusOK, h.deps.Dashboard.Snapshot(r.Context()))
}

func (h *adminHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))
	filter := run.Filter{
		AgentSlug: query.Get("agent"),
		Position:  strings.ToUpper(query.Get("position")),
		TeamRunID: query.Get("team_run_id"),
		Limit:     limit, Offset: offset,
		SingleRunsOnly: query.Get("single_only") == "true",
	}
	if status := query.Get("status"); status != "" {
		filter.Statuses = []run.Status{run.Status(status)}
	}
	items, total, err := h.deps.Runs.List(r.Context(), filter)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list runs failed")
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (h *adminHandler) listAudits(w http.ResponseWriter, r *http.Request) {
	user := sessionUserFrom(r)
	// IA 决策：审计日志仅管理员可见（八模块导航里对普通用户隐藏，
	// 这里是后端强制）。
	if user.Role != account.RoleAdmin {
		writeAdminError(w, http.StatusForbidden, "forbidden", "audit log requires admin role")
		return
	}
	query := audit.Query{
		Actor:  r.URL.Query().Get("actor"),
		Action: audit.Action(r.URL.Query().Get("action")),
	}
	query.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	query.Offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if err := query.Authorize(user.Role, user.Username); err != nil {
		writeAdminError(w, http.StatusForbidden, "forbidden", "audit log requires admin role")
		return
	}
	items, total, err := h.deps.Audits.List(r.Context(), query)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list audits failed")
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// ---------------------------------------------------------------------------
// 岗位 / 智能体 / 团队 CRUD
// ---------------------------------------------------------------------------

type positionPayload struct {
	Code         string          `json:"code"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	ContractRef  string          `json:"contract_ref"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Enabled      *bool           `json:"enabled"`
}

func positionView(p *position.Position) map[string]any {
	return map[string]any{
		"code": p.Code, "name": p.Name, "description": p.Description,
		"contract_ref": p.ContractRef, "output_schema": p.OutputSchema,
		"enabled": p.Enabled, "revision": p.Revision,
	}
}

func (h *adminHandler) listPositions(w http.ResponseWriter, r *http.Request) {
	items, err := h.deps.Catalog.ListPositions(r.Context(), r.URL.Query().Get("include_disabled") == "true")
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list positions failed")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for i := range items {
		views = append(views, positionView(&items[i]))
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) getPosition(w http.ResponseWriter, r *http.Request) {
	p, err := h.deps.Catalog.GetPosition(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		writeCatalogError(w, err, "load position failed")
		return
	}
	writeAdminData(w, http.StatusOK, positionView(p))
}

func (h *adminHandler) savePosition(w http.ResponseWriter, r *http.Request) {
	var payload positionPayload
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	if code := chi.URLParam(r, "code"); code != "" {
		payload.Code = code
	}
	entity := &position.Position{
		Code: payload.Code, Name: payload.Name, Description: payload.Description,
		ContractRef: payload.ContractRef, OutputSchema: payload.OutputSchema,
		Enabled: payload.Enabled == nil || *payload.Enabled,
	}
	revision, err := h.deps.Catalog.SavePosition(r.Context(), entity, ifMatch(r), sessionUserFrom(r).Username)
	if err != nil {
		writeCatalogError(w, err, "save position failed")
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"code": entity.Code, "revision": revision})
}

func (h *adminHandler) deletePosition(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Catalog.DeletePosition(r.Context(), chi.URLParam(r, "code"), sessionUserFrom(r).Username); err != nil {
		writeCatalogError(w, err, "delete position failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type agentPayload struct {
	Slug              string   `json:"slug"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Enabled           *bool    `json:"enabled"`
	ExecutorKind      string   `json:"executor_kind"`
	ExecutionMode     string   `json:"execution_mode"`
	Positions         []string `json:"positions"`
	SigningSecret     *string  `json:"signing_secret"`
	SignatureEncoding string   `json:"signature_encoding"`
	SystemPrompt      string   `json:"system_prompt"`
	WorkspacePath     string   `json:"workspace_path"`
	ClaudeModel       *string  `json:"claude_model"`
	AllowedTools      []string `json:"allowed_tools"`
	PermissionMode    string   `json:"permission_mode"`
	TimeoutSecs       *uint64  `json:"timeout_secs"`
	MaxConcurrency    *int     `json:"max_concurrency"`
	QueueCapacity     *int     `json:"queue_capacity"`
}

// agentView 生成智能体展示态。secretConfigured 必须在清空明文密钥**之前**
// 判定；视图与响应永不携带密钥材料。
func agentView(a *agent.Agent, secretConfigured bool) map[string]any {
	return map[string]any{
		"slug": a.Slug, "name": a.Name, "description": a.Description,
		"enabled": a.Enabled, "executor_kind": string(a.Binding.Kind),
		"execution_mode": string(a.Binding.Mode), "positions": a.Positions,
		"signing_secret_configured": secretConfigured,
		"signing_secret_hint":       "已加密配置",
		"signature_encoding":        a.SignatureEncoding,
		"system_prompt":             a.SystemPrompt, "workspace_path": a.WorkspacePath,
		"claude_model": a.Model, "allowed_tools": a.AllowedTools,
		"permission_mode": a.PermissionMode, "timeout_secs": a.TimeoutSecs,
		"max_concurrency": a.MaxConcurrency, "queue_capacity": a.QueueCapacity,
		"revision":   a.Revision,
		"created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

func (h *adminHandler) listAgents(w http.ResponseWriter, r *http.Request) {
	items, err := h.deps.Catalog.ListAgents(r.Context(), r.URL.Query().Get("include_disabled") == "true")
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list agents failed")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for i := range items {
		item := items[i]
		configured := len(item.SigningSecret) > 0
		item.SigningSecret = nil // 视图永不携带明文密钥
		views = append(views, agentView(&item, configured))
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.deps.Catalog.GetAgent(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		writeCatalogError(w, err, "load agent failed")
		return
	}
	configured := len(a.SigningSecret) > 0
	a.SigningSecret = nil
	writeAdminData(w, http.StatusOK, agentView(a, configured))
}

func (h *adminHandler) saveAgent(w http.ResponseWriter, r *http.Request) {
	var payload agentPayload
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	if slug := chi.URLParam(r, "slug"); slug != "" {
		payload.Slug = slug
	}
	entity := &agent.Agent{
		Slug: payload.Slug, Name: payload.Name, Description: payload.Description,
		Enabled:   payload.Enabled == nil || *payload.Enabled,
		Binding:   bindingOf(payload.ExecutorKind, payload.ExecutionMode),
		Positions: payload.Positions, SystemPrompt: payload.SystemPrompt,
		WorkspacePath: payload.WorkspacePath, Model: payload.ClaudeModel,
		AllowedTools: payload.AllowedTools, PermissionMode: payload.PermissionMode,
		SignatureEncoding: payload.SignatureEncoding,
	}
	if payload.TimeoutSecs != nil {
		entity.TimeoutSecs = *payload.TimeoutSecs
	}
	if payload.MaxConcurrency != nil {
		entity.MaxConcurrency = *payload.MaxConcurrency
	}
	if payload.QueueCapacity != nil {
		entity.QueueCapacity = *payload.QueueCapacity
	} else {
		entity.QueueCapacity = agent.DefaultQueueCapacity
	}
	if payload.SigningSecret != nil && *payload.SigningSecret != "" {
		entity.SigningSecret = []byte(*payload.SigningSecret)
	}
	revision, err := h.deps.Catalog.SaveAgent(r.Context(), entity, ifMatch(r), sessionUserFrom(r).Username)
	if err != nil {
		writeCatalogError(w, err, "save agent failed")
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"slug": entity.Slug, "revision": revision})
}

func (h *adminHandler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Catalog.DeleteAgent(r.Context(), chi.URLParam(r, "slug"), sessionUserFrom(r).Username); err != nil {
		writeCatalogError(w, err, "delete agent failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type teamPayload struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	Description       string      `json:"description"`
	Steps             []team.Step `json:"steps"`
	MaxConcurrentRuns int         `json:"max_concurrent_runs"`
	Enabled           *bool       `json:"enabled"`
}

func teamView(t *team.Team) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "description": t.Description,
		"steps": t.Steps, "max_concurrent_runs": t.MaxConcurrentRuns,
		"enabled": t.Enabled, "revision": t.Revision,
	}
}

func (h *adminHandler) listTeams(w http.ResponseWriter, r *http.Request) {
	items, err := h.deps.Catalog.ListTeams(r.Context(), r.URL.Query().Get("include_disabled") == "true")
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list teams failed")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for i := range items {
		views = append(views, teamView(&items[i]))
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) getTeam(w http.ResponseWriter, r *http.Request) {
	t, err := h.deps.Catalog.GetTeam(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeCatalogError(w, err, "load team failed")
		return
	}
	writeAdminData(w, http.StatusOK, teamView(t))
}

func (h *adminHandler) saveTeam(w http.ResponseWriter, r *http.Request) {
	var payload teamPayload
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	if id := chi.URLParam(r, "id"); id != "" {
		payload.ID = id
	}
	entity := &team.Team{
		ID: payload.ID, Name: payload.Name, Description: payload.Description,
		Steps: payload.Steps, MaxConcurrentRuns: payload.MaxConcurrentRuns,
		Enabled: payload.Enabled == nil || *payload.Enabled,
	}
	revision, err := h.deps.Catalog.SaveTeam(r.Context(), entity, ifMatch(r), sessionUserFrom(r).Username)
	if err != nil {
		writeCatalogError(w, err, "save team failed")
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"id": entity.ID, "revision": revision})
}

func (h *adminHandler) deleteTeam(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Catalog.DeleteTeam(r.Context(), chi.URLParam(r, "id"), sessionUserFrom(r).Username); err != nil {
		writeCatalogError(w, err, "delete team failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 团队运行 / 连接器令牌
// ---------------------------------------------------------------------------

func (h *adminHandler) submitTeamRun(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		TeamID         string          `json:"team_id"`
		Input          json.RawMessage `json:"input"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	tr, err := h.deps.Engine.Submit(r.Context(), teamrun.Submission{
		TeamID: payload.TeamID, Input: payload.Input,
		IdempotencyKey: payload.IdempotencyKey, Actor: sessionUserFrom(r).Username,
	})
	if err != nil {
		if errors.Is(err, teamrun.ErrTeamBusy) {
			writeAdminError(w, http.StatusConflict, "team_busy", err.Error())
			return
		}
		if errors.Is(err, teamrun.ErrTeamUnavailable) {
			writeAdminError(w, http.StatusNotFound, "team_unavailable", err.Error())
			return
		}
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeAdminData(w, http.StatusAccepted, teamRunView(tr))
}

func (h *adminHandler) listTeamRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, total, err := h.deps.Engine.List(r.Context(), r.URL.Query().Get("team_id"), limit, offset)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list team runs failed")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for i := range items {
		views = append(views, teamRunView(&items[i]))
	}
	writeAdminData(w, http.StatusOK, map[string]any{"items": views, "total": total})
}

func (h *adminHandler) getTeamRun(w http.ResponseWriter, r *http.Request) {
	tr, err := h.deps.Engine.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, team.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, "not_found", "team run not found")
			return
		}
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "load team run failed")
		return
	}
	writeAdminData(w, http.StatusOK, teamRunView(tr))
}

func (h *adminHandler) cancelTeamRun(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Engine.Cancel(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "cancel_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func teamRunView(tr *team.TeamRun) map[string]any {
	view := map[string]any{
		"id": tr.ID, "team_id": tr.TeamID, "team_revision": tr.TeamRevision,
		"status": string(tr.Status), "steps": tr.Steps,
		"created_at": tr.CreatedAt,
	}
	if tr.CompletedAt != nil {
		view["completed_at"] = *tr.CompletedAt
	}
	if tr.Error != nil {
		view["error"] = tr.Error
	}
	return view
}

func (h *adminHandler) listConnectors(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.deps.Connectors.ListTokens(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list connectors failed")
		return
	}
	views := make([]map[string]any, 0, len(tokens))
	for _, token := range tokens {
		view := map[string]any{
			"id": token.ID, "name": token.Name, "scopes": token.Scopes,
			"created_at": token.CreatedAt,
			"active":     token.Active(time.Now()),
		}
		if token.RevokedAt != nil {
			view["revoked_at"] = *token.RevokedAt
		}
		if token.GraceUntil != nil {
			view["grace_until"] = *token.GraceUntil
		}
		views = append(views, view)
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) issueConnector(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	plaintext, err := h.deps.Connectors.IssueToken(r.Context(), payload.Name, payload.Scopes)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 明文令牌仅此一次出现在响应里。
	writeAdminData(w, http.StatusCreated, map[string]any{
		"name": payload.Name, "token": plaintext,
	})
}

func (h *adminHandler) revokeConnector(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Connectors.RevokeToken(r.Context(), chi.URLParam(r, "name")); err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "connector not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 渲染与小工具
// ---------------------------------------------------------------------------

func writeAdminData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"data": data})
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// writeCatalogError 把 catalog/领域哨兵错误映射为 HTTP 状态。
func writeCatalogError(w http.ResponseWriter, err error, fallbackMessage string) {
	switch {
	case errors.Is(err, catalog.ErrInvalidInput):
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	case errors.Is(err, position.ErrRevisionConflict), errors.Is(err, agent.ErrRevisionConflict),
		errors.Is(err, team.ErrRevisionConflict):
		writeAdminError(w, http.StatusConflict, "revision_conflict",
			"the resource was modified concurrently; refresh and retry with a fresh If-Match")
	case errors.Is(err, position.ErrInUse):
		writeAdminError(w, http.StatusConflict, "in_use", "position is still staffed by agents")
	case errors.Is(err, position.ErrReserved):
		writeAdminError(w, http.StatusForbidden, "reserved", err.Error())
	case errors.Is(err, position.ErrNotFound), errors.Is(err, agent.ErrNotFound),
		errors.Is(err, team.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "resource not found")
	default:
		writeAdminError(w, http.StatusInternalServerError, "storage_error", fallbackMessage)
	}
}

func decodeAdminBody(w http.ResponseWriter, r *http.Request, target any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(target); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return err
	}
	return nil
}

// ifMatch 解析 If-Match 头为 revision；缺省返回 nil（无条件写）。
func ifMatch(r *http.Request) *int64 {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return nil
	}
	raw = strings.Trim(raw, `"`)
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &revision
}

func bindingOf(kind, mode string) exec.Binding {
	return exec.Binding{Kind: exec.Kind(strings.TrimSpace(kind)), Mode: exec.Mode(strings.TrimSpace(mode))}
}

// ---------------------------------------------------------------------------
// 账号管理（admin-only）与通道事件
// ---------------------------------------------------------------------------

// ChannelEventLister 是通道事件查询端口（infra/postgres 的 ChannelEventStore 满足）。
type ChannelEventLister interface {
	ListChannelEvents(ctx context.Context, eventType, slug string, limit int) ([]audit.ChannelEvent, error)
}

func (h *adminHandler) requireAdminRole(w http.ResponseWriter, r *http.Request) bool {
	user := sessionUserFrom(r)
	if user == nil || user.Role != account.RoleAdmin {
		writeAdminError(w, http.StatusForbidden, "forbidden", "admin role is required")
		return false
	}
	return true
}

func (h *adminHandler) listChannelEvents(w http.ResponseWriter, r *http.Request) {
	if h.deps.ChannelEvents == nil {
		writeAdminData(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := h.deps.ChannelEvents.ListChannelEvents(r.Context(),
		r.URL.Query().Get("event_type"), r.URL.Query().Get("slug"), limit)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list channel events failed")
		return
	}
	writeAdminData(w, http.StatusOK, events)
}

func (h *adminHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminRole(w, r) {
		return
	}
	views, err := h.deps.Accounts.List(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list users failed")
		return
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) createUser(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	view, err := h.deps.Accounts.Create(r.Context(), payload.Username, payload.Password,
		account.Role(payload.Role), sessionUserFrom(r).Username)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeAdminData(w, http.StatusCreated, view)
}

func (h *adminHandler) updateUser(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Password *string `json:"password"`
		Role     *string `json:"role"`
		Enabled  *bool   `json:"enabled"`
	}
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	input := admin.UpdateInput{Password: payload.Password, Enabled: payload.Enabled}
	if payload.Role != nil {
		role := account.Role(*payload.Role)
		input.Role = &role
	}
	view, err := h.deps.Accounts.Update(r.Context(), chi.URLParam(r, "username"),
		input, sessionUserFrom(r).Username)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeAdminData(w, http.StatusOK, view)
}

func (h *adminHandler) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Accounts.Delete(r.Context(), chi.URLParam(r, "username"),
		sessionUserFrom(r).Username); err != nil {
		writeAccountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAccountError 映射账号用例错误到 HTTP 状态。
func writeAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrInvalidUsername):
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_username", err.Error())
	case errors.Is(err, admin.ErrInvalidPassword):
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_password", err.Error())
	case errors.Is(err, admin.ErrUsernameExists):
		writeAdminError(w, http.StatusConflict, "username_exists", "用户名已存在")
	case errors.Is(err, account.ErrLastAdmin):
		writeAdminError(w, http.StatusConflict, "last_admin",
			"system must retain at least one enabled admin")
	case errors.Is(err, account.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "account not found")
	default:
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "account operation failed")
	}
}

// ---------------------------------------------------------------------------
// 执行后端（providers，八模块 IA：智能体 → 执行后端子页）
// ---------------------------------------------------------------------------

func (h *adminHandler) listProviders(w http.ResponseWriter, r *http.Request) {
	if h.deps.Providers == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "providers_unavailable",
			"executor backend management is not configured")
		return
	}
	views, err := h.deps.Providers.List(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "list Provider snapshot failed")
		return
	}
	if sessionUserFrom(r).Role != account.RoleAdmin {
		// 普通用户可观察可用性与岗位分配，但不需要（也不应看到）基础设施
		// URL、本机二进制路径与可编辑配置。
		for index := range views {
			views[index].Config = map[string]any{}
			views[index].Editable = false
		}
	}
	writeAdminData(w, http.StatusOK, views)
}

func (h *adminHandler) updateProvider(w http.ResponseWriter, r *http.Request) {
	if h.deps.Providers == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "providers_unavailable",
			"executor backend management is not configured")
		return
	}
	id := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "id")))
	expected, ok := parseProviderIfMatch(w, r)
	if !ok {
		return
	}
	var payload map[string]any
	if err := decodeAdminBody(w, r, &payload); err != nil {
		return
	}
	_, view, err := h.deps.Providers.Update(r.Context(), id, payload, expected,
		sessionUserFrom(r).Username)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeAdminData(w, http.StatusOK, view)
}

// parseProviderIfMatch 严格解析 If-Match：缺省 = 无条件写（用快照 revision
// 兜底 CAS）；带了必须是单个带引号的非负整数。非法时已写好 400。
func parseProviderIfMatch(w http.ResponseWriter, r *http.Request) (*int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		return nil, true
	}
	invalid := func() (*int64, bool) {
		writeAdminError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match 必须是一个带引号的非负整数配置 revision")
		return nil, false
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return invalid()
	}
	value := strings.TrimSpace(values[0])
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return invalid()
	}
	revision, err := strconv.ParseInt(value[1:len(value)-1], 10, 64)
	if err != nil || revision < 0 {
		return invalid()
	}
	return &revision, true
}

// writeProviderError 映射执行后端用例错误：404 未知 / 412 CAS 冲突 /
// 500 存储 / 422 配置校验。
func writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backend.ErrUnknownProvider):
		writeAdminError(w, http.StatusNotFound, "provider_not_found", "智能体 Provider 不存在")
	case errors.Is(err, backend.ErrStaleRevision):
		writeAdminError(w, http.StatusPreconditionFailed, "stale_provider_revision",
			"执行职能体配置已变化，请刷新后重试")
	case errors.Is(err, backend.ErrStorage):
		writeAdminError(w, http.StatusInternalServerError, "storage_error", "update provider failed")
	default:
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_provider_config", err.Error())
	}
}
