package mota

// Agent Pull Connector API 的服务层与 HTTP 端点(/agent-api/v1)。
// 契约见 doc/AGENT-PULL-API.md;存储语义见 agent_pull_store.go。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// PullConnector 是通过令牌鉴权解析出的连接器身份。
type PullConnector struct {
	ID     int64
	Name   string
	Scopes []string
}

// AgentPullService 组合配置、存储与注册表,实现端点逻辑与 dispatcher 集成。
type AgentPullService struct {
	cfg   *AgentAPIConfig
	store *AgentPullStore
	// registry 为 nil 时(理论上不会)仅影响入队路径。
	registry *AgentRegistry
}

// NewAgentPullService 构建服务;registry 供 scope 校验与入队。
func NewAgentPullService(cfg *AgentAPIConfig, store *AgentPullStore, registry *AgentRegistry) *AgentPullService {
	return &AgentPullService{cfg: cfg, store: store, registry: registry}
}

// Config 暴露配置快照。
func (s *AgentPullService) Config() *AgentAPIConfig { return s.cfg }

// Store 暴露存储(reaper 与管理 API 复用)。
func (s *AgentPullService) Store() *AgentPullStore { return s.store }

// ---------------------------------------------------------------------------
// 连接器令牌
// ---------------------------------------------------------------------------

// IssueConnector 签发新令牌;明文仅此一次返回,服务端只存哈希。
func (s *AgentPullService) IssueConnector(ctx context.Context, name string, scopes []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("connector name must not be empty")
	}
	token := "act_" + randomToken(32)
	_, err := s.store.pool.Exec(ctx, `
		INSERT INTO agent_pull_connectors (name, token_hash, scopes, status, created_at)
		VALUES ($1, $2, $3, 'active', $4)
		ON CONFLICT (name) DO UPDATE SET
		  token_hash = $2, scopes = $3, status = 'active', grace_until_ns = NULL`,
		name, tokenHash(token), strings.Join(normalizeScopes(scopes), ","), nowTimestamp())
	if err != nil {
		return "", fmt.Errorf("issue connector token: %w", err)
	}
	return token, nil
}

