package mota

// MOTA Push 入口。对应 Rust 侧 src/mota/push.rs。
//
// 请求必须带完整任务信封、原始 body HMAC、run ID 和幂等键。同步形态返回
// `200 + mota-agent-payload v1`;异步形态尽快返回空 `202`,随后走 v2 callback。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hwhkit/hwhkit-go/core/apierror"
)

const (
	SignatureHeader        = "X-MoTA-Signature"
	RunIDHeader            = "X-MoTA-Run-ID"
	IdempotencyKeyHeader   = "X-MoTA-Idempotency-Key"
	CreatorSignatureHeader = "X-Creator-Signature"
)

// 执行安全预算。平台侧对同步/异步两种形态的等待上限不同,取配置时限与预算的较小值。
const (
	syncExecutionBudget  = 285 * time.Second
	asyncExecutionBudget = 780 * time.Second
)

// pushPositionTypes 是生产协议认可的岗位类型。
var pushPositionTypes = map[string]bool{
	"FUNDAMENTAL_ANALYST": true,
	"TECHNICAL_ANALYST":   true,
	"PORTFOLIO_MANAGER":   true,
	"RISK_MANAGER":        true,
	"EXECUTION_TRADER":    true,
	"STRATEGIST":          true,
	"QUANT_TRADING_AGENT": true,
	"SENTINEL":            true,
}

// pushGlobalParameterKeys 是 bound-task v1 允许的 globalParameters 键。
// 平台评测任务会下发风控参数(2026-09-16 实测样本),属于合法契约内容。
var pushGlobalParameterKeys = map[string]bool{
	"language": true, "targetMarkets": true, "symbolPool": true, "universeMode": true,
	"riskLevel": true, "maxPositionSizeUsd": true, "maxDrawdownThreshold": true,
	"stopLossThresholdPct": true, "maxInvestmentTargetsUsd": true,
}

// ---------------------------------------------------------------------------
// 处理器装配
// ---------------------------------------------------------------------------

// PushHandler 承载 Push 入口的依赖。
type PushHandler struct {
	Config      *Config
	Idempotency *PushIdempotencyStore
	Execution   *TaskExecutionBackend
	// HTTPClient 用于 callback 回传;为 nil 时用 http.DefaultClient。
	HTTPClient *http.Client
}

// NewPushHandler 是嵌入/测试用的便捷构造:内存幂等 + 进程内处理器。
//
// 生产装配必须用 NewPushHandlerWithBackend 并传 PostgreSQL 幂等存储 ——
// 内存状态不跨进程,重启或多副本都会重复执行有副作用的任务。
func NewPushHandler(cfg *Config, processor Processor) http.HandlerFunc {
	handler := &PushHandler{
		Config:      cfg,
		Idempotency: NewInMemoryPushIdempotencyStore(),
		Execution:   NewProcessorBackend(processor),
	}
	return handler.ServeHTTP
}

// NewPushHandlerWithBackend 是生产装配入口。
func NewPushHandlerWithBackend(
	cfg *Config,
	backend *TaskExecutionBackend,
	idempotency *PushIdempotencyStore,
	httpClient *http.Client,
) http.HandlerFunc {
	handler := &PushHandler{
		Config: cfg, Idempotency: idempotency,
		Execution: backend, HTTPClient: httpClient,
	}
	return handler.ServeHTTP
}

