package mota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hwhkit/hwhkit-go/core/shutdown"
)

type mockQueue struct {
	mu             sync.Mutex
	claimCount     atomic.Int64
	completeBodies [][]byte
	heartbeatCount atomic.Int64
	renewCount     atomic.Int64
	renewConflict  bool
	acceptComplete atomic.Bool
	claimsLeft     atomic.Int64
}

func newMockQueue(claims int64) *mockQueue {
	m := &mockQueue{}
	m.acceptComplete.Store(true)
	m.claimsLeft.Store(claims)
	return m
}

func (m *mockQueue) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/claim", func(w http.ResponseWriter, r *http.Request) {
		if m.claimsLeft.Add(-1) >= 0 {
			m.claimCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"runId": "run-test-1",
				"agentCode": "agent--fqud3f",
				"templateId": "tpl-1",
				"templateInputData": {
					"taskBrief": "assess AAPL",
					"params": {"symbols": ["AAPL"]}
				},
				"leaseExpireAt": "2026-01-01T00:00:00Z"
			}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/complete", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.completeBodies = append(m.completeBodies, mustJSON(body))
		accepted := m.acceptComplete.Load()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if accepted {
			_, _ = w.Write([]byte(`{"code":"OK","accepted":true}`))
		} else {
			_, _ = w.Write([]byte(`{"code":"OK","accepted":false,"runStatus":"REJECTED","runMsg":"bad result"}`))
		}
	})
	mux.HandleFunc("/renew", func(w http.ResponseWriter, r *http.Request) {
		m.renewCount.Add(1)
		m.mu.Lock()
		conflict := m.renewConflict
		m.mu.Unlock()
		if conflict {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		m.heartbeatCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queueDepth":0,"selfCheckRequired":false,"heartbeatIntervalSeconds":1}`))
	})
	return mux
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (m *mockQueue) completed() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.completeBodies))
	copy(out, m.completeBodies)
	return out
}

func workerConfig(queueURL string) *Config {
	return &Config{
		PushEnabled:       false,
		PushPath:          "/mota/push",
		SigningSecret:     "unused",
		SignatureEncoding: "hex",
		PullEnabled:       true,
		QueueURL:          queueURL,
		BindToken:         "test-bind-token",
		WorkerID:          "test-worker",
		ClaimTimeout:      60 * time.Second,
		HeartbeatInterval: 1 * time.Second,
		RenewInterval:     30 * time.Second,
		TaskTimeout:       60 * time.Second,
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerClaimsProcessesAndCompletes(t *testing.T) {
	mock := newMockQueue(1)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	worker := NewWorker(workerConfig(srv.URL), PlaceholderProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "complete", func() bool { return len(mock.completed()) > 0 })

	if mock.claimCount.Load() < 1 {
		t.Fatalf("expected at least 1 claim, got %d", mock.claimCount.Load())
	}
	body := mock.completed()[0]
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("bad complete body: %v", err)
	}
	result, ok := payload["payload_result"].(map[string]any)
	if !ok {
		t.Fatalf("missing payload_result: %s", body)
	}
	for _, key := range []string{"assessments", "summary", "extensions"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("payload_result missing %q: %s", key, body)
		}
	}
	assessments := result["assessments"].([]any)
	if len(assessments) != 1 {
		t.Fatalf("expected 1 assessment, got %d", len(assessments))
	}
}

// usageProcessor 是产出 FINAL usage 的处理器,供受信通道相关测试复用。
type workerUsageProcessor struct{}

func (workerUsageProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	out := PlaceholderOutput("pull usage")
	out.Usage = FinalReportedUsage(50, uint64Ptr(10), 20, uint64Ptr(5), 1)
	return out, nil
}

