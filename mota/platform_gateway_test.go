package mota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 纯函数:状态映射与请求头校验
// ---------------------------------------------------------------------------

func TestExecutionErrorStatusMapping(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"execution_timeout":       http.StatusGatewayTimeout,
		"executor_unavailable":    http.StatusServiceUnavailable,
		"agent_disabled":          http.StatusServiceUnavailable,
		"agent_not_found":         http.StatusServiceUnavailable,
		"agent_overloaded":        http.StatusTooManyRequests,
		"output_too_large":        http.StatusBadGateway,
		"input_too_large":         http.StatusBadGateway,
		"invalid_executor_output": http.StatusBadGateway,
		"usage_unavailable":       http.StatusBadGateway,
		"executor_failed":         http.StatusInternalServerError,
		"processor_failed":        http.StatusInternalServerError,
		"":                        http.StatusInternalServerError,
		"something_new":           http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := executionErrorStatus(code); got != want {
			t.Errorf("executionErrorStatus(%q) = %d, want %d", code, got, want)
		}
	}
}

// 两个等价别名合计只允许出现一次。
func TestHeaderOnceRejectsDuplicatesAcrossAliases(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Add("X-Agent-Run-Id", "a")
	if value, ok := headerOnce(h, runIDHeaders...); !ok || value != "a" {
		t.Fatalf("single header => (%q, %v)", value, ok)
	}
	// 同一个名字出现两次。
	h.Add("X-Agent-Run-Id", "b")
	if _, ok := headerOnce(h, runIDHeaders...); ok {
		t.Error("a repeated header must be rejected")
	}
	// 两个别名各出现一次也算重复。
	h2 := http.Header{}
	h2.Add("X-Agent-Run-Id", "a")
	h2.Add("X-MoTA-Run-Id", "b")
	if _, ok := headerOnce(h2, runIDHeaders...); ok {
		t.Error("two equivalent aliases must be rejected together")
	}
	// 缺失时返回空值且 ok=true。
	if value, ok := headerOnce(http.Header{}, runIDHeaders...); !ok || value != "" {
		t.Errorf("missing header => (%q, %v), want (\"\", true)", value, ok)
	}
	// 非 ASCII / 控制字符被拒。
	for _, bad := range []string{"caf\u00e9", "line\x01break"} {
		h3 := http.Header{}
		h3["X-Agent-Run-Id"] = []string{bad}
		if _, ok := headerOnce(h3, runIDHeaders...); ok {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestMetadataValid(t *testing.T) {
	t.Parallel()
	if !metadataValid("run-1", 160) {
		t.Error("a plain value must be accepted")
	}
	if !metadataValid(strings.Repeat("a", 160), 160) {
		t.Error("a value at the limit must be accepted")
	}
	for _, bad := range []string{
		"", " leading", "trailing ", "\tvalue", strings.Repeat("a", 161),
		"caf\u00e9", "with\x00null",
	} {
		if metadataValid(bad, 160) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestValidRequestNonce(t *testing.T) {
	t.Parallel()
	if !validRequestNonce(strings.Repeat("a", 16)) {
		t.Error("16 characters must be accepted")
	}
	if !validRequestNonce(strings.Repeat("a", 128)) {
		t.Error("128 characters must be accepted")
	}
	if !validRequestNonce("abcDEF012345-_xyz") {
		t.Error("URL-safe characters must be accepted")
	}
	for _, bad := range []string{
		strings.Repeat("a", 15), strings.Repeat("a", 129),
		"has spaces here!!", "has+plus+chars!!", "has/slash/chars!",
	} {
		if validRequestNonce(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestSerializedError(t *testing.T) {
	t.Parallel()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(serializedError("bad_thing", "it broke")), &decoded); err != nil {
		t.Fatalf("serializedError did not produce JSON: %v", err)
	}
	inner := decoded["error"].(map[string]any)
	if inner["code"] != "bad_thing" || inner["message"] != "it broke" {
		t.Errorf("error envelope = %#v", inner)
	}
}

// ---------------------------------------------------------------------------
// 端到端脚手架
// ---------------------------------------------------------------------------

const gatewayTestSecret = "gateway-test-secret-0123456789"

// gatewayFixture 是一套带真实 PG 存储与已注册 Agent 的网关测试环境。
type gatewayFixture struct {
	state  *PlatformState
	slug   string
	secret string
}

// newGatewayFixture 建一个 Agent 并把它注册进 Registry。
//
// 默认执行器是进程内 Processor(rust),不 spawn 子进程;调用方可以改 runtime 配置。
func newGatewayFixture(t *testing.T, processor Processor) *gatewayFixture {
	t.Helper()
	requireTestDatabase(t)

	cfg := testPlatformConfig(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg.WorkspaceRoot = root

	state, err := NewPlatformState(context.Background(), cfg, processor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })

	slug := uniqueSlug("gw")
	secret := gatewayTestSecret
	agent, err := state.Store.CreateAgent(context.Background(), &AgentInput{
		Slug: slug, Name: "gateway agent", ExecutorKind: ExecutorKindRust,
		ExecutionMode: ExecutionModeLocal, SigningSecret: &secret,
		SignatureEncoding: "hex", WorkspacePath: "agents",
		PermissionMode: "dontAsk",
		TimeoutSecs:    uint64Ptr(30),
		MaxConcurrency: intPtr(2),
		QueueCapacity:  intPtr(4),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = state.Store.DeleteAgent(context.Background(), slug) })
	state.Registry.Replace(agent)

	return &gatewayFixture{state: state, slug: slug, secret: secret}
}

// runtime 返回当前注册的运行配置。
func (f *gatewayFixture) runtime(t *testing.T) *AgentRuntime {
	t.Helper()
	runtime := f.state.Registry.Get(f.slug)
	if runtime == nil {
		t.Fatalf("agent %s is not registered", f.slug)
	}
	return runtime
}

// gatewayCall 描述一次网关请求。
type gatewayCall struct {
	body []byte
	// signWith 为空时用 fixture 的密钥;设成别的值可以模拟签名不匹配。
	signWith string
	// mode 决定签名方式:"v1"(默认)、"mota"、"none"。
	mode           string
	runID          string
	idempotencyKey string
	timestamp      string
	nonce          string
	// extraHeaders 在签名之后附加,用于构造重复头等异常。
	extraHeaders map[string][]string
	// omitTimestamp / omitNonce 用于构造 v1 缺字段的场景。
	omitTimestamp bool
	omitNonce     bool
}

// do 发一次请求并返回响应记录器。
func (f *gatewayFixture) do(t *testing.T, call gatewayCall) *httptest.ResponseRecorder {
	t.Helper()
	if call.mode == "" {
		call.mode = "v1"
	}
	secret := call.signWith
	if secret == "" {
		secret = f.secret
	}
	request := httptest.NewRequest(http.MethodPost, "/agent/"+f.slug, strings.NewReader(string(call.body)))
	if call.runID != "" {
		request.Header.Set("X-Agent-Run-Id", call.runID)
	}
	if call.idempotencyKey != "" {
		request.Header.Set("X-Idempotency-Key", call.idempotencyKey)
	}

	switch call.mode {
	case "v1":
		timestamp := call.timestamp
		if timestamp == "" {
			timestamp = strconv.FormatInt(time.Now().Unix(), 10)
		}
		nonce := call.nonce
		if nonce == "" {
			nonce = strings.ReplaceAll(unique("nonce"), "-", "_")
		}
		if !call.omitTimestamp {
			request.Header.Set("X-Agent-Timestamp", timestamp)
		}
		if !call.omitNonce {
			request.Header.Set("X-Agent-Nonce", nonce)
		}
		canonical := "v1\nPOST\n/agent/" + f.slug + "\n" + timestamp + "\n" + nonce + "\n" +
			call.runID + "\n" + call.idempotencyKey + "\n" + bodyHash(call.body)
		request.Header.Set("X-Agent-Signature",
			ComputeSignature(secret, []byte(canonical), SignatureEncodingHex))
	case "mota":
		request.Header.Set("X-MoTA-Signature",
			ComputeSignature(secret, call.body, SignatureEncodingHex))
	case "none":
	}
	for name, values := range call.extraHeaders {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	recorder := httptest.NewRecorder()
	f.state.Gateway(recorder, request)
	return recorder
}

// wantGatewayError 断言响应是指定状态与错误码,并返回 message。
func wantGatewayError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, status, recorder.Body.String())
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not an error envelope: %v (body: %s)", err, recorder.Body.String())
	}
	if decoded.Error.Code != code {
		t.Fatalf("code = %q, want %q (body: %s)", decoded.Error.Code, code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
	}
	return decoded.Error.Message
}

// minimalTaskBody 是一份能通过校验的最小任务体。
func minimalTaskBody(runID string) []byte {
	return []byte(`{"runId":"` + runID + `","params":{"symbols":["AAPL.US"]}}`)
}

// intPtr 便于填 AgentInput 的可选整数字段。
func intPtr(value int) *int { return &value }

// ---------------------------------------------------------------------------
// 端到端:Agent 查找与请求头
// ---------------------------------------------------------------------------

func TestGatewayUnknownAndDisabledAgentBothLookAbsent(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})

	// 未注册的 slug。
	request := httptest.NewRequest(http.MethodPost, "/agent/definitely-missing", strings.NewReader("{}"))
	recorder := httptest.NewRecorder()
	fixture.state.Gateway(recorder, request)
	if message := wantGatewayError(t, recorder, http.StatusNotFound, "agent_not_found"); message != "Agent not found" {
		t.Errorf("message = %q", message)
	}

	// 停用的 Agent 也报 404(不泄露它的存在)。
	disabled := false
	updated, err := fixture.state.Store.UpdateAgent(context.Background(), fixture.slug,
		&AgentInput{Slug: fixture.slug, Name: "gateway agent", Enabled: &disabled,
			ExecutorKind: ExecutorKindRust, ExecutionMode: ExecutionModeLocal,
			SignatureEncoding: "hex", WorkspacePath: "agents", PermissionMode: "dontAsk",
			TimeoutSecs: uint64Ptr(30), MaxConcurrency: intPtr(2), QueueCapacity: intPtr(4)})
	if err != nil {
		t.Fatalf("disable agent: %v", err)
	}
	fixture.state.Registry.Replace(updated)
	recorder = fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	wantGatewayError(t, recorder, http.StatusNotFound, "agent_not_found")
}

func TestGatewayRejectsDuplicateAndAmbiguousHeaders(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	body := minimalTaskBody(unique("run"))

	cases := []struct {
		name   string
		call   gatewayCall
		detail string
	}{
		{
			"run ID 两个别名同时出现",
			gatewayCall{body: body, extraHeaders: map[string][]string{"X-MoTA-Run-Id": {"other"}}, runID: "primary"},
			"run ID header must appear once and contain valid ASCII",
		},
		{
			"幂等键重复",
			gatewayCall{body: body, extraHeaders: map[string][]string{"X-Idempotency-Key": {"a", "b"}}},
			"idempotency key header must appear once and contain valid ASCII",
		},
		{
			"Agent 签名重复",
			gatewayCall{body: body, extraHeaders: map[string][]string{"X-Agent-Signature": {"second"}}},
			"X-Agent-Signature header must appear once and contain valid ASCII",
		},
		{
			"MoTA 签名重复",
			gatewayCall{body: body, mode: "mota",
				extraHeaders: map[string][]string{"X-MoTA-Signature": {"second"}}},
			"X-MoTA-Signature header must appear once and contain valid ASCII",
		},
	}
	for _, item := range cases {
		recorder := fixture.do(t, item.call)
		message := wantGatewayError(t, recorder, http.StatusBadRequest, "invalid_header")
		if message != item.detail {
			t.Errorf("%s: message = %q, want %q", item.name, message, item.detail)
		}
	}

	// 两种签名同时出现。
	recorder := fixture.do(t, gatewayCall{body: body,
		extraHeaders: map[string][]string{"X-MoTA-Signature": {"whatever"}}})
	message := wantGatewayError(t, recorder, http.StatusBadRequest, "ambiguous_signature")
	if message != "Send exactly one Agent signature header" {
		t.Errorf("message = %q", message)
	}
}

// ---------------------------------------------------------------------------
// 端到端:签名鉴权
// ---------------------------------------------------------------------------

func TestGatewayRejectsMissingSignature(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run")), mode: "none"})
	message := wantGatewayError(t, recorder, http.StatusUnauthorized, "missing_signature")
	if message != "Missing X-Agent-Signature or X-MoTA-Signature header" {
		t.Errorf("message = %q", message)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != `HMAC realm="agent"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestGatewayV1SignatureRequiresTimestampAndNonce(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	body := minimalTaskBody(unique("run"))

	recorder := fixture.do(t, gatewayCall{body: body, omitTimestamp: true})
	message := wantGatewayError(t, recorder, http.StatusUnauthorized, "missing_timestamp")
	if message != "X-Agent-Timestamp is required for X-Agent-Signature" {
		t.Errorf("message = %q", message)
	}
	if recorder.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing_timestamp must carry WWW-Authenticate")
	}

	recorder = fixture.do(t, gatewayCall{body: body, omitNonce: true})
	message = wantGatewayError(t, recorder, http.StatusUnauthorized, "missing_nonce")
	if message != "X-Agent-Nonce is required for X-Agent-Signature" {
		t.Errorf("message = %q", message)
	}
}

func TestGatewayV1SignatureValidatesTimestampAndNonce(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	body := minimalTaskBody(unique("run"))

	// 非数字时间戳。
	recorder := fixture.do(t, gatewayCall{body: body, timestamp: "not-a-number"})
	message := wantGatewayError(t, recorder, http.StatusUnauthorized, "invalid_timestamp")
	if message != "X-Agent-Timestamp must be Unix seconds" {
		t.Errorf("message = %q", message)
	}

	// 超出 5 分钟窗口(两个方向都验)。
	for _, offset := range []int64{-301, 301} {
		stamp := strconv.FormatInt(time.Now().Unix()+offset, 10)
		recorder := fixture.do(t, gatewayCall{body: body, timestamp: stamp})
		message := wantGatewayError(t, recorder, http.StatusUnauthorized, "stale_request")
		if message != "Signed request timestamp is outside the 5 minute window" {
			t.Errorf("offset %d: message = %q", offset, message)
		}
	}
	// 窗口边界(正好 300 秒)放行到后续校验,不再是 stale。
	stamp := strconv.FormatInt(time.Now().Unix()-300, 10)
	recorder = fixture.do(t, gatewayCall{body: body, timestamp: stamp, runID: unique("run")})
	if recorder.Code == http.StatusUnauthorized &&
		strings.Contains(recorder.Body.String(), "stale_request") {
		t.Error("a timestamp exactly at the window edge must be accepted")
	}

	// nonce 长度与字符集。
	for _, nonce := range []string{strings.Repeat("a", 15), "has spaces here!!"} {
		recorder := fixture.do(t, gatewayCall{body: body, nonce: nonce})
		message := wantGatewayError(t, recorder, http.StatusUnauthorized, "invalid_nonce")
		if message != "X-Agent-Nonce must contain 16-128 URL-safe characters" {
			t.Errorf("nonce %q: message = %q", nonce, message)
		}
	}
}

func TestGatewayRejectsSignatureMismatchOnBothPaths(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	body := minimalTaskBody(unique("run"))
	for _, mode := range []string{"v1", "mota"} {
		recorder := fixture.do(t, gatewayCall{body: body, mode: mode, signWith: "wrong-secret"})
		message := wantGatewayError(t, recorder, http.StatusUnauthorized, "invalid_signature")
		if message != "Request signature is invalid" {
			t.Errorf("mode %s: message = %q", mode, message)
		}
		if recorder.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("mode %s: invalid_signature must carry WWW-Authenticate", mode)
		}
	}
}

// v1 的 canonical 覆盖 runId 与幂等键:请求头改了但签名没重算就必须被拒。
func TestGatewayV1SignatureCoversRunIDAndIdempotencyKey(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	body := minimalTaskBody("")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := strings.ReplaceAll(unique("nonce"), "-", "_")

	// 用空 runId / 空幂等键签名,却在请求头里带上值。
	canonical := "v1\nPOST\n/agent/" + fixture.slug + "\n" + timestamp + "\n" + nonce + "\n\n\n" + bodyHash(body)
	request := httptest.NewRequest(http.MethodPost, "/agent/"+fixture.slug, strings.NewReader(string(body)))
	request.Header.Set("X-Agent-Timestamp", timestamp)
	request.Header.Set("X-Agent-Nonce", nonce)
	request.Header.Set("X-Agent-Run-Id", unique("run"))
	request.Header.Set("X-Agent-Signature",
		ComputeSignature(fixture.secret, []byte(canonical), SignatureEncodingHex))
	recorder := httptest.NewRecorder()
	fixture.state.Gateway(recorder, request)
	wantGatewayError(t, recorder, http.StatusUnauthorized, "invalid_signature")
}

// nonce 一次性:同一个 nonce 第二次必须判重放。
func TestGatewayRejectsReplayedNonce(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	nonce := strings.ReplaceAll(unique("nonce"), "-", "_")
	body := minimalTaskBody(unique("run"))

	first := fixture.do(t, gatewayCall{body: body, nonce: nonce})
	if first.Code != http.StatusOK {
		t.Fatalf("first call status = %d body = %s", first.Code, first.Body.String())
	}
	// 换 runId 但复用 nonce(否则会先撞 run_id_conflict)。
	second := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run")), nonce: nonce})
	message := wantGatewayError(t, second, http.StatusUnauthorized, "replayed_request")
	if message != "This signed nonce has already been used" {
		t.Errorf("message = %q", message)
	}
}

// MoTA 兼容路径只签原始 body,不需要时间戳与 nonce。
func TestGatewayMotaCompatibleSignatureNeedsNoNonce(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run")), mode: "mota"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 端到端:信封校验
// ---------------------------------------------------------------------------

func TestGatewayRejectsMalformedBodies(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})

	// 不是 JSON → 400 invalid_json。
	recorder := fixture.do(t, gatewayCall{body: []byte(`{not json`)})
	message := wantGatewayError(t, recorder, http.StatusBadRequest, "invalid_json")
	if message != "Request body is not valid JSON" {
		t.Errorf("message = %q", message)
	}

	// 是合法 JSON 但不是对象 → 422 invalid_body。
	for _, body := range []string{`[]`, `"text"`, `42`, `true`, `null`} {
		recorder := fixture.do(t, gatewayCall{body: []byte(body)})
		message := wantGatewayError(t, recorder, http.StatusUnprocessableEntity, "invalid_body")
		if message != "Request body must be a JSON object" {
			t.Errorf("body %s: message = %q", body, message)
		}
	}
}

// 已知信封字段类型不符 → 422 invalid_task_envelope。
func TestGatewayRejectsInvalidTaskEnvelope(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	cases := []string{
		`{"runId":123}`,
		`{"agentCode":[]}`,
		`{"positionType":{"a":1}}`,
		`{"callbackUrl":42}`,
		`{"templateInputData":"not an array"}`,
		`{"templateInputData":[{"name":123,"value":1}]}`,
		`{"schemaVersion":true}`,
	}
	for _, body := range cases {
		recorder := fixture.do(t, gatewayCall{body: []byte(body)})
		message := wantGatewayError(t, recorder, http.StatusUnprocessableEntity, "invalid_task_envelope")
		if message != "Known task envelope fields have invalid types" {
			t.Errorf("body %s: message = %q", body, message)
		}
	}
	// 未知字段不受限(宽松信封),类型正确的已知字段照常通过。
	recorder := fixture.do(t, gatewayCall{
		body: []byte(`{"runId":"` + unique("run") + `","brandNewField":{"x":1}}`)})
	if recorder.Code != http.StatusOK {
		t.Errorf("unknown fields must be tolerated, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// 请求头与 body 的 runId 不一致 → 422。
func TestGatewayRejectsConflictingRunID(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	headerRunID := unique("header-run")
	body := minimalTaskBody(unique("body-run"))
	recorder := fixture.do(t, gatewayCall{body: body, runID: headerRunID})
	message := wantGatewayError(t, recorder, http.StatusUnprocessableEntity, "conflicting_run_id")
	if message != "Run ID in the header and request body must match" {
		t.Errorf("message = %q", message)
	}

	// 完全一致时放行。
	runID := unique("same-run")
	recorder = fixture.do(t, gatewayCall{body: minimalTaskBody(runID), runID: runID})
	if recorder.Code != http.StatusOK {
		t.Errorf("matching run IDs must be accepted, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayValidatesRunIDAndIdempotencyKeyShape(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})

	// runId 形状非法(来自 body,避免请求头 ASCII 检查先拦下)。
	for _, runID := range []string{" leading", "trailing ", strings.Repeat("r", 161)} {
		body := []byte(`{"runId":` + jsonString(runID) + `}`)
		recorder := fixture.do(t, gatewayCall{body: body})
		message := wantGatewayError(t, recorder, http.StatusUnprocessableEntity, "invalid_run_id")
		if message != "Run ID must contain 1-160 printable ASCII characters without outer whitespace" {
			t.Errorf("runId %q: message = %q", runID, message)
		}
	}

	// 幂等键形状非法。
	for _, key := range []string{" leading", strings.Repeat("k", 201)} {
		recorder := fixture.do(t, gatewayCall{
			body: minimalTaskBody(unique("run")), idempotencyKey: key})
		message := wantGatewayError(t, recorder, http.StatusUnprocessableEntity, "invalid_idempotency_key")
		if message != "Idempotency key must contain 1-200 printable ASCII characters without outer whitespace" {
			t.Errorf("key %q: message = %q", key, message)
		}
	}

	// 都没给时自动生成 run_ 前缀的 runId。
	recorder := fixture.do(t, gatewayCall{body: []byte(`{"params":{}}`)})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	generated := recorder.Header().Get("X-Agent-Run-Id")
	if !strings.HasPrefix(generated, "run_") || len(generated) <= len("run_") {
		t.Errorf("generated run ID = %q, want a run_ prefixed token", generated)
	}
}

func TestGatewayRejectsOversizedBody(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	fixture.state.Config.MaxRequestBodyBytes = 64
	body := []byte(`{"runId":"` + unique("run") + `","pad":"` + strings.Repeat("x", 200) + `"}`)
	recorder := fixture.do(t, gatewayCall{body: body})
	message := wantGatewayError(t, recorder, http.StatusRequestEntityTooLarge, "body_too_large")
	if message != "Request body exceeded configured limit" {
		t.Errorf("message = %q", message)
	}
}

// ---------------------------------------------------------------------------
// 端到端:成功响应与运行记录
// ---------------------------------------------------------------------------

func TestGatewaySuccessResponseHeadersAndRunRecord(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runID := unique("run")
	body := minimalTaskBody(runID)
	recorder := fixture.do(t, gatewayCall{body: body, runID: runID})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	runtime := fixture.runtime(t)
	if got := recorder.Header().Get("X-Agent-Slug"); got != fixture.slug {
		t.Errorf("X-Agent-Slug = %q", got)
	}
	if got := recorder.Header().Get("X-Agent-Revision"); got != strconv.FormatInt(runtime.Config.Revision, 10) {
		t.Errorf("X-Agent-Revision = %q", got)
	}
	if got := recorder.Header().Get("X-Agent-Run-Id"); got != runID {
		t.Errorf("X-Agent-Run-Id = %q", got)
	}
	if got := recorder.Header().Get("X-Idempotent-Replay"); got != "" {
		t.Errorf("a fresh run must not be marked as a replay, got %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != jsonContentType {
		t.Errorf("Content-Type = %q", got)
	}

	// 响应体是 mota-agent-payload v1 三键信封,且**不含** usage(非受信通道)。
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	for _, key := range []string{"payload_result", "payload_info", "contract_version"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("response is missing %q", key)
		}
	}
	if _, leaked := payload["usage"]; leaked {
		t.Error("an untrusted channel must not report usage")
	}

	// 运行记录落库:原文与结果都要在。
	runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var found bool
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		found = true
		if run.Status != "completed" {
			t.Errorf("status = %q, want completed", run.Status)
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusOK {
			t.Errorf("response_code = %v, want 200", run.ResponseCode)
		}
		if run.Input == nil || !strings.Contains(*run.Input, runID) {
			t.Error("request_json must persist the original body")
		}
		if run.Output == nil || !strings.Contains(*run.Output, "contract_version") {
			t.Error("result_json must persist the response envelope")
		}
	}
	if !found {
		t.Fatalf("run %s was not recorded", runID)
	}
}

// 同一个 runId 第二次必须判冲突。
func TestGatewayRejectsReusedRunID(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runID := unique("run")
	body := minimalTaskBody(runID)
	if first := fixture.do(t, gatewayCall{body: body, runID: runID}); first.Code != http.StatusOK {
		t.Fatalf("first call status = %d", first.Code)
	}
	second := fixture.do(t, gatewayCall{body: body, runID: runID})
	message := wantGatewayError(t, second, http.StatusConflict, "run_id_conflict")
	if message != "This Agent run ID has already been used" {
		t.Errorf("message = %q", message)
	}
}

// ---------------------------------------------------------------------------
// 端到端:幂等
// ---------------------------------------------------------------------------

// 同一幂等键重放要回放原响应,并带 X-Idempotent-Replay 与原始 runId。
func TestGatewayReplaysCachedResponse(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runID := unique("run")
	key := unique("idem")
	body := minimalTaskBody(runID)

	first := fixture.do(t, gatewayCall{body: body, runID: runID, idempotencyKey: key})
	if first.Code != http.StatusOK {
		t.Fatalf("first call status = %d body = %s", first.Code, first.Body.String())
	}
	second := fixture.do(t, gatewayCall{body: body, runID: runID, idempotencyKey: key})
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d body = %s", second.Code, second.Body.String())
	}
	if second.Header().Get("X-Idempotent-Replay") != "true" {
		t.Error("a cached replay must set X-Idempotent-Replay")
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replay body diverged:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if got := second.Header().Get("X-Agent-Run-Id"); got != runID {
		t.Errorf("replay run ID = %q, want the original %q", got, runID)
	}
	if got := second.Header().Get("X-Agent-Slug"); got != fixture.slug {
		t.Errorf("replay slug = %q", got)
	}
}

// 同一幂等键配不同请求体 → 409 idempotency_conflict。
func TestGatewayDetectsIdempotencyConflict(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	key := unique("idem")
	if first := fixture.do(t, gatewayCall{
		body: minimalTaskBody(unique("run")), idempotencyKey: key}); first.Code != http.StatusOK {
		t.Fatalf("first call status = %d", first.Code)
	}
	second := fixture.do(t, gatewayCall{
		body:           []byte(`{"runId":"` + unique("run") + `","params":{"symbols":["MSFT.US"]}}`),
		idempotencyKey: key})
	message := wantGatewayError(t, second, http.StatusConflict, "idempotency_conflict")
	if message != "The idempotency key was already used with different request metadata" {
		t.Errorf("message = %q", message)
	}
}

// 幂等记录按 Agent 修订版本隔离:改配置(revision+1)后同一个键会重新执行。
func TestGatewayIdempotencyIsScopedToRevision(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	key := unique("idem")
	body := minimalTaskBody(unique("run"))
	if first := fixture.do(t, gatewayCall{body: body, idempotencyKey: key}); first.Code != http.StatusOK {
		t.Fatalf("first call status = %d", first.Code)
	}

	updated, err := fixture.state.Store.UpdateAgent(context.Background(), fixture.slug,
		&AgentInput{Slug: fixture.slug, Name: "renamed", ExecutorKind: ExecutorKindRust,
			ExecutionMode: ExecutionModeLocal, SignatureEncoding: "hex",
			WorkspacePath: "agents", PermissionMode: "dontAsk",
			TimeoutSecs: uint64Ptr(30), MaxConcurrency: intPtr(2), QueueCapacity: intPtr(4)})
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	fixture.state.Registry.Replace(updated)

	// 新修订版本下同一个键不再命中缓存(会因为 runId 复用而撞 run_id_conflict,
	// 说明确实走到了执行前的那一步 —— 缓存回放不会到那里)。
	second := fixture.do(t, gatewayCall{body: body, idempotencyKey: key})
	if second.Header().Get("X-Idempotent-Replay") == "true" {
		t.Error("a new Agent revision must not reuse the previous cached response")
	}
	wantGatewayError(t, second, http.StatusConflict, "run_id_conflict")
}

// ---------------------------------------------------------------------------
// 端到端:容量闸
// ---------------------------------------------------------------------------

func TestGatewayRejectsWhenAdmissionFull(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runtime := fixture.runtime(t)
	// 占满准入闸(容量 = MaxConcurrency + QueueCapacity = 6)。
	held := 0
	for runtime.AdmissionSlots.TryAcquire() {
		held++
	}
	defer func() {
		for i := 0; i < held; i++ {
			runtime.AdmissionSlots.Release()
		}
	}()

	started := time.Now()
	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	message := wantGatewayError(t, recorder, http.StatusTooManyRequests, "agent_overloaded")
	if message != "Agent queue is full" {
		t.Errorf("message = %q", message)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("admission rejection took %v, want it immediate", elapsed)
	}
}

func TestGatewayTimesOutWaitingForCapacity(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runtime := fixture.runtime(t)
	// 时限压到 1 秒,并占满该 Agent 的执行闸,让请求真的排队等到超时。
	runtime.Config.TimeoutSecs = 1
	held := 0
	for runtime.ExecutionSlots.TryAcquire() {
		held++
	}
	defer func() {
		for i := 0; i < held; i++ {
			runtime.ExecutionSlots.Release()
		}
	}()

	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	message := wantGatewayError(t, recorder, http.StatusGatewayTimeout, "queue_timeout")
	if message != "Agent did not become available before the deadline" {
		t.Errorf("message = %q", message)
	}
}

func TestGatewayTimesOutWaitingForGlobalCapacity(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runtime := fixture.runtime(t)
	// 时限必须为正,否则会先在「等 Agent 执行闸」那一步超时,走不到全局闸。
	runtime.Config.TimeoutSecs = 1
	held := 0
	for fixture.state.GlobalExecution.TryAcquire() {
		held++
	}
	defer func() {
		for i := 0; i < held; i++ {
			fixture.state.GlobalExecution.Release()
		}
	}()

	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	message := wantGatewayError(t, recorder, http.StatusGatewayTimeout, "queue_timeout")
	if message != "Global execution capacity did not become available before the deadline" {
		t.Errorf("message = %q", message)
	}
	// 前两道许可要归还。
	if !runtime.AdmissionSlots.TryAcquire() || !runtime.ExecutionSlots.TryAcquire() {
		t.Error("slots leaked on the global-capacity timeout path")
	}
}

// 成功路径不泄漏许可:连续多次请求都能通过。
func TestGatewayReleasesSlotsAcrossRequests(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	for i := 0; i < 8; i++ {
		recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
		if recorder.Code != http.StatusOK {
			t.Fatalf("iteration %d: status = %d body = %s", i, recorder.Code, recorder.Body.String())
		}
	}
	runtime := fixture.runtime(t)
	for i := 0; i < runtime.Config.MaxConcurrency; i++ {
		if !runtime.ExecutionSlots.TryAcquire() {
			t.Fatalf("execution slot %d leaked", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端:usage_unavailable 门禁
// ---------------------------------------------------------------------------

// usageProcessor 是可控 usage 的处理器替身。
type usageProcessor struct{ usage *BoundRunUsage }

func (p usageProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	output := PlaceholderOutput("usage probe")
	output.Usage = p.usage
	return output, nil
}

// 受信通道拿不到 FINAL usage 时必须判 502,不能静默降级成「没有用量」。
func TestGatewayRejectsMissingUsageOnTrustedChannel(t *testing.T) {
	fixture := newGatewayFixture(t, usageProcessor{usage: nil})
	fixture.state.TrustedUsageReportingEnabled = true
	runID := unique("run")

	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(runID), runID: runID})
	message := wantGatewayError(t, recorder, http.StatusBadGateway, "usage_unavailable")
	if message != usageUnavailableMessage {
		t.Errorf("message = %q, want %q", message, usageUnavailableMessage)
	}

	// 运行记录落成 failed,并且**落库用的是小写开头的内部文案**。
	runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		if run.Status != "failed" {
			t.Errorf("status = %q, want failed", run.Status)
		}
		if run.ErrorCode == nil || *run.ErrorCode != "usage_unavailable" {
			t.Errorf("error_code = %v", run.ErrorCode)
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusBadGateway {
			t.Errorf("response_code = %v, want 502", run.ResponseCode)
		}
		if run.Output == nil || !strings.Contains(*run.Output, usageUnavailableDetail) {
			t.Errorf("result_json = %v, want the lowercase internal detail", run.Output)
		}
		return
	}
	t.Fatalf("run %s was not recorded", runID)
}

// 受信通道拿到用量时正常返回,并把 usage 写进信封。
func TestGatewayReportsUsageOnTrustedChannel(t *testing.T) {
	fixture := newGatewayFixture(t, usageProcessor{
		usage: FinalReportedUsage(120, uint64Ptr(30), 40, uint64Ptr(10), 2)})
	fixture.state.TrustedUsageReportingEnabled = true

	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %#v, want an object", payload["usage"])
	}
	if usage["status"] != "FINAL" || usage["totalTokens"] != float64(160) {
		t.Errorf("usage = %#v", usage)
	}
}

// 非受信通道即使拿不到用量也照常成功(用量本来就不上报)。
func TestGatewayIgnoresMissingUsageOnUntrustedChannel(t *testing.T) {
	fixture := newGatewayFixture(t, usageProcessor{usage: nil})
	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 端到端:执行失败的状态映射与落库
// ---------------------------------------------------------------------------

// errorProcessor 按指定错误失败。(push_test.go 已占用 failingProcessor 这个名字。)
type errorProcessor struct{ err error }

func (p errorProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	return nil, p.err
}

func TestGatewayMapsExecutionFailureToStatusAndRecord(t *testing.T) {
	fixture := newGatewayFixture(t, errorProcessor{err: errors.New("analysis blew up")})
	runID := unique("run")
	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(runID), runID: runID})
	// processor_failed → 500,对外只暴露稳定文案。
	message := wantGatewayError(t, recorder, http.StatusInternalServerError, "processor_failed")
	if message != "agent execution failed" {
		t.Errorf("message = %q, want the stable public text", message)
	}
	if strings.Contains(recorder.Body.String(), "analysis blew up") {
		t.Error("internal detail must not leak to the caller")
	}

	runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		if run.Status != "failed" {
			t.Errorf("status = %q, want failed", run.Status)
		}
		if run.ErrorCode == nil || *run.ErrorCode != "processor_failed" {
			t.Errorf("error_code = %v", run.ErrorCode)
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusInternalServerError {
			t.Errorf("response_code = %v, want 500", run.ResponseCode)
		}
		return
	}
	t.Fatalf("run %s was not recorded", runID)
}

// 不支持的执行器组合 → executor_unavailable → 503。
func TestGatewayMapsExecutorUnavailableTo503(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	runtime := fixture.runtime(t)
	runtime.Config.ExecutorKind = ExecutorKindGoose
	runtime.Config.ExecutionMode = ExecutionModeLocal

	recorder := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run"))})
	message := wantGatewayError(t, recorder, http.StatusServiceUnavailable, "executor_unavailable")
	if message != "agent executor is unavailable" {
		t.Errorf("message = %q", message)
	}
}

// 失败也要释放幂等租约:同一个键能重新发起。
func TestGatewayReleasesIdempotencyLeaseOnFailure(t *testing.T) {
	fixture := newGatewayFixture(t, errorProcessor{err: errors.New("boom")})
	key := unique("idem")
	first := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run")), idempotencyKey: key})
	wantGatewayError(t, first, http.StatusInternalServerError, "processor_failed")

	// 同一个键再来一次:不能是 request_in_progress(那说明租约没释放)。
	second := fixture.do(t, gatewayCall{body: minimalTaskBody(unique("run")), idempotencyKey: key})
	if second.Code == http.StatusConflict &&
		strings.Contains(second.Body.String(), "request_in_progress") {
		t.Fatal("the idempotency lease leaked on the failure path")
	}
}

// ---------------------------------------------------------------------------
// 端到端:异步 callbackUrl 分支
// ---------------------------------------------------------------------------

// waitForGateway 轮询等待条件成立。(worker_test.go 已占用 waitFor 这个名字。)
func waitForGateway(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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

// asyncTaskBody 生成带 callbackUrl 的任务体。
func asyncTaskBody(runID, callbackURL string) []byte {
	return []byte(`{"runId":"` + runID + `","callbackUrl":` + jsonString(callbackURL) +
		`,"params":{"symbols":["AAPL.US"]}}`)
}

// 带 callbackUrl 时立即 202(空体),结果通过 callback 回传。
func TestGatewayAsyncAcksAndDeliversCallback(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"OK","accepted":true,"runStatus":"SUCCESS"}`,
	})
	fixture.state.CallbackToken = "mpk_gateway_token"
	runID := unique("run")

	started := time.Now()
	recorder := fixture.do(t, gatewayCall{
		body: asyncTaskBody(runID, stub.server.URL), runID: runID})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("202 body = %q, want it empty", recorder.Body.String())
	}
	// ack 必须是立即的,不能等执行完。
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("ack took %v, want it immediate", elapsed)
	}

	waitForGateway(t, 5*time.Second, "callback delivery", func() bool { return stub.callCount() > 0 })
	request := stub.nextRequest(t)
	if request.runID != runID {
		t.Errorf("X-MoTA-Run-ID = %q, want %q", request.runID, runID)
	}
	if request.token != "mpk_gateway_token" {
		t.Errorf("token = %q", request.token)
	}
	// 签名用的是**该 Agent 自己的**密钥。
	assertCallbackSignatureV2(t, fixture.secret, runID, request.body, request.signature)
	var envelope map[string]any
	if err := json.Unmarshal(request.body, &envelope); err != nil {
		t.Fatalf("callback body is not JSON: %v", err)
	}
	if envelope["contract_version"] != "V2" {
		t.Errorf("contract_version = %#v", envelope["contract_version"])
	}

	// 运行记录:completed + response_code 202。
	waitForGateway(t, 5*time.Second, "run finalization", func() bool {
		runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
		if err != nil {
			return false
		}
		for _, run := range runs {
			if run.RunID == runID && run.Status == "completed" {
				return true
			}
		}
		return false
	})
	runs, _ := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusAccepted {
			t.Errorf("response_code = %v, want 202", run.ResponseCode)
		}
	}

	// channel_events 里要有这次 callback 往返。
	events, err := fixture.state.Store.ListChannelEvents(context.Background(), "callback", fixture.slug, 20)
	if err != nil {
		t.Fatalf("list channel events: %v", err)
	}
	for _, event := range events {
		if event.RunID == nil || *event.RunID != runID {
			continue
		}
		if event.Accepted == nil || !*event.Accepted {
			t.Errorf("accepted = %v, want true", event.Accepted)
		}
		if event.StatusCode == nil || *event.StatusCode != http.StatusOK {
			t.Errorf("status_code = %v, want 200", event.StatusCode)
		}
		if event.RunStatus == nil || *event.RunStatus != "SUCCESS" {
			t.Errorf("run_status = %v", event.RunStatus)
		}
		if event.Input == nil || !strings.Contains(*event.Input, "contract_version") {
			t.Error("channel event must persist the delivered envelope")
		}
		if event.Output == nil || !strings.Contains(*event.Output, `"accepted":true`) {
			t.Errorf("channel event output = %v, want the receipt", event.Output)
		}
		return
	}
	t.Fatalf("no callback channel event recorded for %s", runID)
}

// callbackUrl 为空白时退回同步形态。
func TestGatewayBlankCallbackURLStaysSynchronous(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	for _, callbackURL := range []string{"", "   "} {
		recorder := fixture.do(t, gatewayCall{
			body: asyncTaskBody(unique("run"), callbackURL)})
		if recorder.Code != http.StatusOK {
			t.Errorf("callbackUrl %q => status %d, want a synchronous 200", callbackURL, recorder.Code)
		}
	}
}

// callback 投递失败时运行仍记为 completed(结果确实产出了),失败单独记进 channel_events。
func TestGatewayAsyncRecordsCallbackFailure(t *testing.T) {
	fixture := newGatewayFixture(t, PlaceholderProcessor{})
	stub := newCallbackStub(t, callbackStubResponse{status: 400, body: `{"code":"BAD_SIGNATURE"}`})
	runID := unique("run")

	recorder := fixture.do(t, gatewayCall{
		body: asyncTaskBody(runID, stub.server.URL), runID: runID})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	waitForGateway(t, 5*time.Second, "callback attempt", func() bool { return stub.callCount() > 0 })
	waitForGateway(t, 5*time.Second, "run finalization", func() bool {
		runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
		if err != nil {
			return false
		}
		for _, run := range runs {
			if run.RunID == runID && run.Status == "completed" {
				return true
			}
		}
		return false
	})

	events, err := fixture.state.Store.ListChannelEvents(context.Background(), "callback", fixture.slug, 50)
	if err != nil {
		t.Fatalf("list channel events: %v", err)
	}
	for _, event := range events {
		if event.RunID == nil || *event.RunID != runID {
			continue
		}
		if event.Detail == nil || !strings.Contains(*event.Detail, "callback rejected with 400") {
			t.Errorf("detail = %v, want the rejection reason", event.Detail)
		}
		if event.Output == nil || !strings.Contains(*event.Output, "callback_delivery_failed") {
			t.Errorf("output = %v", event.Output)
		}
		if event.Accepted != nil {
			t.Errorf("accepted = %v, want nil on a delivery failure", event.Accepted)
		}
		return
	}
	t.Fatalf("no callback failure event recorded for %s", runID)
}

// 受信通道拿不到 usage 时**连 callback 都不发**,并把运行记成 usage_unavailable。
func TestGatewayAsyncSuppressesCallbackWhenUsageMissing(t *testing.T) {
	fixture := newGatewayFixture(t, usageProcessor{usage: nil})
	fixture.state.TrustedUsageReportingEnabled = true
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"OK","accepted":true}`})
	runID := unique("run")

	recorder := fixture.do(t, gatewayCall{
		body: asyncTaskBody(runID, stub.server.URL), runID: runID})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	waitForGateway(t, 5*time.Second, "run finalization", func() bool {
		runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
		if err != nil {
			return false
		}
		for _, run := range runs {
			if run.RunID == runID && run.Status == "failed" {
				return true
			}
		}
		return false
	})
	if stub.callCount() != 0 {
		t.Errorf("callback was attempted %d times, want 0", stub.callCount())
	}
	runs, _ := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		if run.ErrorCode == nil || *run.ErrorCode != "usage_unavailable" {
			t.Errorf("error_code = %v", run.ErrorCode)
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusBadGateway {
			t.Errorf("response_code = %v, want 502", run.ResponseCode)
		}
	}
}

// 异步执行失败时运行记成 failed,response_code 是已 ack 的 202。
func TestGatewayAsyncRecordsExecutionFailure(t *testing.T) {
	fixture := newGatewayFixture(t, errorProcessor{err: errors.New("async boom")})
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"OK","accepted":true}`})
	runID := unique("run")

	recorder := fixture.do(t, gatewayCall{
		body: asyncTaskBody(runID, stub.server.URL), runID: runID})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	waitForGateway(t, 5*time.Second, "run finalization", func() bool {
		runs, err := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
		if err != nil {
			return false
		}
		for _, run := range runs {
			if run.RunID == runID && run.Status == "failed" {
				return true
			}
		}
		return false
	})
	if stub.callCount() != 0 {
		t.Errorf("a failed execution must not send a callback, got %d attempts", stub.callCount())
	}
	runs, _ := fixture.state.Store.ListRuns(context.Background(), 20, fixture.slug)
	for _, run := range runs {
		if run.RunID != runID {
			continue
		}
		if run.ErrorCode == nil || *run.ErrorCode != "processor_failed" {
			t.Errorf("error_code = %v", run.ErrorCode)
		}
		if run.ResponseCode == nil || *run.ResponseCode != http.StatusAccepted {
			t.Errorf("response_code = %v, want the already-acked 202", run.ResponseCode)
		}
	}
}

