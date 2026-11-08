// Package dynamic 实现动态 Agent 入口（POST /agent/{slug} 与
// /agent-center-go/{slug}）的用例层：签名鉴权（v1 canonical + 旧 body-only
// 兼容路径）、信封解析、网关幂等租约、三道容量闸、同步执行与异步
// 202+callback 双形态。
//
// HTTP 拆解（chi 路由参数、头提取、限长读体、响应头渲染）在 api 层；
// 本包消费原始多值头切片并返回结构化 Response。
package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"ir-agent-server/internal/app/callback"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
)

// usageUnavailableMessage 是受信通道拿不到 FINAL usage 时的对外文案。
const usageUnavailableMessage = "Executor did not return final provider token usage"

// usageUnavailableDetail 是同一情况落库时的内部文案（首字母小写，与 rs 一致）。
const usageUnavailableDetail = "executor did not return final provider token usage"

// Store 是网关状态存储端口（infra/postgres 的 GatewayStore 结构性满足）。
type Store interface {
	ConsumeRequestNonce(ctx context.Context, agentSlug, nonce string, expiresAt int64) (bool, error)
	BeginIdempotency(ctx context.Context, agentSlug, key, requestHash, runID string,
		enforceRunID bool) (platform.GatewayDecision, error)
	ReleaseIdempotency(ctx context.Context, agentSlug, key, leaseID string) error
	StartRun(ctx context.Context, agentSlug, runID string, revision int64,
		executorKind, executionMode string, requestJSON *string) (bool, error)
	FinalizeRun(ctx context.Context, finalization platform.GatewayRunFinalization) error
}

// AgentResolver 按 slug 定位智能体（domain 仓储结构性满足）。
type AgentResolver interface {
	GetBySlug(ctx context.Context, slug string) (*agent.Agent, error)
}

