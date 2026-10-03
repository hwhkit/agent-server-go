package mota

// Pull 接入:worker 轮询队列。对应 rs 侧 src/mota/worker.rs。
//
// 两条独立循环(协议要求各跑一条):
//   - claim 循环:长轮询 POST {queue}/claim?workerId=…,拿到任务后处理并
//     POST {queue}/complete 回传;任务处理期间按 renew_interval 续租,
//     收到 409 BOUND_CLAIM_CONFLICT 立即放弃该任务。
//   - heartbeat 循环:按回执 heartbeatIntervalSeconds(默认 60s)上报存活。
//
// claim 是长轮询,HTTP 客户端读超时由 claim_timeout(>= 35s)控制;
// 收到 204 立即重新 claim,不额外 sleep。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hwhkit/hwhkit-go/core/shutdown"
)

// claimRetryDelay 是 claim 请求失败/异常状态码后的重试等待。
const claimRetryDelay = 1 * time.Second

const (
	// claimResponseBodyLimit follows the service's public 4 MiB request bound.
	// A claimed task may contain the complete input envelope, so it needs more
	// room than the small complete/heartbeat receipts.
	claimResponseBodyLimit = 4 * 1024 * 1024
	// queueReceiptResponseBodyLimit bounds complete receipts.
	queueReceiptResponseBodyLimit = 64 * 1024
	// heartbeatResponseBodyLimit also bounds audit output for Push heartbeats.
	heartbeatResponseBodyLimit = 64 * 1024
	// Server-controlled seconds are capped before time.Duration conversion to
	// prevent uint64 overflow into a negative hot-loop interval.
	maxHeartbeatIntervalSeconds uint64 = 60 * 60
)

type PullWorker struct {
	cfg       *Config
	execution *TaskExecutionBackend
	token     *shutdown.Token
	client    *http.Client

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewWorker 是嵌入/测试用的便捷构造:直接跑进程内 Processor。
//
// 生产装配(#12)应使用 NewWorkerWithBackend 并传 *TaskExecutionBackend,
// 让 Pull 与 Push 共用同一套 Agent 分发逻辑。
func NewWorker(cfg *Config, processor Processor, token *shutdown.Token) *PullWorker {
	return NewWorkerWithBackend(cfg, NewProcessorBackend(processor), token)
}

// NewWorkerWithBackend 是生产装配入口。
func NewWorkerWithBackend(cfg *Config, execution *TaskExecutionBackend, token *shutdown.Token) *PullWorker {
	return &PullWorker{
		cfg:       cfg,
		execution: execution,
		token:     token,
		client: &http.Client{
			Timeout:       cfg.ClaimTimeout,
			CheckRedirect: CredentialedRedirectPolicy,
		},
	}
}

func (w *PullWorker) Start() {
	ctx, cancel := context.WithCancel(w.token.Context())
	w.cancel = cancel
	w.wg.Add(2)
	go w.claimLoop(ctx)
	go w.heartbeatLoop(ctx)
	slog.Info("pull worker started", "queue_origin", callbackOrigin(w.cfg.QueueURL), "worker_id", w.cfg.WorkerID)
}

func (w *PullWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	slog.Info("pull worker stopped")
}

// ---------------------------------------------------------------------------
// claim 循环
// ---------------------------------------------------------------------------

func (w *PullWorker) claimLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		resp, err := w.doPost(ctx, "/claim", map[string]string{"workerId": w.cfg.WorkerID}, nil, nil)
		if err != nil {
			slog.Warn("claim: request failed", "err", err)
			if !w.retryAfterDelay(ctx) {
				return
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			var claim ClaimResponse
			if err := decodeJSON(resp, &claim, claimResponseBodyLimit); err != nil {
				slog.Warn("claim: failed to parse 200 body", "err", err)
				if !w.retryAfterDelay(ctx) {
					return
				}
				continue
			}
			slog.Info("claimed task, processing", "run_id", claim.RunID)
			// 与 Rust 基准一致：一个 worker 串行认领，避免已租约任务无限累积。
			w.handleTask(ctx, claim)
		case http.StatusNoContent:
			closeBody(resp)
		default:
			slog.Warn("claim: unexpected status", "status", resp.StatusCode)
			closeBody(resp)
			if !w.retryAfterDelay(ctx) {
				return
			}
		}
	}
}

