package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apihttp "ir-agent-server/internal/api/http"
	"ir-agent-server/internal/app/agentapi"
	"ir-agent-server/internal/app/push"
	domain "ir-agent-server/internal/domain/connector"
	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 最小假件
// ---------------------------------------------------------------------------

type memIdempotency struct{ records map[string]string }

func (m *memIdempotency) Begin(_ context.Context, key, _, _ string) (*platform.IdempotencyDecision, error) {
	if m.records == nil {
		m.records = map[string]string{}
	}
	if body, ok := m.records[key]; ok {
		return &platform.IdempotencyDecision{Kind: platform.IdemSyncCached, ResponseBody: body}, nil
	}
	m.records[key] = ""
	return &platform.IdempotencyDecision{Kind: platform.IdemNew}, nil
}
func (m *memIdempotency) CompleteSync(_ context.Context, key, _, _, body string) error {
	m.records[key] = body
	return nil
}
func (m *memIdempotency) FailSync(context.Context, string, string, string, string) error { return nil }
func (m *memIdempotency) AcceptAsync(context.Context, string, string, string) error      { return nil }
func (m *memIdempotency) Release(context.Context, string, string, string) error          { return nil }

type stubDispatcher struct{}

func (stubDispatcher) Execute(context.Context, string, *platform.TaskContext, any, time.Duration) (*platform.ProcessedOutput, error) {
	return &platform.ProcessedOutput{Summary: "stub-done"}, nil
}

type stubQueue struct{}

func (stubQueue) Enqueue(context.Context, domain.NewTask) error { return nil }
func (stubQueue) Get(context.Context, string) (*domain.Task, error) {
	return nil, domain.ErrTaskNotFound
}
func (stubQueue) WaitTerminal(context.Context, string) (domain.TaskState, []byte, error) {
	return domain.TaskQueued, nil, context.DeadlineExceeded
}
func (stubQueue) Claim(context.Context, domain.ClaimRequest) (*domain.Task, domain.ClaimOutcome, error) {
	return nil, domain.ClaimEmpty, nil
}
func (stubQueue) WorkerLeaseCount(context.Context, string) (int, error) { return 0, nil }
func (stubQueue) Renew(context.Context, string, string, time.Duration) (domain.RenewOutcome, error) {
	return domain.RenewUnknown, nil
}
func (stubQueue) RequestAbort(context.Context, string) error { return nil }
func (stubQueue) Complete(context.Context, domain.CompleteRequest) (domain.CompleteOutcome, error) {
	return domain.CompleteOutcome{}, nil
}
func (stubQueue) QueueDepth(context.Context, []string) (int, error) { return 0, nil }
func (stubQueue) Sweep(context.Context, domain.SweepPolicy) (domain.SweepStats, error) {
	return domain.SweepStats{}, nil
}

type memTokens struct{ byHash map[string]*domain.Token }

func (m *memTokens) Issue(_ context.Context, t *domain.Token) error {
	if m.byHash == nil {
		m.byHash = map[string]*domain.Token{}
	}
	m.byHash[t.Hash] = t
	return nil
}
func (m *memTokens) GetByHash(_ context.Context, hash string) (*domain.Token, error) {
	if t, ok := m.byHash[hash]; ok {
		return t, nil
	}
	return nil, domain.ErrTokenNotFound
}
func (m *memTokens) GetByName(context.Context, string) (*domain.Token, error) {
	return nil, domain.ErrTokenNotFound
}
func (m *memTokens) List(context.Context) ([]domain.Token, error) { return nil, nil }
func (m *memTokens) RevokeByName(context.Context, string) error   { return nil }
func (m *memTokens) Revoke(context.Context, string, *time.Time) error {
	return nil
}

type memWorkers struct{}

func (memWorkers) Beat(context.Context, domain.Worker) error           { return nil }
func (memWorkers) ListOnline(context.Context) ([]domain.Worker, error) { return nil, nil }
func (memWorkers) MarkOfflineStale(context.Context, time.Time) (int, error) {
	return 0, nil
}

const testSecret = "api-layer-test-secret"

