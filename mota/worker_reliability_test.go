package mota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hwhkit/hwhkit-go/core/shutdown"
)

type pullPanickingProcessor struct{}

func (pullPanickingProcessor) Process(context.Context, *TaskContext) (*ProcessedOutput, error) {
	panic("sensitive panic value")
}

type nilOutputProcessor struct{}

func (nilOutputProcessor) Process(context.Context, *TaskContext) (*ProcessedOutput, error) {
	return nil, nil
}

func assertWorkerCompletesSanitizedFailure(t *testing.T, processor Processor) {
	t.Helper()
	mock := newMockQueue(1)
	server := httptest.NewServer(mock.handler())
	defer server.Close()

	worker := NewWorker(workerConfig(server.URL), processor, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "sanitized failure complete", func() bool {
		return len(mock.completed()) > 0
	})
	body := mock.completed()[0]
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode complete body: %v", err)
	}
	if got, _ := payload["payload_info"].(string); got != "processor failed" {
		t.Fatalf("payload_info = %q, want sanitized failure; body=%s", got, body)
	}
	if strings.Contains(string(body), "sensitive") || strings.Contains(string(body), "panicked") {
		t.Fatalf("internal failure leaked to complete body: %s", body)
	}
}

func TestPullWorkerRecoversExecutionPanic(t *testing.T) {
	assertWorkerCompletesSanitizedFailure(t, pullPanickingProcessor{})
}

func TestPullWorkerRejectsNilExecutionOutput(t *testing.T) {
	assertWorkerCompletesSanitizedFailure(t, nilOutputProcessor{})
}

func TestRunAgentSelfCheckRecoversExecutionPanic(t *testing.T) {
	pass, _, message := runAgentSelfCheck(
		NewProcessorBackend(pullPanickingProcessor{}), "", time.Second)
	if pass {
		t.Fatal("self-check must fail after a processor panic")
	}
	if message != "self-check execution panicked" {
		t.Fatalf("message = %q, want sanitized panic message", message)
	}
	if strings.Contains(message, "sensitive") {
		t.Fatalf("panic value leaked from self-check: %q", message)
	}
}

func TestRunAgentSelfCheckRejectsNilOutput(t *testing.T) {
	pass, _, message := runAgentSelfCheck(
		NewProcessorBackend(nilOutputProcessor{}), "", time.Second)
	if pass {
		t.Fatal("self-check must fail when execution returns nil output")
	}
	if message != "self-check returned no output" {
		t.Fatalf("message = %q, want nil-output failure", message)
	}
}

func TestCredentialedHeartbeatClientsRejectCrossOriginRedirect(t *testing.T) {
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	pullWorker := NewWorker(
		workerConfig(redirect.URL), PlaceholderProcessor{}, shutdown.New())
	clients := map[string]*http.Client{
		"pull":           pullWorker.client,
		"push-heartbeat": newPushHeartbeatHTTPClient(),
	}
	for name, client := range clients {
		request, err := http.NewRequest(http.MethodPost, redirect.URL+"/heartbeat", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer should-not-leak")
		response, err := client.Do(request)
		if response != nil {
			closeBody(response)
		}
		if err == nil {
			t.Errorf("%s client followed a cross-origin redirect", name)
		}
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d credentialed request(s), want 0", got)
	}
}

func TestHeartbeatResponseBodyLimit(t *testing.T) {
	_, err := readHeartbeatResponse(strings.NewReader(
		strings.Repeat("x", heartbeatResponseBodyLimit+1)))
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized heartbeat response error = %v", err)
	}
}

func TestDecodeJSONRejectsOversizedQueueResponses(t *testing.T) {
	tests := []struct {
		name  string
		limit int64
	}{
		{name: "claim", limit: claimResponseBodyLimit},
		{name: "complete", limit: queueReceiptResponseBodyLimit},
		{name: "heartbeat", limit: heartbeatResponseBodyLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{Body: io.NopCloser(strings.NewReader(
				strings.Repeat("x", int(test.limit+1))))}
			var decoded map[string]any
			err := decodeJSON(response, &decoded, test.limit)
			if err == nil || !strings.Contains(err.Error(), "exceeded") {
				t.Fatalf("oversized %s response error = %v", test.name, err)
			}
		})
	}
}

func TestSelfCheckRequestConsumedUntilValidReceiptRearms(t *testing.T) {
	state := &agentHeartbeatState{intervalSecs: 5, selfCheckDue: true}
	if !consumeSelfCheckRequest(state) {
		t.Fatal("pending self-check request was not consumed")
	}
	if state.selfCheckDue {
		t.Fatal("consumed self-check must be cleared locally")
	}
	if consumeSelfCheckRequest(state) {
		t.Fatal("failed heartbeat must not repeat the paid self-check automatically")
	}

	if err := applyHeartbeatResponse(state, []byte(`not-json`)); err == nil {
		t.Fatal("invalid heartbeat receipt must be rejected")
	}
	if state.selfCheckDue || state.intervalSecs != 5 {
		t.Fatalf("invalid receipt mutated state: %+v", state)
	}

	if err := applyHeartbeatResponse(state, []byte(
		`{"heartbeatIntervalSeconds":9,"selfCheckRequired":true}`)); err != nil {
		t.Fatalf("apply valid heartbeat receipt: %v", err)
	}
	if !state.selfCheckDue || state.intervalSecs != 9 {
		t.Fatalf("valid receipt did not rearm self-check: %+v", state)
	}
	if !consumeSelfCheckRequest(state) || state.selfCheckDue {
		t.Fatalf("rearmed self-check was not consumed exactly once: %+v", state)
	}

	if err := applyHeartbeatResponse(state, []byte(
		`{"heartbeatIntervalSeconds":1,"selfCheckRequired":false}`)); err != nil {
		t.Fatalf("apply clear heartbeat receipt: %v", err)
	}
	if state.selfCheckDue || state.intervalSecs != 5 {
		t.Fatalf("valid clear receipt produced wrong state: %+v", state)
	}
}
