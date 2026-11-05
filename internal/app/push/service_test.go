package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 内存幂等假件（语义与 infra/postgres 实现同构：六分支 + 陈旧接管 + 租约归属）
// ---------------------------------------------------------------------------

type memRecord struct {
	runID, hash, status, body string
	updatedAt                 int64
}

type memIdempotency struct {
	mu      sync.Mutex
	records map[string]*memRecord
	byRun   map[string]string // runID → key（run_id 唯一防线）
}

func newMemIdempotency() *memIdempotency {
	return &memIdempotency{records: map[string]*memRecord{}, byRun: map[string]string{}}
}

func (m *memIdempotency) Begin(_ context.Context, key, runID, hash string) (*platform.IdempotencyDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	if rec, ok := m.records[key]; ok {
		switch {
		case rec.runID != runID || rec.hash != hash:
			return &platform.IdempotencyDecision{Kind: platform.IdemConflict}, nil
		case rec.status == platform.IdempotencySyncDone:
			if rec.body == "" {
				return &platform.IdempotencyDecision{Kind: platform.IdemConflict}, nil
			}
			return &platform.IdempotencyDecision{Kind: platform.IdemSyncCached, ResponseBody: rec.body}, nil
		case rec.status == platform.IdempotencySyncFailed:
			return &platform.IdempotencyDecision{Kind: platform.IdemSyncFailed, ResponseBody: rec.body}, nil
		case rec.status == platform.IdempotencyAsyncAccepted:
			return &platform.IdempotencyDecision{Kind: platform.IdemAsyncAccepted}, nil
		case rec.updatedAt <= now-int64(platform.PushLeaseStaleAfter/time.Second):
			rec.status, rec.body, rec.updatedAt = platform.IdempotencyProcessing, "", now
			return &platform.IdempotencyDecision{Kind: platform.IdemNew}, nil
		default:
			return &platform.IdempotencyDecision{Kind: platform.IdemInProgress}, nil
		}
	}
	if existingKey, ok := m.byRun[runID]; ok {
		rec := m.records[existingKey]
		if existingKey != key || rec.hash != hash {
			return &platform.IdempotencyDecision{Kind: platform.IdemConflict}, nil
		}
		return &platform.IdempotencyDecision{Kind: platform.IdemInProgress}, nil
	}
	m.records[key] = &memRecord{runID: runID, hash: hash, status: platform.IdempotencyProcessing, updatedAt: now}
	m.byRun[runID] = key
	return &platform.IdempotencyDecision{Kind: platform.IdemNew}, nil
}

func (m *memIdempotency) transition(key, runID, hash, status, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok || rec.runID != runID || rec.hash != hash {
		return platform.ErrPushLeaseLost
	}
	rec.status, rec.body, rec.updatedAt = status, body, time.Now().Unix()
	return nil
}

func (m *memIdempotency) CompleteSync(_ context.Context, key, runID, hash, body string) error {
	return m.transition(key, runID, hash, platform.IdempotencySyncDone, body)
}
func (m *memIdempotency) FailSync(_ context.Context, key, runID, hash, reason string) error {
	return m.transition(key, runID, hash, platform.IdempotencySyncFailed, reason)
}
func (m *memIdempotency) AcceptAsync(_ context.Context, key, runID, hash string) error {
	return m.transition(key, runID, hash, platform.IdempotencyAsyncAccepted, "")
}
func (m *memIdempotency) Release(_ context.Context, key, runID, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.records[key]; ok && rec.runID == runID && rec.hash == hash &&
		rec.status == platform.IdempotencyProcessing {
		delete(m.records, key)
		delete(m.byRun, runID)
	}
	return nil
}

func (m *memIdempotency) status(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.records[key]; ok {
		return rec.status
	}
	return "<absent>"
}

// ---------------------------------------------------------------------------
// 分发/回调假件与请求构造
// ---------------------------------------------------------------------------

type fakeDispatcher struct {
	output *platform.ProcessedOutput
	err    error
	block  time.Duration
	calls  int
	mu     sync.Mutex
}

func (f *fakeDispatcher) Execute(ctx context.Context, _ string, _ *platform.TaskContext,
	_ any, _ time.Duration) (*platform.ProcessedOutput, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.output, nil
}

type fakeSender struct {
	mu       sync.Mutex
	delivery []sentCallback
	err      error
}

