package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/agentapi"
)

// AgentAPI 端点常量与线协议头名（AGENT-PULL-API v1）。
const (
	agentRunIDHeader  = "X-Agent-Run-ID"
	agentWorkerHeader = "X-Agent-Worker-ID"
	reasonHeader      = "X-Agent-Api-Reason"

	claimBodyLimit     = 64 * 1024
	completeBodyLimit  = 4 * 1024 * 1024
	heartbeatBodyLimit = 64 * 1024
	runsBodyLimit      = 4 * 1024 * 1024

	agentSlugHeader    = "X-Agent-Slug"
	agentClusterHeader = "X-Agent-Cluster"
)

// AgentAPIHandler 是 /agent-api/v1 五端点的 HTTP 适配。
type AgentAPIHandler struct {
	Service *agentapi.Service
}

// Router 挂载 claim / renew / complete / heartbeat（Bearer 鉴权中间件先行）。
func (h AgentAPIHandler) Router() chi.Router {
	router := chi.NewRouter()
	router.Use(h.authenticate)
	router.Post("/claim", h.handleClaim)
	router.Post("/renew", h.handleRenew)
	router.Post("/complete", h.handleComplete)
	router.Post("/runs", h.handleSubmitRun)
	router.Post("/heartbeat", h.handleHeartbeat)
	return router
}

type principalKey struct{}

