// Package push 实现 Push 入口用例（POST /ir/push 的业务半边）：
// 信封校验矩阵、HMAC 验签、幂等决策六分支、同步/异步两种响应形态。
//
// HTTP 拆解（读头、限长读体、渲染 problem+json）属于 api 层（P6）；本包
// 只消费已提取的原始值并返回结构化结果，全部路径可脱离 HTTP 单测。
package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// Mode 是 Push 响应形态。
type Mode string

const (
	// ModeSync 同步：200 + 结果信封。
	ModeSync Mode = "sync"
	// ModeAsync 异步：202 + callback 回传。
	ModeAsync Mode = "async"
)

// 执行安全预算。平台侧对同步/异步两种形态的等待上限不同，
// 取配置时限与预算的较小值。
const (
	SyncExecutionBudget  = 285 * time.Second
	AsyncExecutionBudget = 780 * time.Second
)

// Config 是 Push 用例的策略配置（由装配层从 infra/config 映射注入，
// app 层不依赖配置包）。
type Config struct {
	SigningSecret     string
	SignatureEncoding platform.SignatureEncoding
	Mode              Mode
	// TaskTimeout 是配置的单任务时限；<=0 或超过安全预算时退回预算值。
	TaskTimeout time.Duration
	// TrustedUsageReporting 开启后，拿不到 FINAL usage 的结果不投递
	// （保留受理状态防止平台重投触发副作用）。
	TrustedUsageReporting bool
	// MaxRequestBytes 限制请求体大小（0 = 不限）。
	MaxRequestBytes int
}

