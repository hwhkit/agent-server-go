package mota

// 管理台 HTTP 入口:/admin 页面与 /api/admin/* 数据接口。
// 对应 Rust 侧 src/platform/admin.rs。

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed web/admin.html
var adminHTML string

// adminContentSecurityPolicy 是管理台页面的 CSP。
//
// 页面是单文件内联样式与脚本,所以必须放开 'unsafe-inline';但图片、字体、对象
// 一律禁掉,base-uri 与 frame-ancestors 也锁死,把 XSS 的可利用面压到最小。
const adminContentSecurityPolicy = "default-src 'self'; style-src 'unsafe-inline'; " +
	"script-src 'unsafe-inline'; connect-src 'self'; img-src 'none'; font-src 'none'; " +
	"object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// adminNoStore 是所有管理台响应共用的缓存策略。会话与 Agent 配置都不能被缓存。
const adminNoStore = "no-store, max-age=0"

// 登录请求的长度上限。超长直接按凭证错误拒掉,不进 argon2。
const (
	maxLoginUsernameBytes = 120
	maxLoginPasswordBytes = 512
)

// AdminHandler 是管理台路由。
type AdminHandler struct {
	State *PlatformState
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

// writeAdminData 输出 {"data": ...} 信封。
func writeAdminData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", adminNoStore)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// writeAdminError 输出 {"error":{...}} 信封。
func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", adminNoStore)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, serializedError(code, message))
}

// writeStorageError 记录并返回统一的存储故障响应。
func writeStorageError(w http.ResponseWriter, err error, operation string) {
	slog.Error("admin data operation failed", "operation", operation, "error", err)
	writeAdminError(w, http.StatusInternalServerError, "storage_unavailable", "平台数据暂时不可用")
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func (a *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/admin" || path == "/admin/" {
		if r.Method != http.MethodGet {
			writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		a.adminPage(w)
		return
	}
	switch path {
	case "/api/admin/login":
		a.route(w, r, http.MethodPost, a.login)
		return
	case "/api/admin/logout":
		a.route(w, r, http.MethodPost, a.logout)
		return
	case "/api/admin/session":
		a.route(w, r, http.MethodGet, a.session)
		return
	case "/api/admin/agents":
		switch r.Method {
		case http.MethodGet:
			a.listAgents(w, r)
		case http.MethodPost:
			a.createAgent(w, r)
		default:
			writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		}
		return
	case "/api/admin/providers":
		a.route(w, r, http.MethodGet, a.listProviders)
		return
	case "/api/admin/runs":
		a.route(w, r, http.MethodGet, a.listRuns)
		return
	case "/api/admin/channel-events":
		a.route(w, r, http.MethodGet, a.listChannelEvents)
		return
	}
	if providerID, ok := strings.CutPrefix(path, "/api/admin/providers/"); ok && providerID != "" {
		if r.Method != http.MethodPut {
			writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		a.updateProvider(w, r, providerID)
		return
	}
	if slug, ok := strings.CutPrefix(path, "/api/admin/agents/"); ok && slug != "" {
		switch r.Method {
		case http.MethodGet:
			a.getAgent(w, r, slug)
		case http.MethodPut:
			a.updateAgent(w, r, slug)
		case http.MethodDelete:
			a.deleteAgent(w, r, slug)
		default:
			writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		}
		return
	}
	writeAdminError(w, http.StatusNotFound, "not_found", "Not found")
}

// route 是「只允许一个方法」的小包装。
func (a *AdminHandler) route(
	w http.ResponseWriter,
	r *http.Request,
	method string,
	handler func(http.ResponseWriter, *http.Request),
) {
	if r.Method != method {
		writeAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	handler(w, r)
}

// adminPage 输出内嵌的管理台单页。
func (a *AdminHandler) adminPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", adminNoStore)
	w.Header().Set("Content-Security-Policy", adminContentSecurityPolicy)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, adminHTML)
}

// ---------------------------------------------------------------------------
// 智能体中心 Provider
// ---------------------------------------------------------------------------

func (a *AdminHandler) listProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireSession(w, r, false); !ok {
		return
	}
	writeAdminData(w, http.StatusOK, ListProviders(a.State.ConfigSnapshot()))
}