func (h AgentAPIHandler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hints []agentapi.AgentKeyHint
		bearer := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer ")), "act_") {
			// 智能体密钥连接点：凭据指向哪个智能体由头声明，可选集群子密钥。
			hints = []agentapi.AgentKeyHint{{
				Slug:    r.Header.Get(agentSlugHeader),
				Cluster: r.Header.Get(agentClusterHeader),
			}}
		}
		principal, failure := h.Service.Authenticate(r.Context(), bearer, hints...)
		if failure != nil {
			writeAgentAPIError(w, failure)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func principalFrom(r *http.Request) *agentapi.Principal {
	principal, _ := r.Context().Value(principalKey{}).(*agentapi.Principal)
	return principal
}

// claimRequest 是 /claim 请求体（§1）。
type claimRequest struct {
	WorkerID       string   `json:"workerId"`
	MaxWaitSeconds int      `json:"maxWaitSeconds"`
	InFlight       int      `json:"inFlight"`
	Capacity       int      `json:"capacity"`
	PreferredSlugs []string `json:"preferredSlugs"`
}

func (h AgentAPIHandler) handleClaim(w http.ResponseWriter, r *http.Request) {
	var request claimRequest
	if err := decodeLimited(r.Body, claimBodyLimit, &request); err != nil {
		writeAgentAPIError(w, &agentapi.Failure{Status: 400, Code: "invalid_request",
			Message: "claim body must be valid JSON"})
		return
	}
	task, failure := h.Service.Claim(r.Context(), principalFrom(r), agentapi.ClaimInput{
		WorkerID: request.WorkerID, MaxWaitSeconds: request.MaxWaitSeconds,
		InFlight: request.InFlight, Capacity: request.Capacity,
		PreferredSlugs: request.PreferredSlugs,
	})
	if failure != nil {
		if failure.Status == http.StatusNoContent {
			if failure.EmptyReason != "" {
				w.Header().Set(reasonHeader, failure.EmptyReason)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeAgentAPIError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, claimResponse(task))
}

// claimResponse 渲染 agent-api.claim.v1 信封。
func claimResponse(task *agentapi.ClaimedTask) map[string]any {
	var input any
	if err := json.Unmarshal(task.Input, &input); err != nil {
		input = map[string]any{"raw": string(task.Input)}
	}
	return map[string]any{
		"schemaVersion":     "agent-api.claim.v1",
		"runId":             task.RunID,
		"agentSlug":         task.AgentSlug,
		"agentRevision":     task.AgentRevision,
		"executorKind":      task.ExecutorKind,
		"input":             input,
		"attempts":          task.Attempts,
		"executionDeadline": rfc3339OrEmpty(task.ExecutionDeadline),
		"leaseExpireAt":     rfc3339OrEmpty(task.LeaseExpiresAt),
		"leaseSeconds":      task.LeaseSeconds,
		"renewAfterSeconds": task.RenewAfterSeconds,
	}
}

func rfc3339OrEmpty(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (h AgentAPIHandler) handleRenew(w http.ResponseWriter, r *http.Request) {
	result, failure := h.Service.Renew(r.Context(),
		r.Header.Get(agentRunIDHeader), r.Header.Get(agentWorkerHeader))
	if failure != nil {
		writeAgentAPIError(w, failure)
		return
	}
	body := map[string]any{
		"abort":      result.Abort,
		"serverTime": result.ServerTime.UTC().Format(time.RFC3339),
	}
	if !result.Abort {
		body["leaseSeconds"] = result.LeaseSeconds
	}
	writeJSON(w, http.StatusOK, body)
}

// completeRequest 是 /complete 请求体（§3）。payload 保持原始字节——
// 规范化结果文档按字节做幂等重放比较，重排键序会改变冲突判定语义。
type completeRequest struct {
	Status  string                    `json:"status"`
	Payload json.RawMessage           `json:"payload"`
	Error   *agentapi.CompleteError   `json:"error"`
	Usage   *agentapi.CompleteUsage   `json:"usage"`
	Metrics *agentapi.CompleteMetrics `json:"metrics"`
}

func (h AgentAPIHandler) handleComplete(w http.ResponseWriter, r *http.Request) {
	var request completeRequest
	if err := decodeLimited(r.Body, completeBodyLimit, &request); err != nil {
		writeAgentAPIError(w, &agentapi.Failure{Status: 400, Code: "invalid_request",
			Message: "complete body must be valid JSON"})
		return
	}
	result, failure := h.Service.Complete(r.Context(),
		r.Header.Get(agentRunIDHeader), r.Header.Get(agentWorkerHeader),
		agentapi.CompleteInput{
			Status: request.Status, Payload: request.Payload,
			Error: request.Error, Usage: request.Usage, Metrics: request.Metrics,
		})
	if failure != nil {
		writeAgentAPIError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":  result.Accepted,
		"runStatus": result.RunStatus,
		"runMsg":    nil,
	})
}

// heartbeatRequest 是 /heartbeat 请求体（§4）。
type heartbeatRequest struct {
	WorkerID     string `json:"workerId"`
	InFlight     int    `json:"inFlight"`
	Capacity     int    `json:"capacity"`
	Capabilities *struct {
		Slugs   []string `json:"slugs"`
		Version string   `json:"version"`
	} `json:"capabilities"`
}

func (h AgentAPIHandler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var request heartbeatRequest
	if err := decodeLimited(r.Body, heartbeatBodyLimit, &request); err != nil {
		writeAgentAPIError(w, &agentapi.Failure{Status: 400, Code: "invalid_request",
			Message: "heartbeat body must be valid JSON"})
		return
	}
	input := agentapi.HeartbeatInput{
		WorkerID: request.WorkerID, InFlight: request.InFlight, Capacity: request.Capacity,
	}
	if request.Capabilities != nil {
		input.Slugs = request.Capabilities.Slugs
		input.Version = request.Capabilities.Version
	}
	result, failure := h.Service.Heartbeat(r.Context(), principalFrom(r), input)
	if failure != nil {
		writeAgentAPIError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"heartbeatIntervalSeconds": result.HeartbeatIntervalSeconds,
		"renewIntervalSeconds":     result.RenewIntervalSeconds,
		"serverTime":               result.ServerTime.UTC().Format(time.RFC3339),
		"queueDepth":               result.QueueDepth,
	})
}

// submitRunRequest 是 /runs 请求体（§4.5，机器侧直接提交）。
type submitRunRequest struct {
	Slug    string                    `json:"slug"`
	Status  string                    `json:"status"`
	RunID   string                    `json:"runId"`
	Payload json.RawMessage           `json:"payload"`
	Input   json.RawMessage           `json:"input"`
	Error   *agentapi.CompleteError   `json:"error"`
	Usage   *agentapi.CompleteUsage   `json:"usage"`
	Metrics *agentapi.CompleteMetrics `json:"metrics"`
}

func (h AgentAPIHandler) handleSubmitRun(w http.ResponseWriter, r *http.Request) {
	var request submitRunRequest
	if err := decodeLimited(r.Body, runsBodyLimit, &request); err != nil {
		writeAgentAPIError(w, &agentapi.Failure{Status: 400, Code: "invalid_request",
			Message: "runs body must be valid JSON"})
		return
	}
	result, failure := h.Service.SubmitRun(r.Context(), principalFrom(r), agentapi.SubmitRunInput{
		Slug: request.Slug, Status: request.Status, RunID: request.RunID,
		Payload: request.Payload, Input: request.Input,
		Error: request.Error, Usage: request.Usage, Metrics: request.Metrics,
	})
	if failure != nil {
		writeAgentAPIError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":  result.Accepted,
		"runId":     result.RunID,
		"runStatus": result.RunStatus,
	})
}

// writeAgentAPIError 渲染协议错误体 {"code","message"}（旧栈线格式）。
func writeAgentAPIError(w http.ResponseWriter, failure *agentapi.Failure) {
	if failure.RetryAfter != "" {
		w.Header().Set("Retry-After", failure.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(failure.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": failure.Code, "message": failure.Message,
	})
}

func decodeLimited(body io.ReadCloser, limit int64, target any) error {
	defer func() { _ = body.Close() }()
	return json.NewDecoder(io.LimitReader(body, limit)).Decode(target)
}
