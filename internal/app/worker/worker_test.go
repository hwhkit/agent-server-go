package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

type fakeResponse struct {
	status int
	body   string
	err    error
}

type recordedCall struct {
	path    string
	query   map[string]string
	body    []byte
	headers map[string]string
}

type fakeClient struct {
	mu       sync.Mutex
	handlers map[string][]fakeResponse // 按 path 顺序消费；耗尽后取最后一项
	counts   map[string]int
	last     map[string]recordedCall
	calls    chan recordedCall
}

func newFakeClient() *fakeClient {
	return &fakeClient{handlers: map[string][]fakeResponse{},
		counts: map[string]int{}, last: map[string]recordedCall{},
		calls: make(chan recordedCall, 256)}
}

func (f *fakeClient) on(path string, responses ...fakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[path] = append(f.handlers[path], responses...)
}

func (f *fakeClient) Post(_ context.Context, path string, query map[string]string,
	body []byte, headers map[string]string, _ int) (int, []byte, error) {
	f.mu.Lock()
	queue := f.handlers[path]
	var response fakeResponse
	switch {
	case len(queue) == 0:
		response = fakeResponse{status: 204}
	case len(queue) == 1:
		response = queue[0]
	default:
		response = queue[0]
		f.handlers[path] = queue[1:]
	}
	call := recordedCall{path: path, query: query, body: body, headers: headers}
	f.counts[path]++
	f.last[path] = call
	f.mu.Unlock()
	select {
	case f.calls <- call:
	default:
	}
	if response.err != nil {
		return response.status, nil, response.err
	}
	return response.status, []byte(response.body), nil
}

// waitFor 轮询等待指定 path 至少第 want 次调用（对记录通道丢弃免疫：
// claim 204 热循环会冲爆任何有界通道，计数才是可靠观察面）。
func (f *fakeClient) waitFor(t *testing.T, path string, want int, timeout time.Duration) recordedCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		count := f.counts[path]
		call := f.last[path]
		f.mu.Unlock()
		if count >= want {
			return call
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s call #%d", path, want)
	return recordedCall{}
}

// countOf 返回指定 path 的累计调用数。
func (f *fakeClient) countOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

type fakeDispatcher struct {
	output *platform.ProcessedOutput
	err    error
	block  chan struct{}
	calls  chan *platform.TaskContext
}