// RevokeConnector 立即吊销(不宽限)。
func (s *AgentPullService) RevokeConnector(ctx context.Context, name string) error {
	tag, err := s.store.pool.Exec(ctx,
		`UPDATE agent_pull_connectors SET status='revoked' WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("revoke connector: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("connector %q not found", name)
	}
	return nil
}

// ListConnectors 返回管理台展示态(永不含令牌)。
func (s *AgentPullService) ListConnectors(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT id, name, scopes, status, grace_until_ns, created_at
		FROM agent_pull_connectors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list connectors: %w", err)
	}
	defer rows.Close()
	connectors := []map[string]any{}
	for rows.Next() {
		var (
			id          int64
			name, scope string
			status      string
			graceNS     *int64
			createdAt   string
		)
		if err := rows.Scan(&id, &name, &scope, &status, &graceNS, &createdAt); err != nil {
			return nil, err
		}
		item := map[string]any{
			"id": id, "name": name, "scopes": strings.Split(scope, ","), "status": status,
			"created_at": createdAt,
		}
		if graceNS != nil {
			item["grace_until"] = time.Unix(0, *graceNS).UTC().Format(time.RFC3339)
		}
		connectors = append(connectors, item)
	}
	return connectors, rows.Err()
}

// AuthenticateConnector 校验 Bearer 令牌;吊销即失效,轮换宽限期内仍有效(§9)。
func (s *AgentPullService) AuthenticateConnector(ctx context.Context, bearer string) (*PullConnector, error) {
	token := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if token == "" || !strings.HasPrefix(token, "act_") {
		return nil, fmt.Errorf("missing connector token")
	}
	var (
		id     int64
		name   string
		scopes string
		status string
		grace  *int64
	)
	err := s.store.pool.QueryRow(ctx, `
		SELECT id, name, scopes, status, grace_until_ns
		FROM agent_pull_connectors WHERE token_hash = $1`, tokenHash(token)).
		Scan(&id, &name, &scopes, &status, &grace)
	if err != nil {
		return nil, fmt.Errorf("invalid connector token")
	}
	switch status {
	case "active":
	case "rotated":
		if grace == nil || *grace < nowNS() {
			return nil, fmt.Errorf("connector token expired after rotation")
		}
	default:
		return nil, fmt.Errorf("connector token revoked")
	}
	return &PullConnector{ID: id, Name: name, Scopes: normalizeScopes(strings.Split(scopes, ","))}, nil
}

func normalizeScopes(scopes []string) []string {
	cleaned := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			cleaned = append(cleaned, scope)
		}
	}
	if len(cleaned) == 0 {
		return []string{"*"}
	}
	return cleaned
}

func scopeAllows(slugs []string, slug string) bool {
	for _, s := range slugs {
		if s == "*" || s == slug {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP 装配
// ---------------------------------------------------------------------------

// AgentPullRouter 挂载 /agent-api/v1 四端点。
func AgentPullRouter(service *AgentPullService) http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connector, err := service.AuthenticateConnector(r.Context(), r.Header.Get("Authorization"))
			if err != nil {
				writePullError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(withPullConnector(r.Context(), connector)))
		})
	})
	router.Post("/claim", service.handleClaim)
	router.Post("/renew", service.handleRenew)
	router.Post("/complete", service.handleComplete)
	router.Post("/heartbeat", service.handleHeartbeat)
	return router
}

type pullConnectorKey struct{}

func withPullConnector(ctx context.Context, connector *PullConnector) context.Context {
	return context.WithValue(ctx, pullConnectorKey{}, connector)
}

func pullConnectorFrom(ctx context.Context) *PullConnector {
	connector, _ := ctx.Value(pullConnectorKey{}).(*PullConnector)
	return connector
}

func writePullError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message})
}

func writePullJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// claimRequest 是 /claim 请求体。
type claimRequest struct {
	WorkerID       string   `json:"workerId"`
	MaxWaitSeconds int      `json:"maxWaitSeconds"`
	InFlight       int      `json:"inFlight"`
	Capacity       int      `json:"capacity"`
	PreferredSlugs []string `json:"preferredSlugs"`
}

// handleClaim 长轮询认领(§1)。
func (s *AgentPullService) handleClaim(w http.ResponseWriter, r *http.Request) {
	connector := pullConnectorFrom(r.Context())
	var req claimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writePullError(w, http.StatusBadRequest, "invalid_request", "claim body must be valid JSON")
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if len(req.WorkerID) < 2 || len(req.WorkerID) > 64 || !validWorkerID(req.WorkerID) {
		writePullError(w, http.StatusBadRequest, "invalid_worker_id",
			"workerId must be 2-64 chars of [a-z0-9-]")
		return
	}
	slugs := s.claimableSlugs(connector, req.PreferredSlugs)
	if len(slugs) == 0 {
		// scope 与可用队列不交叉:给出可诊断的空转标记。
		w.Header().Set("X-Agent-Api-Reason", "no-scope-match")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	maxWait := s.cfg.ClaimMaxWait()
	if req.MaxWaitSeconds > 0 && time.Duration(req.MaxWaitSeconds)*time.Second < maxWait {
		maxWait = time.Duration(req.MaxWaitSeconds) * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for {
		// 双实例分叉检测:服务端可见活跃租约多于自报在途数。
		if mismatch, err := s.store.WorkerMismatchLeases(r.Context(), req.WorkerID, req.InFlight); err == nil && mismatch > 0 {
			writePullError(w, http.StatusConflict, "WORKER_LEASE_ACTIVE",
				fmt.Sprintf("worker %s holds %d active leases but reported %d in flight; likely duplicate instance",
					req.WorkerID, mismatch, req.InFlight))
			return
		}
		task, outcome, err := s.store.ClaimTask(r.Context(), req.WorkerID, slugs, req.Capacity, s.cfg)
		if err != nil {
			writePullError(w, http.StatusInternalServerError, "storage_error", "claim failed")
			return
		}
		switch outcome {
		case ClaimDelivered:
			writePullJSON(w, http.StatusOK, s.claimResponse(task))
			return
		case ClaimRateLimited:
			w.Header().Set("Retry-After", "2")
			writePullError(w, http.StatusTooManyRequests, "rate_limited",
				"worker or global outstanding lease cap reached")
			return
		}
		if time.Now().After(deadline) {
			w.Header().Set("X-Agent-Api-Reason", "empty")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func validWorkerID(id string) bool {
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// claimableSlugs 求令牌 scope 与请求 preferred 的交集;preferred 为空取全部 scope。
func (s *AgentPullService) claimableSlugs(connector *PullConnector, preferred []string) []string {
	allowed := make([]string, 0, len(connector.Scopes))
	for _, slug := range connector.Scopes {
		if len(preferred) == 0 {
			allowed = append(allowed, slug)
			continue
		}
		for _, want := range preferred {
			if strings.ToLower(strings.TrimSpace(want)) == slug {
				allowed = append(allowed, slug)
				break
			}
		}
	}
	// 通配 scope 展开为注册表中的 remote-pull Agent。
	if len(allowed) == 1 && allowed[0] == "*" {
		expanded := []string{}
		if s.registry != nil {
			for _, agent := range s.registry.List() {
				if agent.Config.ExecutionMode == ExecutionModeRemotePull {
					expanded = append(expanded, agent.Config.Slug)
				}
			}
		}
		return expanded
	}
	return allowed
}

func (s *AgentPullService) claimResponse(task *PullTask) map[string]any {
	var input any
	if err := json.Unmarshal([]byte(task.PayloadJSON), &input); err != nil {
		input = map[string]any{"raw": task.PayloadJSON}
	}
	return map[string]any{
		"schemaVersion":     "agent-api.claim.v1",
		"runId":             task.RunID,
		"agentSlug":         task.AgentSlug,
		"agentRevision":     task.AgentRevision,
		"executorKind":      "remote-pull",
		"input":             input,
		"attempts":          task.Attempts,
		"executionDeadline": nsToRFC3339(task.ExecutionDeadNS),
		"leaseExpireAt":     nsToRFC3339(task.LeaseExpiresNS),
		"leaseSeconds":      s.cfg.LeaseSeconds,
		"renewAfterSeconds": int(s.cfg.RenewInterval().Seconds()),
	}
}

func nsToRFC3339(ns int64) string {
	if ns <= 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// handleRenew 续租(§2)。
func (s *AgentPullService) handleRenew(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.Header.Get("X-Agent-Run-ID"))
	if runID == "" {
		writePullError(w, http.StatusBadRequest, "invalid_request", "X-Agent-Run-ID header is required")
		return
	}
	workerID := strings.TrimSpace(r.Header.Get("X-Agent-Worker-ID"))
	if workerID == "" {
		writePullError(w, http.StatusBadRequest, "invalid_request", "X-Agent-Worker-ID header is required")
		return
	}
	outcome, err := s.store.RenewLease(r.Context(), runID, workerID, s.cfg)
	if err != nil {
		writePullError(w, http.StatusInternalServerError, "storage_error", "renew failed")
		return
	}
	switch outcome {
	case RenewOK:
		writePullJSON(w, http.StatusOK, map[string]any{
			"abort": false, "leaseSeconds": s.cfg.LeaseSeconds,
			"serverTime": time.Now().UTC().Format(time.RFC3339),
		})
	case RenewAbort:
		writePullJSON(w, http.StatusOK, map[string]any{
			"abort": true, "serverTime": time.Now().UTC().Format(time.RFC3339),
		})
	case RenewLeaseLost:
		writePullError(w, http.StatusConflict, "LEASE_LOST", "lease expired and task was re-dispatched")
	case RenewDeadline:
		writePullError(w, http.StatusConflict, "TASK_DEADLINE_EXCEEDED", "execution deadline reached")
	default:
		writePullError(w, http.StatusNotFound, "run_not_found", "run is unknown or already terminal")
	}
}

// completeRequest 是 /complete 请求体(§3)。
type completeRequest struct {
	Status  string          `json:"status"`
	Payload json.RawMessage `json:"payload"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage   *pullUsage   `json:"usage"`
	Metrics *pullMetrics `json:"metrics"`
}

type pullUsage struct {
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
	TotalTokens  uint64 `json:"totalTokens"`
}

type pullMetrics struct {
	DurationMS int64 `json:"durationMs"`
}

// handleComplete 幂等回传(§3)。
func (s *AgentPullService) handleComplete(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.Header.Get("X-Agent-Run-ID"))
	workerID := strings.TrimSpace(r.Header.Get("X-Agent-Worker-ID"))
	if runID == "" || workerID == "" {
		writePullError(w, http.StatusBadRequest, "invalid_request",
			"X-Agent-Run-ID and X-Agent-Worker-ID headers are required")
		return
	}
	var req completeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024)).Decode(&req); err != nil {
		writePullError(w, http.StatusBadRequest, "invalid_request", "complete body must be valid JSON")
		return
	}
	switch req.Status {
	case "succeeded", "failed", "aborted":
	default:
		writePullError(w, http.StatusBadRequest, "invalid_status",
			"status must be succeeded, failed, or aborted")
		return
	}
	if req.Status == "succeeded" && len(req.Payload) == 0 {
		writePullError(w, http.StatusBadRequest, "invalid_request", "payload is required for succeeded")
		return
	}
	if req.Status == "failed" && (req.Error == nil || req.Error.Code == "") {
		writePullError(w, http.StatusBadRequest, "invalid_request", "error.code is required for failed")
		return
	}

	// 规范化结果文档:幂等重放按此文档字节比较。
	resultDoc := map[string]any{
		"status":  req.Status,
		"payload": json.RawMessage(req.Payload),
		"usage":   req.Usage,
		"metrics": req.Metrics,
	}
	if req.Error != nil {
		resultDoc["error"] = req.Error
	}
	normalized, err := json.Marshal(resultDoc)
	if err != nil {
		writePullError(w, http.StatusBadRequest, "invalid_request", "result is not serializable")
		return
	}
	errorCode := ""
	if req.Error != nil {
		errorCode = req.Error.Code
	}
	outcome, err := s.store.CompleteTask(r.Context(), runID, workerID, req.Status, string(normalized), errorCode)
	if err != nil {
		if err.Error() == "pgx: no rows in result set" {
			writePullError(w, http.StatusNotFound, "run_not_found", "run is unknown")
			return
		}
		writePullError(w, http.StatusInternalServerError, "storage_error", "complete failed")
		return
	}
	if outcome.Conflict {
		writePullError(w, http.StatusConflict, "RESULT_CONFLICT",
			"a different result was already accepted for this run")
		return
	}
	writePullJSON(w, http.StatusOK, map[string]any{
		"accepted":  outcome.Accepted,
		"runStatus": outcome.RunStatus,
		"runMsg":    nil,
	})
}