// Dispatcher 是任务分发端口。app/dispatch 的 *Service 结构上满足。
type Dispatcher interface {
	Execute(ctx context.Context, agentCode string, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// CallbackSender 是异步结果回传端口（app/callback 的 Sender 实现）。
// 回执是 domain/platform 的 wire 形态：Accepted 表示平台采纳结果。
type CallbackSender interface {
	Send(ctx context.Context, callbackURL, runID string, payload *platform.PayloadV1) (*platform.CallbackReceipt, error)
}

// Request 是一次 Push 请求的已提取形态。头部保留多值切片——
// 「恰好出现一次」本身是校验语义的一部分。
type Request struct {
	ContentType     string
	RunIDHeader     []string
	IdempotencyKey  []string
	SignatureHeader []string
	Body            []byte
	BodyReadFailed  bool
}

// Response 是一次成功受理的响应。
type Response struct {
	// Status 是 200（同步结果/回放）或 202（异步受理/回放）。
	Status int
	// Body 是同步结果信封 JSON；202 时为空。
	Body string
}

// Service 是 Push 用例服务。并发安全。
type Service struct {
	config      Config
	idempotency platform.PushIdempotencyStore
	dispatcher  Dispatcher
	callback    CallbackSender
	background  sync.WaitGroup
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Config      Config
	Idempotency platform.PushIdempotencyStore
	Dispatcher  Dispatcher
	// Callback 仅异步模式必需；同步模式可为 nil。
	Callback CallbackSender
}

// NewService 组装 Push 用例服务。
func NewService(deps Deps) *Service {
	return &Service{
		config:      deps.Config,
		idempotency: deps.Idempotency,
		dispatcher:  deps.Dispatcher,
		callback:    deps.Callback,
	}
}

// WaitBackground 等待全部异步后台任务结束（优雅退出口径）。
func (s *Service) WaitBackground() { s.background.Wait() }

// Handle 执行完整 Push 用例。返回的 error 一定是 *RequestError
// （结构化拒绝，api 层按 Status/Type/Fields 渲染）。
func (s *Service) Handle(ctx context.Context, req Request) (Response, error) {
	if apiErr := validateContentType(req.ContentType); apiErr != nil {
		return Response{}, apiErr
	}
	runID, apiErr := requiredUniqueHeader(req.RunIDHeader, platform.HeaderRunID)
	if apiErr != nil {
		return Response{}, apiErr
	}
	idempotencyKey, apiErr := requiredUniqueHeader(req.IdempotencyKey, platform.HeaderIdempotencyKey)
	if apiErr != nil {
		return Response{}, apiErr
	}
	signature, apiErr := requiredSignatureHeader(req.SignatureHeader)
	if apiErr != nil {
		return Response{}, apiErr
	}
	if apiErr := validatePushMetadata(runID, platform.HeaderRunID, 200); apiErr != nil {
		return Response{}, apiErr
	}
	if apiErr := validatePushMetadata(idempotencyKey, platform.HeaderIdempotencyKey, 240); apiErr != nil {
		return Response{}, apiErr
	}
	if req.BodyReadFailed {
		return Response{}, badRequest("failed to read request body")
	}
	if s.config.MaxRequestBytes > 0 && len(req.Body) > s.config.MaxRequestBytes {
		return Response{}, badRequest("request body exceeds the configured size limit")
	}
	if !platform.VerifyBodySignature([]byte(s.config.SigningSecret), req.Body,
		signature, s.config.SignatureEncoding) {
		slog.Warn("Push HMAC verification failed", "run_id", runID)
		return Response{}, unauthorized(platform.HeaderSignature + " mismatch")
	}

	var decoded any
	if err := json.Unmarshal(req.Body, &decoded); err != nil {
		return Response{}, invalidJSON(fmt.Sprintf("request body is not valid JSON: %v", err))
	}
	raw, isObject := decoded.(map[string]any)
	if !isObject {
		return Response{}, pushValidation("body", "task envelope must be a JSON object")
	}
	var task platform.BoundTask
	if err := json.Unmarshal(req.Body, &task); err != nil {
		slog.Warn("Push envelope unmarshal failed",
			"run_id", runID, "request_bytes", len(req.Body), "error", err.Error())
		return Response{}, pushValidation("body", fmt.Sprintf("invalid task envelope: %v", err))
	}
	if apiErr := validateTask(&task, runID); apiErr != nil {
		slog.Warn("Push task validation failed", "run_id", runID,
			"request_bytes", len(req.Body), "field_errors", len(apiErr.Fields))
		return Response{}, apiErr
	}

	requestHash := hex.EncodeToString(sha256Sum(req.Body))
	slog.Info("validated Push task",
		"run_id", runID,
		"agent_code", deref(task.AgentCode),
		"template_id", deref(task.TemplateID),
		"position_type", deref(task.PositionType),
		"request_bytes", len(req.Body),
		"request_hash", requestHash[:16])

	decision, err := s.idempotency.Begin(ctx, idempotencyKey, runID, requestHash)
	if err != nil {
		slog.Error("Push idempotency lookup failed", "run_id", runID, "error", err)
		return Response{}, internalError("Push idempotency state unavailable")
	}
	switch decision.Kind {
	case platform.IdemSyncCached:
		slog.Info("replaying cached synchronous Push response", "run_id", runID)
		return Response{Status: 200, Body: decision.ResponseBody}, nil
	case platform.IdemSyncFailed:
		slog.Warn("replaying terminal synchronous Push failure", "run_id", runID)
		return Response{}, syncFailure(decision.ResponseBody)
	case platform.IdemAsyncAccepted:
		slog.Info("replaying asynchronous Push acknowledgement", "run_id", runID)
		return Response{Status: 202}, nil
	case platform.IdemInProgress:
		return Response{}, conflict("a request with this idempotency key is already processing")
	case platform.IdemConflict:
		return Response{}, conflict("idempotency key was reused with a different runId or request body")
	}

	taskContext := pushTaskContext(&task, raw, runID)
	agentCode := deref(task.AgentCode)

	if s.config.Mode == ModeAsync {
		return s.handleAsync(ctx, &task, taskContext, raw, runID, idempotencyKey, requestHash, agentCode)
	}
	return s.handleSync(ctx, taskContext, raw, runID, idempotencyKey, requestHash, agentCode)
}

// handleSync 在同步预算内执行并直接回结果信封。
//
// 执行前/执行中失败要 releaseLease；执行已成功但结果无法序列化时，
// 必须记录终态失败，避免重试再次触发任务副作用。
func (s *Service) handleSync(
	ctx context.Context,
	taskContext *platform.TaskContext,
	raw map[string]any,
	runID, idempotencyKey, requestHash, agentCode string,
) (Response, error) {
	timeout := executionBudget(s.config.TaskTimeout, SyncExecutionBudget)
	output, err, timedOut := s.executeWithBudget(ctx, agentCode, taskContext, raw, timeout)

	switch {
	case timedOut:
		s.releaseLease(ctx, idempotencyKey, runID, requestHash)
		return Response{}, taskTimeout(int64(timeout.Seconds()))
	case err != nil:
		s.releaseLease(ctx, idempotencyKey, runID, requestHash)
		slog.Error("synchronous processor failed", "run_id", runID, "error", err)
		// 对外只暴露稳定文案，内部细节留在日志里。
		return Response{}, internalError("task processing failed")
	}

	if s.config.TrustedUsageReporting && output.Usage == nil {
		if persistErr := s.idempotency.FailSync(ctx, idempotencyKey, runID, requestHash,
			"usage_unavailable"); persistErr != nil {
			slog.Error("persist synchronous Push usage failure failed",
				"run_id", runID, "error", persistErr)
			return Response{}, internalError("persist task failure failed")
		}
		return Response{}, syncFailure("usage_unavailable")
	}

	payload := output.ToTransportPayload(taskContext.Language)
	encoded, err := json.Marshal(payload)
	if err != nil {
		if persistErr := s.idempotency.FailSync(ctx, idempotencyKey, runID, requestHash,
			"invalid_result"); persistErr != nil {
			slog.Error("persist synchronous Push failure failed",
				"run_id", runID, "error", persistErr)
			return Response{}, internalError("persist task failure failed")
		}
		return Response{}, syncFailure("invalid_result")
	}
	responseBody := string(encoded)
	if err := s.idempotency.CompleteSync(ctx, idempotencyKey, runID, requestHash, responseBody); err != nil {
		slog.Error("persist Push result failed", "run_id", runID, "error", err)
		return Response{}, internalError("persist task result failed")
	}
	return Response{Status: 200, Body: responseBody}, nil
}

// handleAsync 先落「已受理」再 202，随后在后台执行并 callback 回传。
func (s *Service) handleAsync(
	ctx context.Context,
	task *platform.BoundTask,
	taskContext *platform.TaskContext,
	raw map[string]any,
	runID, idempotencyKey, requestHash, agentCode string,
) (Response, error) {
	if err := s.idempotency.AcceptAsync(ctx, idempotencyKey, runID, requestHash); err != nil {
		slog.Error("persist Push acceptance failed", "run_id", runID, "error", err)
		return Response{}, internalError("persist asynchronous acceptance failed")
	}

	callbackURL := deref(task.CallbackURL)
	positionType := deref(task.PositionType)
	language := taskContext.Language
	trusted := s.config.TrustedUsageReporting
	timeout := executionBudget(s.config.TaskTimeout, AsyncExecutionBudget)
	sender := s.callback

	s.background.Add(1)
	go func() {
		defer s.background.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("asynchronous Push worker panicked; acceptance retained for reconciliation",
					"run_id", runID)
			}
		}()
		// 已经 202 了，后台执行完全脱离请求生命周期。
		bgCtx := context.Background()
		output, err, timedOut := s.executeWithBudget(bgCtx, agentCode, taskContext, raw, timeout)

		var payload *platform.PayloadV1
		switch {
		case timedOut:
			slog.Error("asynchronous processor timed out",
				"run_id", runID, "timeout_secs", int64(timeout.Seconds()))
			if trusted {
				// 受信通道拿不到 FINAL usage 就不投递。执行已经开始，保留
				// async_accepted 防止平台重投再次触发可能有副作用的任务。
				slog.Error("trusted async execution timed out without FINAL usage; callback suppressed",
					"run_id", runID)
				return
			}
			payload = conservativeFailurePayload(positionType, language)
		case err != nil:
			slog.Error("asynchronous processor failed", "run_id", runID, "error", err)
			if trusted {
				slog.Error("trusted async execution failed before FINAL usage; callback suppressed",
					"run_id", runID)
				return
			}
			payload = conservativeFailurePayload(positionType, language)
		default:
			if trusted && output.Usage == nil {
				slog.Error("trusted async result has no FINAL provider usage; callback suppressed",
					"run_id", runID)
				return
			}
			payload = output.ToTransportPayload(language)
		}

		if sender == nil {
			slog.Error("async Push completed but no callback sender is wired",
				"run_id", runID)
			return
		}
		receipt, err := sender.Send(bgCtx, callbackURL, runID, payload)
		switch {
		case err != nil:
			slog.Error("callback delivery exhausted; acceptance retained for reconciliation",
				"run_id", runID, "error", err)
		case receipt == nil:
			slog.Error("callback delivery returned no receipt", "run_id", runID)
		case receipt.Accepted:
			slog.Info("async Push completed", "run_id", runID, "run_status", deref(receipt.RunStatus))
		default:
			slog.Warn("async Push result was rejected", "run_id", runID,
				"run_status", deref(receipt.RunStatus))
		}
	}()

	return Response{Status: 202}, nil
}