// 受信 Pull 通道:complete 必须携带 FINAL usage。对应 rs
// trusted_worker_complete_includes_final_usage。
func TestWorkerTrustedCompleteIncludesFinalUsage(t *testing.T) {
	mock := newMockQueue(1)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := workerConfig(srv.URL)
	cfg.TrustedUsageReportingEnabled = true

	worker := NewWorker(cfg, workerUsageProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "trusted complete", func() bool { return len(mock.completed()) > 0 })

	body := mock.completed()[0]
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("bad complete body: %v", err)
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage in complete body: %s", body)
	}
	if usage["schemaVersion"] != "1.0" {
		t.Errorf("usage.schemaVersion = %v, want 1.0", usage["schemaVersion"])
	}
	if usage["promptTokens"] != float64(50) {
		t.Errorf("usage.promptTokens = %v, want 50", usage["promptTokens"])
	}
	if usage["cachedInputTokens"] != float64(10) {
		t.Errorf("usage.cachedInputTokens = %v, want 10", usage["cachedInputTokens"])
	}
	if usage["completionTokens"] != float64(20) {
		t.Errorf("usage.completionTokens = %v, want 20", usage["completionTokens"])
	}
	if usage["totalTokens"] != float64(70) {
		t.Errorf("usage.totalTokens = %v, want 70", usage["totalTokens"])
	}
	if usage["estimated"] != false {
		t.Errorf("usage.estimated = %v, want false", usage["estimated"])
	}
}

// 受信 Pull 通道拿不到 FINAL usage 时必须抑制 complete,而不是发一份不完整的
// 结果。对应 rs trusted_worker_suppresses_complete_when_usage_is_missing。
func TestWorkerTrustedSuppressesCompleteWhenUsageIsMissing(t *testing.T) {
	mock := newMockQueue(1)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := workerConfig(srv.URL)
	cfg.TrustedUsageReportingEnabled = true

	// PlaceholderProcessor 从不产出 Usage,天然命中抑制路径。
	worker := NewWorker(cfg, PlaceholderProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	// 给足时间让 claim → 处理 → (本该 complete 的时刻)跑完。
	time.Sleep(1500 * time.Millisecond)

	if got := len(mock.completed()); got != 0 {
		t.Fatalf("expected no complete when trusted usage is missing, got %d", got)
	}
}

func TestWorkerSendsHeartbeats(t *testing.T) {
	mock := newMockQueue(0)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	worker := NewWorker(workerConfig(srv.URL), PlaceholderProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "2 heartbeats", func() bool { return mock.heartbeatCount.Load() >= 2 })
}

func TestWorkerAbandonsTaskOnRenewConflict(t *testing.T) {
	mock := newMockQueue(1)
	mock.renewConflict = true
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := workerConfig(srv.URL)
	cfg.RenewInterval = 100 * time.Millisecond

	worker := NewWorker(cfg, slowProcessor{}, shutdown.New())
	worker.Start()

	time.Sleep(1500 * time.Millisecond)
	worker.Stop()

	if len(mock.completed()) != 0 {
		t.Fatalf("expected no complete after lease conflict, got %d", len(mock.completed()))
	}
	if mock.renewCount.Load() < 1 {
		t.Fatalf("expected at least 1 renew")
	}
}

type slowProcessor struct{}

func (slowProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(1 * time.Second):
		return &ProcessedOutput{Summary: "slow done"}, nil
	}
}

func TestWorkerReportsProcessorFailure(t *testing.T) {
	mock := newMockQueue(1)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	worker := NewWorker(workerConfig(srv.URL), failingProcessor{msg: "boom"}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "complete", func() bool { return len(mock.completed()) > 0 })

	body := mock.completed()[0]
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	info, _ := payload["payload_info"].(string)
	if info != "processor failed" {
		t.Fatalf("expected sanitized payload_info, got %s", body)
	}
	if strings.Contains(string(body), "boom") {
		t.Fatalf("internal processor error leaked: %s", body)
	}
}

func TestWorkerCompleteNotAccepted(t *testing.T) {
	mock := newMockQueue(1)
	mock.acceptComplete.Store(false)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	worker := NewWorker(workerConfig(srv.URL), PlaceholderProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "complete", func() bool { return len(mock.completed()) > 0 })

	if got := len(mock.completed()); got != 1 {
		t.Fatalf("expected exactly 1 complete, got %d", got)
	}
}

