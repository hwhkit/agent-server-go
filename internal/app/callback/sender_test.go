package callback

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// ---------------------------------------------------------------------------
// 假 Poster：可编程的响应序列
// ---------------------------------------------------------------------------

type postedAttempt struct {
	target  string
	headers map[string]string
	body    []byte
}

type fakePoster struct {
	statuses []int
	bodies   []string
	errs     []error
	posted   []postedAttempt
}

func (f *fakePoster) Post(_ context.Context, target string, headers map[string]string, body []byte) (int, string, error) {
	index := len(f.posted)
	f.posted = append(f.posted, postedAttempt{target: target, headers: headers, body: body})
	if index < len(f.errs) && f.errs[index] != nil {
		status := 0
		if index < len(f.statuses) {
			status = f.statuses[index]
		}
		return status, "", f.errs[index]
	}
	status := 500
	if index < len(f.statuses) {
		status = f.statuses[index]
	}
	bodyText := ""
	if index < len(f.bodies) {
		bodyText = f.bodies[index]
	}
	return status, bodyText, nil
}

func testSender(poster Poster) *Sender {
	sender := NewSender(Config{SigningSecret: "cb-secret"}, poster)
	sender.retryDelays = []time.Duration{0, 0, 0, 0, 0} // 测试免等待
	return sender
}

func payload() *platform.PayloadV1 {
	return &platform.PayloadV1{PayloadResult: map[string]any{"assessments": []any{}},
		PayloadInfo: "ok", ContractVersion: "V2"}
}

func wantDeliveryError(t *testing.T, err error, status int, unconfirmed bool) *DeliveryError {
	t.Helper()
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("expected *DeliveryError, got %T: %v", err, err)
	}
	if deliveryErr.StatusCode != status || deliveryErr.Unconfirmed != unconfirmed {
		t.Fatalf("delivery error = status %d unconfirmed %v, want %d/%v (%v)",
			deliveryErr.StatusCode, deliveryErr.Unconfirmed, status, unconfirmed, deliveryErr)
	}
	return deliveryErr
}

const okReceipt = `{"code":"OK","accepted":true,"runStatus":"completed"}`

// ---------------------------------------------------------------------------
// 成功与重试路径
// ---------------------------------------------------------------------------

func TestSendSuccessFirstAttempt(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{200}, bodies: []string{okReceipt}}
	receipt, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !receipt.Accepted || receipt.Code != "OK" || *receipt.RunStatus != "completed" {
		t.Errorf("receipt = %+v", receipt)
	}
	if len(poster.posted) != 1 {
		t.Fatalf("attempts = %d", len(poster.posted))
	}
	posted := poster.posted[0]
	if posted.headers["Content-Type"] != "application/json" ||
		posted.headers[platform.HeaderRunID] != "run-1" {
		t.Errorf("headers = %+v", posted.headers)
	}
	signature := posted.headers[platform.CreatorSignatureHeader]
	if !strings.HasPrefix(signature, "t=") || !strings.Contains(signature, ",v2=") {
		t.Errorf("signature format = %q", signature)
	}
	// 签名可被平台侧用同一 canonical 复算。
	stamp := strings.TrimPrefix(strings.Split(signature, ",")[0], "t=")
	timestamp, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		t.Fatalf("signature timestamp %q: %v", stamp, err)
	}
	want := platform.CallbackSignatureV2("cb-secret", "run-1", posted.body, timestamp)
	if signature != want {
		t.Error("signature must match the v2 canonical recomputation")
	}
}

func TestSendRetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{
		statuses: []int{503, 429, 200},
		bodies:   []string{`{"code":"RETRY_ME"}`, ``, okReceipt},
	}
	receipt, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	if err != nil || !receipt.Accepted {
		t.Fatalf("receipt = %+v err = %v", receipt, err)
	}
	if len(poster.posted) != 3 {
		t.Errorf("attempts = %d, want 3 (503 与 429 都应重试)", len(poster.posted))
	}
}

func TestSendNonRetryable4xxIsTerminal(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{401}, bodies: []string{`{"code":"UNAUTHORIZED"}`}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	deliveryErr := wantDeliveryError(t, err, 401, false)
	if deliveryErr.Attempts != 1 || len(poster.posted) != 1 {
		t.Errorf("4xx must not retry: attempts=%d posted=%d", deliveryErr.Attempts, len(poster.posted))
	}
}

func TestSendRetryExhaustedIsUnconfirmed(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{500, 500, 500, 500, 500, 500}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	deliveryErr := wantDeliveryError(t, err, 500, true)
	if deliveryErr.Attempts != 6 || len(poster.posted) != 6 {
		t.Errorf("attempts = %d/%d, want 6", deliveryErr.Attempts, len(poster.posted))
	}
}

