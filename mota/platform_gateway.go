package mota

// 动态 Agent 的 HTTP 入口 POST /agent/{slug}。
// 对应 Rust 侧 src/platform/gateway.rs。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 请求头常量
// ---------------------------------------------------------------------------

const (
	agentSignatureHeader = "X-Agent-Signature"
	motaSignatureHeader  = "X-MoTA-Signature"
	agentTimestampHeader = "X-Agent-Timestamp"
	agentNonceHeader     = "X-Agent-Nonce"
)

// runIDHeaders / idempotencyHeaders 是两组等价别名。**合起来只允许出现一次** ——
// 两个别名同时出现会让签名 canonical 的取值有歧义。
var (
	runIDHeaders       = []string{"X-Agent-Run-Id", "X-MoTA-Run-Id"}
	idempotencyHeaders = []string{"X-Idempotency-Key", "X-MoTA-Idempotency-Key"}
)

// signedRequestWindow 是签名请求允许的时钟偏移。
const signedRequestWindow = 300

// requestNonceRetention 是 nonce 去重记录的保留时长(秒)。
const requestNonceRetention = 600

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

const jsonContentType = "application/json; charset=utf-8"

// serializedError 生成 {"error":{"code":...,"message":...}} 文本。
func serializedError(code, message string) string {
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return `{"error":{"code":"serialization_failed"}}`
	}
	return string(encoded)
}

// writeAPIError 输出错误信封。
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, serializedError(code, message))
}

// writeAuthError 输出带 WWW-Authenticate 的鉴权错误。
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("WWW-Authenticate", `HMAC realm="agent"`)
	writeAPIError(w, status, code, message)
}