// executeWithBudget 在预算内执行任务。
//
// timedOut 与 err 互斥：超时时**放弃等待**而不是等执行完——后端可能不响应
// 取消，但调用方不能被拖住。执行协程捕获为局部引用，避免与被放弃的
// goroutine 产生字段级数据竞争。
func (s *Service) executeWithBudget(
	ctx context.Context,
	agentCode string,
	taskContext *platform.TaskContext,
	raw map[string]any,
	timeout time.Duration,
) (output *platform.ProcessedOutput, err error, timedOut bool) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dispatcher := s.dispatcher

	type result struct {
		output *platform.ProcessedOutput
		err    error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- result{err: fmt.Errorf("task execution panicked")}
			}
		}()
		got, execErr := dispatcher.Execute(execCtx, agentCode, taskContext, raw, timeout)
		done <- result{output: got, err: execErr}
	}()

	select {
	case got := <-done:
		if got.err == nil && got.output == nil {
			return nil, fmt.Errorf("task execution returned a nil output"), false
		}
		return got.output, got.err, false
	case <-execCtx.Done():
		return nil, nil, true
	}
}

// releaseLease 释放本请求的 processing 租约。
func (s *Service) releaseLease(ctx context.Context, key, runID, requestHash string) {
	if err := s.idempotency.Release(ctx, key, runID, requestHash); err != nil {
		slog.Error("release Push idempotency lease failed", "run_id", runID, "error", err)
	}
}

