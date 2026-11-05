package http_test

// 动态 Agent 入口（/agent/{slug}）端到端集成测试：v1 canonical 签名、
// nonce 防重放、时间窗、旧签名兼容闸、网关幂等（回放/冲突）、显式 runId
// 冲突、异步 202+callback、容量闸 429。门禁同 admin_test.go（真 PostgreSQL）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apihttp "ir-agent-server/internal/api/http"
	"ir-agent-server/internal/app/callback"
	"ir-agent-server/internal/app/dynamic"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/httpx"
	"ir-agent-server/internal/infra/postgres"
)

type dynEngine struct {
	mu      sync.Mutex
	output  *platform.ProcessedOutput
	err     error
	block   chan struct{}
	calls   int
	lastRaw any
}

func (e *dynEngine) Execute(_ context.Context, _ *agent.ResolvedConfig, _ *platform.TaskContext,
	raw any, _ time.Duration) (*platform.ProcessedOutput, error) {
	e.mu.Lock()
	e.calls++
	block, out, err := e.block, e.output, e.err
	e.lastRaw = raw
	e.mu.Unlock()
	if block != nil {
		<-block
	}
	return out, err
}

type dynFixture struct {
	server   *httptest.Server
	service  *dynamic.Service
	engine   *dynEngine
	secret   string
	suffix   string
	callback *callbackServer
}

type callbackServer struct {
	mu       sync.Mutex
	bodies   []string
	server   *httptest.Server
	received chan string
}

func newCallbackServer() *callbackServer {
	cs := &callbackServer{received: make(chan string, 8)}
	cs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buffer bytes.Buffer
		_, _ = buffer.ReadFrom(r.Body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, buffer.String())
		cs.mu.Unlock()
		select {
		case cs.received <- buffer.String():
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"OK","accepted":true,"runStatus":"completed"}`))
	}))
	return cs
}

func setupDynamic(t *testing.T, allowLegacy bool) *dynFixture {
	t.Helper()
	url := os.Getenv("V2_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no test database configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(db.Close)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatal(err)
	}
	agents := postgres.NewAgentStore(db, cipher)
	secret := "dyn-secret-" + suffix
	slugs := []string{"dyn-" + suffix, "dyn-blocked-" + suffix}
	for index, slug := range slugs {
		concurrency, queue := 4, 4
		if index == 1 {
			concurrency, queue = 1, 1
		}
		entity := &agent.Agent{
			Slug: slug, Name: slug, Enabled: true,
			Binding:   exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
			Positions: []string{"FUNDAMENTAL_ANALYST"}, SigningSecret: []byte(secret),
			TimeoutSecs: 60, MaxConcurrency: concurrency, QueueCapacity: queue,
		}
		if _, err := agents.Upsert(ctx, entity, nil); err != nil {
			t.Fatalf("seed agent %s: %v", slug, err)
		}
	}
	t.Cleanup(func() {
		for _, slug := range slugs {
			_ = agents.Delete(context.Background(), slug)
		}
	})

	cb := newCallbackServer()
	t.Cleanup(cb.server.Close)
	sender := callback.NewSender(callback.Config{}, httpx.CallbackPoster{})
	engine := &dynEngine{output: &platform.ProcessedOutput{Summary: "dyn-ok"}}
	service := dynamic.NewService(dynamic.Deps{
		Config: dynamic.Config{
			AllowLegacyBodySignature: allowLegacy,
			MaxRequestBodyBytes:      1 << 20, GlobalMaxConcurrency: 8,
		},
		Store: postgres.NewGatewayStore(db), Agents: agents, Engine: engine,
		Callback: sender, Events: postgres.NewChannelEventStore(db),
	})
	t.Cleanup(service.WaitBackground)
	router := apihttp.NewRouter(apihttp.RouterDeps{
		Dynamic: &apihttp.DynamicHandler{Service: service, MaxRequestBytes: 1 << 20},
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &dynFixture{server: server, service: service, engine: engine,
		secret: secret, suffix: suffix, callback: cb}
}

func (f *dynFixture) slug() string { return "dyn-" + f.suffix }

func (f *dynFixture) signV1(t *testing.T, slug, runID, idemKey string, body []byte, timestamp int64, nonce string) map[string]string {
	t.Helper()
	canonical := platform.AgentSignatureV1Canonical(slug,
		strconv.FormatInt(timestamp, 10), nonce, runID, idemKey, body)
	signature := platform.EncodeSignature(
		platform.SignBody([]byte(f.secret), []byte(canonical)), platform.EncodingHex)
	headers := map[string]string{
		"Content-Type":                "application/json",
		platform.HeaderAgentSignature: signature,
		platform.HeaderAgentTimestamp: strconv.FormatInt(timestamp, 10),
		platform.HeaderAgentNonce:     nonce,
	}
	if runID != "" {
		headers[platform.HeaderAgentRunID] = runID
	}
	if idemKey != "" {
		headers[platform.HeaderIdempotencyKeyAgent] = idemKey
	}
	return headers
}

func (f *dynFixture) post(t *testing.T, path string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := readAllLimitedT(t, resp)
	return resp, raw
}

func envelopeBody(runID, callbackURL string) []byte {
	callback := ""
	if callbackURL != "" {
		callback = fmt.Sprintf(`,"callbackUrl":%q`, callbackURL)
	}
	runPart := ""
	if runID != "" {
		runPart = fmt.Sprintf(`"runId":%q,`, runID)
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":"1.0",%s"agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST","templateInputData":[],"taskBrief":{"instruction":"go"}%s}`, runPart, callback))
}

