package mota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const testSecret = "test-signing-secret-0123456789"

// pushTaskBody 生成一份能通过 validate_task 全量校验的任务信封。
//
// 生产协议要求 schemaVersion 恰好是 "1.0"、runId 与 X-MoTA-Run-ID 严格一致、
// agentCode / callbackUrl 非空、templateInputData 存在、taskBrief 是对象。
func pushTaskBody(runID string) string {
	return pushTaskBodyWith(runID, nil)
}

// pushTaskBodyWith 在合法信封基础上覆盖/删除字段(值为 nil 表示删除)。
func pushTaskBodyWith(runID string, overrides map[string]any) string {
	envelope := map[string]any{
		"schemaVersion": "1.0",
		"runId":         runID,
		"agentCode":     "agent-abc",
		"templateId":    "tpl-1",
		"templateInputData": []any{
			map[string]any{"name": "symbols", "value": []any{"AAPL", "MSFT"}},
			map[string]any{"name": "chart_period", "value": "1y"},
		},
		"taskBrief":   map[string]any{"userCustomInstructions": "assess AAPL and MSFT"},
		"callbackUrl": "https://cb.example/cb",
	}
	for key, value := range overrides {
		if value == nil {
			delete(envelope, key)
			continue
		}
		envelope[key] = value
	}
	return jsonString(envelope)
}

func testConfig() *Config {
	return &Config{
		PushEnabled:       true,
		PushPath:          "/mota/push",
		SigningSecret:     testSecret,
		SignatureEncoding: "hex",
		PushMode:          PushModeSync,
		CallbackToken:     "mpk_test_token",
		ClaimTimeout:      60 * time.Second,
		TaskTimeout:       600 * time.Second,
	}
}

func newPushServer(t *testing.T, cfg *Config, processor Processor) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Post(cfg.PushPath, NewPushHandler(cfg, processor))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// postPush 发一次 Push 请求。
//
// Content-Type、runId 与幂等键都是生产协议的硬性要求,所以这里默认补齐:
// runID 留空会自动生成(但那样 body 里的 runId 就对不上,只适合鉴权类用例),
// idemKey 留空则每次生成唯一键,避免用例之间互相命中幂等缓存。
func postPush(t *testing.T, baseURL string, body []byte, sig, runID, idemKey string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mota/push", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set(SignatureHeader, sig)
	}
	if runID == "" {
		runID = unique("run")
	}
	req.Header.Set(RunIDHeader, runID)
	if idemKey == "" {
		idemKey = unique("idem")
	}
	req.Header.Set(IdempotencyKeyHeader, idemKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func decodePayload(t *testing.T, raw []byte) *PayloadV1 {
	t.Helper()
	var env PayloadV1
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("bad envelope JSON: %v", err)
	}
	return &env
}

func payloadResult(t *testing.T, env *PayloadV1) map[string]any {
	t.Helper()
	m, ok := env.PayloadResult.(map[string]any)
	if !ok {
		t.Fatalf("payload_result is not an object: %v", env.PayloadResult)
	}
	return m
}

func assertEnvelope(t *testing.T, raw []byte, wantAssessments int) {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if len(top) != 4 {
		t.Fatalf("envelope must have exactly 4 top-level keys (three business keys + usage), got %d: %s", len(top), raw)
	}
	if _, ok := top["usage"]; !ok {
		t.Fatalf("envelope must carry usage (RULE-BND-001): %s", raw)
	}
	env := decodePayload(t, raw)
	if env.ContractVersion != "V2" {
		t.Fatalf("expected contract_version V2, got %q", env.ContractVersion)
	}
	// payload_info 就是 summary 的人读文本。Rust 侧刻意不再追加固定免责声明
	// (to_transport_payload 的 language 参数只为兼容保留),Go 侧与之一致。
	if env.PayloadInfo == "" {
		t.Fatalf("payload_info must not be empty: %s", raw)
	}
	if strings.Contains(env.PayloadInfo, "AI 生成的研究参考") {
		t.Fatalf("payload_info must not append a fixed disclaimer: %q", env.PayloadInfo)
	}
	result := payloadResult(t, env)
	assessments, ok := result["assessments"].([]any)
	if !ok {
		t.Fatalf("payload_result.assessments missing: %s", raw)
	}
	if len(assessments) != wantAssessments {
		t.Fatalf("expected %d assessments, got %d: %s", wantAssessments, len(assessments), raw)
	}
	for _, a := range assessments {
		am, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("assessment is not an object: %v", a)
		}
		if _, ok := am["reasoning"]; !ok {
			t.Fatalf("assessment missing reasoning: %v", a)
		}
	}
	// 占位处理器不得凭空生成 evidenceRefs:普通模板入参不是证据,不能伪装成
	// MCP_TOOL / UPSTREAM / DOC。只有拿到真实来源标识的实现才应填该字段。
	// 因此 evidence_refs 为空时,信封里根本不出现 evidenceRefs 键。
	if _, present := result["evidenceRefs"]; present {
		t.Fatalf("placeholder output must not fabricate evidenceRefs: %s", raw)
	}
}