// ServeHTTP 处理一次 Push 请求,并记录耗时与结果。
func (h *PushHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status, apiErr := h.handle(w, r)
	elapsed := time.Since(started).Milliseconds()
	if apiErr != nil {
		slog.Warn("Push request rejected", "elapsed_ms", elapsed, "status", apiErr.Status)
		apiErr.WriteJSON(w)
		return
	}
	slog.Info("Push response", "elapsed_ms", elapsed, "status", status)
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

// handle 执行完整流程。返回 (已写出的状态码, nil) 或 (0, 错误)。
//
// 返回错误时**尚未**向 w 写任何东西,由 ServeHTTP 统一输出 problem+json。
func (h *PushHandler) handle(w http.ResponseWriter, r *http.Request) (int, *apierror.ApiError) {
	if apiErr := validateContentType(r); apiErr != nil {
		return 0, apiErr
	}
	runID, apiErr := requiredUniqueHeader(r, RunIDHeader, "X-MoTA-Run-ID")
	if apiErr != nil {
		return 0, apiErr
	}
	idempotencyKey, apiErr := requiredUniqueHeader(r, IdempotencyKeyHeader, "X-MoTA-Idempotency-Key")
	if apiErr != nil {
		return 0, apiErr
	}
	signature, apiErr := requiredSignatureHeader(r)
	if apiErr != nil {
		return 0, apiErr
	}
	if apiErr := validatePushMetadata(runID, "X-MoTA-Run-ID", 200); apiErr != nil {
		return 0, apiErr
	}
	if apiErr := validatePushMetadata(idempotencyKey, "X-MoTA-Idempotency-Key", 240); apiErr != nil {
		return 0, apiErr
	}

	body, err := readAllLimited(r)
	if err != nil {
		return 0, apierror.BadRequest("failed to read request body")
	}
	if !VerifySignature(h.Config.SigningSecret, body, signature, h.Config.SignatureEncoding) {
		slog.Warn("Push HMAC verification failed", "run_id", runID)
		return 0, apierror.Unauthorized("X-MoTA-Signature mismatch")
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, &apierror.ApiError{
			Status: http.StatusBadRequest, Type: "urn:mota:invalid-json", Title: "Invalid JSON",
			Detail: fmt.Sprintf("request body is not valid JSON: %v", err),
		}
	}
	raw, isObject := decoded.(map[string]any)
	if !isObject {
		return 0, pushValidation("body", "task envelope must be a JSON object")
	}
	var task BoundTask
	if err := json.Unmarshal(body, &task); err != nil {
		slog.Warn("Push envelope unmarshal failed",
			"run_id", runID, "request_bytes", len(body), "error", err.Error())
		return 0, pushValidation("body", fmt.Sprintf("invalid task envelope: %v", err))
	}
	if apiErr := validateTask(&task, runID); apiErr != nil {
		slog.Warn("Push task validation failed",
			"run_id", runID, "request_bytes", len(body), "api_error", fmt.Sprintf("%+v", apiErr))
		return 0, apiErr
	}

	requestHash := hex.EncodeToString(sha256Sum(body))
	slog.Info("validated Push task",
		"run_id", runID,
		"agent_code", derefOrEmpty(task.AgentCode),
		"template_id", derefOrEmpty(task.TemplateID),
		"position_type", derefOrEmpty(task.PositionType),
		"request_bytes", len(body),
		"request_hash", requestHash[:16])

	decision, err := h.Idempotency.Begin(r.Context(), idempotencyKey, runID, requestHash)
	if err != nil {
		slog.Error("Push idempotency lookup failed", "run_id", runID, "error", err)
		return 0, apierror.Internal("Push idempotency state unavailable")
	}
	switch decision.Kind {
	case PushSyncCached:
		slog.Info("replaying cached synchronous Push response", "run_id", runID)
		writePushJSON(w, http.StatusOK, decision.ResponseBody)
		return http.StatusOK, nil
	case PushSyncFailed:
		slog.Warn("replaying terminal synchronous Push failure", "run_id", runID)
		return 0, syncPushFailure(decision.ResponseBody)
	case PushAsyncAccepted:
		slog.Info("replaying asynchronous Push acknowledgement", "run_id", runID)
		w.WriteHeader(http.StatusAccepted)
		return http.StatusAccepted, nil
	case PushInProgress:
		return 0, apierror.Conflict("a request with this idempotency key is already processing")
	case PushConflict:
		return 0, apierror.Conflict("idempotency key was reused with a different runId or request body")
	}

	taskContext := pushTaskContext(&task, raw, runID)
	agentCode := derefOrEmpty(task.AgentCode)

	if h.Config.PushMode == PushModeAsync {
		return h.handleAsync(w, r, &task, taskContext, raw, runID, idempotencyKey, requestHash, agentCode)
	}
	return h.handleSync(w, r, taskContext, raw, runID, idempotencyKey, requestHash, agentCode)
}

// pushTaskContext 从信封构造执行上下文。
//
// 注意与 Gateway 的两处差异:taskBrief **不回退到整个 body**,params 也**无条件**
// 走 ParamsView —— Push 的信封是平台严格下发的,没有「业务字段平铺在顶层」这种形态。
func pushTaskContext(task *BoundTask, raw map[string]any, runID string) *TaskContext {
	upstream := task.UpstreamPayload
	if upstream == nil {
		upstream = task.Upstream
	}
	return &TaskContext{
		RunID:               &runID,
		TaskBrief:           task.TaskBrief,
		Params:              task.ParamsView(raw),
		PositionType:        task.PositionType,
		Language:            task.Language(),
		SymbolPool:          ExtractSymbolPool(task.GlobalParameters),
		UniverseMode:        ExtractUniverseMode(task.GlobalParameters),
		UpstreamSymbols:     ExtractUpstreamSymbols(upstream, task.PositionType),
		ForceFailureDefault: ExtractFailurePath(task.TaskBrief),
		RuntimeContext:      task.RuntimeContext,
	}
}

// ---------------------------------------------------------------------------
// 同步形态
// ---------------------------------------------------------------------------

// handleSync 在同步预算内执行并直接回结果信封。
//
// 执行前/执行中失败要 releaseLease；执行已成功但结果无法序列化时，
// 必须记录终态失败，避免重试再次触发任务副作用。
func (h *PushHandler) handleSync(
	w http.ResponseWriter,
	r *http.Request,
	taskContext *TaskContext,
	raw map[string]any,
	runID, idempotencyKey, requestHash, agentCode string,
) (int, *apierror.ApiError) {
	timeout := executionBudget(h.Config.TaskTimeout, syncExecutionBudget)
	output, err, timedOut := h.executeWithBudget(r.Context(), agentCode, taskContext, raw, timeout)

	switch {
	case timedOut:
		h.releaseLease(r.Context(), idempotencyKey, runID, requestHash)
		return 0, &apierror.ApiError{
			Status: http.StatusGatewayTimeout, Type: "urn:mota:task-timeout", Title: "Task Timeout",
			Detail: fmt.Sprintf("task exceeded the synchronous %d second safety budget",
				int64(timeout.Seconds())),
		}
	case err != nil:
		h.releaseLease(r.Context(), idempotencyKey, runID, requestHash)
		slog.Error("synchronous processor failed", "run_id", runID, "error", err)
		// 对外只暴露稳定文案,内部细节留在日志里。
		return 0, apierror.Internal("task processing failed")
	}

	if h.Config.TrustedUsageReportingEnabled && output.Usage == nil {
		if persistErr := h.Idempotency.FailSync(
			r.Context(), idempotencyKey, runID, requestHash, "usage_unavailable"); persistErr != nil {
			slog.Error("persist synchronous Push usage failure failed",
				"run_id", runID, "error", persistErr)
			return 0, apierror.Internal("persist task failure failed")
		}
		return 0, syncPushFailure("usage_unavailable")
	}

	payload := output.ToTransportPayload(taskContext.Language)
	encoded, err := json.Marshal(payload)
	if err != nil {
		if persistErr := h.Idempotency.FailSync(
			r.Context(), idempotencyKey, runID, requestHash, "invalid_result"); persistErr != nil {
			slog.Error("persist synchronous Push failure failed",
				"run_id", runID, "error", persistErr)
			return 0, apierror.Internal("persist task failure failed")
		}
		return 0, syncPushFailure("invalid_result")
	}
	responseBody := string(encoded)
	if err := h.Idempotency.CompleteSync(r.Context(), idempotencyKey, runID, requestHash, responseBody); err != nil {
		slog.Error("persist Push result failed", "run_id", runID, "error", err)
		return 0, apierror.Internal("persist task result failed")
	}
	writePushJSON(w, http.StatusOK, responseBody)
	return http.StatusOK, nil
}

func syncPushFailure(reason string) *apierror.ApiError {
	if reason == "usage_unavailable" {
		return &apierror.ApiError{
			Status: http.StatusBadGateway, Type: "urn:mota:usage-unavailable",
			Title:  "Usage Unavailable",
			Detail: "executor did not return final provider token usage",
		}
	}
	return apierror.Internal("previous task result could not be serialized")
}

// ---------------------------------------------------------------------------
// 异步形态
// ---------------------------------------------------------------------------

// handleAsync 先落「已受理」再 202,随后在后台执行并 callback 回传。
func (h *PushHandler) handleAsync(
	w http.ResponseWriter,
	r *http.Request,
	task *BoundTask,
	taskContext *TaskContext,
	raw map[string]any,
	runID, idempotencyKey, requestHash, agentCode string,
) (int, *apierror.ApiError) {
	if err := h.Idempotency.AcceptAsync(r.Context(), idempotencyKey, runID, requestHash); err != nil {
		slog.Error("persist Push acceptance failed", "run_id", runID, "error", err)
		return 0, apierror.Internal("persist asynchronous acceptance failed")
	}

	callbackURL := derefOrEmpty(task.CallbackURL)
	positionType := derefOrEmpty(task.PositionType)
	language := taskContext.Language
	trusted := h.Config.TrustedUsageReportingEnabled
	timeout := executionBudget(h.Config.TaskTimeout, asyncExecutionBudget)
	client := h.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	signingSecret := h.Config.SigningSecret
	callbackToken := h.Config.CallbackToken
	callbackTokenOrigin := h.Config.CallbackTokenOrigin

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("asynchronous Push worker panicked; acceptance retained for reconciliation",
					"run_id", runID)
			}
		}()
		// 已经 202 了,后台执行完全脱离请求生命周期。
		ctx := context.Background()
		output, err, timedOut := h.executeWithBudget(ctx, agentCode, taskContext, raw, timeout)

		var payload *PayloadV1
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

		receipt, err := SendCallback(ctx, client, callbackURL, runID,
			signingSecret, callbackToken, callbackTokenOrigin, payload)
		switch {
		case err != nil:
			slog.Error("callback delivery exhausted; acceptance retained for reconciliation",
				"run_id", runID, "error", err)
		case receipt == nil:
			slog.Error("callback delivery returned no receipt", "run_id", runID)
		case receipt.Accepted:
			slog.Info("async Push completed",
				"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus))
		default:
			slog.Warn("async Push result was rejected",
				"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus))
		}
	}()

	w.WriteHeader(http.StatusAccepted)
	return http.StatusAccepted, nil
}