// retryAfterDelay 等待重试或立刻响应关闭,避免退避 sleep 延迟进程退出。
func (w *PullWorker) retryAfterDelay(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(claimRetryDelay):
		return true
	}
}

// ---------------------------------------------------------------------------
// 单任务:处理 + 续租 + 回传
// ---------------------------------------------------------------------------

type processResult struct {
	out *ProcessedOutput
	err error
}

func (w *PullWorker) handleTask(ctx context.Context, claim ClaimResponse) {
	runID := claim.RunID
	agentCode := derefOrEmpty(claim.AgentCode)

	// templateInputData 可能直接是任务体,也可能是 { taskBrief, params } 包裹;
	// 尽量提取 params,否则整段作为 params。
	params := claimParams(claim.TemplateInputData)

	taskBrief := claim.TaskBrief
	if taskBrief == nil {
		if object, ok := asMapValue(claim.TemplateInputData); ok {
			taskBrief = object["taskBrief"]
		} else if claim.TemplateInputData != nil {
			taskBrief = claim.TemplateInputData
		}
	}

	task := &TaskContext{
		RunID:               &runID,
		TaskBrief:           taskBrief,
		Params:              params,
		PositionType:        claim.PositionType,
		Language:            claimLanguage(claim.GlobalParameters),
		SymbolPool:          ExtractSymbolPool(claim.GlobalParameters),
		UniverseMode:        ExtractUniverseMode(claim.GlobalParameters),
		UpstreamSymbols:     ExtractUpstreamSymbols(claim.UpstreamPayload, claim.PositionType),
		ForceFailureDefault: ExtractFailurePath(taskBrief),
		RuntimeContext:      claim.RuntimeContext,
	}

	// rawInput 是执行器审计/网关请求体使用的原始负载:claim 响应体本身。
	rawInput := claimRawInput(claim)

	// 续租循环:处理期间定期 renew;409 时提前退出(放弃任务)。
	renewCtx, renewCancel := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		w.renewLoop(renewCtx, runID)
	}()

	taskCtx, taskCancel := context.WithTimeout(ctx, w.cfg.TaskTimeout)
	defer taskCancel()

	resultCh := make(chan *processResult, 1)
	go func() {
		result := &processResult{}
		defer func() {
			if recover() != nil {
				result.out = nil
				result.err = errors.New("pull task execution panicked")
			}
			resultCh <- result
		}()
		result.out, result.err = w.execution.Execute(
			taskCtx, agentCode, task, rawInput, w.cfg.TaskTimeout)
		if result.err == nil && result.out == nil {
			result.err = errors.New("pull task execution returned no output")
		}
	}()

	select {
	case <-renewDone:
		renewCancel()
		slog.Warn("renew loop exited early (likely BOUND_CLAIM_CONFLICT); abandoning task", "run_id", runID)
	case <-taskCtx.Done():
		renewCancel()
		slog.Warn("task processing timed out; abandoning task", "run_id", runID, "timeout", w.cfg.TaskTimeout.String())
	case res := <-resultCh:
		renewCancel()
		if res.err != nil {
			// MOTA 返回通道不是内部错误日志;避免把凭据、供应商响应等实现细节
			// 暴露给平台或下游消费者。
			slog.Error("processor failed; reporting sanitized failure to queue", "run_id", runID, "err", res.err)
			failureInfo := "processor failed"
			w.completeTask(ctx, runID, PlaceholderOutput("processor failed"), &failureInfo)
			return
		}
		w.completeTask(ctx, runID, res.out, nil)
	}
}

// claimParams 把 templateInputData 展平成任务参数:
//   - 数组形态 [{name, value}] 展平成 {name: value};
//   - 对象形态且带 params 键时取该键;
//   - 否则整段原样当 params。
func claimParams(input any) any {
	if entries, ok := input.([]any); ok {
		params := make(map[string]any, len(entries))
		for _, raw := range entries {
			entry, ok := asMapValue(raw)
			if !ok {
				continue
			}
			name, nameOK := entry["name"].(string)
			value, valueOK := entry["value"]
			if nameOK && valueOK {
				params[name] = value
			}
		}
		return params
	}
	if object, ok := asMapValue(input); ok {
		if params, ok := object["params"]; ok {
			return params
		}
	}
	if input == nil {
		return map[string]any{}
	}
	return input
}