func TestPushOKWithValidSignature(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	assertEnvelope(t, raw, 2)
}

func TestPushOKEmptyParams(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	// templateInputData 是必填字段,但可以是空数组 —— 此时没有 symbols 可评估。
	body := []byte(pushTaskBodyWith(runID, map[string]any{"templateInputData": []any{}}))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	assertEnvelope(t, raw, 0)
}

func TestPushRejectsMissingSignature(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	resp, _ := postPush(t, srv.URL, []byte(pushTaskBody(runID)), "", runID, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPushRejectsWrongSignature(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature("wrong-secret", body, "hex")
	resp, _ := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPushRejectsTamperedBody(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	original := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, original, "hex")
	tampered := strings.Replace(string(original), "AAPL", "TSLA", 1)
	resp, _ := postPush(t, srv.URL, []byte(tampered), sig, runID, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPushRejectsInvalidJSON(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	body := []byte("not-json")
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, "", "")
	// 生产协议把「不是 JSON」与「JSON 但信封不合法」分开:前者 400,后者 422。
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "urn:mota:invalid-json") {
		t.Fatalf("expected the invalid-json problem type, got %s", raw)
	}
}

func TestPushBase64SignatureAccepted(t *testing.T) {
	cfg := testConfig()
	cfg.SignatureEncoding = "base64"
	srv := newPushServer(t, cfg, PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "base64")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	assertEnvelope(t, raw, 2)
}

func TestPushCaseInsensitiveSignatureAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := strings.ToUpper(ComputeSignature(testSecret, body, "hex"))
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	assertEnvelope(t, raw, 2)
}

type failingProcessor struct{ msg string }

func (f failingProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	return nil, errors.New(f.msg)
}

func TestPushProcessorErrorIs500WithoutLeak(t *testing.T) {
	srv := newPushServer(t, testConfig(), failingProcessor{msg: "internal-db-credential: hunter2"})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "internal-db-credential") {
		t.Fatalf("internal details leaked in response: %s", raw)
	}
}

func TestPushConcurrentRequests(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("conc-%d-%d", os.Getpid(), i)
			body := []byte(pushTaskBody(runID))
			sig := ComputeSignature(testSecret, body, "hex")
			resp, raw := postPush(t, srv.URL, body, sig, runID, "")
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("request %d: expected 200, got %d: %s", i, resp.StatusCode, raw)
				return
			}
			env := decodePayload(t, raw)
			result := payloadResult(t, env)
			assessments := result["assessments"].([]any)
			if len(assessments) != 2 {
				errs <- fmt.Errorf("request %d: expected 2 assessments, got %d", i, len(assessments))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPushIdempotencyReplaysSyncResult(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run-idem")
	key := unique("key")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	resp1, raw1 := postPush(t, srv.URL, body, sig, runID, key)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on first push, got %d: %s", resp1.StatusCode, raw1)
	}
	resp2, raw2 := postPush(t, srv.URL, body, sig, runID, key)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d", resp2.StatusCode)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("idempotent replay must return byte-identical body")
	}
	assertEnvelope(t, raw2, 2)
}

type callbackCapture struct {
	mu     sync.Mutex
	items  []callbackItem
	status int
}

type callbackItem struct {
	runID string
	sig   string
	body  []byte
	token string
}