// heartbeatRequest 是 /heartbeat 请求体(§4)。
type heartbeatRequest struct {
	WorkerID     string `json:"workerId"`
	InFlight     int    `json:"inFlight"`
	Capacity     int    `json:"capacity"`
	Capabilities *struct {
		Slugs   []string `json:"slugs"`
		Version string   `json:"version"`
	} `json:"capabilities"`
}

// handleHeartbeat 存活与容量上报;首次心跳即注册(§4)。
func (s *AgentPullService) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	connector := pullConnectorFrom(r.Context())
	var req heartbeatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writePullError(w, http.StatusBadRequest, "invalid_request", "heartbeat body must be valid JSON")
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if !validWorkerID(req.WorkerID) {
		writePullError(w, http.StatusBadRequest, "invalid_worker_id",
			"workerId must be 2-64 chars of [a-z0-9-]")
		return
	}
	capabilities := ""
	if req.Capabilities != nil {
		capabilities = strings.Join([]string{
			strings.Join(req.Capabilities.Slugs, ","), req.Capabilities.Version}, "|")
	}
	if err := s.store.Heartbeat(r.Context(), req.WorkerID, connector.ID,
		req.InFlight, req.Capacity, capabilities); err != nil {
		writePullError(w, http.StatusInternalServerError, "storage_error", "heartbeat failed")
		return
	}
	depth, err := s.store.QueueDepth(r.Context(), s.claimableSlugs(connector, nil))
	if err != nil {
		depth = 0
	}
	writePullJSON(w, http.StatusOK, map[string]any{
		"heartbeatIntervalSeconds": 60,
		"renewIntervalSeconds":     int(s.cfg.RenewInterval().Seconds()),
		"serverTime":               time.Now().UTC().Format(time.RFC3339),
		"queueDepth":               depth,
	})
}