// executeWithBudget 在预算内执行任务。
//
// timedOut 与 err 互斥:超时时**放弃等待**而不是等执行完(与 rs 的
// tokio::time::timeout 一致)—— 后端可能不响应取消,但调用方不能被拖住。
func (h *PushHandler) executeWithBudget(
	ctx context.Context,
	agentCode string,
	taskContext *TaskContext,
	raw map[string]any,
	timeout time.Duration,
) (output *ProcessedOutput, err error, timedOut bool) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 捕获成局部变量:超时后这个 goroutine 会被放弃但不会被杀死,它仍然会
	// 跑完并读取执行后端。如果直接闭包引用 h.Execution 字段,而调用方在
	// 这个放弃的 goroutine 还没退出时又换了后端(例如测试里重试用了不同
	// 的 Execution),就会与该字段的写入产生数据竞争。
	execution := h.Execution

	type result struct {
		output *ProcessedOutput
		err    error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- result{err: fmt.Errorf("task execution panicked")}
			}
		}()
		got, err := execution.Execute(execCtx, agentCode, taskContext, raw, timeout)
		done <- result{output: got, err: err}
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
func (h *PushHandler) releaseLease(ctx context.Context, key, runID, requestHash string) {
	if err := h.Idempotency.Release(ctx, key, runID, requestHash); err != nil {
		slog.Error("release Push idempotency lease failed", "run_id", runID, "error", err)
	}
}