func (a *AdminHandler) updateProvider(w http.ResponseWriter, r *http.Request, providerID string) {
	session, ok := a.requireSession(w, r, true)
	if !ok {
		return
	}
	config := a.State.ConfigSnapshot()
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body,
		int64(config.MaxRequestBodyBytes)+1)).Decode(&payload); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_provider_config",
			"智能体 Provider 配置格式不正确："+err.Error())
		return
	}

	a.State.Mutation.Lock()
	defer a.State.Mutation.Unlock()
	next, stored, err := UpdateProvider(a.State.ConfigSnapshot(), providerID, payload)
	if err != nil {
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_provider_config", err.Error())
		return
	}
	ctx := mutationContext(r)
	if err := a.State.Store.PutExecutorProvider(ctx, providerID, stored); err != nil {
		writeStorageError(w, err, "update provider")
		return
	}
	a.State.ApplyConfig(next)
	if err := a.State.Store.RecordAudit(ctx, session.Username, "provider.update", providerID,
		map[string]any{"providerId": providerID}); err != nil {
		slog.Warn("record provider audit failed", "provider", providerID, "error", err)
	}
	for _, view := range ListProviders(next) {
		if view.ID == providerID {
			writeAdminData(w, http.StatusOK, view)
			return
		}
	}
	writeAdminError(w, http.StatusNotFound, "provider_not_found", "智能体 Provider 不存在")
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

// requireSession 取出当前会话;requireCSRF 为 true 时还要校验 X-CSRF-Token。
//
// 所有写操作都必须带 CSRF 校验 —— 会话 Cookie 是 SameSite=Strict,但 CSRF 令牌
// 是第二道防线,不能省。
func (a *AdminHandler) requireSession(
	w http.ResponseWriter,
	r *http.Request,
	requireCSRF bool,
) (*SessionRecord, bool) {
	session, err := a.State.Auth.Authenticate(r.Context(), r)
	if err != nil {
		slog.Error("read admin session failed", "error", err)
		writeAdminError(w, http.StatusInternalServerError,
			"session_unavailable", "管理会话服务暂时不可用")
		return nil, false
	}
	if session == nil {
		writeAdminError(w, http.StatusUnauthorized,
			"authentication_required", "管理会话不存在或已过期")
		return nil, false
	}
	if requireCSRF && !a.State.Auth.VerifyCSRF(r, session) {
		writeAdminError(w, http.StatusForbidden, "invalid_csrf_token", "CSRF token 缺失或无效")
		return nil, false
	}
	return session, true
}

// sessionData 是 /api/admin/session 与登录成功的响应体。
type sessionData struct {
	Username  string `json:"username"`
	CSRFToken string `json:"csrfToken"`
	ExpiresAt int64  `json:"expiresAt"`
}

func (a *AdminHandler) login(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body,
		int64(a.State.ConfigSnapshot().MaxRequestBodyBytes)+1)).Decode(&payload); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式不正确")
		return
	}
	// 超长凭证不进 argon2:那是廉价的资源消耗攻击面。
	if len(payload.Username) > maxLoginUsernameBytes || len(payload.Password) > maxLoginPasswordBytes {
		writeAdminError(w, http.StatusBadRequest, "invalid_credentials", "用户名或密码不正确")
		return
	}

	outcome, err := a.State.Auth.Login(r.Context(), strings.TrimSpace(payload.Username), payload.Password)
	if err != nil {
		slog.Error("admin login failed", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "login_unavailable", "登录服务暂时不可用")
		return
	}

	switch outcome.Kind {
	case LoginAuthenticated:
		if err := a.State.Store.RecordAudit(r.Context(), a.State.Auth.Username(),
			"admin.login", "session", map[string]any{"result": "success"}); err != nil {
			slog.Warn("record admin login audit failed", "error", err)
		}
		w.Header().Add("Set-Cookie", a.State.Auth.SessionCookie(outcome.Session))
		writeAdminData(w, http.StatusOK, sessionData{
			Username:  a.State.Auth.Username(),
			CSRFToken: outcome.Session.CSRFToken,
			ExpiresAt: outcome.Session.ExpiresAt,
		})
	case LoginRateLimited:
		// 限流不写审计:否则一次廉价的洪泛就变成每请求一条 PostgreSQL 写入。
		// 真正的凭证失败仍然会被审计。
		slog.Warn("admin login rate limited", "retry_after", outcome.RetryAfterSecs)
		w.Header().Set("Retry-After", strconv.FormatInt(outcome.RetryAfterSecs, 10))
		writeAdminError(w, http.StatusTooManyRequests,
			"login_rate_limited", "登录尝试过于频繁，请稍后重试")
	default:
		a.recordLoginFailure(r.Context(), "invalid_credentials")
		writeAdminError(w, http.StatusUnauthorized, "invalid_credentials", "用户名或密码不正确")
	}
}

