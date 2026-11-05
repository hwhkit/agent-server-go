package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 心跳假件
// ---------------------------------------------------------------------------

type fakeChannels struct{ channels []agent.Channel }

func (f fakeChannels) ListChannels(context.Context) ([]agent.Channel, error) {
	return f.channels, nil
}

type fakeRecorder struct {
	mu     sync.Mutex
	events []audit.ChannelEvent
}

func (f *fakeRecorder) RecordChannelEvent(_ context.Context, event audit.ChannelEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakeRecorder) last(t *testing.T) audit.ChannelEvent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		t.Fatal("no channel event recorded")
	}
	return f.events[len(f.events)-1]
}

type fakeHeartbeatPoster struct {
	mu       sync.Mutex
	status   int
	body     string
	err      error
	requests []map[string]string
	bodies   []string
}

func (f *fakeHeartbeatPoster) Post(_ context.Context, _ string, headers map[string]string, body []byte) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, headers)
	f.bodies = append(f.bodies, string(body))
	if f.err != nil {
		return 0, "", f.err
	}
	return f.status, f.body, nil
}

func (f *fakeHeartbeatPoster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestHeartbeat(poster HeartbeatPoster, channels ChannelLister, recorder EventRecorder,
	dispatcher Dispatcher) *PushHeartbeat {
	heartbeat := NewPushHeartbeat(PushHeartbeatConfig{
		HeartbeatURL:    "https://platform.example.com/heartbeat",
		DefaultInterval: 5 * time.Second, TaskTimeout: time.Second,
	}, poster, channels, recorder, dispatcher)
	heartbeat.failureBackoff = 0
	heartbeat.now = func() time.Time { return time.Unix(1762300000, 0) }
	return heartbeat
}

func testChannels() []agent.Channel {
	return []agent.Channel{
		{Slug: "desk-a", SigningSecret: "secret-a"},
		{Slug: "desk-b", SigningSecret: "secret-b"},
	}
}

// ---------------------------------------------------------------------------
// 发送与鉴权头
// ---------------------------------------------------------------------------

func TestHeartbeatSendsPerAgentBearer(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 200, body: `{"heartbeatIntervalSeconds":60}`}
	recorder := &fakeRecorder{}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()}, recorder, &fakeDispatcher{})

	heartbeat.RunCycle(context.Background())

	if poster.count() != 2 {
		t.Fatalf("posts = %d, want one per channel", poster.count())
	}
	if got := poster.requests[0]["Authorization"]; got != "Bearer secret-a" {
		t.Errorf("authorization = %q", got)
	}
	if poster.bodies[0] != "{}" {
		t.Errorf("plain heartbeat body = %s", poster.bodies[0])
	}
	event := recorder.last(t)
	if event.EventType != "heartbeat" || event.Pass == nil || !*event.Pass {
		t.Errorf("event = %+v", event)
	}
	if event.URL != "https://platform.example.com" {
		t.Errorf("event url must be credential-free origin: %q", event.URL)
	}
}

func TestHeartbeatDueGating(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 200, body: `{}`}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, &fakeRecorder{}, &fakeDispatcher{})
	now := time.Unix(1762300000, 0)
	heartbeat.now = func() time.Time { return now }

	heartbeat.RunCycle(context.Background())
	if poster.count() != 1 {
		t.Fatalf("first cycle posts = %d", poster.count())
	}
	// 服务端下发 60s 间隔：30s 后仍未到期。
	poster.body = `{"heartbeatIntervalSeconds":60}`
	now = now.Add(61 * time.Second)
	heartbeat.RunCycle(context.Background())
	if poster.count() != 2 {
		t.Fatalf("after interval posts = %d", poster.count())
	}
	now = now.Add(30 * time.Second)
	heartbeat.RunCycle(context.Background())
	if poster.count() != 2 {
		t.Errorf("not due yet but posted again: %d", poster.count())
	}
}

func TestHeartbeatIntervalClamping(t *testing.T) {
	t.Parallel()
	// 低于下限抬到 5s。
	poster := &fakeHeartbeatPoster{status: 200, body: `{"heartbeatIntervalSeconds":2}`}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, &fakeRecorder{}, &fakeDispatcher{})
	heartbeat.RunCycle(context.Background())
	if got := heartbeat.states["desk-a"].intervalSecs; got != 5 {
		t.Errorf("interval clamped low = %d, want 5", got)
	}
	// 高于上限截到 3600s（防 uint64 溢出成负数热循环）。
	poster.body = `{"heartbeatIntervalSeconds":99999999}`
	now := time.Unix(1762300000, 0)
	heartbeat.now = func() time.Time { return now }
	now = now.Add(10 * time.Second)
	heartbeat.RunCycle(context.Background())
	if got := heartbeat.states["desk-a"].intervalSecs; got != maxHeartbeatIntervalSeconds {
		t.Errorf("interval clamped high = %d", got)
	}
}