// blockingProcessor 在收到信号前一直不返回,用来观察后台执行期间的容量占用。
type blockingProcessor struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingProcessor() *blockingProcessor {
	return &blockingProcessor{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *blockingProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return PlaceholderOutput("unblocked"), nil
}

// 异步执行期间必须**继续持有**三道容量许可 —— 否则后台任务不受并发上限约束。
//
// 这是 Go 侧相对 rs 的一处结构差异:rs 把 permit move 进 spawn 的 task,Go 需要
// 显式把释放函数移交给协程,不能让 Gateway 返回 202 时的 defer 提前放掉。
func TestGatewayAsyncKeepsHoldingCapacityWhileRunning(t *testing.T) {
	processor := newBlockingProcessor()
	fixture := newGatewayFixture(t, processor)
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"OK","accepted":true}`})
	runtime := fixture.runtime(t)

	recorder := fixture.do(t, gatewayCall{
		body: asyncTaskBody(unique("run"), stub.server.URL)})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	// 等后台真正开始执行。
	select {
	case <-processor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the async execution to start")
	}

	// 此刻每道闸都应各少一个可用许可。
	admission := drainSemaphore(runtime.AdmissionSlots)
	execution := drainSemaphore(runtime.ExecutionSlots)
	global := drainSemaphore(fixture.state.GlobalExecution)
	wantAdmission := runtime.Config.MaxConcurrency + runtime.Config.QueueCapacity - 1
	wantExecution := runtime.Config.MaxConcurrency - 1
	if admission != wantAdmission {
		t.Errorf("free admission slots = %d, want %d", admission, wantAdmission)
	}
	if execution != wantExecution {
		t.Errorf("free execution slots = %d, want %d", execution, wantExecution)
	}
	if global != fixture.state.Config.GlobalMaxConcurrency-1 {
		t.Errorf("free global slots = %d, want %d",
			global, fixture.state.Config.GlobalMaxConcurrency-1)
	}

	// 放开后台执行,许可要归还。
	close(processor.release)
	for i := 0; i < admission; i++ {
		runtime.AdmissionSlots.Release()
	}
	for i := 0; i < execution; i++ {
		runtime.ExecutionSlots.Release()
	}
	for i := 0; i < global; i++ {
		fixture.state.GlobalExecution.Release()
	}
	waitForGateway(t, 5*time.Second, "async slot release", func() bool {
		if !runtime.ExecutionSlots.TryAcquire() {
			return false
		}
		runtime.ExecutionSlots.Release()
		return drainAndRestore(runtime.ExecutionSlots) == runtime.Config.MaxConcurrency
	})
}

// drainSemaphore 把信号量抽干并返回抽到的许可数(不归还)。
func drainSemaphore(s *semaphore) int {
	count := 0
	for s.TryAcquire() {
		count++
	}
	return count
}

// drainAndRestore 数出当前可用许可数并原样还回去。
func drainAndRestore(s *semaphore) int {
	count := drainSemaphore(s)
	for i := 0; i < count; i++ {
		s.Release()
	}
	return count
}