func (a *AdminHandler) session(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requireSession(w, r, false)
	if !ok {
		return
	}
	writeAdminData(w, http.StatusOK, sessionData{
		Username:  session.Username,
		CSRFToken: session.CSRFToken,
		ExpiresAt: session.ExpiresAt,
	})
}

func (a *AdminHandler) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requireSession(w, r, true)
	if !ok {
		return
	}
	if err := a.State.Auth.Logout(r.Context(), r); err != nil {
		slog.Error("admin logout failed", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "logout_unavailable", "退出登录失败，请重试")
		return
	}
	if err := a.State.Store.RecordAudit(r.Context(), session.Username,
		"admin.logout", "session", map[string]any{}); err != nil {
		slog.Warn("record admin logout audit failed", "error", err)
	}
	w.Header().Add("Set-Cookie", a.State.Auth.ClearCookie())
	writeAdminData(w, http.StatusOK, map[string]any{"loggedOut": true})
}

// recordLoginFailure 审计一次凭证失败。
func (a *AdminHandler) recordLoginFailure(ctx context.Context, reason string) {
	if err := a.State.Store.RecordAudit(ctx, "anonymous",
		"admin.login_failed", "session", map[string]any{"reason": reason}); err != nil {
		slog.Warn("record failed admin login audit failed", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Agent CRUD
// ---------------------------------------------------------------------------

func (a *AdminHandler) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireSession(w, r, false); !ok {
		return
	}
	agents, err := a.State.Store.ListAgents(r.Context())
	if err != nil {
		writeStorageError(w, err, "list agents")
		return
	}
	views := make([]AgentView, 0, len(agents))
	for _, agent := range agents {
		views = append(views, agent.ToView(a.State.ConfigSnapshot().PublicBaseURL))
	}
	writeAdminData(w, http.StatusOK, views)
}

func (a *AdminHandler) getAgent(w http.ResponseWriter, r *http.Request, slug string) {
	if _, ok := a.requireSession(w, r, false); !ok {
		return
	}
	agent, err := a.State.Store.GetAgent(r.Context(), slug)
	if err != nil {
		writeStorageError(w, err, "get agent")
		return
	}
	if agent == nil {
		writeAdminError(w, http.StatusNotFound, "agent_not_found", "Agent 不存在")
		return
	}
	writeAdminData(w, http.StatusOK, agent.ToView(a.State.ConfigSnapshot().PublicBaseURL))
}

// decodeAgentInput 解析并校验 Agent 配置。
//
// 两类失败要分开:JSON 解析失败是 400 invalid_agent_config(格式不对),
// 业务校验失败是 422(格式对但内容不合法)。
func (a *AdminHandler) decodeAgentInput(
	w http.ResponseWriter,
	r *http.Request,
	slug string,
	requireSecret bool,
) (*AgentInput, bool) {
	var input AgentInput
	if err := json.NewDecoder(io.LimitReader(r.Body,
		int64(a.State.ConfigSnapshot().MaxRequestBodyBytes)+1)).Decode(&input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_agent_config",
			"Agent 配置格式不正确："+err.Error())
		return nil, false
	}
	// 更新时 slug 由路径决定,忽略请求体里的值。
	if slug != "" {
		input.Slug = slug
	}
	if err := input.Validate(requireSecret); err != nil {
		writeAdminError(w, http.StatusUnprocessableEntity, "invalid_agent_config", err.Error())
		return nil, false
	}
	if status, code, message, ok := a.validateWorkspace(&input); !ok {
		writeAdminError(w, status, code, message)
		return nil, false
	}
	return &input, true
}

// validateWorkspace 校验本地 CLI 执行器的工作区。
//
// 只有「本机 CLI 模式」才会真的在 workspace 里 spawn 子进程,所以只对这类组合
// 要求目录存在且在根目录内;网关模式与进程内处理器只需路径合法(Validate 已保证)。
func (a *AdminHandler) validateWorkspace(input *AgentInput) (int, string, string, bool) {
	config := a.State.ConfigSnapshot()
	if input.ExecutorKind == ExecutorKindOpenAIAgent {
		if !config.OpenAIAgents.Enabled {
			return http.StatusUnprocessableEntity, "openai_agent_executor_disabled",
				"OpenAI 托管智能体执行器尚未在服务端显式启用", false
		}
	}
	if input.ExecutorKind == ExecutorKindClaudeManaged {
		if !config.ClaudeManaged.Enabled {
			return http.StatusUnprocessableEntity, "claude_managed_executor_disabled",
				"Claude 托管智能体执行器尚未在服务端显式启用", false
		}
		if input.PermissionMode == "manual" {
			return http.StatusUnprocessableEntity, "interactive_permission_mode_unsupported",
				"Claude 托管智能体的同步服务端执行不支持 manual 工具确认", false
		}
	}
	localCLI := input.ExecutionMode == ExecutionModeLocal && input.ExecutorKind.SupportsLocalCLI()
	if localCLI {
		if input.ExecutorKind == ExecutorKindClaudeCode && !config.ClaudeEnabled {
			return http.StatusUnprocessableEntity, "claude_executor_disabled",
				"Claude Code 本地执行器尚未在服务端显式启用", false
		}
		root, err := canonicalizePath(config.WorkspaceRoot)
		if err != nil {
			return http.StatusUnprocessableEntity, "invalid_workspace",
				"本地执行器工作区不存在或不可访问", false
		}
		resolved, err := canonicalizePath(filepath.Join(root, input.WorkspacePath))
		if err != nil {
			return http.StatusUnprocessableEntity, "invalid_workspace",
				"本地执行器工作区不存在或不可访问", false
		}
		// 按路径分量比较,避免 /root-evil 被 /root 的字符串前缀误判为通过。
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return http.StatusUnprocessableEntity, "invalid_workspace",
				"本地执行器工作区必须位于配置的根目录内", false
		}
	}
	if !IsAssignmentConfigured(config, input.ExecutorKind, input.ExecutionMode) {
		return http.StatusUnprocessableEntity, "executor_provider_not_configured",
			"请先在智能体中心完成对应执行器配置，再创建或更新岗位智能体", false
	}
	return 0, "", "", true
}

