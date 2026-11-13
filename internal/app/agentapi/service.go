// Package agentapi 实现 Agent Pull Connector API（/agent-api/v1）的用例层：
// 令牌鉴权、claim 长轮询、renew 取消传播、complete 幂等回执、heartbeat
// 注册与自适应间隔，以及后台清扫循环（reaper）。
//
// HTTP 拆解（Bearer 头提取、请求体限长解码、状态码/诊断头渲染）属于 api
// 层（P6）；本包以 Failure 值携带状态码与协议错误码，全部路径可脱离
// HTTP 单测。协议契约见 doc/AGENT-PULL-API.md。
package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	domain "ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/exec"
)

// Config 是 agent-api 的策略配置（装配层从 infra/config 的 AgentAPIConfig
// 映射注入；app 层不依赖配置包）。
type Config struct {
	// LeaseSeconds 是租约时长（默认 90）。
	LeaseSeconds int
	// RenewInterval 是建议续租间隔（= lease/3，默认 30s），也是 reaper 节奏。
	RenewInterval time.Duration
	// HeartbeatIntervalSeconds 是下发给连接器的心跳间隔（默认 60）。
	HeartbeatIntervalSeconds int
	// ClaimMaxWait 是长轮询窗口服务端上限（1–35s，默认 30s）；
	// 请求值更小时取请求值。
	ClaimMaxWait time.Duration
	// MaxOutstandingLeases 是全局在途租约上限（429 判定）。
	MaxOutstandingLeases int
	// MaxAttempts 是租约过期重派上限。
	MaxAttempts int
	// QueueTTL 是入队 → 首次派发存活期。
	QueueTTL time.Duration
	// WorkerOfflineAfterMiss 是心跳丢失离线判定倍数。
	WorkerOfflineAfterMiss int
}

// LeaseTTL 返回租约时长。
func (c Config) LeaseTTL() time.Duration {
	seconds := c.LeaseSeconds
	if seconds <= 0 {
		seconds = domain.DefaultLeaseSeconds
	}
	return time.Duration(seconds) * time.Second
}

// Principal 是一个已鉴权的连接器。
type Principal struct {
	ID     string
	Name   string
	Scopes []string
}

// Failure 是一次结构化拒绝。Status 是 HTTP 状态码，Code/Message 是协议
// 错误体（{"code":…,"message":…}），RetryAfter 是 429 提示，EmptyReason
// 是 204 的诊断头取值（X-Agent-Api-Reason）。
type Failure struct {
	Status      int
	Code        string
	Message     string
	RetryAfter  string
	EmptyReason string
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }

func failure(status int, code, message string) *Failure {
	return &Failure{Status: status, Code: code, Message: message}
}

// RemoteAgentLister 提供通配 scope 的展开来源：当前注册的 remote-pull
// 智能体 slug 集合（P6 由 agent 仓储适配）。
type RemoteAgentLister interface {
	RemotePullSlugs(ctx context.Context) ([]string, error)
}

// Service 是 agent-api 用例服务。并发安全。
type Service struct {
	cfg     Config
	queue   domain.TaskQueue
	tokens  domain.TokenRepository
	workers domain.WorkerRegistry
	remote  RemoteAgentLister
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Config  Config
	Queue   domain.TaskQueue
	Tokens  domain.TokenRepository
	Workers domain.WorkerRegistry
	Remote  RemoteAgentLister
}

// NewService 组装 agent-api 用例服务。
func NewService(deps Deps) *Service {
	return &Service{cfg: deps.Config, queue: deps.Queue, tokens: deps.Tokens,
		workers: deps.Workers, remote: deps.Remote}
}

// Sweeper 暴露队列给 reaper 与 dashboard 复用。
// Queue 暴露队列给 reaper 与 dashboard 复用。
func (s *Service) Queue() domain.TaskQueue { return s.queue }

// ListOnlineWorkers 返回活跃心跳的连接器 worker（管理台连接状态数据源）。
func (s *Service) ListOnlineWorkers(ctx context.Context) ([]domain.Worker, error) {
	return s.workers.ListOnline(ctx)
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

// Authenticate 校验 Bearer 令牌；吊销即失效，轮换宽限期内仍有效（§9）。
func (s *Service) Authenticate(ctx context.Context, bearer string) (*Principal, *Failure) {
	token := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if token == "" || !strings.HasPrefix(token, domain.TokenPrefix) {
		return nil, failure(401, "unauthorized", "missing connector token")
	}
	stored, err := s.tokens.GetByHash(ctx, domain.HashToken(token))
	if err != nil {
		return nil, failure(401, "unauthorized", "invalid connector token")
	}
	if !stored.Active(time.Now()) {
		message := "connector token revoked"
		if stored.GraceUntil != nil {
			message = "connector token expired after rotation"
		}
		return nil, failure(401, "unauthorized", message)
	}
	return &Principal{ID: stored.ID, Name: stored.Name, Scopes: normalizeScopes(stored.Scopes)}, nil
}

// IssueToken 签发新令牌；明文仅此一次返回，服务端只存哈希。
// 同名再签发即轮换（旧哈希立即失效）。
func (s *Service) IssueToken(ctx context.Context, name string, scopes []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("connector name must not be empty")
	}
	plaintext := domain.TokenPrefix + randomToken(32)
	record := &domain.Token{
		Name:   name,
		Hash:   domain.HashToken(plaintext),
		Scopes: normalizeScopes(scopes),
	}
	if err := s.tokens.Issue(ctx, record); err != nil {
		return "", fmt.Errorf("issue connector token: %w", err)
	}
	return plaintext, nil
}