type sentCallback struct {
	url, runID string
	payload    *platform.PayloadV1
}

func (f *fakeSender) Send(_ context.Context, url, runID string, payload *platform.PayloadV1) (*platform.CallbackReceipt, error) {
	f.mu.Lock()
	f.delivery = append(f.delivery, sentCallback{url: url, runID: runID, payload: payload})
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	status := "completed"
	return &platform.CallbackReceipt{Code: "OK", Accepted: true, RunStatus: &status}, nil
}

const testSecret = "push-test-secret"

func signedRequest(t *testing.T, body string, runID, key string) Request {
	t.Helper()
	sig := platform.EncodeSignature(platform.SignBody([]byte(testSecret), []byte(body)), platform.EncodingHex)
	return Request{
		ContentType:     "application/json; charset=utf-8",
		RunIDHeader:     []string{runID},
		IdempotencyKey:  []string{key},
		SignatureHeader: []string{sig},
		Body:            []byte(body),
	}
}

func envelope(runID string) string {
	return fmt.Sprintf(`{"schemaVersion":"1.0","runId":%q,"agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST",`+
		`"templateInputData":[],"taskBrief":{},"callbackUrl":"https://platform.example.com/callback"}`, runID)
}

func newTestService(t *testing.T, mode Mode, dispatcher Dispatcher, sender CallbackSender) (*Service, *memIdempotency) {
	t.Helper()
	store := newMemIdempotency()
	svc := NewService(Deps{
		Config: Config{
			SigningSecret: testSecret, SignatureEncoding: platform.EncodingHex,
			Mode: mode, TaskTimeout: 5 * time.Second,
		},
		Idempotency: store, Dispatcher: dispatcher, Callback: sender,
	})
	return svc, store
}

func wantRequestError(t *testing.T, err error, status int) *RequestError {
	t.Helper()
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected *RequestError, got %T: %v", err, err)
	}
	if reqErr.Status != status {
		t.Fatalf("status = %d, want %d (err: %v)", reqErr.Status, status, reqErr)
	}
	return reqErr
}

// ---------------------------------------------------------------------------
// 请求头与信封校验矩阵
// ---------------------------------------------------------------------------

func TestHeaderAndContentTypeMatrix(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "ok"}}
	svc, _ := newTestService(t, ModeSync, dispatcher, nil)
	ctx := context.Background()

	if _, err := svc.Handle(ctx, Request{ContentType: "text/plain",
		RunIDHeader: []string{"r"}, IdempotencyKey: []string{"k"}, SignatureHeader: []string{"s"},
		Body: []byte("{}")}); err == nil || wantRequestError(t, err, 415).Type != "urn:ir:unsupported-content-type" {
		t.Error("bad content type must be 415")
	}

	valid := signedRequest(t, envelope("run-1"), "run-1", "key-1")
	valid.RunIDHeader = []string{"a", "b"}
	if _, err := svc.Handle(ctx, valid); err == nil {
		t.Error("duplicated run id header must be rejected")
	} else {
		wantRequestError(t, err, 422)
	}

	valid = signedRequest(t, envelope("run-1"), "run-1", "key-1")
	valid.SignatureHeader = nil
	if _, err := svc.Handle(ctx, valid); err == nil {
		t.Error("missing signature must be 401")
	} else {
		wantRequestError(t, err, 401)
	}

	valid = signedRequest(t, envelope("run-1"), "run-1", "key-1")
	valid.SignatureHeader = []string{"a", "b"}
	if _, err := svc.Handle(ctx, valid); err == nil {
		t.Error("duplicated signature must be 422")
	} else {
		wantRequestError(t, err, 422)
	}

	valid = signedRequest(t, envelope("run-1"), " padded ", "key-1")
	if _, err := svc.Handle(ctx, valid); err == nil {
		t.Error("idempotency key with outer whitespace must be rejected")
	} else {
		wantRequestError(t, err, 422)
	}

	valid = signedRequest(t, envelope("run-1"), "run-1", "key-1")
	valid.SignatureHeader = []string{"deadbeef"}
	if _, err := svc.Handle(ctx, valid); err == nil {
		t.Error("wrong signature must be 401")
	} else {
		wantRequestError(t, err, 401)
	}
}