// ---------------------------------------------------------------------------
// dispatcher 集成:remote-pull 执行路径
// ---------------------------------------------------------------------------

// ExecuteRemote 入队并做有界同步等待(§5.2):push/A2A/网关同步入口统一经此
// 拿结果;等待超窗任务继续执行,结果落 runs,不回滚已入队状态。
func (s *AgentPullService) ExecuteRemote(
	ctx context.Context,
	agent *AgentConfig,
	task *TaskContext,
	timeout time.Duration,
) (*ProcessedOutput, error) {
	runID := "run_" + randomToken(12)
	if task.RunID != nil && strings.TrimSpace(*task.RunID) != "" {
		runID = strings.TrimSpace(*task.RunID)
	}
	payload, err := json.Marshal(map[string]any{
		"taskBrief":      task.TaskBrief,
		"params":         task.Params,
		"positionType":   task.PositionType,
		"language":       task.Language,
		"symbolPool":     task.SymbolPool,
		"universeMode":   task.UniverseMode,
		"runtimeContext": task.RuntimeContext,
	})
	if err != nil {
		return nil, newDispatchError("invalid_input", "encode remote task payload: "+err.Error())
	}
	if err := s.store.EnqueueTask(ctx, runID, agent, string(payload), s.cfg.QueueTTL()); err != nil {
		if err == PullOverloadedError {
			return nil, newDispatchError("agent_overloaded",
				fmt.Sprintf("Agent %s remote queue is full", agent.Slug))
		}
		return nil, newDispatchError("executor_unavailable", err.Error())
	}

	waitBudget := s.cfg.SyncHoldMax()
	if timeout > 0 && timeout < waitBudget {
		waitBudget = timeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitBudget)
	defer cancel()
	state, resultJSON, err := s.store.WaitTerminal(waitCtx, runID)
	if err != nil {
		// 等待超窗/调用方断开:任务仍在远端执行,结果会落 runs。
		return nil, newDispatchError("execution_timeout",
			fmt.Sprintf("remote task %s still running after sync hold window", runID))
	}
	return remoteResultToOutput(state, resultJSON)
}