func randomNonce() string { return crypto.RandomToken(16) }

func TestDynamicSignatureMatrix(t *testing.T) {
	fixture := setupDynamic(t, false)
	body := envelopeBody("", "")

	// 缺签名 → 401 + WWW-Authenticate。
	resp, raw := fixture.post(t, "/agent/"+fixture.slug(),
		map[string]string{"Content-Type": "application/json"}, body)
	_ = raw
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("missing signature = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// 有效 v1 签名 → 200 + 溯源头 + 信封。
	headers := fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix(), randomNonce())
	resp, raw = fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 200 {
		t.Fatalf("valid v1 = %d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-Agent-Slug") != fixture.slug() || resp.Header.Get("X-Agent-Run-Id") == "" {
		t.Errorf("trace headers missing: %+v", resp.Header)
	}
	if !bytes.Contains(raw, []byte("dyn-ok")) {
		t.Errorf("envelope = %s", raw)
	}

	// nonce 重放 → 401 replayed_request（重签时间戳也不行）。
	replayNonce := randomNonce()
	headers = fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix(), replayNonce)
	if resp, _ := fixture.post(t, "/agent/"+fixture.slug(), headers, body); resp.StatusCode != 200 {
		t.Fatalf("first use of nonce = %d", resp.StatusCode)
	}
	headers = fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix(), replayNonce)
	resp, raw = fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 401 || !bytes.Contains(raw, []byte("replayed_request")) {
		t.Errorf("nonce replay = %d %s", resp.StatusCode, raw)
	}

	// 时间窗外 → 401 stale_request。
	headers = fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix()-600, randomNonce())
	resp, raw = fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 401 || !bytes.Contains(raw, []byte("stale_request")) {
		t.Errorf("stale timestamp = %d %s", resp.StatusCode, raw)
	}

	// 篡改 body → 401 invalid_signature。
	headers = fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix(), randomNonce())
	resp, _ = fixture.post(t, "/agent/"+fixture.slug(), headers, []byte(`{"schemaVersion":"1.0","agentCode":"demo","templateInputData":[],"tampered":true}`))
	if resp.StatusCode != 401 {
		t.Errorf("tampered body = %d", resp.StatusCode)
	}

	// 旧 body-only 签名默认禁用 → 401 legacy_signature_disabled。
	legacySig := platform.EncodeSignature(platform.SignBody([]byte(fixture.secret), body), platform.EncodingHex)
	resp, raw = fixture.post(t, "/agent/"+fixture.slug(), map[string]string{
		"Content-Type": "application/json", platform.HeaderSignature: legacySig,
	}, body)
	if resp.StatusCode != 401 || !bytes.Contains(raw, []byte("legacy_signature_disabled")) {
		t.Errorf("legacy signature = %d %s", resp.StatusCode, raw)
	}

	// 未知 slug → 404。
	resp, _ = fixture.post(t, "/agent/ghost-"+fixture.suffix,
		fixture.signV1(t, "ghost-"+fixture.suffix, "", "", body, time.Now().Unix(), randomNonce()), body)
	if resp.StatusCode != 404 {
		t.Errorf("ghost agent = %d", resp.StatusCode)
	}
}

func TestDynamicLegacySignatureWhenEnabled(t *testing.T) {
	fixture := setupDynamic(t, true)
	body := envelopeBody("", "")
	legacySig := platform.EncodeSignature(platform.SignBody([]byte(fixture.secret), body), platform.EncodingHex)
	resp, raw := fixture.post(t, "/agent/"+fixture.slug(), map[string]string{
		"Content-Type": "application/json", platform.HeaderSignature: legacySig,
	}, body)
	if resp.StatusCode != 200 {
		t.Errorf("legacy allowed = %d %s", resp.StatusCode, raw)
	}
}

