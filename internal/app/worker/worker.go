// Package worker 实现上游平台 Pull 协议的出站工作循环：
// claim（串行认领）→ 处理（并发续租）→ complete（V2 信封回传），以及
// 独立的 heartbeat 存活上报（服务端可下发新间隔）。
//
// HTTP 细节在 infra/platformhttp；本包只编排用例，全部路径可脱离网络单测。
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// 响应体上限（与旧栈一致）：claim 可能携带完整输入信封（对齐服务的
// 4 MiB 公开请求上限），complete/heartbeat 回执是小 JSON。
const (
	claimResponseBodyLimit     = 4 * 1024 * 1024
	receiptResponseBodyLimit   = 64 * 1024
	heartbeatResponseBodyLimit = 64 * 1024
	// 服务端下发的间隔在上限内截断，防止 uint64 溢出成负数热循环。
	maxHeartbeatIntervalSeconds uint64 = 60 * 60
)

// PlatformClient 是平台队列的出站端口。
type PlatformClient interface {
	Post(ctx context.Context, path string, query map[string]string,
		body []byte, headers map[string]string, responseLimit int) (status int, response []byte, err error)
}

// Dispatcher 是任务分发端口（app/dispatch 的 *Service 结构上满足）。
type Dispatcher interface {
	Execute(ctx context.Context, agentCode string, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// Config 是 worker 运行配置（装配层从 infra/config 的 IR 段映射注入）。
type Config struct {
	WorkerID          string
	TaskTimeout       time.Duration
	RenewInterval     time.Duration
	HeartbeatInterval time.Duration
	// TrustedUsageReporting 开启后，拿不到 FINAL usage 的结果不回传
	// （防止平台重投触发副作用）。
	TrustedUsageReporting bool
}

// Worker 是 Pull 工作循环。Start/Stop 管理两个后台 goroutine
// （claim 循环 + heartbeat 循环），Stop 等待两者退出。
type Worker struct {
	config     Config
	client     PlatformClient
	dispatcher Dispatcher
	// retryDelay 是 claim 异常后的退避（可注入短值供测试）。
	retryDelay time.Duration

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// New 组装 worker。
func New(config Config, client PlatformClient, dispatcher Dispatcher) *Worker {
	return &Worker{config: config, client: client, dispatcher: dispatcher,
		retryDelay: time.Second}
}

// Start 启动 claim 与 heartbeat 循环。parent 取消即整体退出。
func (w *Worker) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.wg.Add(2)
	go w.claimLoop(ctx)
	go w.heartbeatLoop(ctx)
	slog.Info("pull worker started", "worker_id", w.config.WorkerID)
}

// Stop 取消循环并等待退出。
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	slog.Info("pull worker stopped")
}

// ---------------------------------------------------------------------------
// claim 循环
// ---------------------------------------------------------------------------

func (w *Worker) claimLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		status, body, err := w.client.Post(ctx, "/claim",
			map[string]string{"workerId": w.config.WorkerID}, nil, nil, claimResponseBodyLimit)
		if err != nil {
			slog.Warn("claim: request failed", "err", err)
			if !w.retryAfterDelay(ctx) {
				return
			}
			continue
		}
		switch status {
		case http.StatusOK:
			var claim platform.ClaimResponse
			if err := json.Unmarshal(body, &claim); err != nil {
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
			// 真空闲：立即重新 claim。
		default:
			slog.Warn("claim: unexpected status", "status", status)
			if !w.retryAfterDelay(ctx) {
				return
			}
		}
	}
}

// retryAfterDelay 等待重试或立刻响应关闭，避免退避 sleep 延迟进程退出。
func (w *Worker) retryAfterDelay(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(w.retryDelay):
		return true
	}
}

// ---------------------------------------------------------------------------
// 单任务：处理 + 续租 + 回传
// ---------------------------------------------------------------------------

type processResult struct {
	out *platform.ProcessedOutput
	err error
}

func (w *Worker) handleTask(ctx context.Context, claim platform.ClaimResponse) {
	runID := claim.RunID
	agentCode := deref(claim.AgentCode)

	// templateInputData 可能直接是任务体，也可能是 { taskBrief, params } 包裹；
	// 尽量提取 params，否则整段作为 params。
	params := claimParams(claim.TemplateInputData)

	taskBrief := claim.TaskBrief
	if taskBrief == nil {
		if object, ok := platform.AsMapValue(claim.TemplateInputData); ok {
			taskBrief = object["taskBrief"]
		} else if claim.TemplateInputData != nil {
			taskBrief = claim.TemplateInputData
		}
	}

	task := &platform.TaskContext{
		RunID:               &runID,
		TaskBrief:           taskBrief,
		Params:              params,
		PositionType:        claim.PositionType,
		Language:            claimLanguage(claim.GlobalParameters),
		SymbolPool:          platform.ExtractSymbolPool(claim.GlobalParameters),
		UniverseMode:        platform.ExtractUniverseMode(claim.GlobalParameters),
		UpstreamSymbols:     platform.ExtractUpstreamSymbols(claim.UpstreamPayload, claim.PositionType),
		ForceFailureDefault: platform.ExtractFailurePath(taskBrief),
		RuntimeContext:      claim.RuntimeContext,
	}

	// rawInput 是执行器审计/网关请求体使用的原始负载：claim 响应体本身。
	rawInput := claimRawInput(claim)

	// 续租循环：处理期间定期 renew；409 时提前退出（放弃任务）。
	renewCtx, renewCancel := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		w.renewLoop(renewCtx, runID)
	}()

	taskCtx, taskCancel := context.WithTimeout(ctx, w.config.TaskTimeout)
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
		result.out, result.err = w.dispatcher.Execute(
			taskCtx, agentCode, task, rawInput, w.config.TaskTimeout)
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
		slog.Warn("task processing timed out; abandoning task", "run_id", runID,
			"timeout", w.config.TaskTimeout.String())
	case res := <-resultCh:
		renewCancel()
		if res.err != nil {
			// 平台返回通道不是内部错误日志；避免把凭据、供应商响应等实现
			// 细节暴露给平台或下游消费者。
			slog.Error("processor failed; reporting sanitized failure to queue",
				"run_id", runID, "err", res.err)
			failureInfo := "processor failed"
			w.completeTask(ctx, runID, platform.PlaceholderOutput("processor failed"), &failureInfo)
			return
		}
		w.completeTask(ctx, runID, res.out, nil)
	}
}

