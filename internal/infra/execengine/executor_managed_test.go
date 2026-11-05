package execengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const managedFundamentalOutput = `{"payload_result":{"assessments":[{"symbol":"AAPL","rating":"HOLD","fair_value":200,"reasoning":"managed test"}],"summary":"managed"}}`

type countingReader struct {
	reader io.Reader
	read   int
}

type managedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f managedRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += n
	return n, err
}

func TestManagedRequestDoesNotExposeProviderErrorBody(t *testing.T) {
	const secret = "provider-secret-that-must-not-leak"
	client := &http.Client{Transport: managedRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusBadGateway,
			ContentLength: int64(len(secret)),
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(secret)),
		}, nil
	})}

	response, execErr := managedRequest(context.Background(), client, http.MethodGet, "https://provider.test/v1/session", nil, nil)
	if response != nil {
		response.Body.Close()
		t.Fatal("expected a failed managed request")
	}
	if execErr == nil {
		t.Fatal("expected an execution error")
	}
	if execErr.Code != "executor_unavailable" {
		t.Fatalf("code = %q, want executor_unavailable", execErr.Code)
	}
	if strings.Contains(execErr.Detail, secret) || strings.Contains(execErr.Error(), secret) {
		t.Fatalf("provider response body leaked into execution error: %q", execErr.Detail)
	}
	if !strings.Contains(execErr.Detail, "HTTP 502") || !strings.Contains(execErr.Detail, fmt.Sprintf("response_body_bytes=%d", len(secret))) {
		t.Fatalf("error is missing low-sensitivity response metadata: %q", execErr.Detail)
	}
}

func TestReadSSEOversizedUnterminatedLineStopsAtBound(t *testing.T) {
	const maxBytes = 1024
	source := &countingReader{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	response := &http.Response{Body: io.NopCloser(source)}

	execErr := readSSE(context.Background(), response, maxBytes, func(map[string]any) (bool, *ExecutionError) {
		t.Fatal("consumer must not be called for an oversized unterminated line")
		return false, nil
	})
	if execErr == nil || execErr.Code != "output_too_large" {
		t.Fatalf("error = %#v, want output_too_large", execErr)
	}
	if source.read > 2*bufioReaderSizeForTest {
		t.Fatalf("read %d bytes from an oversized unterminated line; want a bounded read", source.read)
	}
}

const bufioReaderSizeForTest = 4096

func TestOpenAIManagedAgentSessionLifecycle(t *testing.T) {
	inputSent := make(chan struct{})
	var deleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/sessions":
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("missing bearer auth")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sess-openai"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents/sessions/sess-openai/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-inputSent
			fmt.Fprintf(w, "data: {\"type\":\"agent.session.turn.output_text.done\",\"text\":%q}\n\n", managedFundamentalOutput)
			fmt.Fprint(w, "data: {\"type\":\"agent.session.turn.completed\",\"turn\":{\"id\":\"turn-1\",\"usage\":{\"input_tokens\":10}}}\n\n")
			w.(http.Flusher).Flush()
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/sessions/sess-openai/events":
			select {
			case <-inputSent:
			default:
				close(inputSent)
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/agents/sessions/sess-openai":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := executorTestConfig(t)
	cfg.OpenAIAgents = OpenAIAgentsConfig{Enabled: true, BaseURL: server.URL + "/v1", APIKey: "test-key", DefaultModel: "gpt-test", ReasoningEffort: "medium", Environment: "none", DeleteSession: true, MaxResponseBytes: 1 << 20}
	executor, err := NewAgentExecutor(PlaceholderProcessor{}, server.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtime := executorRuntime("fundamental_analyst", ExecutorKindOpenAIAgent, ExecutionModeManaged)
	output, err := executor.Execute(context.Background(), runtime, &TaskContext{}, map[string]any{"runId": "RUN-openai"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if output.PayloadResult.(map[string]any)["assessments"] == nil {
		t.Fatalf("unexpected output: %#v", output.PayloadResult)
	}
	if output.Usage != nil {
		t.Fatal("OpenAI Agents best-effort usage must not be reported as authoritative FINAL usage")
	}
	if deleted.Load() != 1 {
		t.Fatalf("delete calls = %d", deleted.Load())
	}
}

func TestClaudeManagedAgentSessionLifecycleAndUsage(t *testing.T) {
	inputSent := make(chan struct{})
	var deleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			if r.Header.Get("x-api-key") != "test-key" {
				t.Error("missing Anthropic API key")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sess-claude"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/sess-claude/events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-inputSent
			fmt.Fprintf(w, "data: {\"type\":\"agent.message\",\"content\":[{\"type\":\"text\",\"text\":%q}]}\n\n", managedFundamentalOutput)
			fmt.Fprint(w, "data: {\"type\":\"session.usage\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":2,\"output_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"span.model_request_end\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"session.status_idle\",\"stop_reason\":\"end_turn\"}\n\n")
			w.(http.Flusher).Flush()
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/sess-claude/events":
			select {
			case <-inputSent:
			default:
				close(inputSent)
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sessions/sess-claude":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := executorTestConfig(t)
	cfg.ClaudeManaged = ClaudeManagedAgentsConfig{Enabled: true, BaseURL: server.URL + "/v1", APIKey: "test-key", AgentID: "agent-test", EnvironmentID: "env-test", DeleteSession: true, MaxResponseBytes: 1 << 20}
	executor, err := NewAgentExecutor(PlaceholderProcessor{}, server.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtime := executorRuntime("fundamental_analyst", ExecutorKindClaudeManaged, ExecutionModeManaged)
	output, err := executor.Execute(context.Background(), runtime, &TaskContext{}, map[string]any{"runId": "RUN-claude"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if output.Usage == nil || output.Usage.PromptTokens != 12 || output.Usage.CompletionTokens != 4 || output.Usage.TotalTokens != 16 {
		t.Fatalf("unexpected usage: %#v", output.Usage)
	}
	if deleted.Load() != 1 {
		t.Fatalf("delete calls = %d", deleted.Load())
	}
}