// ---------------------------------------------------------------------------
// 健康端点
// ---------------------------------------------------------------------------

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	router := apihttp.NewRouter(apihttp.RouterDeps{})
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health = %v/%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	resp, err = http.Get(server.URL + "/health/ready")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("ready = %v/%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	// 安全头。
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff header = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Push 适配
// ---------------------------------------------------------------------------

func newPushServer(t *testing.T) *httptest.Server {
	t.Helper()
	service := push.NewService(push.Deps{
		Config: push.Config{
			SigningSecret: testSecret, SignatureEncoding: platform.EncodingHex,
			Mode: push.ModeSync, TaskTimeout: 5 * time.Second, MaxRequestBytes: 1 << 20,
		},
		Idempotency: &memIdempotency{}, Dispatcher: stubDispatcher{},
	})
	router := apihttp.NewRouter(apihttp.RouterDeps{
		PushPath: "/ir/push",
		Push:     &apihttp.PushHandler{Service: service, MaxRequestBytes: 1 << 20},
	})
	return httptest.NewServer(router)
}

func TestPushAdapterSuccessAndErrors(t *testing.T) {
	t.Parallel()
	server := newPushServer(t)
	defer server.Close()

	body := `{"schemaVersion":"1.0","runId":"run-api-1","agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST","templateInputData":[],"taskBrief":{},"callbackUrl":"https://platform.example.com/callback"}`
	signature := platform.EncodeSignature(
		platform.SignBody([]byte(testSecret), []byte(body)), platform.EncodingHex)

	do := func(contentType, sig string) (*http.Response, []byte) {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/ir/push", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", contentType)
		request.Header.Set(platform.HeaderRunID, "run-api-1")
		request.Header.Set(platform.HeaderIdempotencyKey, "key-api-1")
		if sig != "" {
			request.Header.Set(platform.HeaderSignature, sig)
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := readAllLimitedT(t, resp)
		return resp, raw
	}

	// 成功路径：200 + V2 信封。
	resp, raw := do("application/json; charset=utf-8", signature)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope: %v (%s)", err, raw)
	}
	if envelope["contract_version"] != "V2" || envelope["payload_info"] != "stub-done" {
		t.Errorf("envelope = %v", envelope)
	}

	// 415：problem+json 线格式。
	resp, raw = do("text/plain", signature)
	if resp.StatusCode != 415 {
		t.Errorf("content type status = %d", resp.StatusCode)
	}
	var problem map[string]any
	if err := json.Unmarshal(raw, &problem); err != nil || problem["title"] != "Unsupported Media Type" {
		t.Errorf("problem = %v (%s)", problem, raw)
	}

	// 401：缺签名头。
	resp, _ = do("application/json", "")
	if resp.StatusCode != 401 {
		t.Errorf("missing signature status = %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Agent API 适配
// ---------------------------------------------------------------------------

func newAgentAPIServer(t *testing.T, tokens *memTokens) *httptest.Server {
	t.Helper()
	service := agentapi.NewService(agentapi.Deps{
		Config: agentapi.Config{
			LeaseSeconds: 90, RenewInterval: 30 * time.Second,
			HeartbeatIntervalSeconds: 60, ClaimMaxWait: 60 * time.Millisecond,
			MaxOutstandingLeases: 64, MaxAttempts: 3, QueueTTL: time.Hour,
		},
		Queue: stubQueue{}, Tokens: tokens, Workers: memWorkers{},
	})
	router := apihttp.NewRouter(apihttp.RouterDeps{
		AgentAPI: &apihttp.AgentAPIHandler{Service: service},
	})
	return httptest.NewServer(router)
}

func TestAgentAPIAdapter(t *testing.T) {
	t.Parallel()
	tokens := &memTokens{}
	plaintext := "act_api-test-token"
	if err := tokens.Issue(context.Background(), &domain.Token{
		Name: "api-test", Hash: domain.HashToken(plaintext), Scopes: []string{"other-slug"},
	}); err != nil {
		t.Fatal(err)
	}
	server := newAgentAPIServer(t, tokens)
	defer server.Close()

	// 未带令牌 → 401 + {"code":"unauthorized"}。
	resp, err := http.Post(server.URL+"/agent-api/v1/claim", "application/json",
		bytes.NewReader([]byte(`{"workerId":"w-1"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("no token status = %d", resp.StatusCode)
	}
	var errBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if errBody["code"] != "unauthorized" {
		t.Errorf("error body = %v", errBody)
	}

	post := func(path, body string) (*http.Response, map[string]any) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader([]byte(body)))
		request.Header.Set("Authorization", "Bearer "+plaintext)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		decoded := map[string]any{}
		if response.StatusCode != 204 {
			_ = json.NewDecoder(response.Body).Decode(&decoded)
		}
		response.Body.Close()
		return response, decoded
	}

	// scope 不交叉 → 204 + X-Agent-Api-Reason: no-scope-match。
	resp, _ = post("/agent-api/v1/claim", `{"workerId":"w-1","preferredSlugs":["demo"]}`)
	if resp.StatusCode != 204 || resp.Header.Get("X-Agent-Api-Reason") != "no-scope-match" {
		t.Errorf("claim = %d reason=%q", resp.StatusCode, resp.Header.Get("X-Agent-Api-Reason"))
	}
	// 队列空 → 204 + reason: empty（长轮询窗口后）。
	resp, _ = post("/agent-api/v1/claim", `{"workerId":"w-1","preferredSlugs":["other-slug"]}`)
	if resp.StatusCode != 204 || resp.Header.Get("X-Agent-Api-Reason") != "empty" {
		t.Errorf("empty claim = %d reason=%q", resp.StatusCode, resp.Header.Get("X-Agent-Api-Reason"))
	}
	// heartbeat → 200 + 间隔回执。
	resp, body := post("/agent-api/v1/heartbeat",
		`{"workerId":"w-1","inFlight":0,"capacity":2,"capabilities":{"slugs":["other-slug"],"version":"1.0"}}`)
	if resp.StatusCode != 200 || body["heartbeatIntervalSeconds"] != float64(60) ||
		body["renewIntervalSeconds"] != float64(30) {
		t.Errorf("heartbeat = %d %v", resp.StatusCode, body)
	}
	// renew 未知 run → 404 run_not_found。
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/agent-api/v1/renew", nil)
	request.Header.Set("Authorization", "Bearer "+plaintext)
	request.Header.Set("X-Agent-Run-ID", "run-unknown")
	request.Header.Set("X-Agent-Worker-ID", "w-1")
	renewResp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	renewBody := map[string]any{}
	_ = json.NewDecoder(renewResp.Body).Decode(&renewBody)
	renewResp.Body.Close()
	if renewResp.StatusCode != 404 || renewBody["code"] != "run_not_found" {
		t.Errorf("renew = %d %v", renewResp.StatusCode, renewBody)
	}
}

func readAllLimitedT(t *testing.T, resp *http.Response) ([]byte, error) {
	t.Helper()
	defer resp.Body.Close()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