// claimLanguage 从 globalParameters.language 提取输出语言。
func claimLanguage(global any) *string {
	object, ok := asMapValue(global)
	if !ok {
		return nil
	}
	language, ok := object["language"].(string)
	if !ok {
		return nil
	}
	return &language
}

// claimRawInput 把 ClaimResponse 转成执行器/网关消费的原始 JSON 负载。
func claimRawInput(claim ClaimResponse) any {
	encoded, err := json.Marshal(claim)
	if err != nil {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

// renewLoop 在任务处理期间持续续租;409 BOUND_CLAIM_CONFLICT 表示租约已过期
// 被抢走,立即退出(上层据此放弃该任务)。网络错误继续重试。
func (w *PullWorker) renewLoop(ctx context.Context, runID string) {
	ticker := time.NewTicker(w.cfg.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := w.doPost(ctx, "/renew", nil, nil, map[string]string{RunIDHeader: runID})
			if err != nil {
				slog.Warn("renew: request failed", "run_id", runID, "err", err)
				continue
			}
			switch resp.StatusCode {
			case http.StatusOK:
			case http.StatusConflict:
				slog.Warn("renew: 409 BOUND_CLAIM_CONFLICT, abandoning task", "run_id", runID)
				closeBody(resp)
				return
			default:
				slog.Warn("renew: unexpected status", "run_id", runID, "status", resp.StatusCode)
			}
			closeBody(resp)
		}
	}
}

// completeTask 回传结果:POST {queue}/complete,带 X-MoTA-Run-ID。
//
// Pull complete 与 Push callback 使用同一 V2 信封,保留技术面 signals、风控
// checks、交易 orders 等岗位专属 payload_result;信封必须携带 usage
// (RULE-BND-001,缺计量时兜底最小合规值)。
func (w *PullWorker) completeTask(ctx context.Context, runID string, out *ProcessedOutput, errorInfo *string) {
	if out == nil {
		slog.Error("complete suppressed because task output is nil", "run_id", runID)
		return
	}
	trusted := w.cfg.TrustedUsageReportingEnabled
	if trusted && out.Usage == nil {
		slog.Warn("trusted Pull result has no FINAL provider usage; complete suppressed", "run_id", runID)
		return
	}
	payload := out.ToTransportPayload(nil)
	if errorInfo != nil {
		payload.PayloadInfo = *errorInfo
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("complete: failed to marshal body", "run_id", runID, "err", err)
		return
	}
	resp, err := w.doPost(ctx, "/complete", nil, raw, map[string]string{RunIDHeader: runID})
	if err != nil {
		slog.Warn("complete: request failed", "run_id", runID, "err", err)
		return
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		slog.Warn("complete: unexpected status", "run_id", runID, "status", resp.StatusCode)
		return
	}
	var receipt CompleteResponse
	if err := decodeJSON(resp, &receipt, queueReceiptResponseBodyLimit); err != nil {
		slog.Warn("complete: failed to parse receipt", "run_id", runID, "err", err)
		return
	}
	if receipt.Accepted {
		slog.Info("complete accepted", "run_id", runID)
	} else {
		slog.Warn("complete NOT accepted", "run_id", runID,
			"run_status", derefOrEmpty(receipt.RunStatus), "run_msg", derefOrEmpty(receipt.RunMsg))
	}
}

// ---------------------------------------------------------------------------
// Pull heartbeat 循环(与 claim/complete 共用 bind token 的简单存活上报)
// ---------------------------------------------------------------------------

func (w *PullWorker) heartbeatLoop(ctx context.Context) {
	defer w.wg.Done()
	interval := w.cfg.HeartbeatInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			resp, err := w.doPost(ctx, "/heartbeat", map[string]string{"workerId": w.cfg.WorkerID}, nil, nil)
			if err != nil {
				slog.Warn("heartbeat: request failed",
					"queue_origin", callbackOrigin(w.cfg.QueueURL),
					"error_kind", callbackRequestErrorKind(err))
				continue
			}
			if resp.StatusCode == http.StatusOK {
				var hb HeartbeatResponse
				if err := decodeJSON(resp, &hb, heartbeatResponseBodyLimit); err != nil {
					slog.Warn("heartbeat: invalid response body", "err", err)
					continue
				}
				if hb.HeartbeatIntervalSeconds != nil && *hb.HeartbeatIntervalSeconds > 0 {
					seconds := min(*hb.HeartbeatIntervalSeconds, maxHeartbeatIntervalSeconds)
					interval = time.Duration(seconds) * time.Second
				}
				slog.Debug("heartbeat ok", "interval", interval.String())
			} else {
				slog.Warn("heartbeat: unexpected status", "status", resp.StatusCode)
				closeBody(resp)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Push 形态心跳(新协议 §2.3,必需)
// ---------------------------------------------------------------------------

// agentHeartbeatState 是单个 Agent 通道的心跳调度状态。
type agentHeartbeatState struct {
	intervalSecs uint64
	selfCheckDue bool
	lastSent     time.Time
}

// RunPushHeartbeat 是 Push 形态的心跳:按每个启用 Agent 各自的通道密钥独立发送
// POST {heartbeatUrl} + Authorization: Bearer <该 Agent 的 signing_secret>。
//
// 连续 15 分钟无有效心跳即被平台判离线。按回执动态调整周期;平台要求自检时
// (selfCheckRequired)下一次心跳带上 selfCheck 结果。
//
// store 为 nil 表示平台管理层未启用(嵌入模式没有 Registry 可以枚举 Agent 通道
// 密钥),此时心跳直接跳过并记 warn —— 与 rs「heartbeat skipped: platform store
// unavailable」的语义一致。
func RunPushHeartbeat(cfg *Config, execution *TaskExecutionBackend, token *shutdown.Token, store *AgentStore) {
	heartbeatURL := strings.TrimSpace(cfg.HeartbeatURL)
	if heartbeatURL == "" {
		return
	}
	if store == nil {
		slog.Warn("push heartbeat skipped: platform store unavailable")
		return
	}
	ctx := token.Context()
	client := newPushHeartbeatHTTPClient()
	defaultInterval := cfg.HeartbeatInterval
	if defaultInterval < 5*time.Second {
		defaultInterval = 5 * time.Second
	}
	defaultIntervalSecs := uint64(defaultInterval.Seconds())

	// 每个 Agent(通道)各自维护:是否需自检、心跳周期、上次发送时间。
	states := map[string]*agentHeartbeatState{}

	heartbeatOrigin := callbackOrigin(heartbeatURL)
	slog.Info("push heartbeat started (per-agent Bearer keys)", "heartbeat_origin", heartbeatOrigin)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		agents, err := store.ListAgentSecrets(ctx)
		if err != nil {
			slog.Warn("list agent secrets for heartbeat failed", "err", err)
			continue
		}
		for _, pair := range agents {
			slug, secret := pair[0], pair[1]
			state, ok := states[slug]
			if !ok {
				state = &agentHeartbeatState{intervalSecs: defaultIntervalSecs}
				states[slug] = state
			}
			interval := state.intervalSecs
			if interval < 5 {
				interval = 5
			}
			if !state.lastSent.IsZero() && time.Since(state.lastSent) < time.Duration(interval)*time.Second {
				continue
			}
			state.lastSent = time.Now()
			sendAgentHeartbeat(ctx, client, store, execution, heartbeatURL, slug, secret, state, cfg.TaskTimeout)
		}
	}
}

// sendAgentHeartbeat 发送单个 Agent 的一次心跳(可能带自检),并记 channel_events。
func sendAgentHeartbeat(
	ctx context.Context,
	client *http.Client,
	store *AgentStore,
	execution *TaskExecutionBackend,
	heartbeatURL, agentSlug, agentSecret string,
	state *agentHeartbeatState,
	taskTimeout time.Duration,
) {
	body := map[string]any{}
	heartbeatOrigin := callbackOrigin(heartbeatURL)
	var selfCheckPass bool
	var selfCheckLatencyMs int64
	var selfCheckErr string
	ranSelfCheck := consumeSelfCheckRequest(state)
	if ranSelfCheck {
		// 真自检:真跑一次最短任务,证明「收任务 → 出结果」整条链路是活的。
		// 端口通不算;跑挂了就如实报 pass:false,让平台标记 degraded 而不是继续派活。
		checkTimeout := taskTimeout
		if checkTimeout <= 0 || checkTimeout > 30*time.Second {
			checkTimeout = 30 * time.Second
		}
		pass, latencyMs, errMsg := runAgentSelfCheck(execution, agentSlug, checkTimeout)
		selfCheckPass, selfCheckLatencyMs, selfCheckErr = pass, latencyMs, errMsg
		selfCheck := map[string]any{"pass": pass, "latencyMs": latencyMs}
		if errMsg == "" {
			selfCheck["error"] = nil
		} else {
			selfCheck["error"] = errMsg
		}
		body["selfCheck"] = selfCheck
		slog.Info("push heartbeat self-check executed", "agent", agentSlug, "pass", pass, "latency_ms", latencyMs)
	}

	requestJSON, _ := json.Marshal(body)
	started := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(requestJSON))
	var statusCode *int64
	var detail *string
	responseText := ""
	responseAccepted := false
	if err != nil {
		text := "invalid heartbeat request"
		detail = &text
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agentSecret)
		resp, sendErr := client.Do(req)
		if sendErr != nil {
			slog.Warn("push heartbeat failed", "agent", agentSlug,
				"heartbeat_origin", heartbeatOrigin,
				"error_kind", callbackRequestErrorKind(sendErr))
			text := "heartbeat request failed"
			detail = &text
			encoded, _ := json.Marshal(map[string]string{"error": text})
			responseText = string(encoded)
		} else {
			raw, readErr := readHeartbeatResponse(resp.Body)
			closeBody(resp)
			if readErr != nil {
				text := readErr.Error()
				detail = &text
				raw = []byte(`{"error":"invalid heartbeat response body"}`)
			}
			responseText = string(raw)
			status := int64(resp.StatusCode)
			statusCode = &status
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if readErr == nil {
					if err := applyHeartbeatResponse(state, raw); err == nil {
						responseAccepted = true
					} else {
						text := "invalid heartbeat response"
						detail = &text
					}
				}
				if responseAccepted {
					slog.Debug("push heartbeat ok", "agent", agentSlug,
						"heartbeat_origin", heartbeatOrigin)
				} else {
					slog.Warn("push heartbeat returned an invalid response", "agent", agentSlug,
						"heartbeat_origin", heartbeatOrigin, "response_bytes", len(raw))
				}
			} else {
				slog.Warn("push heartbeat rejected", "agent", agentSlug, "status", resp.StatusCode)
				if detail == nil {
					text := fmt.Sprintf("heartbeat rejected with %d", resp.StatusCode)
					detail = &text
				}
			}
		}
	}

	transportOK := statusCode != nil && *statusCode >= 200 && *statusCode < 300 && responseAccepted
	if !transportOK {
		// 失败时退避 5s,避免对平台造成压力。
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	// 成功判定:自检通过(未自检视为通过)且 HTTP 2xx,两者缺一即失败。
	selfCheckOK := !ranSelfCheck || selfCheckPass
	pass := selfCheckOK && transportOK
	heartbeatMs := time.Since(started).Milliseconds()

	// 耗时列展示本次心跳请求往返耗时;自检延迟并入 detail。
	if detail == nil {
		if ranSelfCheck && selfCheckErr != "" {
			detail = &selfCheckErr
		} else if ranSelfCheck && selfCheckLatencyMs > 0 {
			text := fmt.Sprintf("selfCheck %dms", selfCheckLatencyMs)
			detail = &text
		}
	}

	inputJSON := string(requestJSON)
	if err := store.RecordChannelEvent(ctx, ChannelEventRecord{
		EventType: "heartbeat", AgentSlug: &agentSlug,
		URL: heartbeatOrigin, StatusCode: statusCode,
		Pass: &pass, LatencyMs: &heartbeatMs,
		Detail: detail, Input: &inputJSON, Output: &responseText,
	}); err != nil {
		slog.Warn("record heartbeat channel event failed", "agent", agentSlug, "err", err)
	}
}