func (a *AdminHandler) createAgent(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requireSession(w, r, true)
	if !ok {
		return
	}
	input, ok := a.decodeAgentInput(w, r, "", true)
	if !ok {
		return
	}

	// 存储变更与 Registry 应用必须串行,否则持久配置与活动注册表会不一致。
	a.State.Mutation.Lock()
	defer a.State.Mutation.Unlock()

	ctx := mutationContext(r)
	existing, err := a.State.Store.GetAgent(ctx, input.Slug)
	if err != nil {
		writeStorageError(w, err, "create agent")
		return
	}
	if existing != nil {
		writeAdminError(w, http.StatusConflict, "slug_exists", "该 slug 已被其他 Agent 使用")
		return
	}
	agent, err := a.State.Store.CreateAgent(ctx, input)
	if err != nil {
		writeStorageError(w, err, "create agent")
		return
	}
	a.State.Registry.Replace(agent)
	a.recordAgentAudit(ctx, session.Username, "agent.create", agent, true)
	writeAdminData(w, http.StatusCreated, agent.ToView(a.State.ConfigSnapshot().PublicBaseURL))
}

func (a *AdminHandler) updateAgent(w http.ResponseWriter, r *http.Request, slug string) {
	session, ok := a.requireSession(w, r, true)
	if !ok {
		return
	}
	input, ok := a.decodeAgentInput(w, r, slug, false)
	if !ok {
		return
	}
	// 请求体带了 signing_secret 就是一次密钥轮换,要记进审计。
	secretRotated := input.SigningSecret != nil

	a.State.Mutation.Lock()
	defer a.State.Mutation.Unlock()

	ctx := mutationContext(r)
	agent, err := a.State.Store.UpdateAgent(ctx, slug, input)
	if err != nil {
		writeStorageError(w, err, "update agent")
		return
	}
	if agent == nil {
		writeAdminError(w, http.StatusNotFound, "agent_not_found", "Agent 不存在")
		return
	}
	a.State.Registry.Replace(agent)
	a.recordAgentAudit(ctx, session.Username, "agent.update", agent, secretRotated)
	writeAdminData(w, http.StatusOK, agent.ToView(a.State.ConfigSnapshot().PublicBaseURL))
}