// conservativeFailurePayload 是异步失败时回传的保守结果。
//
// 非受信通道即使执行失败也要给平台一个 schema 合规的答复(否则平台会一直重推);
// 内容按岗位给出「不采取行动」的最小结构。
func conservativeFailurePayload(positionType string, language *string) *PayloadV1 {
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
				// 这条 reasoning 在 rs 里是硬编码中文,与 summary 的语言无关。
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
	return &PayloadV1{
		PayloadResult:   payloadResult,
		PayloadInfo:     summary,
		ContractVersion: "V2",
	}
}

// ---------------------------------------------------------------------------
// 信封校验
// ---------------------------------------------------------------------------

// validateTask 校验整个任务信封。所有字段错误一次性收集后统一返回。
func validateTask(task *BoundTask, headerRunID string) *apierror.ApiError {
	var errors []apierror.FieldError

	requireExact(&errors, "schemaVersion", task.SchemaVersion, "1.0")
	requireNonEmpty(&errors, "runId", task.RunID)
	requireNonEmpty(&errors, "agentCode", task.AgentCode)
	// templateId / positionType 是平台下发的条件字段:正式任务与连通测试的探测任务
	// (taskBrief.probe=true)都可能不带,不能当作必填。
	requireNonEmpty(&errors, "callbackUrl", task.CallbackURL)

	if task.RunID != nil && *task.RunID != headerRunID {
		errors = append(errors, fieldError("runId", "must exactly match X-MoTA-Run-ID"))
	}
	if task.PositionType != nil && !pushPositionTypes[*task.PositionType] {
		errors = append(errors,
			fieldError("positionType", "is not a supported production position type"))
	}

	if task.TemplateInputData == nil {
		errors = append(errors, fieldError("templateInputData", "is required"))
	} else {
		seen := map[string]bool{}
		for index, entry := range *task.TemplateInputData {
			field := fmt.Sprintf("templateInputData[%d].name", index)
			switch {
			case strings.TrimSpace(entry.Name) == "":
				errors = append(errors, fieldError(field, "must not be empty"))
			case seen[entry.Name]:
				errors = append(errors, fieldError(field, "must be unique within templateInputData"))
			default:
				seen[entry.Name] = true
			}
		}
	}

	if task.TaskBrief != nil {
		if _, ok := asJSONObject(task.TaskBrief); !ok {
			errors = append(errors, fieldError("taskBrief", "must be an object"))
		}
	}
	validateGlobalParameters(task.GlobalParameters, &errors)
	validateUpstreamPayload(task.UpstreamPayload, &errors)
	validateUpstream(task.Upstream, &errors)
	validateRuntimeContext(task.RuntimeContext, &errors)
	validatePushCallbackURL(task.CallbackURL, &errors)

	if len(errors) == 0 {
		return nil
	}
	return apierror.Validation(errors)
}

