package worker

// Push 形态心跳（新协议 §2.3，必需）：按每个启用智能体各自的通道密钥独立
// 发送 POST {heartbeatUrl} + Authorization: Bearer <该智能体的 signing_secret>。
//
// 连续 15 分钟无有效心跳即被平台判离线。按回执动态调整周期（下限 5s、
// 上限 3600s）；平台要求自检时（selfCheckRequired）下一次心跳带上
// selfCheck 结果——真跑一次最短任务证明「收任务 → 出结果」整条链路是活的，
// 端口通不算；跑挂了就如实报 pass:false，让平台标记 degraded 而不是继续派活。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/platform"
)

// 心跳预算（与旧栈一致）。
const (
	// heartbeatResponseBodyLimit 复用 worker.go 的同名常量（64KiB）。
	minHeartbeatIntervalSecs = uint64(5)
	selfCheckTimeoutCap      = 30 * time.Second
)

// ChannelLister 提供启用智能体的心跳通道凭据。
type ChannelLister interface {
	ListChannels(ctx context.Context) ([]agent.Channel, error)
}

// EventRecorder 记录通道事件（infra/postgres 的 ChannelEventStore 结构性满足）。
type EventRecorder interface {
	RecordChannelEvent(ctx context.Context, event audit.ChannelEvent) error
}

// HeartbeatPoster 是单次心跳投递端口（与 callback 的 Poster 同形，
// infra/httpx.CallbackPoster 结构性满足：30s 限时 + 64KiB 有界读取 + 禁重定向）。
type HeartbeatPoster interface {
	Post(ctx context.Context, target string, headers map[string]string, body []byte) (int, string, error)
}

// PushHeartbeatConfig 是 Push 心跳配置。
type PushHeartbeatConfig struct {
	// HeartbeatURL 为空表示未启用（Run 直接返回）。
	HeartbeatURL string
	// DefaultInterval 是缺省心跳周期（<5s 抬到 5s）。
	DefaultInterval time.Duration
	// TaskTimeout 约束自检执行（>30s 截到 30s）。
	TaskTimeout time.Duration
}

// agentHeartbeatState 是单个智能体通道的心跳调度状态。
type agentHeartbeatState struct {
	intervalSecs uint64
	selfCheckDue bool
	lastSent     time.Time
}

// PushHeartbeat 是 Push 形态心跳服务。
type PushHeartbeat struct {
	config     PushHeartbeatConfig
	poster     HeartbeatPoster
	channels   ChannelLister
	recorder   EventRecorder
	dispatcher Dispatcher

	// states 只在 Run 循环 goroutine 内读写（无并发访问）。
	states map[string]*agentHeartbeatState
	// tick 与 failureBackoff 可注入短值供测试。
	tick           time.Duration
	failureBackoff time.Duration
	// now 可注入（测试控制到期判定）。
	now func() time.Time
}

// NewPushHeartbeat 组装 Push 心跳服务。
func NewPushHeartbeat(config PushHeartbeatConfig, poster HeartbeatPoster,
	channels ChannelLister, recorder EventRecorder, dispatcher Dispatcher) *PushHeartbeat {
	return &PushHeartbeat{
		config: config, poster: poster, channels: channels,
		recorder: recorder, dispatcher: dispatcher,
		states: map[string]*agentHeartbeatState{},
		tick:   5 * time.Second, failureBackoff: 5 * time.Second,
		now: time.Now,
	}
}

// Run 是心跳主循环：每 tick 扫描一次通道，对到期者发送心跳。ctx 取消即退出。
func (h *PushHeartbeat) Run(ctx context.Context) {
	url := trimSpace(h.config.HeartbeatURL)
	if url == "" {
		return
	}
	if h.channels == nil {
		slog.Warn("push heartbeat skipped: platform store unavailable")
		return
	}
	slog.Info("push heartbeat started (per-agent Bearer keys)",
		"heartbeat_origin", platform.CallbackOrigin(url))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.tick):
		}
		h.RunCycle(ctx)
	}
}