// newCallbackServer 模拟平台的 callback 端点。
//
// 200 时必须回一份合法回执({code, accepted, ...})—— SendCallback 会解析它,
// 空体会被判成 "callback returned an invalid receipt"。
func newCallbackServer(status int) (*httptest.Server, *callbackCapture) {
	c := &callbackCapture{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.items = append(c.items, callbackItem{
			runID: r.Header.Get(RunIDHeader),
			sig:   r.Header.Get(CreatorSignatureHeader),
			body:  raw,
			token: r.URL.Query().Get("token"),
		})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"code":"OK","accepted":true,"runStatus":"SUCCESS"}`))
		}
	}))
	return srv, c
}

// assertCallbackSignatureV2 校验 X-Creator-Signature 是 "t=<ts>,v2=<hmac>" 形式,
// 且 hmac 覆盖 canonical 串 "v2\n<ts>\n<runId>\n<body>"。
func assertCallbackSignatureV2(t *testing.T, secret, runID string, body []byte, signature string) {
	t.Helper()
	stamp, digest, found := strings.Cut(signature, ",")
	if !found {
		t.Fatalf("signature %q is not in t=...,v2=... form", signature)
	}
	timestampText, ok := strings.CutPrefix(stamp, "t=")
	if !ok {
		t.Fatalf("signature %q is missing the t= prefix", signature)
	}
	if _, ok := strings.CutPrefix(digest, "v2="); !ok {
		t.Fatalf("signature %q is missing the v2= prefix", signature)
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		t.Fatalf("signature timestamp %q is not an integer: %v", timestampText, err)
	}
	if want := CallbackSignatureV2(secret, runID, body, timestamp); want != signature {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
}

func (c *callbackCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *callbackCapture) first() *callbackItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) == 0 {
		return nil
	}
	item := c.items[0]
	return &item
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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

func TestPushAsyncReturns202AndSendsCallback(t *testing.T) {
	cbSrv, cb := newCallbackServer(http.StatusOK)
	cfg := testConfig()
	cfg.PushMode = PushModeAsync
	cfg.CallbackToken = "mpk_async_token"
	srv := newPushServer(t, cfg, PlaceholderProcessor{})

	runID := unique("run-async")
	body := []byte(pushTaskBodyWith(runID, map[string]any{"callbackUrl": cbSrv.URL}))
	sig := ComputeSignature(testSecret, body, "hex")

	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, raw)
	}
	if len(raw) != 0 {
		t.Fatalf("expected empty 202 body, got %s", raw)
	}

	waitForCondition(t, 5*time.Second, "callback delivery", func() bool { return cb.count() > 0 })

	item := cb.first()
	if item.token != "mpk_async_token" {
		t.Fatalf("expected mpk_ token in query, got %q", item.token)
	}
	if item.runID != runID {
		t.Fatalf("expected X-MoTA-Run-ID echoed, got %q", item.runID)
	}
	assertCallbackSignatureV2(t, testSecret, runID, item.body, item.sig)
	assertEnvelope(t, item.body, 2)
}

func TestPushAsyncIdempotencyReplays202(t *testing.T) {
	cbSrv, cb := newCallbackServer(http.StatusOK)
	cfg := testConfig()
	cfg.PushMode = PushModeAsync
	cfg.CallbackToken = "mpk_async_token"
	srv := newPushServer(t, cfg, PlaceholderProcessor{})

	runID := unique("run-async-idem")
	key := unique("idem-async")
	body := []byte(pushTaskBodyWith(runID, map[string]any{"callbackUrl": cbSrv.URL}))
	sig := ComputeSignature(testSecret, body, "hex")

	resp1, _ := postPush(t, srv.URL, body, sig, runID, key)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 on first push, got %d", resp1.StatusCode)
	}
	waitForCondition(t, 5*time.Second, "first callback", func() bool { return cb.count() >= 1 })

	resp2, raw2 := postPush(t, srv.URL, body, sig, runID, key)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 on replay, got %d: %s", resp2.StatusCode, raw2)
	}
	time.Sleep(300 * time.Millisecond)
	if got := cb.count(); got != 1 {
		t.Fatalf("expected exactly 1 callback after idempotent replay, got %d", got)
	}
}

func TestPushAsyncAcceptedWithoutCallbackToken(t *testing.T) {
	cfg := testConfig()
	cfg.PushMode = PushModeAsync
	cfg.CallbackToken = ""
	srv := newPushServer(t, cfg, PlaceholderProcessor{})
	runID := unique("run-async")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, _ := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 even without configured token, got %d", resp.StatusCode)
	}
}