func (f *fakeDispatcher) Execute(_ context.Context, _ string, task *platform.TaskContext,
	_ any, _ time.Duration) (*platform.ProcessedOutput, error) {
	if f.calls != nil {
		select {
		case f.calls <- task:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	return f.output, f.err
}

func testConfig() Config {
	return Config{
		WorkerID: "worker-1", TaskTimeout: 2 * time.Second,
		RenewInterval: 5 * time.Millisecond, HeartbeatInterval: 5 * time.Millisecond,
	}
}

func testWorker(client *fakeClient, dispatcher Dispatcher) *Worker {
	worker := New(testConfig(), client, dispatcher)
	worker.retryDelay = 5 * time.Millisecond
	return worker
}

func claimBody(runID string) string {
	return `{"runId":"` + runID + `","agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST",` +
		`"templateInputData":[{"name":"symbols","value":["AAPL"]}],"taskBrief":{"instruction":"analyze"},` +
		`"globalParameters":{"language":"zh-CN"},"leaseExpireAt":"2026-11-05T00:00:00Z"}`
}

// ---------------------------------------------------------------------------
// 主路径
// ---------------------------------------------------------------------------

func TestClaimProcessComplete(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	client.on("/claim",
		fakeResponse{status: 200, body: claimBody("run-w1")},
		fakeResponse{status: 204})
	dispatcher := &fakeDispatcher{
		output: &platform.ProcessedOutput{Summary: "分析完成"},
		calls:  make(chan *platform.TaskContext, 1),
	}
	worker := testWorker(client, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	// 执行上下文映射：语言/参数/岗位来自 claim 信封。
	task := <-dispatcher.calls
	if task.Language == nil || *task.Language != "zh-CN" {
		t.Errorf("language = %v", task.Language)
	}
	params, _ := task.Params.(map[string]any)
	if params == nil || params["symbols"] == nil {
		t.Errorf("params not flattened: %v", task.Params)
	}

	call := client.waitFor(t, "/complete", 1, 3*time.Second)
	if call.headers[platform.HeaderRunID] != "run-w1" {
		t.Errorf("complete headers = %+v", call.headers)
	}
	var envelope map[string]any
	if err := json.Unmarshal(call.body, &envelope); err != nil {
		t.Fatalf("complete body not JSON: %v", err)
	}
	if envelope["contract_version"] != "V2" {
		t.Errorf("contract_version = %v", envelope["contract_version"])
	}
	// RULE-BND-001：缺计量时兜底最小合规 usage。
	usage, ok := envelope["usage"].(map[string]any)
	if !ok || usage["status"] != "FINAL" {
		t.Errorf("usage fallback missing: %v", envelope["usage"])
	}
	if total, _ := usage["total_tokens"].(float64); total <= 0 {
		if total2, _ := usage["totalTokens"].(float64); total2 <= 0 {
			t.Errorf("usage total must be > 0: %+v", usage)
		}
	}
	worker.Stop()
}

func TestProcessorFailureIsSanitized(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	client.on("/claim",
		fakeResponse{status: 200, body: claimBody("run-w2")},
		fakeResponse{status: 204})
	dispatcher := &fakeDispatcher{err: errors.New("provider API key leaked details: sk-123")}
	worker := testWorker(client, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	call := client.waitFor(t, "/complete", 1, 3*time.Second)
	body := string(call.body)
	if strings.Contains(body, "sk-123") {
		t.Error("internal error details must not reach the platform channel")
	}
	if !strings.Contains(body, "processor failed") {
		t.Errorf("sanitized payload_info missing: %s", body)
	}
	worker.Stop()
}

func TestRenewConflictAbandonsTask(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	client.on("/claim",
		fakeResponse{status: 200, body: claimBody("run-w3")},
		fakeResponse{status: 204})
	client.on("/renew", fakeResponse{status: 409, body: `{"code":"BOUND_CLAIM_CONFLICT"}`})
	block := make(chan struct{})
	defer close(block)
	dispatcher := &fakeDispatcher{block: block, output: &platform.ProcessedOutput{}}
	worker := testWorker(client, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	// renew 409 → 放弃任务：不得出现 complete。
	client.waitFor(t, "/renew", 1, 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	if got := client.countOf("/complete"); got != 0 {
		t.Fatalf("task must be abandoned after renew conflict, completes = %d", got)
	}
	worker.Stop()
}

func TestTrustedUsageSuppressesComplete(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	client.on("/claim",
		fakeResponse{status: 200, body: claimBody("run-w4")},
		fakeResponse{status: 204})
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "no usage"}}
	worker := testWorker(client, dispatcher)
	worker.config.TrustedUsageReporting = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	client.waitFor(t, "/claim", 1, 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	if got := client.countOf("/complete"); got != 0 {
		t.Fatalf("trusted channel must suppress complete without FINAL usage, completes = %d", got)
	}
	worker.Stop()
}

func TestHeartbeatAppliesServerInterval(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	client.on("/heartbeat", fakeResponse{status: 200, body: `{"heartbeatIntervalSeconds":3600}`})
	worker := testWorker(client, &fakeDispatcher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 只跑心跳循环：claim 循环与本测试无关，且 204 即重试的节奏会制造噪音。
	// heartbeatLoop 自带 wg.Done，需要配对的 Add（Start 里是 Add(2)）。
	worker.wg.Add(1)
	go worker.heartbeatLoop(ctx)

	// 首次心跳后间隔被服务端下发值(3600s)接管：短窗口内只应看到一次心跳。
	client.waitFor(t, "/heartbeat", 1, 2*time.Second)
	time.Sleep(150 * time.Millisecond)
	if got := client.countOf("/heartbeat"); got != 1 {
		t.Errorf("heartbeat count = %d, server-controlled interval must suppress rapid re-beats", got)
	}
}

func TestClaimParamsFlattening(t *testing.T) {
	t.Parallel()
	flat := claimParams([]any{
		map[string]any{"name": "symbols", "value": []any{"AAPL"}},
		map[string]any{"name": "horizon", "value": "1M"},
		"garbage",
	})
	params, ok := flat.(map[string]any)
	if !ok || params["horizon"] != "1M" {
		t.Fatalf("flattened = %v", flat)
	}
	wrapped := claimParams(map[string]any{"params": map[string]any{"a": 1}})
	if wrapped.(map[string]any)["a"] != 1 {
		t.Errorf("wrapped params = %v", wrapped)
	}
	if got := claimParams(nil); len(got.(map[string]any)) != 0 {
		t.Errorf("nil input = %v", got)
	}
	language := claimLanguage(map[string]any{"language": "en"})
	if language == nil || *language != "en" {
		t.Errorf("language = %v", language)
	}
	if got := claimLanguage("not-an-object"); got != nil {
		t.Errorf("non-object language = %v", got)
	}
}