// RunCycle 扫描一次全部通道并发送到期的心跳（Run 的单元可测切面）。
func (h *PushHeartbeat) RunCycle(ctx context.Context) {
	url := trimSpace(h.config.HeartbeatURL)
	channels, err := h.channels.ListChannels(ctx)
	if err != nil {
		slog.Warn("list agent secrets for heartbeat failed", "err", err)
		return
	}
	defaultInterval := h.config.DefaultInterval
	if defaultInterval < 5*time.Second {
		defaultInterval = 5 * time.Second
	}
	defaultIntervalSecs := uint64(defaultInterval.Seconds())

	for _, channel := range channels {
		state, ok := h.states[channel.Slug]
		if !ok {
			state = &agentHeartbeatState{intervalSecs: defaultIntervalSecs}
			h.states[channel.Slug] = state
		}
		interval := state.intervalSecs
		if interval < minHeartbeatIntervalSecs {
			interval = minHeartbeatIntervalSecs
		}
		if !state.lastSent.IsZero() && h.now().Sub(state.lastSent) < time.Duration(interval)*time.Second {
			continue
		}
		state.lastSent = h.now()
		h.sendAgentHeartbeat(ctx, url, channel, state)
	}
}

// sendAgentHeartbeat 发送单个智能体的一次心跳（可能带自检），并记 channel_events。
func (h *PushHeartbeat) sendAgentHeartbeat(ctx context.Context, heartbeatURL string,
	channel agent.Channel, state *agentHeartbeatState) {
	body := map[string]any{}
	heartbeatOrigin := platform.CallbackOrigin(heartbeatURL)
	var selfCheckPass bool
	var selfCheckLatencyMs int64
	var selfCheckErr string
	ranSelfCheck := consumeSelfCheckRequest(state)
	if ranSelfCheck {
		checkTimeout := h.config.TaskTimeout
		if checkTimeout <= 0 || checkTimeout > selfCheckTimeoutCap {
			checkTimeout = selfCheckTimeoutCap
		}
		selfCheckPass, selfCheckLatencyMs, selfCheckErr = h.runAgentSelfCheck(channel.Slug, checkTimeout)
		selfCheck := map[string]any{"pass": selfCheckPass, "latencyMs": selfCheckLatencyMs}
		if selfCheckErr == "" {
			selfCheck["error"] = nil
		} else {
			selfCheck["error"] = selfCheckErr
		}
		body["selfCheck"] = selfCheck
		slog.Info("push heartbeat self-check executed", "agent", channel.Slug,
			"pass", selfCheckPass, "latency_ms", selfCheckLatencyMs)
	}

	requestJSON, _ := json.Marshal(body)
	started := h.now()

	var statusCode *int64
	var detail *string
	responseText := ""
	responseAccepted := false

	status, text, err := h.poster.Post(ctx, heartbeatURL, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + channel.SigningSecret,
	}, requestJSON)
	if err != nil {
		slog.Warn("push heartbeat failed", "agent", channel.Slug,
			"heartbeat_origin", heartbeatOrigin, "error_kind", requestErrorKind(err))
		failure := "heartbeat request failed"
		detail = &failure
		encoded, _ := json.Marshal(map[string]string{"error": failure})
		responseText = string(encoded)
		if status > 0 {
			// 收到过响应（如回执超限）：带观测状态码记账。
			observed := int64(status)
			statusCode = &observed
			responseText = text
		}
	} else {
		responseText = text
		observed := int64(status)
		statusCode = &observed
		if status >= 200 && status < 300 {
			if applyErr := applyHeartbeatResponse(state, []byte(text)); applyErr == nil {
				responseAccepted = true
			} else {
				invalid := "invalid heartbeat response"
				detail = &invalid
				slog.Warn("push heartbeat returned an invalid response", "agent", channel.Slug,
					"heartbeat_origin", heartbeatOrigin, "response_bytes", len(text))
			}
		} else {
			slog.Warn("push heartbeat rejected", "agent", channel.Slug, "status", status)
			rejected := fmt.Sprintf("heartbeat rejected with %d", status)
			detail = &rejected
		}
	}

	transportOK := statusCode != nil && *statusCode >= 200 && *statusCode < 300 && responseAccepted
	if !transportOK && h.failureBackoff > 0 {
		// 失败时退避，避免对平台造成压力。
		select {
		case <-ctx.Done():
		case <-time.After(h.failureBackoff):
		}
	}
	// 成功判定：自检通过（未自检视为通过）且 HTTP 2xx + 有效回执，缺一即失败。
	selfCheckOK := !ranSelfCheck || selfCheckPass
	pass := selfCheckOK && transportOK
	heartbeatMs := h.now().Sub(started).Milliseconds()

	// 耗时列展示本次心跳请求往返耗时；自检延迟并入 detail。
	if detail == nil {
		if ranSelfCheck && selfCheckErr != "" {
			detail = &selfCheckErr
		} else if ranSelfCheck && selfCheckLatencyMs > 0 {
			text := fmt.Sprintf("selfCheck %dms", selfCheckLatencyMs)
			detail = &text
		}
	}

	inputJSON := string(requestJSON)
	slug := channel.Slug
	if h.recorder != nil {
		if err := h.recorder.RecordChannelEvent(ctx, audit.ChannelEvent{
			EventType: "heartbeat", AgentSlug: &slug,
			URL: heartbeatOrigin, StatusCode: statusCode,
			Pass: &pass, LatencyMs: &heartbeatMs,
			Detail: detail, Input: &inputJSON, Output: &responseText,
		}); err != nil {
			slog.Warn("record heartbeat channel event failed", "agent", slug, "err", err)
		}
	}
}