// remoteResultToOutput 把 complete 结果文档映射回 ProcessedOutput。
func remoteResultToOutput(state PullTaskState, resultJSON string) (*ProcessedOutput, error) {
	var doc completeRequest
	if err := json.Unmarshal([]byte(resultJSON), &doc); err != nil {
		return nil, newDispatchError("invalid_executor_output", "remote result is not valid JSON")
	}
	if state != PullTaskCompleted {
		code := "executor_failed"
		message := "remote execution failed"
		if doc.Error != nil {
			if doc.Error.Code != "" {
				code = doc.Error.Code
			}
			if doc.Error.Message != "" {
				message = doc.Error.Message
			}
		} else if state == PullTaskCanceled {
			code, message = "aborted", "remote execution was canceled"
		}
		return nil, newDispatchError(code, message)
	}
	output := &ProcessedOutput{PayloadResult: json.RawMessage(doc.Payload)}
	if doc.Usage != nil {
		output.Usage = &BoundRunUsage{
			SchemaVersion:    "mota-bound-usage-v2",
			Status:           "FINAL",
			PromptTokens:     doc.Usage.InputTokens,
			CompletionTokens: doc.Usage.OutputTokens,
		}
		if output.Usage.PromptTokens+output.Usage.CompletionTokens == 0 &&
			doc.Usage.TotalTokens > 0 {
			output.Usage.PromptTokens = doc.Usage.TotalTokens
		}
	}
	return output, nil
}