func TestSendNetworkErrorRetriesThenUnconfirmed(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{errs: []error{errors.New("dial failed"), errors.New("dial failed"),
		errors.New("dial failed"), errors.New("dial failed"), errors.New("dial failed"),
		errors.New("dial failed")}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	wantDeliveryError(t, err, 0, true)
	if len(poster.posted) != 6 {
		t.Errorf("attempts = %d", len(poster.posted))
	}
}

func TestSendUnparsableReceiptIsUnconfirmedNoRetry(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{200}, bodies: []string{"not json"}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	wantDeliveryError(t, err, 200, true)
	if len(poster.posted) != 1 {
		t.Errorf("unconfirmed must not retry, posted = %d", len(poster.posted))
	}
}

func TestSendTransportCodeNotOKIsUnconfirmed(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{200}, bodies: []string{`{"code":"RETRY_ME","accepted":false}`}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	deliveryErr := wantDeliveryError(t, err, 200, true)
	if !strings.Contains(deliveryErr.Error(), "transport code") {
		t.Errorf("error = %v", deliveryErr)
	}
}

func TestSendAcceptedFalseReturnsReceiptWithoutRetry(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{200},
		bodies: []string{`{"code":"OK","accepted":false,"runStatus":"superseded"}`}}
	receipt, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	if err != nil {
		t.Fatalf("accepted=false is a business verdict, not an error: %v", err)
	}
	if receipt.Accepted || *receipt.RunStatus != "superseded" {
		t.Errorf("receipt = %+v", receipt)
	}
	if len(poster.posted) != 1 {
		t.Errorf("must not retry a rejected result, posted = %d", len(poster.posted))
	}
}

func TestSendResponseWithRequestErrorUnder500IsUnconfirmed(t *testing.T) {
	t.Parallel()
	// 收到 200 但读回执失败（超限）→ 无法确认，不重试。
	poster := &fakePoster{statuses: []int{200}, errs: []error{errors.New("read callback response failed")}}
	_, err := testSender(poster).Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload())
	wantDeliveryError(t, err, 200, true)
	if len(poster.posted) != 1 {
		t.Errorf("posted = %d", len(poster.posted))
	}
}

func TestSendContextCancelDuringRetryIsUnconfirmed(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{500, 500}}
	sender := NewSender(Config{SigningSecret: "cb-secret"}, poster)
	sender.retryDelays = []time.Duration{time.Hour, 0, 0, 0, 0}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := sender.Send(ctx, "https://platform.example.com/callback", "run-1", payload())
	deliveryErr := wantDeliveryError(t, err, 500, true)
	if !strings.Contains(deliveryErr.Error(), "retry aborted") {
		t.Errorf("error = %v", deliveryErr)
	}
}

// ---------------------------------------------------------------------------
// 输入校验与令牌 origin 规则
// ---------------------------------------------------------------------------

func TestValidateInputs(t *testing.T) {
	t.Parallel()
	if err := ValidateInputs("https://ok.example/cb", "run", "secret"); err != nil {
		t.Errorf("https must pass: %v", err)
	}
	if err := ValidateInputs("http://127.0.0.1:9999/cb", "run", "secret"); err != nil {
		t.Errorf("loopback http must pass: %v", err)
	}
	if err := ValidateInputs("http://public.example/cb", "run", "secret"); err == nil {
		t.Error("public http must be rejected")
	}
	if err := ValidateInputs("https://user:pw@ok.example/cb", "run", "secret"); err == nil {
		t.Error("credentials must be rejected")
	}
	if err := ValidateInputs("https://ok.example/cb", " ", "secret"); err == nil {
		t.Error("empty runId must be rejected")
	}
	if err := ValidateInputs("https://ok.example/cb", "run", ""); err == nil {
		t.Error("empty secret must be rejected")
	}
	if err := ValidateInputs("not-a-url", "run", "secret"); err == nil {
		t.Error("relative URL must be rejected")
	}
}

func TestSendTokenOnlyToConfiguredOrigin(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{statuses: []int{200}, bodies: []string{okReceipt}}
	sender := testSender(poster)
	sender.config.Token = "legacy-token"
	sender.config.TokenOrigin = "https://platform.example.com"

	if _, err := sender.Send(context.Background(),
		"https://platform.example.com/callback", "run-1", payload()); err != nil {
		t.Fatalf("allowed origin: %v", err)
	}
	if !strings.Contains(poster.posted[0].target, "token=legacy-token") {
		t.Errorf("token must be appended for the allowed origin: %s", poster.posted[0].target)
	}

	// 其他 origin 拒绝携带令牌。
	poster2 := &fakePoster{statuses: []int{200}, bodies: []string{okReceipt}}
	sender2 := testSender(poster2)
	sender2.config.Token = "legacy-token"
	sender2.config.TokenOrigin = "https://platform.example.com"
	_, err := sender2.Send(context.Background(), "https://evil.example/cb", "run-1", payload())
	deliveryErr := wantDeliveryError(t, err, 0, false)
	if !strings.Contains(deliveryErr.Error(), "not allowed to receive the callback token") {
		t.Errorf("error = %v", deliveryErr)
	}
	if len(poster2.posted) != 0 {
		t.Error("must not send when the token origin check fails")
	}
	// 未配置 origin 时令牌同样被拒（不发裸令牌）。
	sender3 := testSender(&fakePoster{})
	sender3.config.Token = "legacy-token"
	if _, err := sender3.Send(context.Background(),
		"https://platform.example.com/cb", "run-1", payload()); err == nil {
		t.Error("token without configured origin must be rejected")
	}
}

func TestSendNilPayloadRejected(t *testing.T) {
	t.Parallel()
	poster := &fakePoster{}
	if _, err := testSender(poster).Send(context.Background(),
		"https://ok.example/cb", "run-1", nil); err == nil {
		t.Error("nil payload must be rejected")
	}
	if len(poster.posted) != 0 {
		t.Error("must not post for nil payload")
	}
}