// runAgentSelfCheck 真跑一次最短任务，返回（是否通过, 耗时毫秒, 错误信息）。
// 链路包括解析任务信封 → 执行 → 产出结果信封的完整通路。
func (h *PushHeartbeat) runAgentSelfCheck(agentSlug string, timeout time.Duration) (
	pass bool, latencyMs int64, errorMessage string) {
	selfCheckRunID := "self-check"
	task := &platform.TaskContext{RunID: &selfCheckRunID, Params: map[string]any{}}
	started := h.now()
	defer func() {
		latencyMs = h.now().Sub(started).Milliseconds()
		if recover() != nil {
			pass = false
			errorMessage = "self-check execution panicked"
		}
	}()
	out, err := h.dispatcher.Execute(context.Background(), agentSlug, task, map[string]any{}, timeout)
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

// consumeSelfCheckRequest 在做付费自检前先消费掉平台的自检请求。失败的
// 心跳因此不会每个周期都重复自检；只有后续有效回执才能再次武装自检。
func consumeSelfCheckRequest(state *agentHeartbeatState) bool {
	if state == nil || !state.selfCheckDue {
		return false
	}
	state.selfCheckDue = false
	return true
}

// applyHeartbeatResponse 只在 2xx + 完整合法 JSON 回执时更新调度状态。
func applyHeartbeatResponse(state *agentHeartbeatState, raw []byte) error {
	if state == nil {
		return errors.New("heartbeat state is missing")
	}
	var heartbeat *platform.HeartbeatResponse
	if err := json.Unmarshal(bytes.TrimSpace(raw), &heartbeat); err != nil || heartbeat == nil {
		return errors.New("invalid heartbeat response")
	}
	if heartbeat.HeartbeatIntervalSeconds != nil && *heartbeat.HeartbeatIntervalSeconds >= minHeartbeatIntervalSecs {
		interval := *heartbeat.HeartbeatIntervalSeconds
		if interval > maxHeartbeatIntervalSeconds {
			interval = maxHeartbeatIntervalSeconds
		}
		state.intervalSecs = interval
	} else if heartbeat.HeartbeatIntervalSeconds != nil && *heartbeat.HeartbeatIntervalSeconds > 0 {
		state.intervalSecs = minHeartbeatIntervalSecs
	}
	state.selfCheckDue = heartbeat.SelfCheckRequired
	return nil
}

func requestErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "request"
	}
}

func trimSpace(value string) string {
	return string(bytes.TrimSpace([]byte(value)))
}
