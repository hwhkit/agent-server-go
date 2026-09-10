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
)

// ---------------------------------------------------------------------------
// CallbackSignatureV2
// ---------------------------------------------------------------------------

// 对应 rs signature_v2_matches_canonical_bytes。
func TestCallbackSignatureV2MatchesCanonicalBytes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"contract_version":"V2"}`)
	const timestamp int64 = 1775555555
	actual := CallbackSignatureV2("mbt_secret", "RUN-42", body, timestamp)

	canonical := append([]byte("v2\n1775555555\nRUN-42\n"), body...)
	expected := ComputeSignature("mbt_secret", canonical, SignatureEncodingHex)
	if want := "t=1775555555,v2=" + expected; actual != want {
		t.Errorf("signature = %q, want %q", actual, want)
	}
}

// canonical 覆盖的是**原始 body 字节**:重排键序会换签名。
func TestCallbackSignatureV2IsByteExact(t *testing.T) {
	t.Parallel()
	const timestamp int64 = 1700000000
	a := CallbackSignatureV2("s", "r", []byte(`{"a":1,"b":2}`), timestamp)
	b := CallbackSignatureV2("s", "r", []byte(`{"b":2,"a":1}`), timestamp)
	if a == b {
		t.Error("re-ordered JSON keys must produce a different signature")
	}
	// 时间戳、runId、密钥各自参与签名。
	if CallbackSignatureV2("s", "r", []byte(`{}`), timestamp) ==
		CallbackSignatureV2("s", "r", []byte(`{}`), timestamp+1) {
		t.Error("timestamp must participate in the signature")
	}
	if CallbackSignatureV2("s", "r1", []byte(`{}`), timestamp) ==
		CallbackSignatureV2("s", "r2", []byte(`{}`), timestamp) {
		t.Error("runId must participate in the signature")
	}
	if CallbackSignatureV2("s1", "r", []byte(`{}`), timestamp) ==
		CallbackSignatureV2("s2", "r", []byte(`{}`), timestamp) {
		t.Error("signing secret must participate in the signature")
	}
}

// ---------------------------------------------------------------------------
// appendCallbackToken / maskCallbackToken
// ---------------------------------------------------------------------------

// 对应 rs append_token_handles_empty_and_existing_query。
func TestAppendCallbackToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url, token, want string
	}{
		{"https://api.example.com/callback", "", "https://api.example.com/callback"},
		{"https://api.example.com/callback", "   ", "https://api.example.com/callback"},
		{
			"https://api.example.com/mota/creator/publics/bound/callback", "mpk_probe-token",
			"https://api.example.com/mota/creator/publics/bound/callback?token=mpk_probe-token",
		},
		{"https://api.example.com/callback?a=1", "mpk_x", "https://api.example.com/callback?a=1&token=mpk_x"},
	}
	for _, item := range cases {
		if got := appendCallbackToken(item.url, item.token); got != item.want {
			t.Errorf("appendCallbackToken(%q, %q) = %q, want %q", item.url, item.token, got, item.want)
		}
	}
}

func TestMaskCallbackToken(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://a/cb":                  "https://a/cb",
		"https://a/cb?token=secret":     "https://a/cb?token=***",
		"https://a/cb?token=secret&x=1": "https://a/cb?token=***&x=1",
		"https://a/cb?x=1&token=secret": "https://a/cb?x=1&token=***",
		// 大小写不敏感地匹配,但写回的是小写字面量(与 rs 一致)。
		"https://a/cb?TOKEN=secret":        "https://a/cb?token=***",
		"https://a/cb?token=secret#frag":   "https://a/cb?token=***#frag",
		"https://a/cb?token=one&token=two": "https://a/cb?token=***&token=***",
	}
	for input, want := range cases {
		if got := maskCallbackToken(input); got != want {
			t.Errorf("maskCallbackToken(%q) = %q, want %q", input, got, want)
		}
	}
	// 掩码后不能残留明文。
	if strings.Contains(maskCallbackToken("https://a/cb?token=mpk_super_secret"), "mpk_super_secret") {
		t.Error("masked URL still leaks the token")
	}
}

func TestTruncateForLog(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("x", callbackLogBodyLimit)
	if truncateForLog(short) != short {
		t.Error("a body at the limit must be kept verbatim")
	}
	long := strings.Repeat("y", callbackLogBodyLimit+10)
	got := truncateForLog(long)
	if !strings.HasSuffix(got, "…<truncated 10 bytes>") {
		t.Errorf("got %q, want a truncation marker", got[len(got)-40:])
	}
}

// ---------------------------------------------------------------------------
// validateCallbackInputs
// ---------------------------------------------------------------------------

func TestValidateCallbackInputs(t *testing.T) {
	t.Parallel()
	// HTTPS 放行;HTTP 只对回环放行。
	for _, target := range []string{
		"https://api.example.com/cb",
		"http://localhost:8080/cb",
		"http://127.0.0.1:9000/cb",
		"http://[::1]:9000/cb",
	} {
		if err := validateCallbackInputs(target, "run-1", "secret"); err != nil {
			t.Errorf("%s must be accepted, got %v", target, err)
		}
	}
	for _, target := range []string{
		"http://api.example.com/cb",
		"ftp://api.example.com/cb",
		"ws://localhost/cb",
	} {
		err := validateCallbackInputs(target, "run-1", "secret")
		if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
			t.Errorf("%s => %v, want an HTTPS rejection", target, err)
		}
	}
	// 非绝对地址。
	for _, target := range []string{"/relative/cb", "not a url at all", "", "example.com/cb"} {
		err := validateCallbackInputs(target, "run-1", "secret")
		if err == nil || !strings.Contains(err.Error(), "absolute URL") {
			t.Errorf("%s => %v, want an absolute-URL rejection", target, err)
		}
	}
	// runId / 密钥不能为空白。
	if err := validateCallbackInputs("https://a/cb", "   ", "secret"); err == nil ||
		err.Error() != "callback runId is empty" {
		t.Errorf("blank runId => %v", err)
	}
	if err := validateCallbackInputs("https://a/cb", "run-1", "  "); err == nil ||
		err.Error() != "callback signing secret is empty" {
		t.Errorf("blank secret => %v", err)
	}
}

// ---------------------------------------------------------------------------
// parseCallbackReceipt
// ---------------------------------------------------------------------------

// code 与 accepted 是必填字段:缺失必须报错,不能静默当成零值。
func TestParseCallbackReceiptRequiresCodeAndAccepted(t *testing.T) {
	t.Parallel()
	receipt, err := parseCallbackReceipt(`{"code":"OK","accepted":true,"runStatus":"SUCCESS","runMsg":"done"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receipt.Code != "OK" || !receipt.Accepted {
		t.Errorf("receipt = %+v", receipt)
	}
	if receipt.RunStatus == nil || *receipt.RunStatus != "SUCCESS" {
		t.Errorf("runStatus = %v", receipt.RunStatus)
	}
	if receipt.RunMsg == nil || *receipt.RunMsg != "done" {
		t.Errorf("runMsg = %v", receipt.RunMsg)
	}
	// 可选字段缺失是允许的。
	if _, err := parseCallbackReceipt(`{"code":"OK","accepted":false}`); err != nil {
		t.Errorf("optional fields must be omittable: %v", err)
	}
	for _, text := range []string{`{}`, `{"code":"OK"}`, `{"accepted":true}`, `not json`, ``} {
		if _, err := parseCallbackReceipt(text); err == nil {
			t.Errorf("%q must be rejected", text)
		}
	}
}