// runAgentSelfCheck 真跑一次最短任务,返回 (是否通过, 耗时毫秒, 错误信息)。
// 链路包括解析任务信封 → 执行 → 产出结果信封的完整通路。
func runAgentSelfCheck(
	execution *TaskExecutionBackend,
	agentSlug string,
	timeout time.Duration,
) (pass bool, latencyMs int64, errorMessage string) {
	selfCheckRunID := "self-check"
	task := &TaskContext{RunID: &selfCheckRunID, Params: map[string]any{}}
	started := time.Now()
	defer func() {
		latencyMs = time.Since(started).Milliseconds()
		if recover() != nil {
			pass = false
			errorMessage = "self-check execution panicked"
		}
	}()
	out, err := execution.Execute(context.Background(), agentSlug, task, map[string]any{}, timeout)
	if err != nil {
		slog.Warn("push heartbeat self-check execution failed", "agent", agentSlug, "err", err)
		return false, latencyMs, "self-check execution failed"
	}
	if out == nil {
		return false, latencyMs, "self-check returned no output"
	}
	payload := out.ToPayload()
	if _, err := json.Marshal(payload); err != nil {
		return false, latencyMs, "envelope serialize failed: " + err.Error()
	}
	return true, latencyMs, ""
}

func newPushHeartbeatHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: CredentialedRedirectPolicy,
	}
}