func (a *AdminHandler) deleteAgent(w http.ResponseWriter, r *http.Request, slug string) {
	session, ok := a.requireSession(w, r, true)
	if !ok {
		return
	}
	a.State.Mutation.Lock()
	defer a.State.Mutation.Unlock()

	ctx := mutationContext(r)
	deleted, err := a.State.Store.DeleteAgent(ctx, slug)
	if err != nil {
		writeStorageError(w, err, "delete agent")
		return
	}
	if !deleted {
		writeAdminError(w, http.StatusNotFound, "agent_not_found", "Agent 不存在")
		return
	}
	a.State.Registry.Remove(slug)
	if err := a.State.Store.RecordAudit(ctx, session.Username,
		"agent.delete", slug, map[string]any{}); err != nil {
		slog.Warn("record agent delete audit failed", "agent", slug, "error", err)
	}
	writeAdminData(w, http.StatusOK, map[string]any{"deleted": true})
}

// mutationContext 返回一个**不随请求取消**的上下文。
//
// rs 侧把 Agent 变更放进 detached task,正是为了让 HTTP 取消无法在
// 「已写库、未应用到 Registry」之间把状态劈成两半。Go 的 handler 不会被中止,
// 但 r.Context() 会在客户端断开时取消并让写库中途失败 —— 所以这里剥掉取消信号,
// 只保留请求携带的值。
func mutationContext(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// recordAgentAudit 记录 Agent 创建/更新的审计明细。
func (a *AdminHandler) recordAgentAudit(
	ctx context.Context,
	actor, action string,
	agent *AgentConfig,
	secretRotated bool,
) {
	if err := a.State.Store.RecordAudit(ctx, actor, action, agent.Slug, map[string]any{
		"revision":      agent.Revision,
		"enabled":       agent.Enabled,
		"executorKind":  agent.ExecutorKind,
		"secretRotated": secretRotated,
	}); err != nil {
		slog.Warn("record agent audit failed", "agent", agent.Slug, "error", err)
	}
}

// ---------------------------------------------------------------------------
// 运行记录与通道事件
// ---------------------------------------------------------------------------

// adminQueryLimit 读取 ?limit=,缺省或不合法时用 50(存储层还会再钳到 1..200)。
func adminQueryLimit(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return 50
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 50
	}
	return limit
}

func (a *AdminHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireSession(w, r, false); !ok {
		return
	}
	runs, err := a.State.Store.ListRuns(r.Context(), adminQueryLimit(r),
		strings.TrimSpace(r.URL.Query().Get("slug")))
	if err != nil {
		writeStorageError(w, err, "list runs")
		return
	}
	writeAdminData(w, http.StatusOK, runs)
}

func (a *AdminHandler) listChannelEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireSession(w, r, false); !ok {
		return
	}
	// URL 参数名是 `type`(与前端一致),不是 event_type。
	events, err := a.State.Store.ListChannelEvents(r.Context(),
		strings.TrimSpace(r.URL.Query().Get("type")),
		strings.TrimSpace(r.URL.Query().Get("slug")),
		adminQueryLimit(r))
	if err != nil {
		writeStorageError(w, err, "list channel events")
		return
	}
	writeAdminData(w, http.StatusOK, events)
}