func TestWorkerTaskTimeoutAbandons(t *testing.T) {
	mock := newMockQueue(1)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := workerConfig(srv.URL)
	cfg.TaskTimeout = 300 * time.Millisecond

	worker := NewWorker(cfg, slowProcessor{}, shutdown.New())
	worker.Start()

	time.Sleep(1500 * time.Millisecond)
	worker.Stop()

	if got := len(mock.completed()); got != 0 {
		t.Fatalf("expected no complete after task timeout, got %d", got)
	}
}

func TestWorkerHeartbeatUsesReceiptInterval(t *testing.T) {
	mock := newMockQueue(0)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := workerConfig(srv.URL)
	cfg.HeartbeatInterval = 3 * time.Second

	worker := NewWorker(cfg, PlaceholderProcessor{}, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "2 heartbeats", func() bool { return mock.heartbeatCount.Load() >= 2 })
}

func TestRunAgentSelfCheckPassesWithPlaceholderProcessor(t *testing.T) {
	execution := NewProcessorBackend(PlaceholderProcessor{})
	pass, latencyMs, errMsg := runAgentSelfCheck(execution, "", time.Second)
	if !pass {
		t.Fatalf("self-check should pass with placeholder processor, error: %q", errMsg)
	}
	if errMsg != "" {
		t.Fatalf("expected no error message, got %q", errMsg)
	}
	if latencyMs < 0 || latencyMs > 5000 {
		t.Fatalf("unexpected latency: %dms", latencyMs)
	}
}

type failingCheckProcessor struct{}

func (failingCheckProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	return nil, context.DeadlineExceeded
}

func TestRunAgentSelfCheckReportsFailureWhenProcessorErrors(t *testing.T) {
	execution := NewProcessorBackend(failingCheckProcessor{})
	pass, _, errMsg := runAgentSelfCheck(execution, "", time.Second)
	if pass {
		t.Fatal("self-check must fail when processor errors")
	}
	if errMsg != "self-check execution failed" {
		t.Fatalf("self-check error = %q, want stable sanitized message", errMsg)
	}
	if strings.Contains(errMsg, context.DeadlineExceeded.Error()) {
		t.Fatalf("internal execution error leaked through self-check: %q", errMsg)
	}
}

type heartbeatCapture struct {
	mu      sync.Mutex
	bodies  [][]byte
	tokens  []string
	replies []map[string]any
}

func (h *heartbeatCapture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *heartbeatCapture) bodyAt(i int) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bodies[i]
}

func (h *heartbeatCapture) tokenAt(i int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tokens[i]
}

// heartbeatTestAgent 建一个启用中的 Agent,返回它的 slug 与明文通道密钥,
// 供按 Agent 心跳测试使用。
func heartbeatTestAgent(t *testing.T, store *AgentStore) (slug, secret string) {
	t.Helper()
	slug = uniqueSlug("hb")
	secret = unique("hb-secret")
	_, err := store.CreateAgent(context.Background(), &AgentInput{
		Slug: slug, Name: "heartbeat agent", ExecutorKind: ExecutorKindRust,
		ExecutionMode: ExecutionModeLocal, SigningSecret: &secret,
		SignatureEncoding: "hex", WorkspacePath: "agents",
		PermissionMode: "dontAsk",
		TimeoutSecs:    uint64Ptr(30),
		MaxConcurrency: intPtr(2),
		QueueCapacity:  intPtr(4),
	})
	if err != nil {
		t.Fatalf("create heartbeat agent: %v", err)
	}
	t.Cleanup(func() { _, _ = store.DeleteAgent(context.Background(), slug) })
	return slug, secret
}

// newHeartbeatTestStore 连接测试数据库,返回一个独立的 AgentStore(不依赖
// PlatformState,按 Agent 心跳只需要 ListAgentSecrets/RecordChannelEvent 两个方法)。
func newHeartbeatTestStore(t *testing.T) *AgentStore {
	t.Helper()
	return newTestStore(t)
}