// validateGlobalParameters 校验 globalParameters:键白名单 + 各字段类型。
func validateGlobalParameters(value any, errors *[]apierror.FieldError) {
	if value == nil {
		return
	}
	parameters, ok := asJSONObject(value)
	if !ok {
		*errors = append(*errors, fieldError("globalParameters", "must be an object"))
		return
	}
	// 键必须逐个过白名单:多余键说明调用方在用未约定的扩展。
	for key := range parameters {
		if !pushGlobalParameterKeys[key] {
			*errors = append(*errors, fieldError("globalParameters."+key,
				"is not allowed by the bound-task v1 contract"))
		}
	}
	if language, present := parameters["language"]; present {
		if _, ok := language.(string); !ok {
			*errors = append(*errors, fieldError("globalParameters.language", "must be a string"))
		}
	}
	if markets, present := parameters["targetMarkets"]; present {
		if !isStringArray(markets, false) {
			*errors = append(*errors,
				fieldError("globalParameters.targetMarkets", "must be an array of strings"))
		}
	}
	if symbols, present := parameters["symbolPool"]; present {
		if !isStringArray(symbols, true) {
			*errors = append(*errors,
				fieldError("globalParameters.symbolPool", "must be an array of non-empty strings"))
		}
	}
	if mode, present := parameters["universeMode"]; present {
		text, ok := mode.(string)
		if !ok || (text != "RESTRICTED" && text != "SEED") {
			*errors = append(*errors,
				fieldError("globalParameters.universeMode", "must be RESTRICTED or SEED"))
		}
	}
}

