//go:build unix

package mota

// 标准 Push/Pull 入口必须通过 AgentDispatcher 启动所选本机 Agent。
// 对应 rs tests/protocol_dispatch_test.rs::push_and_pull_both_dispatch_to_local_pi。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hwhkit/hwhkit-go/core/shutdown"
)

const protocolDispatchSecret = "mbt_protocol-dispatch-test-secret"

// protocolDispatchAgent 是 Push/Pull 两条入口共同分发到的本机 Pi Agent。
func protocolDispatchAgent(piBinary string) *AgentConfig {
	return &AgentConfig{
		ID: 1, Slug: "local-pi", Name: "Local Pi", Enabled: true,
		ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeLocal,
		SigningSecret: protocolDispatchSecret, SignatureEncoding: "hex",
		SystemPrompt: "fundamental_analyst", WorkspacePath: ".",
		PermissionMode: "dontAsk", TimeoutSecs: 10,
		MaxConcurrency: 1, QueueCapacity: 2, Revision: 1,
	}
}

// protocolDispatchBackend 组装一个走本机 Pi 的共享执行后端,Push 与 Pull 都用它。
// 假 Pi 脚本每次调用都往 marker 文件追加一行,用于统计调用次数。
func protocolDispatchBackend(t *testing.T, marker string) *TaskExecutionBackend {
	t.Helper()
	cfg := executorTestConfig(t)
	cfg.LocalAgents.PiBinary = writeFakeCLI(t,
		"printf 'run\\n' >> '"+marker+"'\n"+
			`printf '%s' '{"payload_result":{"assessments":[{"symbol":"AAPL.US","rating":"HOLD","fair_value":200,"reasoning":"Local Pi test."}],"summary":"local pi"}}'`+"\n")
	executor := newTestExecutor(t, cfg, PlaceholderProcessor{})
	registry := NewAgentRegistry([]*AgentConfig{protocolDispatchAgent(cfg.LocalAgents.PiBinary)})
	dispatcher := NewAgentDispatcher(registry, executor, newSemaphore(4), "")
	return NewAgentBackend(dispatcher)
}

func TestPushAndPullBothDispatchToLocalPi(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs.txt")
	execution := protocolDispatchBackend(t, marker)

	// --- Push 入口 -----------------------------------------------------

	pushCfg := &Config{
		PushEnabled: true, PushPath: "/mota/push",
		SigningSecret: protocolDispatchSecret, SignatureEncoding: "hex",
		PushMode: PushModeSync, ClaimTimeout: 60 * time.Second, TaskTimeout: 10 * time.Second,
	}
	idempotency := NewInMemoryPushIdempotencyStore()
	pushHandler := NewPushHandlerWithBackend(pushCfg, execution, idempotency, nil)
	r := chi.NewRouter()
	r.Post(pushCfg.PushPath, pushHandler)
	pushSrv := httptest.NewServer(r)
	defer pushSrv.Close()

	body := []byte(pushTaskBodyWith("RUN-push-local", map[string]any{
		"agentCode":    "local-pi",
		"positionType": "FUNDAMENTAL_ANALYST",
		"templateInputData": []any{
			map[string]any{"name": "securities", "value": []any{"AAPL.US"}},
		},
	}))
	sig := ComputeSignature(protocolDispatchSecret, body, "hex")
	resp, raw := postPush(t, pushSrv.URL, body, sig, "RUN-push-local", "idem-push-local")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var pushPayload PayloadV1
	if err := json.Unmarshal(raw, &pushPayload); err != nil {
		t.Fatalf("push: bad payload: %v", err)
	}
	result, ok := pushPayload.PayloadResult.(map[string]any)
	if !ok {
		t.Fatalf("push: payload_result is not an object: %v", pushPayload.PayloadResult)
	}
	assessments, ok := result["assessments"].([]any)
	if !ok || len(assessments) == 0 {
		t.Fatalf("push: expected assessments, got %v", result)
	}
	first, ok := assessments[0].(map[string]any)
	if !ok || first["symbol"] != "AAPL.US" {
		t.Fatalf("push: unexpected assessment: %v", assessments[0])
	}

	// --- Pull 入口 -------------------------------------------------------

	mock := newMockQueue(1)
	// 覆盖默认的 claim 响应,让它走 local-pi Agent(默认 mock 用的是
	// agent--fqud3f,与本测试的 Agent 不匹配)。
	mux := http.NewServeMux()
	mux.HandleFunc("/claim", func(w http.ResponseWriter, r *http.Request) {
		if mock.claimsLeft.Add(-1) >= 0 {
			mock.claimCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"schemaVersion": "1.0",
				"runId": "RUN-pull-local",
				"agentCode": "local-pi",
				"templateId": "fundamental",
				"positionType": "FUNDAMENTAL_ANALYST",
				"templateInputData": [{"name": "securities", "value": ["AAPL.US"]}]
			}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/complete", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var completeBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&completeBody)
		mock.mu.Lock()
		mock.completeBodies = append(mock.completeBodies, mustJSON(completeBody))
		mock.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"OK","accepted":true,"runStatus":"SUCCEEDED"}`))
	})
	mux.HandleFunc("/renew", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"selfCheckRequired":false,"heartbeatIntervalSeconds":60}`))
	})
	queueSrv := httptest.NewServer(mux)
	defer queueSrv.Close()

	pullCfg := workerConfig(queueSrv.URL)
	worker := NewWorkerWithBackend(pullCfg, execution, shutdown.New())
	worker.Start()
	defer worker.Stop()

	waitFor(t, 8*time.Second, "pull complete", func() bool { return len(mock.completed()) > 0 })

	completed := mock.completed()[0]
	var pullPayload map[string]any
	if err := json.Unmarshal(completed, &pullPayload); err != nil {
		t.Fatalf("pull: bad complete body: %v", err)
	}
	if pullPayload["contract_version"] != "V2" {
		t.Fatalf("pull: contract_version = %v, want V2", pullPayload["contract_version"])
	}
	pullResult, ok := pullPayload["payload_result"].(map[string]any)
	if !ok {
		t.Fatalf("pull: payload_result is not an object: %v", pullPayload["payload_result"])
	}
	pullAssessments, ok := pullResult["assessments"].([]any)
	if !ok || len(pullAssessments) == 0 {
		t.Fatalf("pull: expected assessments, got %v", pullResult)
	}
	pullFirst, ok := pullAssessments[0].(map[string]any)
	if !ok || pullFirst["symbol"] != "AAPL.US" {
		t.Fatalf("pull: unexpected assessment: %v", pullAssessments[0])
	}

	// --- 两条入口都必须真的调用了本机 Pi(不是各自兜底出一份占位结果) ------

	raw2, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	lines := 0
	for _, b := range raw2 {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("expected local Pi to run exactly twice (once via Push, once via Pull), got %d runs: %s",
			lines, raw2)
	}
}