// RevokeToken 立即吊销（无宽限）。
func (s *Service) RevokeToken(ctx context.Context, name string) error {
	return s.tokens.RevokeByName(ctx, name)
}

// ListTokens 返回令牌记录（永不含明文/哈希外的敏感材料；展示态由 api 层组装）。
func (s *Service) ListTokens(ctx context.Context) ([]domain.Token, error) {
	return s.tokens.List(ctx)
}

// ---------------------------------------------------------------------------
// claim（§1）
// ---------------------------------------------------------------------------

// ClaimInput 是一次认领请求。
type ClaimInput struct {
	WorkerID       string
	MaxWaitSeconds int
	InFlight       int
	Capacity       int
	PreferredSlugs []string
}

// ClaimedTask 是认领成功的任务视图（api 层渲染 agent-api.claim.v1）。
type ClaimedTask struct {
	RunID             string
	AgentSlug         string
	AgentRevision     int64
	ExecutorKind      string
	Input             json.RawMessage
	Attempts          int
	ExecutionDeadline time.Time
	LeaseExpiresAt    time.Time
	LeaseSeconds      int
	RenewAfterSeconds int
}

// Claim 长轮询认领任务。
//
// 返回语义：
//   - (*ClaimedTask, nil)：认领成功（200）；
//   - (nil, nil)：窗口内无任务（204，EmptyReason 在返回的 Failure 里——
//     为保持两值签名，空窗口以 Status=204 的 Failure 表达，api 层见
//     Status==204 时只写诊断头不写错误体）；
//   - (nil, *Failure)：其余拒绝（400/401/409/429/500）。
func (s *Service) Claim(ctx context.Context, principal *Principal, in ClaimInput) (*ClaimedTask, *Failure) {
	workerID := strings.TrimSpace(in.WorkerID)
	if len(workerID) < 2 || len(workerID) > 64 || !validWorkerID(workerID) {
		return nil, failure(400, "invalid_worker_id", "workerId must be 2-64 chars of [a-z0-9-]")
	}
	slugs := s.claimableSlugs(ctx, principal, in.PreferredSlugs)
	if len(slugs) == 0 {
		// scope 与可用队列不交叉：给出可诊断的空转标记。
		return nil, &Failure{Status: 204, EmptyReason: "no-scope-match"}
	}

	maxWait := s.claimMaxWait()
	if in.MaxWaitSeconds > 0 && time.Duration(in.MaxWaitSeconds)*time.Second < maxWait {
		maxWait = time.Duration(in.MaxWaitSeconds) * time.Second
	}
	deadline := time.Now().Add(maxWait)
	policy := domain.ClaimPolicy{
		LeaseTTL:             s.cfg.LeaseTTL(),
		MaxOutstandingLeases: s.cfg.MaxOutstandingLeases,
	}
	for {
		// 双实例分叉检测：服务端可见活跃租约多于自报在途数。
		if held, err := s.queue.WorkerLeaseCount(ctx, workerID); err == nil && held > in.InFlight {
			return nil, failure(409, "WORKER_LEASE_ACTIVE",
				fmt.Sprintf("worker %s holds %d active leases but reported %d in flight; likely duplicate instance",
					workerID, held, in.InFlight))
		}
		task, outcome, err := s.queue.Claim(ctx, domain.ClaimRequest{
			WorkerID: workerID, Slugs: slugs, Capacity: in.Capacity, Policy: policy,
		})
		if err != nil {
			return nil, failure(500, "storage_error", "claim failed")
		}
		switch outcome {
		case domain.ClaimDelivered:
			return s.claimResult(task), nil
		case domain.ClaimRateLimited:
			return nil, &Failure{Status: 429, Code: "rate_limited",
				Message: "worker or global outstanding lease cap reached", RetryAfter: "2"}
		}
		if time.Now().After(deadline) {
			return nil, &Failure{Status: 204, EmptyReason: "empty"}
		}
		select {
		case <-ctx.Done():
			return nil, failure(499, "client_gone", "claim wait canceled")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Service) claimResult(task *domain.Task) *ClaimedTask {
	leaseSeconds := s.cfg.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = domain.DefaultLeaseSeconds
	}
	renewAfter := int(s.renewInterval().Seconds())
	return &ClaimedTask{
		RunID: task.RunID, AgentSlug: task.AgentSlug, AgentRevision: task.AgentRevision,
		ExecutorKind: string(exec.KindRemotePull), Input: task.Payload, Attempts: task.Attempts,
		ExecutionDeadline: task.ExecutionDeadline, LeaseExpiresAt: task.LeaseExpiresAt,
		LeaseSeconds: leaseSeconds, RenewAfterSeconds: renewAfter,
	}
}

func (s *Service) claimMaxWait() time.Duration {
	if s.cfg.ClaimMaxWait > 0 {
		return s.cfg.ClaimMaxWait
	}
	return domain.DefaultClaimWaitSeconds * time.Second
}

func (s *Service) renewInterval() time.Duration {
	if s.cfg.RenewInterval > 0 {
		return s.cfg.RenewInterval
	}
	return domain.DefaultRenewAfterSeconds * time.Second
}

func validWorkerID(id string) bool {
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return len(id) > 0
}

// claimableSlugs 求令牌 scope 与请求 preferred 的交集；preferred 为空取全部
// scope；通配 scope 展开为注册表中的 remote-pull 智能体。
func (s *Service) claimableSlugs(ctx context.Context, principal *Principal, preferred []string) []string {
	allowed := make([]string, 0, len(principal.Scopes))
	for _, slug := range principal.Scopes {
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
	if len(allowed) == 1 && allowed[0] == "*" {
		if s.remote == nil {
			return nil
		}
		expanded, err := s.remote.RemotePullSlugs(ctx)
		if err != nil {
			slog.Warn("expand wildcard connector scope failed", "err", err)
			return nil
		}
		return expanded
	}
	return allowed
}

// normalizeScopes 清洗 scope 列表；空列表按通配处理。
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

// ---------------------------------------------------------------------------
// renew（§2）
// ---------------------------------------------------------------------------

// RenewResult 是一次成功续租。Abort=true 表示平台已请求取消（§7），
// 连接器应立即停止本地执行并停止续租。
type RenewResult struct {
	Abort        bool
	LeaseSeconds int
	ServerTime   time.Time
}

// Renew 为活跃租约续期。
func (s *Service) Renew(ctx context.Context, runID, workerID string) (*RenewResult, *Failure) {
	runID = strings.TrimSpace(runID)
	workerID = strings.TrimSpace(workerID)
	if runID == "" {
		return nil, failure(400, "invalid_request", "X-Agent-Run-ID header is required")
	}
	if workerID == "" {
		return nil, failure(400, "invalid_request", "X-Agent-Worker-ID header is required")
	}
	outcome, err := s.queue.Renew(ctx, runID, workerID, s.cfg.LeaseTTL())
	if err != nil {
		return nil, failure(500, "storage_error", "renew failed")
	}
	serverTime := time.Now().UTC()
	switch outcome {
	case domain.RenewOK:
		leaseSeconds := s.cfg.LeaseSeconds
		if leaseSeconds <= 0 {
			leaseSeconds = domain.DefaultLeaseSeconds
		}
		return &RenewResult{Abort: false, LeaseSeconds: leaseSeconds, ServerTime: serverTime}, nil
	case domain.RenewAbort:
		return &RenewResult{Abort: true, ServerTime: serverTime}, nil
	case domain.RenewLeaseLost:
		return nil, failure(409, "LEASE_LOST", "lease expired and task was re-dispatched")
	case domain.RenewDeadline:
		return nil, failure(409, "TASK_DEADLINE_EXCEEDED", "execution deadline reached")
	default:
		return nil, failure(404, "run_not_found", "run is unknown or already terminal")
	}
}

// ---------------------------------------------------------------------------
// complete（§3）
// ---------------------------------------------------------------------------

// CompleteError 是失败回执的错误体。Message 不得包含凭据或供应商原始响应。
type CompleteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CompleteUsage 是计量回执。
type CompleteUsage struct {
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
	TotalTokens  uint64 `json:"totalTokens"`
}

// CompleteMetrics 是连接器侧耗时。
type CompleteMetrics struct {
	DurationMs int64 `json:"durationMs"`
}

// CompleteInput 是一次结果回传。
type CompleteInput struct {
	Status  string
	Payload json.RawMessage
	Error   *CompleteError
	Usage   *CompleteUsage
	Metrics *CompleteMetrics
}

// CompleteResult 是回传判定。Accepted=false 且 RunStatus=superseded 表示
// 结果仅归档（租约已失效）。
type CompleteResult struct {
	Accepted  bool
	RunStatus string
}

// Complete 幂等回传结果。
func (s *Service) Complete(ctx context.Context, runID, workerID string, in CompleteInput) (*CompleteResult, *Failure) {
	runID = strings.TrimSpace(runID)
	workerID = strings.TrimSpace(workerID)
	if runID == "" || workerID == "" {
		return nil, failure(400, "invalid_request",
			"X-Agent-Run-ID and X-Agent-Worker-ID headers are required")
	}
	switch in.Status {
	case "succeeded", "failed", "aborted":
	default:
		return nil, failure(400, "invalid_status", "status must be succeeded, failed, or aborted")
	}
	if in.Status == "succeeded" && len(in.Payload) == 0 {
		return nil, failure(400, "invalid_request", "payload is required for succeeded")
	}
	if in.Status == "failed" && (in.Error == nil || in.Error.Code == "") {
		return nil, failure(400, "invalid_request", "error.code is required for failed")
	}

	// 规范化结果文档：幂等重放按此文档字节比较（Go map 序列化键序稳定）。
	resultDoc := map[string]any{
		"status":  in.Status,
		"payload": json.RawMessage(in.Payload),
		"usage":   in.Usage,
		"metrics": in.Metrics,
	}
	if in.Error != nil {
		resultDoc["error"] = in.Error
	}
	normalized, err := json.Marshal(resultDoc)
	if err != nil {
		return nil, failure(400, "invalid_request", "result is not serializable")
	}
	errorCode := ""
	if in.Error != nil {
		errorCode = in.Error.Code
	}
	outcome, err := s.queue.Complete(ctx, domain.CompleteRequest{
		RunID: runID, WorkerID: workerID, Status: in.Status,
		Result: normalized, ErrorCode: errorCode,
	})
	if err != nil {
		if err == domain.ErrTaskNotFound {
			return nil, failure(404, "run_not_found", "run is unknown")
		}
		return nil, failure(500, "storage_error", "complete failed")
	}
	if outcome.Conflict {
		return nil, failure(409, "RESULT_CONFLICT", "a different result was already accepted for this run")
	}
	return &CompleteResult{Accepted: outcome.Accepted, RunStatus: outcome.RunStatus}, nil
}

// ---------------------------------------------------------------------------
// heartbeat（§4）
// ---------------------------------------------------------------------------

// HeartbeatInput 是一次存活与容量上报。
type HeartbeatInput struct {
	WorkerID string
	InFlight int
	Capacity int
	Slugs    []string
	Version  string
}

// HeartbeatResult 是心跳回执（服务端可下发新间隔实现自适应降频）。
type HeartbeatResult struct {
	HeartbeatIntervalSeconds int
	RenewIntervalSeconds     int
	ServerTime               time.Time
	QueueDepth               int
}

// Heartbeat 上报存活与容量；首次心跳即完成注册。
func (s *Service) Heartbeat(ctx context.Context, principal *Principal, in HeartbeatInput) (*HeartbeatResult, *Failure) {
	workerID := strings.TrimSpace(in.WorkerID)
	if !validWorkerID(workerID) || len(workerID) < 2 || len(workerID) > 64 {
		return nil, failure(400, "invalid_worker_id", "workerId must be 2-64 chars of [a-z0-9-]")
	}
	interval := s.cfg.HeartbeatIntervalSeconds
	if interval <= 0 {
		interval = domain.DefaultHeartbeatSeconds
	}
	err := s.workers.Beat(ctx, domain.Worker{
		WorkerID: workerID, TokenID: principal.ID,
		Slugs: in.Slugs, Version: in.Version,
		InFlight: in.InFlight, Capacity: in.Capacity,
		BeatIntervalSeconds: interval,
	})
	if err != nil {
		return nil, failure(500, "storage_error", "heartbeat failed")
	}
	depth, err := s.queue.QueueDepth(ctx, s.claimableSlugs(ctx, principal, nil))
	if err != nil {
		depth = 0
	}
	return &HeartbeatResult{
		HeartbeatIntervalSeconds: interval,
		RenewIntervalSeconds:     int(s.renewInterval().Seconds()),
		ServerTime:               time.Now().UTC(),
		QueueDepth:               depth,
	}, nil
}

// ---------------------------------------------------------------------------
// reaper
// ---------------------------------------------------------------------------

// RunReaper 按续租间隔节奏清扫：过期租约重派/落终态、队列 TTL、worker
// 离线判定。ctx 取消即退出（优雅停机）。
func RunReaper(ctx context.Context, service *Service) {
	interval := service.renewInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			stats, err := service.queue.Sweep(sweepCtx, domain.SweepPolicy{
				MaxAttempts:            service.cfg.MaxAttempts,
				WorkerOfflineAfterMiss: service.cfg.WorkerOfflineAfterMiss,
				RenewInterval:          interval,
			})
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

// randomToken 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