// isStringArray 判断值是否为字符串数组。requireNonBlank 为 true 时元素还不能是空白。
func isStringArray(value any, requireNonBlank bool) bool {
	items, ok := asJSONArray(value)
	if !ok {
		return false
	}
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return false
		}
		if requireNonBlank && strings.TrimSpace(text) == "" {
			return false
		}
	}
	return true
}

// validateUpstream 校验上游 DAG 结构:self 是对象,nodes/edges 是数组,
// 每条边都要有非空的 from 与 to。
func validateUpstream(value any, errors *[]apierror.FieldError) {
	if value == nil {
		return
	}
	upstream, ok := asJSONObject(value)
	if !ok {
		*errors = append(*errors, fieldError("upstream", "must be an object"))
		return
	}
	if _, ok := asJSONObject(upstream["self"]); !ok {
		*errors = append(*errors, fieldError("upstream.self", "must be an object"))
	}
	nodes, nodesOK := asJSONArray(upstream["nodes"])
	if !nodesOK {
		*errors = append(*errors, fieldError("upstream.nodes", "must be an array"))
	}
	edges, edgesOK := asJSONArray(upstream["edges"])
	if !edgesOK {
		*errors = append(*errors, fieldError("upstream.edges", "must be an array"))
	}
	for index, node := range nodes {
		if _, ok := asJSONObject(node); !ok {
			*errors = append(*errors,
				fieldError(fmt.Sprintf("upstream.nodes[%d]", index), "must be an object"))
		}
	}
	for index, edge := range edges {
		valid := false
		if object, ok := asJSONObject(edge); ok {
			from, fromOK := object["from"].(string)
			to, toOK := object["to"].(string)
			valid = fromOK && toOK &&
				strings.TrimSpace(from) != "" && strings.TrimSpace(to) != ""
		}
		if !valid {
			*errors = append(*errors, fieldError(fmt.Sprintf("upstream.edges[%d]", index),
				"must contain non-empty from and to"))
		}
	}
}

// validateUpstreamPayload 校验上游产出数组:每项都要有非空 agentInstanceId 与
// **存在**的 payloadResult(值本身不限类型,允许 null)。
func validateUpstreamPayload(value any, errors *[]apierror.FieldError) {
	if value == nil {
		return
	}
	items, ok := asJSONArray(value)
	if !ok {
		*errors = append(*errors, fieldError("upstreamPayload", "must be an array"))
		return
	}
	for index, raw := range items {
		item, ok := asJSONObject(raw)
		if !ok {
			*errors = append(*errors,
				fieldError(fmt.Sprintf("upstreamPayload[%d]", index), "must be an object"))
			continue
		}
		agentInstanceID, isString := item["agentInstanceId"].(string)
		if !isString || strings.TrimSpace(agentInstanceID) == "" {
			*errors = append(*errors,
				fieldError(fmt.Sprintf("upstreamPayload[%d].agentInstanceId", index),
					"is required and must be a non-empty string"))
		}
		if _, present := item["payloadResult"]; !present {
			*errors = append(*errors,
				fieldError(fmt.Sprintf("upstreamPayload[%d].payloadResult", index), "is required"))
		}
	}
}

// validateRuntimeContext 校验运行期上下文:只允许 isFinal,且必须是布尔。
func validateRuntimeContext(value any, errors *[]apierror.FieldError) {
	if value == nil {
		return
	}
	context, ok := asJSONObject(value)
	if !ok {
		*errors = append(*errors, fieldError("runtimeContext", "must be an object"))
		return
	}
	for key := range context {
		if key != "isFinal" {
			*errors = append(*errors, fieldError("runtimeContext."+key,
				"is not allowed by the bound-task v1 contract"))
		}
	}
	if _, ok := context["isFinal"].(bool); !ok {
		*errors = append(*errors, fieldError("runtimeContext.isFinal",
			"is required and must be a boolean when runtimeContext is present"))
	}
}