func TestPushHeartbeatSendsSelfCheckOnDemandPerAgent(t *testing.T) {
	store := newHeartbeatTestStore(t)
	slug, secret := heartbeatTestAgent(t, store)

	c := &heartbeatCapture{}
	var replies = []map[string]any{
		{"heartbeatIntervalSeconds": 5, "selfCheckRequired": true},
		{"heartbeatIntervalSeconds": 5, "selfCheckRequired": false},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAllLimited(r)
		c.mu.Lock()
		c.bodies = append(c.bodies, raw)
		c.tokens = append(c.tokens, r.Header.Get("Authorization"))
		idx := len(c.bodies) - 1
		reply := replies[idx]
		if idx >= len(replies)-1 {
			reply = replies[len(replies)-1]
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.HeartbeatURL = strings.Replace(
		srv.URL, "http://", "http://user:password@", 1,
	) + "/private/heartbeat?token=must-not-be-audited"
	cfg.HeartbeatInterval = 5 * time.Second

	execution := NewProcessorBackend(PlaceholderProcessor{})
	token := shutdown.New()
	go RunPushHeartbeat(cfg, execution, token, store)

	waitFor(t, 20*time.Second, "2 push heartbeats", func() bool { return c.count() >= 2 })

	if got := c.tokenAt(0); got != "Bearer "+secret {
		t.Fatalf("expected Bearer %s auth, got %q", secret, got)
	}
	first := c.bodyAt(0)
	var firstBody map[string]any
	if err := json.Unmarshal(first, &firstBody); err != nil {
		t.Fatalf("bad first heartbeat body: %v", err)
	}
	if _, ok := firstBody["selfCheck"]; ok {
		t.Fatalf("plain heartbeat must not carry selfCheck: %s", first)
	}

	second := c.bodyAt(1)
	var secondBody map[string]any
	if err := json.Unmarshal(second, &secondBody); err != nil {
		t.Fatalf("bad second heartbeat body: %v", err)
	}
	sc, ok := secondBody["selfCheck"].(map[string]any)
	if !ok {
		t.Fatalf("second heartbeat missing selfCheck: %s", second)
	}
	if pass, _ := sc["pass"].(bool); !pass {
		t.Fatalf("selfCheck.pass must be true: %s", second)
	}
	if _, ok := sc["latencyMs"]; !ok {
		t.Fatalf("selfCheck missing latencyMs: %s", second)
	}
	if sc["error"] != nil {
		t.Fatalf("selfCheck.error should be null on success: %s", second)
	}

	// 每次心跳都必须落一条 channel_events 记录,供管理台展示通道健康度。
	//
	// RecordChannelEvent 发生在「HTTP 心跳请求已收到响应」之后,所以
	// c.count() >= 2 只能保证第二次心跳的 HTTP 往返已完成,不能保证它的
	// RecordChannelEvent 也落库了。这里必须再等一轮,而不是直接 Cancel ——
	// 否则会用 token 的 ctx 打断这次还在飞的写入,让断言变得不稳定。
	var events []ChannelEventView
	waitFor(t, 5*time.Second, "2 heartbeat channel events", func() bool {
		var err error
		events, err = store.ListChannelEvents(context.Background(), "heartbeat", slug, 10)
		return err == nil && len(events) >= 2
	})

	token.Cancel()

	if len(events) < 2 {
		t.Fatalf("expected at least 2 heartbeat channel events, got %d", len(events))
	}
	for _, event := range events {
		if event.URL != srv.URL {
			t.Errorf("heartbeat audit URL = %q, want credential-free origin %q", event.URL, srv.URL)
		}
	}
}

// 平台关闭(store 为 nil)时,按 Agent 心跳必须直接跳过,不 panic、不空转。
func TestPushHeartbeatSkippedWithoutStore(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatURL = "http://127.0.0.1:1/unreachable"
	execution := NewProcessorBackend(PlaceholderProcessor{})
	token := shutdown.New()
	done := make(chan struct{})
	go func() {
		RunPushHeartbeat(cfg, execution, token, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPushHeartbeat(..., nil) must return immediately when store is nil")
	}
}