// ---------------------------------------------------------------------------
// reaper
// ---------------------------------------------------------------------------

// RunPullReaper 周期回收过期租约/队列 TTL 与离线 worker;间隔 = 续租间隔。
func RunPullReaper(ctx context.Context, service *AgentPullService) {
	interval := service.cfg.RenewInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			stats, err := service.store.SweepExpired(sweepCtx, service.cfg)
			cancel()
			if err != nil {
				slog.Warn("pull reaper sweep failed", "err", err)
				continue
			}
			if stats.Requeued+stats.TerminalLease+stats.TerminalQueue > 0 {
				slog.Info("pull reaper sweep",
					"requeued", stats.Requeued,
					"terminal_lease", stats.TerminalLease,
					"terminal_queue", stats.TerminalQueue)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 管理 API:连接器令牌签发/列表/吊销与 worker 状态(复用管理会话鉴权)
// ---------------------------------------------------------------------------

// listAgentConnectors GET /api/admin/agent-connectors
func (a *AdminHandler) listAgentConnectors(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r, false); !ok {
		return
	}
	if a.State == nil || a.State.Pull == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "pull_disabled",
			"AGENT_API is disabled on this server")
		return
	}
	connectors, err := a.State.Pull.ListConnectors(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	writeAdminData(w, http.StatusOK, connectors)
}

// createAgentConnectorRequest 是签发请求体。
type createAgentConnectorRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// createAgentConnector POST /api/admin/agent-connectors
// 令牌明文仅在响应中出现一次,服务端只存哈希。
func (a *AdminHandler) createAgentConnector(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r, true); !ok {
		return
	}
	if a.State == nil || a.State.Pull == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "pull_disabled",
			"AGENT_API is disabled on this server")
		return
	}
	var req createAgentConnectorRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "body must be valid JSON")
		return
	}
	token, err := a.State.Pull.IssueConnector(r.Context(), req.Name, req.Scopes)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_connector", err.Error())
		return
	}
	writeAdminData(w, http.StatusCreated, map[string]any{
		"name": strings.TrimSpace(req.Name), "token": token,
		"notice": "token is shown only once; store it securely",
	})
}

// revokeAgentConnector DELETE /api/admin/agent-connectors/{name}
func (a *AdminHandler) revokeAgentConnector(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := a.requireAdminSession(w, r, true); !ok {
		return
	}
	if a.State == nil || a.State.Pull == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "pull_disabled",
			"AGENT_API is disabled on this server")
		return
	}
	if err := a.State.Pull.RevokeConnector(r.Context(), name); err != nil {
		writeAdminError(w, http.StatusNotFound, "connector_not_found", err.Error())
		return
	}
	writeAdminData(w, http.StatusOK, map[string]any{"revoked": name})
}

// listAgentWorkers GET /api/admin/agent-workers
func (a *AdminHandler) listAgentWorkers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r, false); !ok {
		return
	}
	if a.State == nil || a.State.Pull == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "pull_disabled",
			"AGENT_API is disabled on this server")
		return
	}
	workers, err := a.State.Pull.Store().ListWorkers(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	writeAdminData(w, http.StatusOK, workers)
}