// consumeSelfCheckRequest consumes one platform request before doing the paid
// check. A failed heartbeat therefore does not repeat the check every cycle;
// only a later valid receipt can explicitly arm another check.
func consumeSelfCheckRequest(state *agentHeartbeatState) bool {
	if state == nil || !state.selfCheckDue {
		return false
	}
	state.selfCheckDue = false
	return true
}

// applyHeartbeatResponse updates scheduling state only after a complete valid
// JSON receipt was received over a successful 2xx response.
func applyHeartbeatResponse(state *agentHeartbeatState, raw []byte) error {
	if state == nil {
		return errors.New("heartbeat state is missing")
	}
	var heartbeat *HeartbeatResponse
	if err := json.Unmarshal(raw, &heartbeat); err != nil || heartbeat == nil {
		return errors.New("invalid heartbeat response")
	}
	if heartbeat.HeartbeatIntervalSeconds != nil && *heartbeat.HeartbeatIntervalSeconds >= 5 {
		state.intervalSecs = min(*heartbeat.HeartbeatIntervalSeconds, maxHeartbeatIntervalSeconds)
	} else if heartbeat.HeartbeatIntervalSeconds != nil && *heartbeat.HeartbeatIntervalSeconds > 0 {
		state.intervalSecs = 5
	}
	state.selfCheckDue = heartbeat.SelfCheckRequired
	return nil
}

func readHeartbeatResponse(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errors.New("heartbeat response body is missing")
	}
	raw, err := io.ReadAll(io.LimitReader(body, heartbeatResponseBodyLimit+1))
	if err != nil {
		return nil, errors.New("read heartbeat response failed")
	}
	if len(raw) > heartbeatResponseBodyLimit {
		return nil, fmt.Errorf("heartbeat response exceeded %d bytes", heartbeatResponseBodyLimit)
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (w *PullWorker) doPost(
	ctx context.Context,
	path string,
	query map[string]string,
	body []byte,
	extraHeaders map[string]string,
) (*http.Response, error) {
	url := strings.TrimRight(w.cfg.QueueURL, "/") + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+w.cfg.BindToken)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return w.client.Do(req)
}

func decodeJSON(resp *http.Response, v any, limit int64) error {
	defer closeBody(resp)
	if resp == nil || resp.Body == nil {
		return errors.New("response body is missing")
	}
	if limit <= 0 {
		return errors.New("response body limit must be positive")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errors.New("read response body failed")
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("response body exceeded %d bytes", limit)
	}
	return json.Unmarshal(raw, v)
}

func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}