// claimParams 把 templateInputData 展平成任务参数：
//   - 数组形态 [{name, value}] 展平成 {name: value}；
//   - 对象形态且带 params 键时取该键；
//   - 否则整段原样当 params。
func claimParams(input any) any {
	if entries, ok := input.([]any); ok {
		params := make(map[string]any, len(entries))
		for _, raw := range entries {
			entry, ok := platform.AsMapValue(raw)
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
	if object, ok := platform.AsMapValue(input); ok {
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
	object, ok := platform.AsMapValue(global)
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
func claimRawInput(claim platform.ClaimResponse) any {
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

// renewLoop 在任务处理期间持续续租；409 BOUND_CLAIM_CONFLICT 表示租约已
// 过期被抢走，立即退出（上层据此放弃该任务）。网络错误继续重试。
func (w *Worker) renewLoop(ctx context.Context, runID string) {
	ticker := time.NewTicker(w.config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, _, err := w.client.Post(ctx, "/renew", nil, nil,
				map[string]string{platform.HeaderRunID: runID}, receiptResponseBodyLimit)
			if err != nil {
				slog.Warn("renew: request failed", "run_id", runID, "err", err)
				continue
			}
			switch status {
			case http.StatusOK:
			case http.StatusConflict:
				slog.Warn("renew: 409 BOUND_CLAIM_CONFLICT, abandoning task", "run_id", runID)
				return
			default:
				slog.Warn("renew: unexpected status", "run_id", runID, "status", status)
			}
		}
	}
}

// completeTask 回传结果：POST {queue}/complete，带 X-IR-Run-ID。
//
// Pull complete 与 Push callback 使用同一 V2 信封，保留岗位专属
// payload_result；信封必须携带 usage（RULE-BND-001，缺计量时兜底最小合规值）。
func (w *Worker) completeTask(ctx context.Context, runID string, out *platform.ProcessedOutput, errorInfo *string) {
	if out == nil {
		slog.Error("complete suppressed because task output is nil", "run_id", runID)
		return
	}
	if w.config.TrustedUsageReporting && out.Usage == nil {
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
	status, body, err := w.client.Post(ctx, "/complete", nil, raw,
		map[string]string{platform.HeaderRunID: runID}, receiptResponseBodyLimit)
	if err != nil {
		slog.Warn("complete: request failed", "run_id", runID, "err", err)
		return
	}
	if status != http.StatusOK {
		slog.Warn("complete: unexpected status", "run_id", runID, "status", status)
		return
	}
	var receipt platform.CompleteResponse
	if err := json.Unmarshal(body, &receipt); err != nil {
		slog.Warn("complete: failed to parse receipt", "run_id", runID, "err", err)
		return
	}
	if receipt.Accepted {
		slog.Info("complete accepted", "run_id", runID)
	} else {
		slog.Warn("complete NOT accepted", "run_id", runID,
			"run_status", deref(receipt.RunStatus), "run_msg", deref(receipt.RunMsg))
	}
}

// ---------------------------------------------------------------------------
// heartbeat 循环
// ---------------------------------------------------------------------------

func (w *Worker) heartbeatLoop(ctx context.Context) {
	defer w.wg.Done()
	interval := w.config.HeartbeatInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			status, body, err := w.client.Post(ctx, "/heartbeat",
				map[string]string{"workerId": w.config.WorkerID}, nil, nil, heartbeatResponseBodyLimit)
			if err != nil {
				slog.Warn("heartbeat: request failed", "err", err)
				continue
			}
			if status != http.StatusOK {
				slog.Warn("heartbeat: unexpected status", "status", status)
				continue
			}
			var heartbeat platform.HeartbeatResponse
			if err := json.Unmarshal(body, &heartbeat); err != nil {
				slog.Warn("heartbeat: invalid response body", "err", err)
				continue
			}
			if heartbeat.HeartbeatIntervalSeconds != nil && *heartbeat.HeartbeatIntervalSeconds > 0 {
				seconds := *heartbeat.HeartbeatIntervalSeconds
				if seconds > maxHeartbeatIntervalSeconds {
					seconds = maxHeartbeatIntervalSeconds
				}
				interval = time.Duration(seconds) * time.Second
			}
			slog.Debug("heartbeat ok", "interval", interval.String())
		}
	}
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