func TestHTTPStatusText(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		429: "429 Too Many Requests",
		500: "500 Internal Server Error",
		503: "503 Service Unavailable",
		401: "401 Unauthorized",
		599: "599 <unknown status code>",
	}
	for status, want := range cases {
		if got := httpStatusText(status); got != want {
			t.Errorf("httpStatusText(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestCallbackResponseCode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"code":"RATE_LIMITED"}`: "RATE_LIMITED",
		`{"code":123}`:            "no code",
		`{}`:                      "no code",
		`not json`:                "no code",
		``:                        "no code",
	}
	for text, want := range cases {
		if got := callbackResponseCode(text); got != want {
			t.Errorf("callbackResponseCode(%q) = %q, want %q", text, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// SendCallback
// ---------------------------------------------------------------------------

// callbackStub 是可编排应答序列的假平台 callback 端点。
type callbackStub struct {
	server    *httptest.Server
	responses []callbackStubResponse
	calls     atomic.Int64
	requests  chan callbackStubRequest
}

type callbackStubResponse struct {
	status int
	body   string
}

type callbackStubRequest struct {
	runID     string
	signature string
	token     string
	body      []byte
}

func newCallbackStub(t *testing.T, responses ...callbackStubResponse) *callbackStub {
	t.Helper()
	stub := &callbackStub{
		responses: responses,
		requests:  make(chan callbackStubRequest, 32),
	}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(stub.calls.Add(1)) - 1
		raw, _ := io.ReadAll(r.Body)
		stub.requests <- callbackStubRequest{
			runID:     r.Header.Get(RunIDHeader),
			signature: r.Header.Get(CreatorSignatureHeader),
			token:     r.URL.Query().Get("token"),
			body:      raw,
		}
		response := callbackStubResponse{status: 500, body: `{"code":"STUB_EXHAUSTED"}`}
		if index < len(stub.responses) {
			response = stub.responses[index]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.status)
		_, _ = io.WriteString(w, response.body)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *callbackStub) callCount() int64 { return s.calls.Load() }

func (s *callbackStub) nextRequest(t *testing.T) callbackStubRequest {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a callback request")
		return callbackStubRequest{}
	}
}

// testPayload 是一份最小的结果信封。
func testPayload() *PayloadV1 {
	return &PayloadV1{
		PayloadResult:   map[string]any{"assessments": []any{}, "summary": "s"},
		PayloadInfo:     "s",
		ContractVersion: "V2",
	}
}

func TestSendCallbackHappyPath(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"OK","accepted":true,"runStatus":"SUCCESS"}`,
	})
	receipt, err := SendCallback(context.Background(), stub.server.Client(),
		stub.server.URL, "run-1", "mbt_secret", "mpk_token", testPayload())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receipt.Code != "OK" || !receipt.Accepted {
		t.Errorf("receipt = %+v", receipt)
	}
	if receipt.RunStatus == nil || *receipt.RunStatus != "SUCCESS" {
		t.Errorf("runStatus = %v", receipt.RunStatus)
	}
	if stub.callCount() != 1 {
		t.Errorf("callCount = %d, want 1", stub.callCount())
	}

	request := stub.nextRequest(t)
	if request.runID != "run-1" {
		t.Errorf("X-MoTA-Run-ID = %q", request.runID)
	}
	if request.token != "mpk_token" {
		t.Errorf("token = %q, want it appended to the query", request.token)
	}
	// 签名必须覆盖平台实际收到的字节。
	assertCallbackSignatureV2(t, "mbt_secret", "run-1", request.body, request.signature)
	var envelope map[string]any
	if err := json.Unmarshal(request.body, &envelope); err != nil {
		t.Fatalf("callback body is not JSON: %v", err)
	}
	if envelope["contract_version"] != "V2" {
		t.Errorf("contract_version = %#v", envelope["contract_version"])
	}
}

// accepted=false 仍然返回回执(传输成功),但**不重试**。
func TestSendCallbackReturnsRejectedReceiptWithoutRetry(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200,
		body:   `{"code":"OK","accepted":false,"runStatus":"FAILED","runMsg":"schema invalid"}`,
	})
	receipt, err := SendCallback(context.Background(), stub.server.Client(),
		stub.server.URL, "run-1", "mbt_secret", "", testPayload())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receipt.Accepted {
		t.Error("accepted must stay false")
	}
	if receipt.RunMsg == nil || *receipt.RunMsg != "schema invalid" {
		t.Errorf("runMsg = %v", receipt.RunMsg)
	}
	if stub.callCount() != 1 {
		t.Errorf("callCount = %d, want 1 (no retry on a business rejection)", stub.callCount())
	}
	// 没有 token 时 URL 上不该出现 token 参数。
	if request := stub.nextRequest(t); request.token != "" {
		t.Errorf("token = %q, want none", request.token)
	}
}

// code != OK 是传输层失败:报错且不重试。
func TestSendCallbackRejectsNonOKTransportCode(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t, callbackStubResponse{
		status: 200, body: `{"code":"SIGNATURE_INVALID","accepted":false}`,
	})
	_, err := SendCallback(context.Background(), stub.server.Client(),
		stub.server.URL, "run-1", "mbt_secret", "", testPayload())
	if err == nil || err.Error() != "callback transport code was SIGNATURE_INVALID" {
		t.Fatalf("err = %v", err)
	}
	if stub.callCount() != 1 {
		t.Errorf("callCount = %d, want 1", stub.callCount())
	}
}

// 200 但回执无法解析 → 报错,不重试。
func TestSendCallbackRejectsUnparsableReceipt(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `not json`, `{}`, `{"code":"OK"}`} {
		stub := newCallbackStub(t, callbackStubResponse{status: 200, body: body})
		_, err := SendCallback(context.Background(), stub.server.Client(),
			stub.server.URL, "run-1", "mbt_secret", "", testPayload())
		if err == nil || !strings.HasPrefix(err.Error(), "callback returned an invalid receipt: ") {
			t.Errorf("body %q => %v", body, err)
		}
		if stub.callCount() != 1 {
			t.Errorf("body %q: callCount = %d, want 1", body, stub.callCount())
		}
	}
}

// 4xx(非 429)不重试。
func TestSendCallbackDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 403, 404, 422} {
		stub := newCallbackStub(t,
			callbackStubResponse{status: status, body: `{"code":"BAD_REQUEST"}`},
			callbackStubResponse{status: 200, body: `{"code":"OK","accepted":true}`},
		)
		_, err := SendCallback(context.Background(), stub.server.Client(),
			stub.server.URL, "run-1", "mbt_secret", "", testPayload())
		want := "callback rejected with " + httpStatusText(status) + " (BAD_REQUEST)"
		if err == nil || err.Error() != want {
			t.Errorf("status %d => %v, want %q", status, err, want)
		}
		if stub.callCount() != 1 {
			t.Errorf("status %d: callCount = %d, want 1 (no retry)", status, stub.callCount())
		}
	}
}

// 429 与 5xx 会重试。第一次退避 1s,所以这里只验「确实重试了一次并成功」。
func TestSendCallbackRetriesTransientRejections(t *testing.T) {
	t.Parallel()
	for _, status := range []int{429, 500, 502, 503} {
		stub := newCallbackStub(t,
			callbackStubResponse{status: status, body: `{"code":"RETRY_ME"}`},
			callbackStubResponse{status: 200, body: `{"code":"OK","accepted":true}`},
		)
		started := time.Now()
		receipt, err := SendCallback(context.Background(), stub.server.Client(),
			stub.server.URL, "run-1", "mbt_secret", "", testPayload())
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if !receipt.Accepted {
			t.Errorf("status %d: receipt = %+v", status, receipt)
		}
		if stub.callCount() != 2 {
			t.Errorf("status %d: callCount = %d, want 2", status, stub.callCount())
		}
		// 第一档退避是 1 秒。
		if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
			t.Errorf("status %d: retried after %v, want about 1s of backoff", status, elapsed)
		}
	}
}

// 每次重试都重新签名(时间戳会变),平台侧不会因为签名过期而拒。
func TestSendCallbackResignsEachAttempt(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t,
		callbackStubResponse{status: 503, body: `{"code":"DOWN"}`},
		callbackStubResponse{status: 200, body: `{"code":"OK","accepted":true}`},
	)
	if _, err := SendCallback(context.Background(), stub.server.Client(),
		stub.server.URL, "run-1", "mbt_secret", "", testPayload()); err != nil {
		t.Fatal(err)
	}
	first := stub.nextRequest(t)
	second := stub.nextRequest(t)
	assertCallbackSignatureV2(t, "mbt_secret", "run-1", first.body, first.signature)
	assertCallbackSignatureV2(t, "mbt_secret", "run-1", second.body, second.signature)
	// body 逐字节相同(重试不重新序列化)。
	if string(first.body) != string(second.body) {
		t.Error("retries must send the exact same body bytes")
	}
}

// ctx 取消会中止退避等待。
func TestSendCallbackHonoursContextCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t, callbackStubResponse{status: 503, body: `{"code":"DOWN"}`})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := SendCallback(ctx, stub.server.Client(),
		stub.server.URL, "run-1", "mbt_secret", "", testPayload())
	if err == nil || !strings.HasPrefix(err.Error(), "callback retry aborted: ") {
		t.Fatalf("err = %v, want a retry-aborted error", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("waited %v, want it to abort during the first backoff", elapsed)
	}
}

// 传输层错误(连不上)在预算内重试,耗尽后报明确文案。
func TestSendCallbackReportsTransportFailure(t *testing.T) {
	t.Parallel()
	// 关掉服务器让连接直接失败,并用一个短 ctx 避免真的等满五档退避。
	stub := newCallbackStub(t, callbackStubResponse{status: 200, body: `{"code":"OK","accepted":true}`})
	target := stub.server.URL
	stub.server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := SendCallback(ctx, http.DefaultClient, target, "run-1", "mbt_secret", "", testPayload())
	if err == nil {
		t.Fatal("expected an error against a closed server")
	}
	// 要么是退避被 ctx 打断,要么是尝试耗尽 —— 两条都可接受,但必须报错。
	if !strings.HasPrefix(err.Error(), "callback retry aborted: ") &&
		!strings.Contains(err.Error(), "callback request failed after 6 attempts") {
		t.Errorf("err = %v", err)
	}
}

// 入参不合法时**一次请求都不发**。
func TestSendCallbackValidatesBeforeSending(t *testing.T) {
	t.Parallel()
	stub := newCallbackStub(t, callbackStubResponse{status: 200, body: `{"code":"OK","accepted":true}`})
	cases := []struct{ url, runID, secret string }{
		{"http://api.example.com/cb", "run-1", "secret"},
		{stub.server.URL, "", "secret"},
		{stub.server.URL, "run-1", ""},
		{"/relative", "run-1", "secret"},
	}
	for _, item := range cases {
		if _, err := SendCallback(context.Background(), stub.server.Client(),
			item.url, item.runID, item.secret, "", testPayload()); err == nil {
			t.Errorf("%+v must be rejected", item)
		}
	}
	if stub.callCount() != 0 {
		t.Errorf("callCount = %d, want 0", stub.callCount())
	}
}