// writeInvalidHeader 输出重复/非 ASCII 请求头的统一错误。
func writeInvalidHeader(w http.ResponseWriter, name string) {
	writeAPIError(w, http.StatusBadRequest, "invalid_header",
		fmt.Sprintf("%s header must appear once and contain valid ASCII", name))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeAgentSuccess 输出成功响应并带上 Agent 溯源头。
func writeAgentSuccess(w http.ResponseWriter, body, slug string, revision int64, runID string, replayed bool) {
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("X-Agent-Slug", slug)
	w.Header().Set("X-Agent-Revision", strconv.FormatInt(revision, 10))
	w.Header().Set("X-Agent-Run-Id", runID)
	if replayed {
		w.Header().Set("X-Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// executionErrorStatus 把执行/分发错误码映射成 HTTP 状态。
func executionErrorStatus(code string) int {
	switch code {
	case "execution_timeout":
		return http.StatusGatewayTimeout
	case "executor_unavailable", "agent_disabled", "agent_not_found":
		return http.StatusServiceUnavailable
	case "agent_overloaded":
		return http.StatusTooManyRequests
	case "output_too_large", "input_too_large", "invalid_executor_output", "usage_unavailable":
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// ---------------------------------------------------------------------------
// 请求头校验
// ---------------------------------------------------------------------------

// headerOnce 读取一组等价请求头,合计出现超过一次即视为非法。
// 值必须是可打印 ASCII(HTTP 头本就该如此,这里显式拦住控制字符与非 ASCII)。
func headerOnce(h http.Header, names ...string) (string, bool) {
	selected := ""
	found := false
	for _, name := range names {
		for _, value := range h.Values(name) {
			if found {
				return "", false
			}
			if !asciiHeaderValue(value) {
				return "", false
			}
			selected = value
			found = true
		}
	}
	return selected, true
}

// asciiHeaderValue 判断请求头取值是否为合法 ASCII(不含控制字符)。
func asciiHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// metadataValid 校验 runId / 幂等键:非空、不超长、无首尾空白、全为可打印 ASCII。
func metadataValid(value string, limit int) bool {
	if value == "" || len(value) > limit || value != strings.TrimSpace(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// validRequestNonce 要求 16-128 个 URL 安全字符。
func validRequestNonce(nonce string) bool {
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	for i := 0; i < len(nonce); i++ {
		c := nonce[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

// agentRequest 是一次请求经过校验后的可用形态。
type agentRequest struct {
	slug     string
	runtime  *AgentRuntime
	body     []byte
	raw      map[string]any
	task     *BoundTask
	runID    string
	explicit bool
	// recordKey / lease 为空表示本请求没有持有幂等租约。
	recordKey string
	lease     string
	context   *TaskContext
}

// Gateway 处理 POST /agent/{slug}。
//
// 流程:Agent 查找 → 读体 → 请求头去重 → 签名鉴权 → 信封解析 → runId/幂等键校验
// → 幂等租约 → 三道容量闸 → 登记运行 → 同步执行或异步 202 → 落库并回响应。
//
// 任何一步失败都必须把已获得的幂等租约释放掉,否则同一个键会被永久占住。
func (s *PlatformState) Gateway(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	slug := strings.TrimPrefix(r.URL.Path, "/agent/")

	runtime := s.Registry.Get(slug)
	if runtime == nil || !runtime.Config.Enabled {
		writeAPIError(w, http.StatusNotFound, "agent_not_found", "Agent not found")
		return
	}

	body, ok := s.readGatewayBody(w, r)
	if !ok {
		return
	}

	signedRunID, signedIdempotencyKey, ok := s.authenticateGateway(w, r, slug, runtime, body)
	if !ok {
		return
	}

	request, ok := s.parseGatewayEnvelope(w, slug, runtime, body, signedRunID, signedIdempotencyKey)
	if !ok {
		return
	}

	if !s.beginGatewayIdempotency(w, r.Context(), request) {
		return
	}

	release := func() {
		s.releaseGatewayIdempotency(r.Context(), request)
	}
	if !s.acquireGatewayCapacity(w, r.Context(), started, request, release) {
		return
	}
	// 三道许可只能释放一次。异步分支会把所有权**移交**给后台协程,所以这里用
	// handOff 抑制 defer —— 否则 202 一返回许可就没了,后台执行不受并发上限约束。
	var releaseOnce sync.Once
	releaseSlots := func() {
		releaseOnce.Do(func() {
			s.GlobalExecution.Release()
			request.runtime.ExecutionSlots.Release()
			request.runtime.AdmissionSlots.Release()
		})
	}
	handOff := false
	defer func() {
		if !handOff {
			releaseSlots()
		}
	}()

	if !s.startGatewayRun(w, r.Context(), request, release) {
		return
	}
	request.context = gatewayTaskContext(request)

	totalTimeout := request.runtime.Config.Timeout()
	// 异步形态:body 带 callbackUrl 时立即 202 ack,后台执行并 callback 回传,
	// 避免同步等待本机 Agent 执行(十几秒)导致平台侧等待超时。
	if callbackURL := gatewayCallbackURL(request.task); callbackURL != "" {
		handOff = true
		s.runGatewayAsync(request, callbackURL, started,
			remainingBudget(started, totalTimeout), releaseSlots)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	s.runGatewaySync(w, r.Context(), request, started, remainingBudget(started, totalTimeout))
}

// readGatewayBody 读取并限制请求体大小。
func (s *PlatformState) readGatewayBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := int64(s.Config.MaxRequestBodyBytes)
	// 多读 1 字节以便区分「正好等于上限」与「超过上限」。
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_body", "Cannot read body")
		return nil, false
	}
	if int64(len(body)) > limit {
		writeAPIError(w, http.StatusRequestEntityTooLarge,
			"body_too_large", "Request body exceeded configured limit")
		return nil, false
	}
	return body, true
}

// gatewayCallbackURL 取出去空白后的 callbackUrl;为空表示走同步形态。
func gatewayCallbackURL(task *BoundTask) string {
	if task.CallbackURL == nil {
		return ""
	}
	return strings.TrimSpace(*task.CallbackURL)
}

// ---------------------------------------------------------------------------
// 签名鉴权
// ---------------------------------------------------------------------------

// authenticateGateway 校验签名并返回签名覆盖的 runId 与幂等键。
//
// 两条互斥路径:
//   - X-Agent-Signature v1:签 canonical 串(方法 + 路径 + 时间戳 + nonce +
//     runId + 幂等键 + body 摘要),额外要求时间窗与 nonce 一次性,防重放;
//   - X-MoTA-Signature:存量 MOTA 协议的兼容路径,**只签原始 body**,
//     因此没有重放保护 —— 新的直连调用方都应该用 v1。
func (s *PlatformState) authenticateGateway(
	w http.ResponseWriter,
	r *http.Request,
	slug string,
	runtime *AgentRuntime,
	body []byte,
) (signedRunID string, signedIdempotencyKey string, ok bool) {
	signedRunID, valid := headerOnce(r.Header, runIDHeaders...)
	if !valid {
		writeInvalidHeader(w, "run ID")
		return "", "", false
	}
	signedIdempotencyKey, valid = headerOnce(r.Header, idempotencyHeaders...)
	if !valid {
		writeInvalidHeader(w, "idempotency key")
		return "", "", false
	}
	agentSignature, valid := headerOnce(r.Header, agentSignatureHeader)
	if !valid {
		writeInvalidHeader(w, agentSignatureHeader)
		return "", "", false
	}
	motaSignature, valid := headerOnce(r.Header, motaSignatureHeader)
	if !valid {
		writeInvalidHeader(w, motaSignatureHeader)
		return "", "", false
	}
	if agentSignature != "" && motaSignature != "" {
		writeAPIError(w, http.StatusBadRequest, "ambiguous_signature",
			"Send exactly one Agent signature header")
		return "", "", false
	}

	switch {
	case agentSignature != "":
		if !s.verifyAgentSignatureV1(w, r, slug, runtime, body,
			agentSignature, signedRunID, signedIdempotencyKey) {
			return "", "", false
		}
	case motaSignature != "":
		if !VerifySignature(runtime.Config.SigningSecret, body, motaSignature,
			runtime.Config.SignatureEncodingOrHex()) {
			slog.Warn("MoTA-compatible Agent request rejected: signature mismatch", "agent", slug)
			writeAuthError(w, http.StatusUnauthorized, "invalid_signature",
				"Request signature is invalid")
			return "", "", false
		}
	default:
		writeAuthError(w, http.StatusUnauthorized, "missing_signature",
			"Missing X-Agent-Signature or X-MoTA-Signature header")
		return "", "", false
	}
	return signedRunID, signedIdempotencyKey, true
}

// verifyAgentSignatureV1 校验 v1 canonical 签名,并消费一次性 nonce。
func (s *PlatformState) verifyAgentSignatureV1(
	w http.ResponseWriter,
	r *http.Request,
	slug string,
	runtime *AgentRuntime,
	body []byte,
	signature, signedRunID, signedIdempotencyKey string,
) bool {
	timestampText, valid := headerOnce(r.Header, agentTimestampHeader)
	if !valid {
		writeInvalidHeader(w, agentTimestampHeader)
		return false
	}
	if timestampText == "" {
		writeAuthError(w, http.StatusUnauthorized, "missing_timestamp",
			"X-Agent-Timestamp is required for X-Agent-Signature")
		return false
	}
	nonce, valid := headerOnce(r.Header, agentNonceHeader)
	if !valid {
		writeInvalidHeader(w, agentNonceHeader)
		return false
	}
	if nonce == "" {
		writeAuthError(w, http.StatusUnauthorized, "missing_nonce",
			"X-Agent-Nonce is required for X-Agent-Signature")
		return false
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "invalid_timestamp",
			"X-Agent-Timestamp must be Unix seconds")
		return false
	}
	now := time.Now().Unix()
	if abs64(now-timestamp) > signedRequestWindow {
		writeAuthError(w, http.StatusUnauthorized, "stale_request",
			"Signed request timestamp is outside the 5 minute window")
		return false
	}
	if !validRequestNonce(nonce) {
		writeAuthError(w, http.StatusUnauthorized, "invalid_nonce",
			"X-Agent-Nonce must contain 16-128 URL-safe characters")
		return false
	}

	canonical := fmt.Sprintf("v1\nPOST\n/agent/%s\n%s\n%s\n%s\n%s\n%s",
		slug, timestampText, nonce, signedRunID, signedIdempotencyKey, bodyHash(body))
	if !VerifySignature(runtime.Config.SigningSecret, []byte(canonical), signature,
		runtime.Config.SignatureEncodingOrHex()) {
		slog.Warn("agent request rejected: canonical signature mismatch", "agent", slug)
		writeAuthError(w, http.StatusUnauthorized, "invalid_signature",
			"Request signature is invalid")
		return false
	}

	fresh, err := s.Store.ConsumeRequestNonce(r.Context(), slug, nonce, now+requestNonceRetention)
	if err != nil {
		slog.Error("request nonce persistence failed", "agent", slug, "error", err)
		// 注意这条**不带** WWW-Authenticate:它是服务端故障,不是凭证问题。
		writeAPIError(w, http.StatusServiceUnavailable, "state_unavailable",
			"Request replay protection is temporarily unavailable")
		return false
	}
	if !fresh {
		writeAuthError(w, http.StatusUnauthorized, "replayed_request",
			"This signed nonce has already been used")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 信封解析
// ---------------------------------------------------------------------------

// parseGatewayEnvelope 解析请求体并校验 runId / 幂等键。
func (s *PlatformState) parseGatewayEnvelope(
	w http.ResponseWriter,
	slug string,
	runtime *AgentRuntime,
	body []byte,
	signedRunID, signedIdempotencyKey string,
) (*agentRequest, bool) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON")
		return nil, false
	}
	raw, isObject := decoded.(map[string]any)
	if !isObject {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_body",
			"Request body must be a JSON object")
		return nil, false
	}
	// 已知信封字段的类型必须正确;类型不符说明调用方发错了协议,不能带着
	// 半解析的信封继续跑。
	var task BoundTask
	if err := json.Unmarshal(body, &task); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_task_envelope",
			"Known task envelope fields have invalid types")
		return nil, false
	}

	bodyRunID := ""
	if task.RunID != nil {
		bodyRunID = *task.RunID
	}
	if signedRunID != "" && bodyRunID != "" && signedRunID != bodyRunID {
		writeAPIError(w, http.StatusUnprocessableEntity, "conflicting_run_id",
			"Run ID in the header and request body must match")
		return nil, false
	}
	explicitRunID := signedRunID
	if explicitRunID == "" {
		explicitRunID = bodyRunID
	}
	runID := ""
	switch {
	case explicitRunID != "" && metadataValid(explicitRunID, 160):
		runID = explicitRunID
	case explicitRunID != "":
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_run_id",
			"Run ID must contain 1-160 printable ASCII characters without outer whitespace")
		return nil, false
	default:
		runID = "run_" + randomToken(12)
	}

	if signedIdempotencyKey != "" && !metadataValid(signedIdempotencyKey, 200) {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_idempotency_key",
			"Idempotency key must contain 1-200 printable ASCII characters without outer whitespace")
		return nil, false
	}

	request := &agentRequest{
		slug: slug, runtime: runtime, body: body, raw: raw, task: &task,
		runID: runID, explicit: explicitRunID != "",
	}
	if signedIdempotencyKey != "" {
		// 缓存的响应只属于产出它的那个 Agent 修订版本。
		request.recordKey = IdempotencyRecordKey(
			runtime.Config.ID, runtime.Config.Revision, signedIdempotencyKey)
	}
	return request, true
}

// gatewayTaskContext 从信封构造执行上下文。
func gatewayTaskContext(request *agentRequest) *TaskContext {
	task := request.task
	// templateInputData 或 params 任一存在时才走入参视图,否则整个 body 就是入参
	// (兼容把业务字段直接平铺在顶层的调用方)。
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
	return &TaskContext{
		RunID:               &runID,
		TaskBrief:           taskBrief,
		Params:              params,
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
// 幂等与容量
// ---------------------------------------------------------------------------

// beginGatewayIdempotency 申请幂等租约。命中缓存时直接回放响应并返回 false。
func (s *PlatformState) beginGatewayIdempotency(
	w http.ResponseWriter,
	ctx context.Context,
	request *agentRequest,
) bool {
	if request.recordKey == "" {
		return true
	}
	decision, err := s.Store.BeginIdempotency(ctx, request.slug, request.recordKey,
		bodyHash(request.body), request.runID, request.explicit)
	if err != nil {
		slog.Error("idempotency lookup failed", "agent", request.slug, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "state_unavailable",
			"Agent state is temporarily unavailable")
		return false
	}
	switch decision.Kind {
	case IdempotencyCached:
		writeAgentSuccess(w, decision.ResponseBody, request.slug,
			request.runtime.Config.Revision, decision.RunID, true)
		return false
	case IdempotencyInProgress:
		writeAPIError(w, http.StatusConflict, "request_in_progress",
			"A request with this idempotency key is already running")
		return false
	case IdempotencyConflict:
		writeAPIError(w, http.StatusConflict, "idempotency_conflict",
			"The idempotency key was already used with different request metadata")
		return false
	default:
		request.lease = decision.LeaseID
		request.runID = decision.RunID
		return true
	}
}

// releaseGatewayIdempotency 释放本请求持有的租约(没有租约时是 no-op)。
func (s *PlatformState) releaseGatewayIdempotency(ctx context.Context, request *agentRequest) {
	if request.recordKey == "" || request.lease == "" {
		return
	}
	if err := s.Store.ReleaseIdempotency(ctx, request.slug, request.recordKey, request.lease); err != nil {
		slog.Error("release idempotency record failed",
			"agent", request.slug, "key", request.recordKey, "error", err)
	}
}

// acquireGatewayCapacity 依次过准入闸、该 Agent 的执行闸、全局执行闸。
//
// 与分发器同一套顺序:先拿 Agent 自己的容量,免得一个热点 Agent 在自己队列里
// 干等的同时把全局槽位全占走。任何一步失败都要 release 幂等租约。
func (s *PlatformState) acquireGatewayCapacity(
	w http.ResponseWriter,
	ctx context.Context,
	started time.Time,
	request *agentRequest,
	release func(),
) bool {
	if !request.runtime.AdmissionSlots.TryAcquire() {
		release()
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "agent_overloaded", "Agent queue is full")
		return false
	}
	total := request.runtime.Config.Timeout()
	if !request.runtime.ExecutionSlots.AcquireTimeout(ctx, remainingBudget(started, total)) {
		request.runtime.AdmissionSlots.Release()
		release()
		writeAPIError(w, http.StatusGatewayTimeout, "queue_timeout",
			"Agent did not become available before the deadline")
		return false
	}
	if !s.GlobalExecution.AcquireTimeout(ctx, remainingBudget(started, total)) {
		request.runtime.ExecutionSlots.Release()
		request.runtime.AdmissionSlots.Release()
		release()
		writeAPIError(w, http.StatusGatewayTimeout, "queue_timeout",
			"Global execution capacity did not become available before the deadline")
		return false
	}
	return true
}

// startGatewayRun 登记运行开始。runId 冲突或存储故障都要释放幂等租约。
func (s *PlatformState) startGatewayRun(
	w http.ResponseWriter,
	ctx context.Context,
	request *agentRequest,
	release func(),
) bool {
	requestJSON := string(request.body)
	started, err := s.Store.StartRun(ctx, request.slug, request.runID,
		request.runtime.Config.Revision,
		string(request.runtime.Config.ExecutorKind),
		string(request.runtime.Config.ExecutionMode), &requestJSON)
	if err != nil {
		slog.Error("record run start failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		release()
		writeAPIError(w, http.StatusServiceUnavailable, "state_unavailable",
			"Agent run state is temporarily unavailable")
		return false
	}
	if !started {
		release()
		writeAPIError(w, http.StatusConflict, "run_id_conflict",
			"This Agent run ID has already been used")
		return false
	}
	return true
}

// gatewayCompleteTransition / gatewayReleaseTransition 构造随运行终态提交的幂等迁移。
// 没有持有租约时返回 nil。
func gatewayCompleteTransition(request *agentRequest, responseBody string) *IdempotencyFinalization {
	if request.recordKey == "" || request.lease == "" {
		return nil
	}
	return CompleteIdempotency(request.recordKey, request.lease, responseBody)
}

func gatewayReleaseTransition(request *agentRequest) *IdempotencyFinalization {
	if request.recordKey == "" || request.lease == "" {
		return nil
	}
	return ReleaseIdempotencyTransition(request.recordKey, request.lease)
}

// int64Ptr 便于填 RunFinalization 的可选字段。
func int64Ptr(value int64) *int64 { return &value }

// ---------------------------------------------------------------------------
// 同步执行
// ---------------------------------------------------------------------------

// usageUnavailableMessage 是受信通道拿不到 FINAL usage 时的对外文案。
const usageUnavailableMessage = "Executor did not return final provider token usage"

// usageUnavailableDetail 是同一情况落库时的内部文案(首字母小写,与 rs 一致)。
const usageUnavailableDetail = "executor did not return final provider token usage"

// runGatewaySync 同步执行并直接回响应。
func (s *PlatformState) runGatewaySync(
	w http.ResponseWriter,
	ctx context.Context,
	request *agentRequest,
	started time.Time,
	remaining time.Duration,
) {
	executor, err := s.Executor()
	if err != nil {
		slog.Error("build agent executor failed", "agent", request.slug, "error", err)
		s.finalizeGatewayFailure(ctx, request, "executor_unavailable",
			"agent executor is unavailable", http.StatusServiceUnavailable,
			nil, time.Since(started).Milliseconds())
		writeAPIError(w, http.StatusServiceUnavailable, "executor_unavailable",
			"agent executor is unavailable")
		return
	}
	output, execErr := executor.Execute(ctx, request.runtime, request.context, request.raw, remaining)
	durationMs := time.Since(started).Milliseconds()

	if execErr != nil {
		execution := toExecutionError(execErr, "processor_failed")
		slog.Error("agent execution failed",
			"agent", request.slug, "run_id", request.runID, "error", execErr)
		status := executionErrorStatus(execution.Code)
		if !s.finalizeGatewayFailure(ctx, request, execution.Code, execution.PublicMessage(),
			status, execution.ExecutorResultJSON(), durationMs) {
			writeAPIError(w, http.StatusServiceUnavailable, "state_unavailable",
				"Agent failed and its terminal state could not be committed")
			return
		}
		writeAPIError(w, status, execution.Code, execution.PublicMessage())
		return
	}

	executorResultJSON := output.ExecutorResultJSON()
	// 受信通道必须回传供应商 FINAL usage;拿不到就判失败,不能静默降级成
	// 「没有用量」—— 平台会按缺失计费口径处理。
	if s.TrustedUsageReportingEnabled && output.Usage == nil {
		s.finalizeGatewayFailure(ctx, request, "usage_unavailable", usageUnavailableDetail,
			http.StatusBadGateway, executorResultJSON, durationMs)
		writeAPIError(w, http.StatusBadGateway, "usage_unavailable", usageUnavailableMessage)
		return
	}

	payload := output.ToTransportPayload(request.context.Language)
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Error("serialize agent output failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		s.finalizeGatewayFailure(ctx, request, "invalid_agent_output",
			"Agent output could not be serialized", http.StatusInternalServerError,
			executorResultJSON, durationMs)
		writeAPIError(w, http.StatusInternalServerError, "invalid_agent_output",
			"Agent output could not be serialized")
		return
	}
	responseBody := string(encoded)

	if err := s.Store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: request.slug, RunID: request.runID, Status: "completed",
		DurationMs: durationMs, ResponseCode: int64Ptr(http.StatusOK),
		ResultJSON: &responseBody, ExecutorResultJSON: executorResultJSON,
		Idempotency: gatewayCompleteTransition(request, responseBody),
	}); err != nil {
		slog.Error("commit Agent result failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "state_unavailable",
			"Agent result was produced but its state could not be committed")
		return
	}
	writeAgentSuccess(w, responseBody, request.slug,
		request.runtime.Config.Revision, request.runID, false)
}

// finalizeGatewayFailure 记录失败终态并释放幂等租约。返回落库是否成功。
//
// message 是写进 result_json 的文案。注意它未必等于回给调用方的文案 ——
// usage_unavailable 落库用小写开头的内部描述,对外用大写开头的公开文案(与 rs 一致)。
func (s *PlatformState) finalizeGatewayFailure(
	ctx context.Context,
	request *agentRequest,
	code, message string,
	responseStatus int,
	executorResultJSON *string,
	durationMs int64,
) bool {
	body := serializedError(code, message)
	if err := s.Store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: request.slug, RunID: request.runID, Status: "failed",
		DurationMs: durationMs, ErrorCode: &code,
		ResponseCode: int64Ptr(int64(responseStatus)), ResultJSON: &body,
		ExecutorResultJSON: executorResultJSON,
		Idempotency:        gatewayReleaseTransition(request),
	}); err != nil {
		slog.Error("record run failure failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 异步执行(callbackUrl 分支)
// ---------------------------------------------------------------------------

// runGatewayAsync 在后台执行并把结果 callback 回传。
//
// 请求已经 202 ack,所以这里**完全脱离请求生命周期**:用 context.Background(),
// 执行时限由 remaining 参数控制。这是有意的 —— 客户端断开不该中止已受理的运行。
// releaseSlots 是 Gateway 移交过来的容量许可释放函数,后台执行结束时调用。
func (s *PlatformState) runGatewayAsync(
	request *agentRequest,
	callbackURL string,
	started time.Time,
	remaining time.Duration,
	releaseSlots func(),
) {
	go func() {
		// 三道容量许可在这条协程里继续持有,直到执行与回传都收尾 ——
		// 与 rs 把 permit move 进 spawn task 的生命周期一致。
		defer releaseSlots()
		ctx := context.Background()
		executor, err := s.Executor()
		if err != nil {
			slog.Error("build agent executor failed", "agent", request.slug, "error", err)
			s.finalizeGatewayFailure(ctx, request, "executor_unavailable",
				"agent executor is unavailable", http.StatusAccepted,
				nil, time.Since(started).Milliseconds())
			return
		}
		output, execErr := executor.Execute(ctx, request.runtime, request.context, request.raw, remaining)
		durationMs := time.Since(started).Milliseconds()

		if execErr != nil {
			execution := toExecutionError(execErr, "processor_failed")
			slog.Error("async agent execution failed",
				"agent", request.slug, "run_id", request.runID, "error", execErr)
			// 异步分支的 response_code 记 202(已 ack 的那次响应),不是错误码对应的状态。
			s.finalizeGatewayFailure(ctx, request, execution.Code, execution.PublicMessage(),
				http.StatusAccepted, execution.ExecutorResultJSON(), durationMs)
			return
		}

		executorResultJSON := output.ExecutorResultJSON()
		if s.TrustedUsageReportingEnabled && output.Usage == nil {
			// 受信通道拿不到 FINAL usage 时**连 callback 都不发** ——
			// 宁可让平台侧超时重推,也不能投递一份不带用量的结果。
			s.finalizeGatewayFailure(ctx, request, "usage_unavailable", usageUnavailableDetail,
				http.StatusBadGateway, executorResultJSON, durationMs)
			slog.Error("trusted callback suppressed because FINAL usage is unavailable",
				"agent", request.slug, "run_id", request.runID)
			return
		}

		payload := output.ToTransportPayload(request.context.Language)
		encoded, err := json.Marshal(payload)
		if err != nil {
			slog.Error("serialize async agent output failed",
				"agent", request.slug, "run_id", request.runID, "error", err)
			s.finalizeGatewayFailure(ctx, request, "invalid_agent_output",
				"Agent output could not be serialized", http.StatusAccepted,
				executorResultJSON, durationMs)
			return
		}
		bodyText := string(encoded)
		transition := gatewayCompleteTransition(request, bodyText)

		s.deliverGatewayCallback(ctx, request, callbackURL, payload, bodyText)

		// 无论 callback 成功与否都把运行记为 completed:结果确实产出了,
		// 投递失败是通道问题,已经单独记进 channel_events。
		if err := s.Store.FinalizeRun(ctx, RunFinalization{
			AgentSlug: request.slug, RunID: request.runID, Status: "completed",
			DurationMs: durationMs, ResponseCode: int64Ptr(http.StatusAccepted),
			ResultJSON: &bodyText, ExecutorResultJSON: executorResultJSON,
			Idempotency: transition,
		}); err != nil {
			slog.Error("record async run completion failed",
				"agent", request.slug, "run_id", request.runID, "error", err)
		}
	}()
}

// deliverGatewayCallback 投递结果并把这次通道往返记进 channel_events。
func (s *PlatformState) deliverGatewayCallback(
	ctx context.Context,
	request *agentRequest,
	callbackURL string,
	payload *PayloadV1,
	bodyText string,
) {
	client := s.CallbackHTTPClient
	if client == nil {
		client = s.HTTPClient
	}
	callbackStarted := time.Now()
	receipt, err := SendCallback(ctx, client, callbackURL, request.runID,
		request.runtime.Config.SigningSecret, s.CallbackToken, payload)
	if err != nil {
		detail := err.Error()
		errorJSON := serializedError("callback_delivery_failed", detail)
		slog.Warn("async callback delivery failed",
			"agent", request.slug, "run_id", request.runID, "error", err)
		_ = s.Store.RecordChannelEvent(ctx, ChannelEventRecord{
			EventType: "callback", AgentSlug: &request.slug, RunID: &request.runID,
			URL: callbackURL, Detail: &detail, Input: &bodyText, Output: &errorJSON,
		})
		return
	}
	elapsedMs := time.Since(callbackStarted).Milliseconds()
	receiptJSON := `{"code":"OK"}`
	if encoded, encodeErr := json.Marshal(receipt); encodeErr == nil {
		receiptJSON = string(encoded)
	}
	slog.Info("async callback delivered",
		"agent", request.slug, "run_id", request.runID,
		"accepted", receipt.Accepted, "run_status", derefOrEmpty(receipt.RunStatus))
	_ = s.Store.RecordChannelEvent(ctx, ChannelEventRecord{
		EventType: "callback", AgentSlug: &request.slug, RunID: &request.runID,
		URL: callbackURL, StatusCode: int64Ptr(http.StatusOK),
		Accepted: &receipt.Accepted, RunStatus: receipt.RunStatus,
		LatencyMs: int64Ptr(elapsedMs), Input: &bodyText, Output: &receiptJSON,
	})
}

// ---------------------------------------------------------------------------
// 管理台鉴权辅助
// ---------------------------------------------------------------------------

// (管理台鉴权已迁到 AdminAuth,见 platform_auth.go。)