// Engine 是执行引擎端口（infra/execengine 的 AgentExecutor 结构性满足）。
type Engine interface {
	Execute(ctx context.Context, runtime *agent.ResolvedConfig, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// CallbackSender 以指定密钥回传结果（app/callback 的 Sender 满足）。
type CallbackSender interface {
	SendAs(ctx context.Context, callbackURL, runID, signingSecret string,
		payload *platform.PayloadV1) (*platform.CallbackReceipt, error)
}

// EventRecorder 记录通道事件（callback 往返审计）。
type EventRecorder interface {
	RecordChannelEvent(ctx context.Context, event audit.ChannelEvent) error
}

// Config 是入口策略配置。
type Config struct {
	// AllowLegacyBodySignature 开启旧 X-IR-Signature body-only 兼容路径
	// （无重放保护，仅迁移期）。
	AllowLegacyBodySignature bool
	TrustedUsageReporting    bool
	MaxRequestBodyBytes      int
	GlobalMaxConcurrency     int
}

// Request 是一次请求的已提取形态。头部保留多值切片——「合计恰好一次」
// 本身是校验语义。
type Request struct {
	Slug            string
	RunIDHeader     []string // X-Agent-Run-Id
	IdempotencyKey  []string // X-Idempotency-Key
	AgentSignature  []string // X-Agent-Signature
	LegacySignature []string // X-IR-Signature
	Timestamp       []string // X-Agent-Timestamp
	Nonce           []string // X-Agent-Nonce
	Body            []byte
}

// Response 是一次处理结果。Kind 决定 api 层的渲染方式。
type Response struct {
	// Accepted 表示 202 异步受理；否则 Status/Body 直接渲染。
	Accepted bool
	Status   int
	// Body 是成功信封或错误信封的原文。
	Body string
	// 溯源头（成功与缓存回放时设置）。
	Slug     string
	Revision int64
	RunID    string
	Replayed bool
	// AuthError 要求 api 层附加 WWW-Authenticate: HMAC realm="agent"。
	AuthError bool
	// RetryAfter 非空时 api 层设置同名响应头（429）。
	RetryAfter string
}

// Service 是动态入口用例服务。并发安全。
type Service struct {
	config   Config
	store    Store
	agents   AgentResolver
	engine   Engine
	callback CallbackSender
	events   EventRecorder

	globalSlots *semaphore
	agentSlots  slotRegistry

	background sync.WaitGroup
	now        func() time.Time
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Config   Config
	Store    Store
	Agents   AgentResolver
	Engine   Engine
	Callback CallbackSender
	Events   EventRecorder
}

// NewService 组装动态入口服务。
func NewService(deps Deps) *Service {
	return &Service{
		config: deps.Config, store: deps.Store, agents: deps.Agents,
		engine: deps.Engine, callback: deps.Callback, events: deps.Events,
		globalSlots: newSemaphore(deps.Config.GlobalMaxConcurrency),
		now:         time.Now,
	}
}

// WaitBackground 等待全部异步执行收尾（优雅停机）。
func (s *Service) WaitBackground() { s.background.Wait() }

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

// gatewayRequest 是一次请求经过校验后的可用形态。
type gatewayRequest struct {
	slug     string
	resolved *agent.ResolvedConfig
	body     []byte
	raw      map[string]any
	task     *platform.BoundTask
	runID    string
	explicit bool
	// recordKey / lease 为空表示本请求没有持有幂等租约。
	recordKey string
	lease     string
	context   *platform.TaskContext
	// slots 是本次请求获取到的容量闸（释放必须归还到同一组）。
	slots *slotEntry
}

// Handle 处理一次动态入口请求。
//
// 流程：Agent 查找 → 体校验 → 请求头去重 → 签名鉴权 → 信封解析 →
// runId/幂等键校验 → 幂等租约 → 三道容量闸 → 登记运行 → 同步执行或
// 异步 202 → 落库并回响应。任何一步失败都必须释放已获得的幂等租约，
// 否则同一个键会被永久占住。
// verifyWithGrace 按宽限窗口双钥验证签名：先当前密钥，失败且旧钥仍在
// 轮换宽限窗口（agent.SecretGraceWindow，自 rotatedAt 起算）内时再试旧钥
// （B3：换钥平滑不断流）。旧钥命中时记录日志供运维观察迁移进度。
func (s *Service) verifyWithGrace(resolved *agent.ResolvedConfig, signed, expected string,
	encoding platform.SignatureEncoding,
) bool {
	if platform.VerifyBodySignature([]byte(resolved.SigningSecret), []byte(signed),
		expected, encoding) {
		return true
	}
	if resolved.PrevSigningSecret == "" || resolved.SecretRotatedAt == "" {
		return false
	}
	rotatedAt, err := time.Parse(time.RFC3339Nano, resolved.SecretRotatedAt)
	if err != nil || time.Since(rotatedAt) >= agent.SecretGraceWindow {
		return false
	}
	ok := platform.VerifyBodySignature([]byte(resolved.PrevSigningSecret), []byte(signed),
		expected, encoding)
	if ok {
		slog.Info("agent request verified with previous signing secret (rotation grace)",
			"agent", resolved.Slug)
	}
	return ok
}

func (s *Service) Handle(ctx context.Context, req Request) Response {
	started := s.now()
	stored, err := s.agents.GetBySlug(ctx, strings.ToLower(strings.TrimSpace(req.Slug)))
	if err != nil || stored == nil || !stored.Enabled {
		return s.apiError(404, "agent_not_found", "Agent not found")
	}
	resolved := stored.Resolved()

	if s.config.MaxRequestBodyBytes > 0 && len(req.Body) > s.config.MaxRequestBodyBytes {
		return s.apiError(413, "body_too_large", "Request body exceeded configured limit")
	}

	signedRunID, signedKey, authResp, ok := s.authenticate(ctx, req, resolved)
	if !ok {
		return authResp
	}

	request, parseResp, ok := s.parseEnvelope(req, resolved, signedRunID, signedKey)
	if !ok {
		return parseResp
	}

	if replay, cached, ok := s.beginIdempotency(ctx, request); !ok {
		if cached {
			return replay
		}
		return replay
	}

	release := func() { s.releaseIdempotency(ctx, request) }
	acquired, capacityResp := s.acquireCapacity(ctx, started, request, release)
	if !acquired {
		return capacityResp
	}
	// 三道许可只能释放一次。异步分支把所有权**移交**给后台协程，所以用
	// handOff 抑制 defer——否则 202 一返回许可就没了，后台执行不受并发上限约束。
	var releaseOnce sync.Once
	releaseSlots := func() {
		releaseOnce.Do(func() {
			s.globalSlots.Release()
			request.slots.execution.Release()
			request.slots.admission.Release()
		})
	}
	handOff := false
	defer func() {
		if !handOff {
			releaseSlots()
		}
	}()

	if startResp, ok := s.startRun(ctx, request, release); !ok {
		return startResp
	}
	request.context = gatewayTaskContext(request)

	totalTimeout := resolved.Timeout()
	// 异步形态：body 带 callbackUrl 时立即 202 ack，后台执行并 callback 回传，
	// 避免同步等待本机 Agent 执行（十几秒）导致平台侧等待超时。
	if callbackURL := gatewayCallbackURL(request.task); callbackURL != "" {
		handOff = true
		s.runAsync(request, callbackURL, started, remainingBudget(started, totalTimeout, s.now), releaseSlots)
		return Response{Accepted: true, Status: 202, Slug: request.slug,
			Revision: resolved.Revision, RunID: request.runID}
	}

	return s.runSync(ctx, request, started, remainingBudget(started, totalTimeout, s.now), releaseSlots)
}

// ---------------------------------------------------------------------------
// 签名鉴权
// ---------------------------------------------------------------------------

// authenticate 校验签名并返回签名覆盖的 runId 与幂等键。
//
// 两条互斥路径：
//   - X-Agent-Signature v1：签 canonical 串（方法+路径+时间戳+nonce+runId+
//     幂等键+body 摘要），额外要求时间窗与 nonce 一次性，防重放；
//   - X-IR-Signature：旧协议兼容路径，**只签原始 body**，无重放保护——
//     新的直连调用方都应该用 v1。
func (s *Service) authenticate(ctx context.Context, req Request, resolved *agent.ResolvedConfig) (
	signedRunID, signedKey string, resp Response, ok bool) {
	signedRunID, valid := headerOnce(req.RunIDHeader)
	if !valid {
		return "", "", s.invalidHeader("run ID"), false
	}
	signedKey, valid = headerOnce(req.IdempotencyKey)
	if !valid {
		return "", "", s.invalidHeader("idempotency key"), false
	}
	agentSignature, valid := headerOnce(req.AgentSignature)
	if !valid {
		return "", "", s.invalidHeader(platform.HeaderAgentSignature), false
	}
	legacySignature, valid := headerOnce(req.LegacySignature)
	if !valid {
		return "", "", s.invalidHeader(platform.HeaderSignature), false
	}
	if agentSignature != "" && legacySignature != "" {
		return "", "", s.apiError(400, "ambiguous_signature",
			"Send exactly one Agent signature header"), false
	}

	switch {
	case agentSignature != "":
		if authResp, verified := s.verifyAgentSignatureV1(ctx, req, resolved, agentSignature,
			signedRunID, signedKey); !verified {
			return "", "", authResp, false
		}
	case legacySignature != "":
		if !s.config.AllowLegacyBodySignature {
			return "", "", s.authError(401, "legacy_signature_disabled",
				"X-IR-Signature is disabled for dynamic Agent requests; use X-Agent-Signature v1"), false
		}
		if !s.verifyWithGrace(resolved, string(req.Body), legacySignature,
			resolved.SignatureEncodingOrHex()) {
			slog.Warn("legacy X-IR-Signature Agent request rejected: signature mismatch",
				"agent", resolved.Slug)
			return "", "", s.authError(401, "invalid_signature", "Request signature is invalid"), false
		}
	default:
		return "", "", s.authError(401, "missing_signature",
			"Missing X-Agent-Signature or X-IR-Signature header"), false
	}
	return signedRunID, signedKey, Response{}, true
}

// verifyAgentSignatureV1 校验 v1 canonical 签名，并消费一次性 nonce。
func (s *Service) verifyAgentSignatureV1(ctx context.Context, req Request, resolved *agent.ResolvedConfig,
	signature, signedRunID, signedKey string) (Response, bool) {
	timestampText, valid := headerOnce(req.Timestamp)
	if !valid {
		return s.invalidHeader(platform.HeaderAgentTimestamp), false
	}
	if timestampText == "" {
		return s.authError(401, "missing_timestamp",
			"X-Agent-Timestamp is required for X-Agent-Signature"), false
	}
	nonce, valid := headerOnce(req.Nonce)
	if !valid {
		return s.invalidHeader(platform.HeaderAgentNonce), false
	}
	if nonce == "" {
		return s.authError(401, "missing_nonce",
			"X-Agent-Nonce is required for X-Agent-Signature"), false
	}
	timestamp, err := parseInt64(timestampText)
	if err != nil {
		return s.authError(401, "invalid_timestamp", "X-Agent-Timestamp must be Unix seconds"), false
	}
	now := s.now().Unix()
	if abs64(now-timestamp) > platform.SignedRequestWindowSecs {
		return s.authError(401, "stale_request",
			"Signed request timestamp is outside the 5 minute window"), false
	}
	if !platform.ValidRequestNonce(nonce) {
		return s.authError(401, "invalid_nonce",
			"X-Agent-Nonce must contain 16-128 URL-safe characters"), false
	}

	canonical := platform.AgentSignatureV1Canonical(resolved.Slug, timestampText, nonce,
		signedRunID, signedKey, req.Body)
	if !s.verifyWithGrace(resolved, canonical, signature,
		resolved.SignatureEncodingOrHex()) {
		slog.Warn("agent request rejected: canonical signature mismatch", "agent", resolved.Slug)
		return s.authError(401, "invalid_signature", "Request signature is invalid"), false
	}

	fresh, err := s.store.ConsumeRequestNonce(ctx, resolved.Slug, nonce,
		now+platform.RequestNonceRetention)
	if err != nil {
		slog.Error("request nonce persistence failed", "agent", resolved.Slug, "error", err)
		// 注意这条**不带** WWW-Authenticate：它是服务端故障，不是凭证问题。
		return s.apiError(503, "state_unavailable",
			"Request replay protection is temporarily unavailable"), false
	}
	if !fresh {
		return s.authError(401, "replayed_request", "This signed nonce has already been used"), false
	}
	return Response{}, true
}

// ---------------------------------------------------------------------------
// 信封解析
// ---------------------------------------------------------------------------

func (s *Service) parseEnvelope(req Request, resolved *agent.ResolvedConfig,
	signedRunID, signedKey string) (*gatewayRequest, Response, bool) {
	var decoded any
	if err := json.Unmarshal(req.Body, &decoded); err != nil {
		return nil, s.apiError(400, "invalid_json", "Request body is not valid JSON"), false
	}
	raw, isObject := decoded.(map[string]any)
	if !isObject {
		return nil, s.apiError(422, "invalid_body", "Request body must be a JSON object"), false
	}
	// 已知信封字段的类型必须正确；类型不符说明调用方发错了协议，不能带着
	// 半解析的信封继续跑。
	var task platform.BoundTask
	if err := json.Unmarshal(req.Body, &task); err != nil {
		return nil, s.apiError(422, "invalid_task_envelope",
			"Known task envelope fields have invalid types"), false
	}

	bodyRunID := ""
	if task.RunID != nil {
		bodyRunID = *task.RunID
	}
	if signedRunID != "" && bodyRunID != "" && signedRunID != bodyRunID {
		return nil, s.apiError(422, "conflicting_run_id",
			"Run ID in the header and request body must match"), false
	}
	explicitRunID := signedRunID
	if explicitRunID == "" {
		explicitRunID = bodyRunID
	}
	runID := ""
	switch {
	case explicitRunID != "" && platform.GatewayMetadataValid(explicitRunID, 160):
		runID = explicitRunID
	case explicitRunID != "":
		return nil, s.apiError(422, "invalid_run_id",
			"Run ID must contain 1-160 printable ASCII characters without outer whitespace"), false
	default:
		runID = "run_" + randomToken(12)
	}
	if callbackURL := gatewayCallbackURL(&task); callbackURL != "" {
		if err := callback.ValidateInputs(callbackURL, runID, resolved.SigningSecret); err != nil {
			return nil, s.apiError(422, "invalid_callback_url", "Callback URL is invalid"), false
		}
	}
	if signedKey != "" && !platform.GatewayMetadataValid(signedKey, 200) {
		return nil, s.apiError(422, "invalid_idempotency_key",
			"Idempotency key must contain 1-200 printable ASCII characters without outer whitespace"), false
	}

	request := &gatewayRequest{
		slug: resolved.Slug, resolved: resolved, body: req.Body, raw: raw,
		task: &task, runID: runID, explicit: explicitRunID != "",
	}
	if signedKey != "" {
		// 缓存的响应只属于产出它的那个 Agent 修订版本。
		request.recordKey = platform.GatewayIdempotencyRecordKey(
			resolved.ID, resolved.Revision, signedKey)
	}
	return request, Response{}, true
}

// gatewayTaskContext 从信封构造执行上下文。
func gatewayTaskContext(request *gatewayRequest) *platform.TaskContext {
	task := request.task
	// templateInputData 或 params 任一存在时才走入参视图，否则整个 body 就是
	// 入参（兼容把业务字段直接平铺在顶层的调用方）。
	var params any = request.raw
	_, hasParams := request.raw["params"]
	if task.TemplateInputData != nil || hasParams {
		params = task.ParamsView(request.raw)
	}
	taskBrief := task.TaskBrief
	if taskBrief == nil {
		taskBrief = request.raw
	}
	runID := request.runID
	upstream := task.UpstreamPayload
	if upstream == nil {
		upstream = task.Upstream
	}
	return &platform.TaskContext{
		RunID:               &runID,
		TaskBrief:           taskBrief,
		Params:              params,
		PositionType:        task.PositionType,
		Language:            task.Language(),
		SymbolPool:          platform.ExtractSymbolPool(task.GlobalParameters),
		UniverseMode:        platform.ExtractUniverseMode(task.GlobalParameters),
		UpstreamSymbols:     platform.ExtractUpstreamSymbols(upstream, task.PositionType),
		ForceFailureDefault: platform.ExtractFailurePath(task.TaskBrief),
		RuntimeContext:      task.RuntimeContext,
	}
}

func gatewayCallbackURL(task *platform.BoundTask) string {
	if task.CallbackURL == nil {
		return ""
	}
	return strings.TrimSpace(*task.CallbackURL)
}

// ---------------------------------------------------------------------------
// 幂等与容量
// ---------------------------------------------------------------------------

// beginIdempotency 申请幂等租约。命中缓存时返回 (回放响应, true, false)。
func (s *Service) beginIdempotency(ctx context.Context, request *gatewayRequest) (Response, bool, bool) {
	if request.recordKey == "" {
		return Response{}, false, true
	}
	decision, err := s.store.BeginIdempotency(ctx, request.slug, request.recordKey,
		platform.BodyHash(request.body), request.runID, request.explicit)
	if err != nil {
		slog.Error("idempotency lookup failed", "agent", request.slug, "error", err)
		return s.apiError(500, "state_unavailable", "Agent state is temporarily unavailable"), true, false
	}
	switch decision.Kind {
	case platform.GatewayCached:
		if status, failed := platform.CachedGatewayErrorStatus(decision.ResponseBody); failed {
			return s.agentResponse(status, decision.ResponseBody, request, decision.RunID, true), true, false
		}
		return s.agentResponse(200, decision.ResponseBody, request, decision.RunID, true), true, false
	case platform.GatewayInProgress:
		return s.apiError(409, "request_in_progress",
			"A request with this idempotency key is already running"), true, false
	case platform.GatewayConflict:
		return s.apiError(409, "idempotency_conflict",
			"The idempotency key was already used with different request metadata"), true, false
	default:
		request.lease = decision.LeaseID
		request.runID = decision.RunID
		return Response{}, false, true
	}
}

func (s *Service) releaseIdempotency(ctx context.Context, request *gatewayRequest) {
	if request.recordKey == "" || request.lease == "" {
		return
	}
	if err := s.store.ReleaseIdempotency(ctx, request.slug, request.recordKey, request.lease); err != nil {
		slog.Error("release idempotency record failed",
			"agent", request.slug, "key", request.recordKey, "error", err)
	}
}

// acquireCapacity 依次过准入闸、该 Agent 的执行闸、全局执行闸。
//
// 与分发器同一套顺序：先拿 Agent 自己的容量，免得一个热点 Agent 在自己
// 队列里干等的同时把全局槽位全占走。任何一步失败都要 release 幂等租约。
func (s *Service) acquireCapacity(ctx context.Context, started time.Time,
	request *gatewayRequest, release func()) (bool, Response) {
	entry := s.agentSlots.get(request.slug, request.resolved.MaxConcurrency, request.resolved.QueueCapacity)
	request.slots = entry
	if !entry.admission.TryAcquire() {
		release()
		resp := s.apiError(429, "agent_overloaded", "Agent queue is full")
		resp.RetryAfter = "1"
		return false, resp
	}
	total := request.resolved.Timeout()
	if !entry.execution.AcquireTimeout(ctx, remainingBudget(started, total, s.now)) {
		entry.admission.Release()
		release()
		return false, s.apiError(504, "queue_timeout",
			"Agent did not become available before the deadline")
	}
	if !s.globalSlots.AcquireTimeout(ctx, remainingBudget(started, total, s.now)) {
		entry.execution.Release()
		entry.admission.Release()
		release()
		return false, s.apiError(504, "queue_timeout",
			"Global execution capacity did not become available before the deadline")
	}
	return true, Response{}
}

func (s *Service) startRun(ctx context.Context, request *gatewayRequest, release func()) (Response, bool) {
	requestJSON := string(request.body)
	started, err := s.store.StartRun(ctx, request.slug, request.runID,
		request.resolved.Revision, string(request.resolved.ExecutorKind),
		string(request.resolved.ExecutionMode), &requestJSON)
	if err != nil {
		slog.Error("record run start failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		release()
		return s.apiError(503, "state_unavailable", "Agent run state is temporarily unavailable"), false
	}
	if !started {
		release()
		return s.apiError(409, "run_id_conflict", "This Agent run ID has already been used"), false
	}
	return Response{}, true
}

func (request *gatewayRequest) completeTransition(responseBody string) *platform.GatewayFinalization {
	if request.recordKey == "" || request.lease == "" {
		return nil
	}
	return platform.CompleteGatewayIdempotency(request.recordKey, request.lease, responseBody)
}

func (request *gatewayRequest) releaseTransition() *platform.GatewayFinalization {
	if request.recordKey == "" || request.lease == "" {
		return nil
	}
	return platform.ReleaseGatewayIdempotency(request.recordKey, request.lease)
}

// ---------------------------------------------------------------------------
// 同步执行
// ---------------------------------------------------------------------------

func (s *Service) runSync(ctx context.Context, request *gatewayRequest,
	started time.Time, remaining time.Duration, releaseSlots func()) Response {
	_ = releaseSlots // 由 Handle 的 defer 统一释放
	output, execErr := s.engine.Execute(ctx, request.resolved, request.context, request.raw, remaining)
	durationMs := s.now().Sub(started).Milliseconds()

	if execErr != nil {
		code, publicMessage, executorOutput := classifyExecutionError(execErr)
		slog.Error("agent execution failed", "agent", request.slug, "run_id", request.runID, "error", execErr)
		status := platform.ExecutionErrorStatus(code)
		if !s.finalizeFailure(ctx, request, code, publicMessage, status, executorOutput, durationMs) {
			return s.apiError(503, "state_unavailable",
				"Agent failed and its terminal state could not be committed")
		}
		return s.apiError(status, code, publicMessage)
	}
	if output == nil {
		s.finalizeFailure(ctx, request, "invalid_agent_output",
			"Agent executor returned no output", 502, nil, durationMs)
		return s.apiError(502, "invalid_agent_output", "Agent output was unavailable")
	}

	executorResultJSON := output.ExecutorResultJSON()
	// 受信通道必须回传供应商 FINAL usage；拿不到就判失败，不能静默降级成
	// 「没有用量」——平台会按缺失计费口径处理。
	if s.config.TrustedUsageReporting && output.Usage == nil {
		s.finalizeFailure(ctx, request, "usage_unavailable", usageUnavailableDetail,
			502, executorResultJSON, durationMs)
		return s.apiError(502, "usage_unavailable", usageUnavailableMessage)
	}

	payload := output.ToTransportPayload(request.context.Language)
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Error("serialize agent output failed", "agent", request.slug, "run_id", request.runID, "error", err)
		s.finalizeFailure(ctx, request, "invalid_agent_output",
			"Agent output could not be serialized", 500, executorResultJSON, durationMs)
		return s.apiError(500, "invalid_agent_output", "Agent output could not be serialized")
	}
	responseBody := string(encoded)

	responseCode := int64(200)
	if err := s.store.FinalizeRun(ctx, platform.GatewayRunFinalization{
		AgentSlug: request.slug, RunID: request.runID, Status: "completed",
		DurationMs: durationMs, ResponseCode: &responseCode,
		ResultJSON: &responseBody, ExecutorResultJSON: executorResultJSON,
		Idempotency: request.completeTransition(responseBody),
	}); err != nil {
		slog.Error("commit Agent result failed", "agent", request.slug, "run_id", request.runID, "error", err)
		return s.apiError(503, "state_unavailable",
			"Agent result was produced but its state could not be committed")
	}
	return s.agentResponse(200, responseBody, request, request.runID, false)
}

// finalizeFailure 记录失败终态并释放幂等租约。返回落库是否成功。
//
// message 是写进 result_json 的文案，未必等于回给调用方的文案——
// usage_unavailable 落库用小写开头的内部描述，对外用大写开头的公开文案。
func (s *Service) finalizeFailure(ctx context.Context, request *gatewayRequest,
	code, message string, responseStatus int, executorResultJSON *string, durationMs int64) bool {
	body := platform.SerializedGatewayError(code, message)
	status := int64(responseStatus)
	if err := s.store.FinalizeRun(ctx, platform.GatewayRunFinalization{
		AgentSlug: request.slug, RunID: request.runID, Status: "failed",
		DurationMs: durationMs, ErrorCode: &code,
		ResponseCode: &status, ResultJSON: &body,
		ExecutorResultJSON: executorResultJSON,
		Idempotency:        request.releaseTransition(),
	}); err != nil {
		slog.Error("record run failure failed", "agent", request.slug, "run_id", request.runID, "error", err)
		return false
	}
	return true
}

// finalizeAcceptedFailure 记录 202 之后的终态失败。执行可能已产生副作用，
// 所以把错误缓存在当前租约下而不是释放租约供重执行。
func (s *Service) finalizeAcceptedFailure(ctx context.Context, request *gatewayRequest,
	code, message string, responseStatus int, executorResultJSON *string, durationMs int64) bool {
	body := platform.SerializedGatewayError(code, message)
	status := int64(responseStatus)
	if err := s.store.FinalizeRun(ctx, platform.GatewayRunFinalization{
		AgentSlug: request.slug, RunID: request.runID, Status: "failed",
		DurationMs: durationMs, ErrorCode: &code,
		ResponseCode: &status, ResultJSON: &body,
		ExecutorResultJSON: executorResultJSON,
		Idempotency:        request.completeTransition(body),
	}); err != nil {
		slog.Error("record accepted run failure failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 异步执行（callbackUrl 分支）
// ---------------------------------------------------------------------------

// runAsync 在后台执行并把结果 callback 回传。
//
// 请求已经 202 ack，所以这里**完全脱离请求生命周期**：用 context.Background()，
// 执行时限由 remaining 控制。客户端断开不该中止已受理的运行。releaseSlots
// 是 Handle 移交过来的容量许可释放函数，后台执行结束时调用。
func (s *Service) runAsync(request *gatewayRequest, callbackURL string,
	started time.Time, remaining time.Duration, releaseSlots func()) {
	s.background.Add(1)
	go func() {
		// 三道容量许可在这条协程里继续持有，直到执行与回传都收尾。
		defer releaseSlots()
		defer s.background.Done()
		ctx := context.Background()
		defer func() {
			if recover() == nil {
				return
			}
			slog.Error("asynchronous Agent worker panicked",
				"agent", request.slug, "run_id", request.runID)
			body := platform.SerializedGatewayError("callback_delivery_failed", "Callback delivery failed")
			accepted := int64(502)
			code := "callback_delivery_failed"
			duration := s.now().Sub(started).Milliseconds()
			if err := s.store.FinalizeRun(ctx, platform.GatewayRunFinalization{
				AgentSlug: request.slug, RunID: request.runID, Status: "failed",
				DurationMs: duration, ErrorCode: &code,
				ResponseCode: &accepted, ResultJSON: &body,
				Idempotency: request.completeTransition(body),
			}); err != nil {
				slog.Error("record asynchronous Agent panic failed",
					"agent", request.slug, "run_id", request.runID, "error", err)
			}
		}()

		output, execErr := s.engine.Execute(ctx, request.resolved, request.context, request.raw, remaining)
		durationMs := s.now().Sub(started).Milliseconds()

		if execErr != nil {
			code, publicMessage, executorOutput := classifyExecutionError(execErr)
			slog.Error("async agent execution failed", "agent", request.slug, "run_id", request.runID, "error", execErr)
			// 异步分支的 response_code 记 202（已 ack 的那次响应），不是错误状态。
			s.finalizeAcceptedFailure(ctx, request, code, publicMessage, 202, executorOutput, durationMs)
			return
		}
		if output == nil {
			s.finalizeAcceptedFailure(ctx, request, "invalid_agent_output",
				"Agent executor returned no output", 502, nil, durationMs)
			return
		}

		executorResultJSON := output.ExecutorResultJSON()
		if s.config.TrustedUsageReporting && output.Usage == nil {
			// 受信通道拿不到 FINAL usage 时**连 callback 都不发**——
			// 宁可让平台侧超时重推，也不能投递一份不带用量的结果。
			s.finalizeAcceptedFailure(ctx, request, "usage_unavailable", usageUnavailableDetail,
				502, executorResultJSON, durationMs)
			slog.Error("trusted callback suppressed because FINAL usage is unavailable",
				"agent", request.slug, "run_id", request.runID)
			return
		}

		payload := output.ToTransportPayload(request.context.Language)
		encoded, err := json.Marshal(payload)
		if err != nil {
			slog.Error("serialize async agent output failed",
				"agent", request.slug, "run_id", request.runID, "error", err)
			s.finalizeAcceptedFailure(ctx, request, "invalid_agent_output",
				"Agent output could not be serialized", 202, executorResultJSON, durationMs)
			return
		}
		bodyText := string(encoded)
		receipt, callbackErr := s.deliverCallback(ctx, request, callbackURL, payload, bodyText)

		runStatus := "completed"
		var errorCode *string
		accepted := int64(202)
		responseCode := &accepted
		resultJSON := bodyText
		switch {
		case callbackErr != nil:
			runStatus = "failed"
			code, status := callbackDeliveryOutcome(callbackErr)
			errorCode = &code
			mapped := int64(status)
			responseCode = &mapped
			message := "Callback delivery failed"
			if code == "outcome_uncertain" {
				message = "Callback delivery outcome is uncertain"
			}
			resultJSON = platform.SerializedGatewayError(code, message)
		case receipt == nil:
			runStatus = "failed"
			code := "callback_delivery_failed"
			errorCode = &code
			badGateway := int64(502)
			responseCode = &badGateway
			resultJSON = platform.SerializedGatewayError(code, "Callback delivery returned no receipt")
		case !receipt.Accepted:
			runStatus = "failed"
			code := "callback_rejected"
			errorCode = &code
			badGateway := int64(502)
			responseCode = &badGateway
			resultJSON = platform.SerializedGatewayError(code, "Callback result was not accepted")
		}
		if err := s.store.FinalizeRun(ctx, platform.GatewayRunFinalization{
			AgentSlug: request.slug, RunID: request.runID, Status: runStatus,
			DurationMs: s.now().Sub(started).Milliseconds(), ErrorCode: errorCode,
			ResponseCode: responseCode, ResultJSON: &resultJSON,
			ExecutorResultJSON: executorResultJSON,
			Idempotency:        request.completeTransition(resultJSON),
		}); err != nil {
			slog.Error("record async run completion failed",
				"agent", request.slug, "run_id", request.runID, "error", err)
		}
	}()
}

// deliverCallback 投递结果并把这次通道往返记进 channel_events。
func (s *Service) deliverCallback(ctx context.Context, request *gatewayRequest,
	callbackURL string, payload *platform.PayloadV1, bodyText string) (*platform.CallbackReceipt, error) {
	callbackStarted := s.now()
	receipt, err := s.callback.SendAs(ctx, callbackURL, request.runID,
		request.resolved.SigningSecret, payload)
	slug := request.slug
	runID := request.runID
	if err != nil {
		detail := callbackDeliveryDetail(err)
		code, _ := callbackDeliveryOutcome(err)
		errorJSON := platform.SerializedGatewayError(code, detail)
		var statusCode *int64
		var delivery *callback.DeliveryError
		if errors.As(err, &delivery) && delivery.StatusCode != 0 {
			observed := int64(delivery.StatusCode)
			statusCode = &observed
		}
		elapsedMs := s.now().Sub(callbackStarted).Milliseconds()
		slog.Warn("async callback delivery failed", "agent", slug, "run_id", runID, "error", err)
		s.recordEvent(ctx, "callback", &slug, &runID, platform.CallbackOrigin(callbackURL),
			statusCode, nil, nil, &elapsedMs, &detail, &bodyText, &errorJSON)
		return nil, err
	}
	if receipt == nil {
		detail := "callback delivery returned no receipt"
		errorJSON := platform.SerializedGatewayError("callback_delivery_failed", detail)
		elapsedMs := s.now().Sub(callbackStarted).Milliseconds()
		ok := int64(200)
		s.recordEvent(ctx, "callback", &slug, &runID, platform.CallbackOrigin(callbackURL),
			&ok, nil, nil, &elapsedMs, &detail, &bodyText, &errorJSON)
		return nil, errors.New(detail)
	}
	elapsedMs := s.now().Sub(callbackStarted).Milliseconds()
	receiptJSON := `{"code":"OK"}`
	if encoded, encodeErr := json.Marshal(receipt); encodeErr == nil {
		receiptJSON = string(encoded)
	}
	slog.Info("async callback delivered", "agent", slug, "run_id", runID,
		"accepted", receipt.Accepted, "run_status", deref(receipt.RunStatus))
	ok := int64(200)
	s.recordEvent(ctx, "callback", &slug, &runID, platform.CallbackOrigin(callbackURL),
		&ok, &receipt.Accepted, receipt.RunStatus, &elapsedMs, nil, &bodyText, &receiptJSON)
	return receipt, nil
}

func (s *Service) recordEvent(ctx context.Context, eventType string, slug, runID *string,
	url string, statusCode *int64, accepted *bool, runStatus *string,
	latencyMs *int64, detail, input, output *string) {
	if s.events == nil {
		return
	}
	event := audit.ChannelEvent{
		EventType: eventType, AgentSlug: slug, RunID: runID, URL: url,
		StatusCode: statusCode, Accepted: accepted, RunStatus: runStatus,
		LatencyMs: latencyMs, Detail: detail, Input: input, Output: output,
	}
	if err := s.events.RecordChannelEvent(ctx, event); err != nil {
		slog.Warn("record channel event failed", "agent", deref(slug), "err", err)
	}
}

func callbackDeliveryDetail(err error) string {
	var delivery *callback.DeliveryError
	if errors.As(err, &delivery) {
		return fmt.Sprintf("callback delivery failed after %d attempt(s), status=%d: %s",
			delivery.Attempts, delivery.StatusCode, err.Error())
	}
	return "callback delivery failed"
}

// callbackDeliveryOutcome 区分「确定未送达」与「无法确认」：后者绝不能
// 按可安全重试的 502 通告——平台可能已经接受过结果。
func callbackDeliveryOutcome(err error) (string, int) {
	var delivery *callback.DeliveryError
	if errors.As(err, &delivery) && delivery.Unconfirmed {
		return "outcome_uncertain", 503
	}
	return "callback_delivery_failed", 502
}

// ---------------------------------------------------------------------------
// 响应构造与小工具
// ---------------------------------------------------------------------------

func (s *Service) apiError(status int, code, message string) Response {
	return Response{Status: status, Body: platform.SerializedGatewayError(code, message)}
}

func (s *Service) authError(status int, code, message string) Response {
	resp := s.apiError(status, code, message)
	resp.AuthError = true
	return resp
}

func (s *Service) invalidHeader(name string) Response {
	return s.apiError(400, "invalid_header",
		fmt.Sprintf("%s header must appear once and contain valid ASCII", name))
}

func (s *Service) agentResponse(status int, body string, request *gatewayRequest, runID string, replayed bool) Response {
	return Response{
		Status: status, Body: body, Slug: request.slug,
		Revision: request.resolved.Revision, RunID: runID, Replayed: replayed,
	}
}

// headerOnce 读取一组等价请求头值：合计超过一次或含非可打印 ASCII 即非法。
func headerOnce(values []string) (string, bool) {
	selected := ""
	found := false
	for _, value := range values {
		if found {
			return "", false
		}
		if !asciiHeaderValue(value) {
			return "", false
		}
		selected = value
		found = true
	}
	return selected, true
}

func asciiHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func remainingBudget(started time.Time, timeout time.Duration, now func() time.Time) time.Duration {
	remaining := timeout - now().Sub(started)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func parseInt64(text string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
}

// classifyExecutionError 从引擎错误提取（稳定码, 公开文案, 原始回执 JSON）。
func classifyExecutionError(err error) (code, publicMessage string, executorOutput *string) {
	code, publicMessage, executorOutput = "executor_failed", "agent execution failed", nil
	var coded interface {
		ErrorCode() string
		PublicMessage() string
		ExecutorResultJSON() *string
	}
	if errors.As(err, &coded) {
		code = coded.ErrorCode()
		publicMessage = coded.PublicMessage()
		executorOutput = coded.ExecutorResultJSON()
		return
	}
	var failure *exec.Failure
	if errors.As(err, &failure) {
		return failure.Code, failure.Message, nil
	}
	return code, publicMessage, executorOutput
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