// ---------------------------------------------------------------------------
// 自检
// ---------------------------------------------------------------------------

func TestHeartbeatSelfCheckLifecycle(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 200, body: `{"selfCheckRequired":true}`}
	recorder := &fakeRecorder{}
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "ok"}}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, recorder, dispatcher)
	now := time.Unix(1762300000, 0)
	heartbeat.now = func() time.Time { return now }

	// 第一轮：武装自检（本轮不带）。
	heartbeat.RunCycle(context.Background())
	if strings.Contains(poster.bodies[0], "selfCheck") {
		t.Error("first beat must not carry self check before it is armed")
	}
	// 第二轮：带自检结果，且只消费一次。
	now = now.Add(10 * time.Second)
	poster.body = `{}`
	heartbeat.RunCycle(context.Background())
	var second map[string]any
	if err := json.Unmarshal([]byte(poster.bodies[1]), &second); err != nil {
		t.Fatalf("second body: %v", err)
	}
	selfCheck, ok := second["selfCheck"].(map[string]any)
	if !ok || selfCheck["pass"] != true {
		t.Fatalf("self check payload = %v", second)
	}
	event := recorder.last(t)
	if event.Pass == nil || !*event.Pass {
		t.Error("self-check pass + 2xx must record pass=true")
	}
	if event.Detail != nil && !strings.Contains(*event.Detail, "selfCheck") {
		t.Errorf("detail should carry self-check latency when present: %v", event.Detail)
	}
	// 第三轮：自检已消费，不再重复。
	now = now.Add(10 * time.Second)
	heartbeat.RunCycle(context.Background())
	if strings.Contains(poster.bodies[2], "selfCheck") {
		t.Error("self check must be consumed exactly once per arming")
	}
}

func TestHeartbeatSelfCheckFailureMarksDegraded(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 200, body: `{"selfCheckRequired":true}`}
	recorder := &fakeRecorder{}
	dispatcher := &fakeDispatcher{err: errors.New("executor died")}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, recorder, dispatcher)
	now := time.Unix(1762300000, 0)
	heartbeat.now = func() time.Time { return now }

	heartbeat.RunCycle(context.Background())
	now = now.Add(10 * time.Second)
	heartbeat.RunCycle(context.Background())

	var body map[string]any
	if err := json.Unmarshal([]byte(poster.bodies[1]), &body); err != nil {
		t.Fatal(err)
	}
	selfCheck := body["selfCheck"].(map[string]any)
	if selfCheck["pass"] != false || selfCheck["error"] != "self-check execution failed" {
		t.Errorf("self check = %v", selfCheck)
	}
	event := recorder.last(t)
	if event.Pass == nil || *event.Pass {
		t.Error("failed self-check must record pass=false (degraded, not silently passing)")
	}
}

// ---------------------------------------------------------------------------
// 失败路径
// ---------------------------------------------------------------------------

func TestHeartbeatInvalidResponseFails(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 200, body: "garbage"}
	recorder := &fakeRecorder{}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, recorder, &fakeDispatcher{})
	heartbeat.RunCycle(context.Background())

	event := recorder.last(t)
	if event.Pass == nil || *event.Pass {
		t.Error("unparsable receipt must not count as a valid heartbeat")
	}
	if event.Detail == nil || *event.Detail != "invalid heartbeat response" {
		t.Errorf("detail = %v", event.Detail)
	}
}

func TestHeartbeatRejectedAndTransportFailures(t *testing.T) {
	t.Parallel()
	poster := &fakeHeartbeatPoster{status: 500, body: ""}
	recorder := &fakeRecorder{}
	heartbeat := newTestHeartbeat(poster, fakeChannels{channels: testChannels()[:1]}, recorder, &fakeDispatcher{})
	now := time.Unix(1762300000, 0)
	heartbeat.now = func() time.Time {
		now = now.Add(3 * time.Second)
		return now
	}
	heartbeat.RunCycle(context.Background())
	event := recorder.last(t)
	if event.Pass == nil || *event.Pass || event.StatusCode == nil || *event.StatusCode != 500 {
		t.Errorf("rejected event = %+v", event)
	}

	poster.err = errors.New("dial timeout")
	poster.status = 0
	heartbeat.RunCycle(context.Background())
	event = recorder.last(t)
	if event.Pass == nil || *event.Pass {
		t.Errorf("transport failure event = %+v", event)
	}
}