func TestEnvelopeValidationMatrix(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "ok"}}
	svc, _ := newTestService(t, ModeSync, dispatcher, nil)
	ctx := context.Background()

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"schemaVersion 必须为 1.0", `{"schemaVersion":"2.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[]}`, "schemaVersion"},
		{"runId 必须与头一致", `{"schemaVersion":"1.0","runId":"other","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[]}`, "runId"},
		{"agentCode 必填", `{"schemaVersion":"1.0","runId":"r","callbackUrl":"https://x.example/cb","templateInputData":[]}`, "agentCode"},
		{"callbackUrl 必填", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","templateInputData":[]}`, "callbackUrl"},
		{"positionType 白名单", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","positionType":"ASTROLOGER","callbackUrl":"https://x.example/cb","templateInputData":[]}`, "positionType"},
		{"templateInputData 必填", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb"}`, "templateInputData"},
		{"templateInputData 名字唯一", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[{"name":"x","value":1},{"name":"x","value":2}]}`, "templateInputData[1].name"},
		{"taskBrief 必须是对象", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"taskBrief":"text"}`, "taskBrief"},
		{"globalParameters 键白名单", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"globalParameters":{"foo":1}}`, "globalParameters.foo"},
		{"universeMode 枚举", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"globalParameters":{"universeMode":"WIDE"}}`, "globalParameters.universeMode"},
		{"symbolPool 非空字符串", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"globalParameters":{"symbolPool":[" "]}}`, "globalParameters.symbolPool"},
		{"upstreamPayload 需要 agentInstanceId", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"upstreamPayload":[{"payloadResult":null}]}`, "upstreamPayload[0].agentInstanceId"},
		{"upstream 边必须有 from/to", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"upstream":{"self":{},"nodes":[],"edges":[{"from":"a"}]}}`, "upstream.edges[0]"},
		{"runtimeContext 只允许 isFinal", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://x.example/cb","templateInputData":[],"runtimeContext":{"isFinal":true,"extra":1}}`, "runtimeContext.extra"},
		{"callbackUrl 必须 HTTPS 或回环", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"http://public.example/cb","templateInputData":[]}`, "callbackUrl"},
		{"callbackUrl 不带凭据", `{"schemaVersion":"1.0","runId":"r","agentCode":"a","callbackUrl":"https://user:pw@x.example/cb","templateInputData":[]}`, "callbackUrl"},
	}
	for index, tc := range cases {
		tc := tc
		key := fmt.Sprintf("k-matrix-%d", index)
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := signedRequest(t, tc.body, "r", key)
			_, err := svc.Handle(ctx, req)
			reqErr := wantRequestError(t, err, 422)
			found := false
			for _, field := range reqErr.Fields {
				if field.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("field %q missing in %+v", tc.field, reqErr.Fields)
			}
		})
	}

	// 非 JSON 与顶层非对象。
	badJSON := signedRequest(t, "{not json", "r", "k-bad")
	if _, err := svc.Handle(ctx, badJSON); err == nil || wantRequestError(t, err, 400).Type != "urn:ir:invalid-json" {
		t.Error("invalid JSON must be 400 urn:ir:invalid-json")
	}
	arrayBody := signedRequest(t, `[]`, "r", "k-arr")
	if _, err := svc.Handle(ctx, arrayBody); err == nil {
		t.Error("non-object envelope must be rejected")
	} else {
		wantRequestError(t, err, 422)
	}
}

// ---------------------------------------------------------------------------
// 同步形态
// ---------------------------------------------------------------------------

func TestSyncSuccessAndCachedReplay(t *testing.T) {
	t.Parallel()
	usage := platform.BoundRunUsage{SchemaVersion: platform.SchemaUsageV2, Status: "FINAL", PromptTokens: 3, CompletionTokens: 4}
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "done", Usage: &usage}}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	ctx := context.Background()
	req := signedRequest(t, envelope("run-sync-1"), "run-sync-1", "key-sync-1")

	resp, err := svc.Handle(ctx, req)
	if err != nil {
		t.Fatalf("sync handle: %v", err)
	}
	if resp.Status != 200 || !strings.Contains(resp.Body, "done") {
		t.Fatalf("resp = %+v", resp)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(resp.Body), &payload); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	if payload["contract_version"] != "V2" {
		t.Errorf("contract_version = %v", payload["contract_version"])
	}

	// 同键重放：不再执行，回放缓存体。
	replay, err := svc.Handle(ctx, req)
	if err != nil || replay.Status != 200 || replay.Body != resp.Body {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	dispatcher.mu.Lock()
	calls := dispatcher.calls
	dispatcher.mu.Unlock()
	if calls != 1 {
		t.Errorf("dispatcher calls = %d, replay must not re-execute", calls)
	}
	if got := store.status("key-sync-1"); got != platform.IdempotencySyncDone {
		t.Errorf("store status = %s", got)
	}
}

func TestSyncFailureReleasesLease(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{err: errors.New("boom")}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	req := signedRequest(t, envelope("run-fail"), "run-fail", "key-fail")

	if _, err := svc.Handle(context.Background(), req); err == nil {
		t.Fatal("dispatch failure must surface")
	} else {
		reqErr := wantRequestError(t, err, 500)
		if reqErr.Detail != "task processing failed" {
			t.Errorf("detail = %q", reqErr.Detail)
		}
	}
	if got := store.status("key-fail"); got != "<absent>" {
		t.Errorf("lease must be released, store status = %s", got)
	}
}

func TestSyncTimeoutIs504AndReleases(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{block: 500 * time.Millisecond}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	svc.config.TaskTimeout = 50 * time.Millisecond
	req := signedRequest(t, envelope("run-slow"), "run-slow", "key-slow")

	_, err := svc.Handle(context.Background(), req)
	reqErr := wantRequestError(t, err, 504)
	if reqErr.Type != "urn:ir:task-timeout" {
		t.Errorf("type = %q", reqErr.Type)
	}
	if got := store.status("key-slow"); got != "<absent>" {
		t.Errorf("lease must be released after timeout, status = %s", got)
	}
}

func TestSyncTrustedUsageMissingIsTerminal502(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "no usage"}}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	svc.config.TrustedUsageReporting = true
	req := signedRequest(t, envelope("run-nousage"), "run-nousage", "key-nousage")

	_, err := svc.Handle(context.Background(), req)
	reqErr := wantRequestError(t, err, 502)
	if reqErr.Type != "urn:ir:usage-unavailable" {
		t.Errorf("type = %q", reqErr.Type)
	}
	if got := store.status("key-nousage"); got != platform.IdempotencySyncFailed {
		t.Errorf("must be terminal sync_failed, got %s", got)
	}
	// 终态失败重放：不重新执行。
	_, err = svc.Handle(context.Background(), req)
	wantRequestError(t, err, 502)
	dispatcher.mu.Lock()
	calls := dispatcher.calls
	dispatcher.mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, terminal failure must not re-execute", calls)
	}
}

// ---------------------------------------------------------------------------
// 异步形态
// ---------------------------------------------------------------------------

func TestAsyncAcceptedThenCallback(t *testing.T) {
	t.Parallel()
	usage := platform.BoundRunUsage{SchemaVersion: platform.SchemaUsageV2, Status: "FINAL", PromptTokens: 1, CompletionTokens: 1}
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "async done", Usage: &usage}}
	sender := &fakeSender{}
	svc, store := newTestService(t, ModeAsync, dispatcher, sender)
	req := signedRequest(t, envelope("run-async"), "run-async", "key-async")

	resp, err := svc.Handle(context.Background(), req)
	if err != nil || resp.Status != 202 || resp.Body != "" {
		t.Fatalf("async resp = %+v err=%v", resp, err)
	}
	svc.WaitBackground()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.delivery) != 1 {
		t.Fatalf("deliveries = %d", len(sender.delivery))
	}
	sent := sender.delivery[0]
	if sent.runID != "run-async" || sent.url != "https://platform.example.com/callback" {
		t.Errorf("delivery = %+v", sent)
	}
	if sent.payload.PayloadInfo != "async done" {
		t.Errorf("payload info = %v", sent.payload.PayloadInfo)
	}
	// 受理重放。
	replay, err := svc.Handle(context.Background(), req)
	if err != nil || replay.Status != 202 {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	if got := store.status("key-async"); got != platform.IdempotencyAsyncAccepted {
		t.Errorf("store status = %s", got)
	}
}

func TestAsyncFailureSendsConservativePayload(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{err: errors.New("executor died")}
	sender := &fakeSender{}
	svc, _ := newTestService(t, ModeAsync, dispatcher, sender)
	req := signedRequest(t, envelope("run-cons"), "run-cons", "key-cons")

	if _, err := svc.Handle(context.Background(), req); err != nil {
		t.Fatalf("async accept: %v", err)
	}
	svc.WaitBackground()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.delivery) != 1 {
		t.Fatalf("deliveries = %d", len(sender.delivery))
	}
	result, ok := sender.delivery[0].payload.PayloadResult.(map[string]any)
	if !ok {
		t.Fatalf("payload result = %T", sender.delivery[0].payload.PayloadResult)
	}
	// FUNDAMENTAL_ANALYST 的保守结构是空 assessments。
	if assessments, present := result["assessments"]; !present || len(assessments.([]any)) != 0 {
		t.Errorf("conservative payload = %+v", result)
	}
}

func TestAsyncTrustedSuppressesCallback(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "no usage"}}
	sender := &fakeSender{}
	svc, store := newTestService(t, ModeAsync, dispatcher, sender)
	svc.config.TrustedUsageReporting = true
	req := signedRequest(t, envelope("run-trusted"), "run-trusted", "key-trusted")

	if _, err := svc.Handle(context.Background(), req); err != nil {
		t.Fatalf("async accept: %v", err)
	}
	svc.WaitBackground()
	sender.mu.Lock()
	deliveries := len(sender.delivery)
	sender.mu.Unlock()
	if deliveries != 0 {
		t.Errorf("trusted channel must suppress callback, deliveries = %d", deliveries)
	}
	// 受理保留（防重投），状态仍是 async_accepted。
	if got := store.status("key-trusted"); got != platform.IdempotencyAsyncAccepted {
		t.Errorf("store status = %s", got)
	}
}

// ---------------------------------------------------------------------------
// 幂等分支
// ---------------------------------------------------------------------------

func TestIdempotencyConflictAndInProgress(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "ok"}}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	ctx := context.Background()

	// 同键不同请求体 → 409 conflict。
	first := signedRequest(t, envelope("run-c1"), "run-c1", "key-c")
	if _, err := svc.Handle(ctx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := signedRequest(t, envelope("run-c1"), "run-c1", "key-c")
	second.Body = []byte(strings.Replace(string(second.Body), `"agentCode":"demo"`, `"agentCode":"other"`, 1))
	second.SignatureHeader = []string{platform.EncodeSignature(
		platform.SignBody([]byte(testSecret), second.Body), platform.EncodingHex)}
	if _, err := svc.Handle(ctx, second); err == nil {
		t.Error("same key different body must conflict")
	} else {
		wantRequestError(t, err, 409)
	}

	// 同 runId 不同键 → 409（run_id 唯一防线）。
	third := signedRequest(t, envelope("run-c1"), "run-c1", "key-other")
	if _, err := svc.Handle(ctx, third); err == nil {
		t.Error("same run under different key must conflict")
	} else {
		wantRequestError(t, err, 409)
	}
	_ = store
}

func TestStaleProcessingIsTakenOver(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeDispatcher{output: &platform.ProcessedOutput{Summary: "ok"}}
	svc, store := newTestService(t, ModeSync, dispatcher, nil)
	ctx := context.Background()

	// 手工放置一个陈旧的 processing 记录。
	store.records["key-stale"] = &memRecord{
		runID: "run-stale", hash: "hash-will-not-match-but-takeover-checks-first",
		status:    platform.IdempotencyProcessing,
		updatedAt: time.Now().Unix() - int64(platform.PushLeaseStaleAfter/time.Second) - 60,
	}
	// hash 不一致优先判 Conflict（决策优先级 1 高于陈旧接管）。
	req := signedRequest(t, envelope("run-stale"), "run-stale", "key-stale")
	if _, err := svc.Handle(ctx, req); err == nil {
		t.Error("hash mismatch must conflict even when stale")
	} else {
		wantRequestError(t, err, 409)
	}
	// hash 一致的陈旧记录 → 接管执行。
	body := envelope("run-stale2")
	hash := fmt.Sprintf("%x", sha256Sum([]byte(body)))
	store.records["key-stale2"] = &memRecord{
		runID: "run-stale2", hash: hash, status: platform.IdempotencyProcessing,
		updatedAt: time.Now().Unix() - int64(platform.PushLeaseStaleAfter/time.Second) - 60,
	}
	store.byRun["run-stale2"] = "key-stale2"
	resp, err := svc.Handle(ctx, signedRequest(t, body, "run-stale2", "key-stale2"))
	if err != nil || resp.Status != 200 {
		t.Fatalf("stale takeover = %+v err=%v", resp, err)
	}
}