// validatePushCallbackURL 要求绝对 HTTPS 地址;HTTP 只对回环放行(本地集成测试用)。
func validatePushCallbackURL(value *string, errors *[]apierror.FieldError) {
	if value == nil {
		return
	}
	const message = "must be an absolute HTTPS URL (HTTP is allowed only for loopback tests)"
	parsed, err := url.Parse(*value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		*errors = append(*errors, fieldError("callbackUrl", message))
		return
	}
	if parsed.User != nil {
		*errors = append(*errors, fieldError("callbackUrl", "must not contain credentials"))
		return
	}
	if parsed.Scheme == "https" {
		return
	}
	if parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		return
	}
	*errors = append(*errors, fieldError("callbackUrl", message))
}

// ---------------------------------------------------------------------------
// 请求头与元数据校验
// ---------------------------------------------------------------------------

// validateContentType 只接受 application/json(允许带参数,如 charset)。
func validateContentType(r *http.Request) *apierror.ApiError {
	contentType := r.Header.Get("Content-Type")
	base, _, _ := strings.Cut(contentType, ";")
	if strings.EqualFold(strings.TrimSpace(base), "application/json") {
		return nil
	}
	return &apierror.ApiError{
		Status: http.StatusUnsupportedMediaType,
		Type:   "urn:mota:unsupported-content-type",
		Title:  "Unsupported Media Type",
		Detail: "Content-Type must be application/json",
	}
}

// requiredUniqueHeader 要求某个请求头恰好出现一次且为合法 ASCII。
func requiredUniqueHeader(r *http.Request, name, displayName string) (string, *apierror.ApiError) {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return "", pushValidation(displayName, "must be present exactly once")
	}
	if !asciiHeaderValue(values[0]) {
		return "", pushValidation(displayName, "must be valid ASCII header text")
	}
	return values[0], nil
}

// requiredSignatureHeader 取签名头。缺失是 401(凭证问题),重复是 422(请求构造问题)。
func requiredSignatureHeader(r *http.Request) (string, *apierror.ApiError) {
	values := r.Header.Values(SignatureHeader)
	switch len(values) {
	case 0:
		return "", apierror.Unauthorized("missing X-MoTA-Signature header")
	case 1:
		if !asciiHeaderValue(values[0]) {
			return "", apierror.Unauthorized("X-MoTA-Signature is invalid")
		}
		return values[0], nil
	default:
		return "", pushValidation("X-MoTA-Signature", "must be present exactly once")
	}
}

// validatePushMetadata 校验 runId / 幂等键:非空、不超长、无首尾空白、无控制字符。
func validatePushMetadata(value, field string, maxLen int) *apierror.ApiError {
	if value == "" || len(value) > maxLen || value != strings.TrimSpace(value) ||
		hasASCIIControl(value) {
		return pushValidation(field, fmt.Sprintf(
			"must contain 1-%d characters without outer whitespace or controls", maxLen))
	}
	return nil
}

// hasASCIIControl 判断是否含 ASCII 控制字符。
func hasASCIIControl(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func fieldError(field, message string) apierror.FieldError {
	return apierror.FieldError{Field: field, Message: message}
}

func pushValidation(field, message string) *apierror.ApiError {
	return apierror.Validation([]apierror.FieldError{fieldError(field, message)})
}

// requireNonEmpty 要求字段存在且去空白后非空。
func requireNonEmpty(errors *[]apierror.FieldError, field string, value *string) {
	if value == nil || strings.TrimSpace(*value) == "" {
		*errors = append(*errors, fieldError(field, "is required and must not be empty"))
	}
}

// requireExact 要求字段严格等于期望值(缺失也算不等)。
func requireExact(errors *[]apierror.FieldError, field string, value *string, expected string) {
	if value == nil || *value != expected {
		*errors = append(*errors, fieldError(field, fmt.Sprintf("must equal %q", expected)))
	}
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// executionBudget 把「配置的任务超时」与「协议安全预算」收敛成实际使用的超时。
//
// 配置层默认 task_timeout_secs=600,但手工构造的 Config 可能压根没设这个字段。
// 直接取 min 会得到 0 —— 每个请求立刻 504,而且看不出是配置漏了还是真超时。
// 所以 configured <= 0 时退回安全预算,让请求至少有机会跑完。
func executionBudget(configured, budget time.Duration) time.Duration {
	if configured <= 0 || configured > budget {
		return budget
	}
	return configured
}

// writePushJSON 输出结果信封。
func writePushJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func readAllLimited(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}
