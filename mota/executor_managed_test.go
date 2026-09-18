package mota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const managedFundamentalOutput = `{"payload_result":{"assessments":[{"symbol":"AAPL","rating":"HOLD","fair_value":200,"reasoning":"managed test"}],"summary":"managed"}}`

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