func TestDynamicIdempotencyReplayAndConflict(t *testing.T) {
	fixture := setupDynamic(t, false)
	body := envelopeBody("", "")
	key := "dyn-key-" + fixture.suffix

	headers := fixture.signV1(t, fixture.slug(), "", key, body, time.Now().Unix(), randomNonce())
	resp, first := fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 200 {
		t.Fatalf("first = %d %s", resp.StatusCode, first)
	}
	if resp.Header.Get("X-Idempotent-Replay") != "" {
		t.Error("first execution must not be a replay")
	}

	// 同键同体（新 nonce/时间戳）→ 缓存回放。
	headers = fixture.signV1(t, fixture.slug(), "", key, body, time.Now().Unix(), randomNonce())
	resp, replayed := fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay = %d header=%q", resp.StatusCode, resp.Header.Get("X-Idempotent-Replay"))
	}
	if !bytes.Equal(first, replayed) {
		t.Error("replayed body must be byte-identical")
	}
	fixture.engine.mu.Lock()
	calls := fixture.engine.calls
	fixture.engine.mu.Unlock()
	if calls != 1 {
		t.Errorf("engine calls = %d, replay must not re-execute", calls)
	}

	// 同键不同体 → 409 idempotency_conflict。
	mutated := bytes.Replace(body, []byte(`"instruction":"go"`), []byte(`"instruction":"stop"`), 1)
	headers = fixture.signV1(t, fixture.slug(), "", key, mutated, time.Now().Unix(), randomNonce())
	resp, raw := fixture.post(t, "/agent/"+fixture.slug(), headers, mutated)
	if resp.StatusCode != 409 || !bytes.Contains(raw, []byte("idempotency_conflict")) {
		t.Errorf("conflict = %d %s", resp.StatusCode, raw)
	}
}

func TestDynamicExplicitRunIDConflict(t *testing.T) {
	fixture := setupDynamic(t, false)
	runID := "dyn-run-" + fixture.suffix
	body := envelopeBody(runID, "")

	headers := fixture.signV1(t, fixture.slug(), runID, "k1-"+fixture.suffix, body, time.Now().Unix(), randomNonce())
	if resp, raw := fixture.post(t, "/agent/"+fixture.slug(), headers, body); resp.StatusCode != 200 {
		t.Fatalf("first = %d %s", resp.StatusCode, raw)
	}
	// 同 runId 换幂等键 → 409（run_id 不可复用）。
	headers = fixture.signV1(t, fixture.slug(), runID, "k2-"+fixture.suffix, body, time.Now().Unix(), randomNonce())
	resp, raw := fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 409 {
		t.Errorf("run id reuse = %d %s", resp.StatusCode, raw)
	}
	// 头体 runId 不一致 → 422 conflicting_run_id。
	other := envelopeBody("dyn-other-"+fixture.suffix, "")
	headers = fixture.signV1(t, fixture.slug(), runID, "k3-"+fixture.suffix, other, time.Now().Unix(), randomNonce())
	resp, raw = fixture.post(t, "/agent/"+fixture.slug(), headers, other)
	if resp.StatusCode != 422 || !bytes.Contains(raw, []byte("conflicting_run_id")) {
		t.Errorf("mismatched run id = %d %s", resp.StatusCode, raw)
	}
}

func TestDynamicAsyncCallbackFlow(t *testing.T) {
	fixture := setupDynamic(t, false)
	body := envelopeBody("", fixture.callback.server.URL+"/cb")

	headers := fixture.signV1(t, fixture.slug(), "", "", body, time.Now().Unix(), randomNonce())
	resp, raw := fixture.post(t, "/agent/"+fixture.slug(), headers, body)
	if resp.StatusCode != 202 {
		t.Fatalf("async accept = %d %s", resp.StatusCode, raw)
	}
	select {
	case delivered := <-fixture.callback.received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(delivered), &payload); err != nil {
			t.Fatalf("callback body: %v", err)
		}
		if payload["payload_info"] != "dyn-ok" || payload["contract_version"] != "V2" {
			t.Errorf("callback payload = %v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback never delivered")
	}
	fixture.service.WaitBackground()
}

func TestDynamicCapacityGate(t *testing.T) {
	fixture := setupDynamic(t, false)
	slug := "dyn-blocked-" + fixture.suffix
	block := make(chan struct{})
	fixture.engine.block = block
	var closeOnce sync.Once
	release := func() { closeOnce.Do(func() { close(block) }) }
	defer release()

	body := envelopeBody("", "")
	first := make(chan int, 1)
	go func() {
		headers := fixture.signV1(t, slug, "", "", body, time.Now().Unix(), randomNonce())
		resp, _ := fixture.post(t, "/agent/"+slug, headers, body)
		first <- resp.StatusCode
	}()
	// 等第一个请求真正进入执行（持有 执行闸+准入闸 各 1）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fixture.engine.mu.Lock()
		calls := fixture.engine.calls
		fixture.engine.mu.Unlock()
		if calls >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 第二个请求：准入闸还有 1（队列容量 1），会卡在执行闸等待——占住最后一个准入许可。
	second := make(chan int, 1)
	go func() {
		headers := fixture.signV1(t, slug, "", "", body, time.Now().Unix(), randomNonce())
		resp, _ := fixture.post(t, "/agent/"+slug, headers, body)
		second <- resp.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	// 第三个请求：准入闸满（并发 1 + 队列 1 均被占）→ 429 + Retry-After。
	headers := fixture.signV1(t, slug, "", "", body, time.Now().Unix(), randomNonce())
	resp, raw := fixture.post(t, "/agent/"+slug, headers, body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("overload = %d retry-after=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}
	if !strings.Contains(string(raw), "agent_overloaded") {
		t.Errorf("overload body = %s", raw)
	}
	// 放行阻塞的执行，回收两个在飞请求。
	release()
	<-second
	<-first
}