// pushTaskContext 从信封构造执行上下文。
//
// 与 Gateway 入口的两处差异：taskBrief **不回退到整个 body**，params 也
// **无条件**走 ParamsView —— Push 的信封是平台严格下发的，没有「业务字段
// 平铺在顶层」这种形态。
func pushTaskContext(task *platform.BoundTask, raw map[string]any, runID string) *platform.TaskContext {
	upstream := task.UpstreamPayload
	if upstream == nil {
		upstream = task.Upstream
	}
	return &platform.TaskContext{
		RunID:               &runID,
		TaskBrief:           task.TaskBrief,
		Params:              task.ParamsView(raw),
		PositionType:        task.PositionType,
		Language:            task.Language(),
		SymbolPool:          platform.ExtractSymbolPool(task.GlobalParameters),
		UniverseMode:        platform.ExtractUniverseMode(task.GlobalParameters),
		UpstreamSymbols:     platform.ExtractUpstreamSymbols(upstream, task.PositionType),
		ForceFailureDefault: platform.ExtractFailurePath(task.TaskBrief),
		RuntimeContext:      task.RuntimeContext,
	}
}

// conservativeFailurePayload 是异步失败时回传的保守结果。
//
// 非受信通道即使执行失败也要给平台一个 schema 合规的答复（否则平台会一直
// 重推）；内容按岗位给出「不采取行动」的最小结构。
func conservativeFailurePayload(positionType string, language *string) *platform.PayloadV1 {
	chinese := language != nil && strings.HasPrefix(strings.ToLower(*language), "zh")
	summary := "Task processing failed; returning a conservative no-action result."
	if chinese {
		summary = "任务处理失败，已返回不采取行动的保守结果。"
	}
	var payloadResult any
	switch positionType {
	case "TECHNICAL_ANALYST":
		payloadResult = map[string]any{"signals": []any{}}
	case "PORTFOLIO_MANAGER":
		payloadResult = map[string]any{"decision_type": "NO_ACTION", "rationale": summary}
	case "RISK_MANAGER":
		payloadResult = map[string]any{
			"gate": "WARN",
			"checks": []any{map[string]any{
				"dimension": "OTHERS", "status": "UNKNOWN",
				// 这条 reasoning 在 rs 里是硬编码中文，与 summary 的语言无关。
				"reasoning": "任务处理失败，已返回不采取行动的保守结果。",
			}},
		}
	case "EXECUTION_TRADER":
		payloadResult = map[string]any{"orders": []any{}}
	case "STRATEGIST":
		payloadResult = map[string]any{"signals": []any{}, "rationale": summary}
	default:
		payloadResult = map[string]any{"assessments": []any{}}
	}
	return &platform.PayloadV1{
		PayloadResult:   payloadResult,
		PayloadInfo:     summary,
		ContractVersion: "V2",
	}
}

// executionBudget 把「配置的任务超时」与「协议安全预算」收敛成实际超时。
// configured <= 0 时退回安全预算，让请求至少有机会跑完（而不是立刻 504）。
func executionBudget(configured, budget time.Duration) time.Duration {
	if configured <= 0 || configured > budget {
		return budget
	}
	return configured
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
