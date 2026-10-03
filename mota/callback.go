package mota

// Push 异步结果回传。对应 Rust 侧 src/mota/callback.rs。
//
// 新通道只使用 `mbt_*` 密钥,不带 query token。签名 v2 的 canonical bytes 是:
//
//	"v2\n" + unix_seconds + "\n" + run_id + "\n" + exact_body_bytes
//
// 注意最后一段是**原始请求体字节**,不是重新序列化的结果 —— 平台按收到的字节
// 复算,任何重排键序都会导致签名不匹配。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// callbackAttempts 是一次回传最多尝试的次数。
const callbackAttempts = 6

// callbackRequestTimeout bounds one complete network attempt, including reading
// the receipt. Callers may inject an http.Client without a Timeout, so the
// delivery helper must enforce its own deadline.
const callbackRequestTimeout = 30 * time.Second

// callbackRetryDelays 是第 1..5 次失败后的退避时长;第 6 次(最后一次)之后不再等待。
var callbackRetryDelays = [callbackAttempts - 1]time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// callbackResponseBodyLimit prevents a callback endpoint from making the
// service buffer an unbounded receipt in memory.
const callbackResponseBodyLimit = 64 * 1024

// CallbackDeliveryError keeps machine-readable delivery metadata without
// exposing the callback URL (which may contain a legacy token).
type CallbackDeliveryError struct {
	StatusCode int
	Attempts   int
	// Unconfirmed means the callback may already have been accepted but no
	// trustworthy receipt was obtained. Re-executing the task is unsafe.
	Unconfirmed bool
	Err         error
}

func (e *CallbackDeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "callback delivery failed"
	}
	return e.Err.Error()
}

func (e *CallbackDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func callbackDeliveryError(status, attempts int, err error) error {
	return &CallbackDeliveryError{StatusCode: status, Attempts: attempts, Err: err}
}

func callbackUnconfirmedError(status, attempts int, err error) error {
	return &CallbackDeliveryError{
		StatusCode: status, Attempts: attempts, Unconfirmed: true, Err: err,
	}
}

// CallbackReceipt 是平台 callback 的结果层回执。
//
// HTTP 200 与 code=OK 只代表**传输**成功;结果是否被采纳必须读 Accepted。
type CallbackReceipt struct {
	Code      string  `json:"code"`
	Accepted  bool    `json:"accepted"`
	RunStatus *string `json:"runStatus,omitempty"`
	RunMsg    *string `json:"runMsg,omitempty"`
}

// callbackReceiptWire 用指针字段区分「字段缺失」与「零值」。
//
// rs 侧 CallbackReceipt 的 code / accepted 是必填字段,serde 遇到缺失会解析失败;
// Go 的 json 包会静默填零值,所以这里显式检查存在性,保持两侧一致。
type callbackReceiptWire struct {
	Code      *string `json:"code"`
	Accepted  *bool   `json:"accepted"`
	RunStatus *string `json:"runStatus"`
	RunMsg    *string `json:"runMsg"`
}

// CallbackSignatureV2 用指定时间戳生成生产 v2 签名。
// 导出这个纯函数便于互操作测试(平台侧可用同一份 canonical 复算)。
func CallbackSignatureV2(signingSecret, runID string, body []byte, timestamp int64) string {
	stamp := strconv.FormatInt(timestamp, 10)
	canonical := make([]byte, 0, len(body)+len(runID)+len(stamp)+8)
	canonical = append(canonical, "v2\n"...)
	canonical = append(canonical, stamp...)
	canonical = append(canonical, '\n')
	canonical = append(canonical, runID...)
	canonical = append(canonical, '\n')
	canonical = append(canonical, body...)
	digest := ComputeSignature(signingSecret, canonical, SignatureEncodingHex)
	return "t=" + stamp + ",v2=" + digest
}

// SendCallback 回传结果信封。
//
// 重试策略:网络异常、408、429 与 5xx 会按 1/3/10/30/60 秒退避重试(共 6 次尝试);
// 鉴权/参数类 4xx 与 accepted=false **不重试** —— 前者重试无意义,后者是平台
// 已经明确表态的业务结论,重复投递只会产生重复运行记录。
//
// callbackToken 非空时附加 ?token=(或 &token=)到 callback URL,兼容要求
// 「令牌 + 签名」双凭证的存量平台;新平台(2026-08-17 起)令牌可选,传空串即可。
func SendCallback(
	ctx context.Context,
	client *http.Client,
	callbackURL, runID, signingSecret, callbackToken, callbackTokenOrigin string,
	result *PayloadV1,
) (receipt *CallbackReceipt, deliveryErr error) {
	attemptsMade := 0
	lastStatus := 0
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("callback delivery panicked",
				"run_id", runID, "attempt", attemptsMade, "status", lastStatus)
			receipt = nil
			deliveryErr = callbackDeliveryError(lastStatus, attemptsMade,
				errors.New("callback delivery panicked"))
		}
	}()

	if err := validateCallbackInputs(callbackURL, runID, signingSecret); err != nil {
		return nil, callbackDeliveryError(0, 0, err)
	}
	if result == nil {
		return nil, callbackDeliveryError(0, 0, errors.New("callback result is nil"))
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, callbackDeliveryError(0, 0,
			fmt.Errorf("serialize callback result failed: %w", err))
	}
	if client == nil {
		client = http.DefaultClient
	}
	// Never follow redirects for signed callbacks. net/http may copy custom
	// headers such as X-Creator-Signature to a redirected request, and a legacy
	// token may also be present in the URL.
	safeClient := *client
	safeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	target, err := appendCallbackToken(callbackURL, callbackToken, callbackTokenOrigin)
	if err != nil {
		return nil, callbackDeliveryError(0, 0, err)
	}
	logTarget := callbackOrigin(target)

	for attempt := 0; attempt < callbackAttempts; attempt++ {
		// 最后一次尝试没有后续退避;retryDelay 为 0 表示「不再重试」。
		var retryDelay time.Duration
		if attempt < len(callbackRetryDelays) {
			retryDelay = callbackRetryDelays[attempt]
		}
		lastAttempt := attempt == callbackAttempts-1

		timestamp := time.Now().Unix()
		signature := CallbackSignatureV2(signingSecret, runID, body, timestamp)
		attemptsMade = attempt + 1
		slog.Info("sending async callback",
			"run_id", runID, "attempt", attempt+1, "callback_origin", logTarget)

		status, responseText, requestErr := postCallback(
			ctx, &safeClient, target, runID, signature, body)
		lastStatus = status

		switch {
		case requestErr != nil:
			// Once an HTTP response was received, a missing/oversized receipt is
			// an unconfirmed outcome. Retrying could duplicate an already accepted
			// result, so return it to the caller with the observed status.
			if status != 0 && status < 500 && status != http.StatusRequestTimeout &&
				status != http.StatusTooManyRequests {
				return nil, callbackUnconfirmedError(status, attemptsMade, requestErr)
			}
			if lastAttempt {
				return nil, callbackUnconfirmedError(status, attemptsMade,
					fmt.Errorf("callback request failed after %d attempts: %w",
						callbackAttempts, requestErr))
			}
			slog.Warn("callback request failed; retrying",
				"run_id", runID, "attempt", attempt+1,
				"status", status, "error_kind", callbackRequestErrorKind(requestErr))

		case status == http.StatusOK:
			receipt, parseErr := parseCallbackReceipt(responseText)
			if parseErr != nil {
				slog.Warn("callback returned an unparsable receipt",
					"run_id", runID, "status", status, "response_bytes", len(responseText))
				return nil, callbackUnconfirmedError(status, attemptsMade,
					fmt.Errorf("callback returned an invalid receipt: %w", parseErr))
			}
			slog.Info("callback response received",
				"run_id", runID, "status", status, "code", receipt.Code,
				"accepted", receipt.Accepted, "run_status", derefOrEmpty(receipt.RunStatus),
				"response_bytes", len(responseText))
			if receipt.Code != "OK" {
				return nil, callbackUnconfirmedError(status, attemptsMade,
					fmt.Errorf("callback transport code was %s", receipt.Code))
			}
			if receipt.Accepted {
				slog.Info("callback result accepted",
					"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus))
			} else {
				slog.Warn("callback delivered but result was not accepted; not retrying",
					"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus))
			}
			return receipt, nil

		default:
			retryable := status == http.StatusRequestTimeout ||
				status == http.StatusTooManyRequests || status >= 500
			responseCode := callbackResponseCode(responseText)
			slog.Warn("callback rejected",
				"run_id", runID, "status", status, "attempt", attempt+1,
				"response_code", responseCode, "response_bytes", len(responseText))
			if !retryable || lastAttempt {
				deliveryError := callbackDeliveryError
				if retryable {
					deliveryError = callbackUnconfirmedError
				}
				return nil, deliveryError(status, attemptsMade,
					fmt.Errorf("callback rejected with %s (%s)",
						httpStatusText(status), responseCode))
			}
			slog.Warn("transient callback rejection; retrying",
				"run_id", runID, "status", status, "attempt", attempt+1,
				"response_code", responseCode)
		}

		if retryDelay > 0 {
			timer := time.NewTimer(retryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, callbackUnconfirmedError(lastStatus, attemptsMade,
					fmt.Errorf("callback retry aborted: %w", ctx.Err()))
			}
		}
	}
	return nil, callbackUnconfirmedError(lastStatus, attemptsMade,
		errors.New("callback retry loop exhausted"))
}

// postCallback 发一次回传请求并读回响应体。
func postCallback(
	ctx context.Context,
	client *http.Client,
	target, runID, signature string,
	body []byte,
) (int, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, callbackRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(RunIDHeader, runID)
	request.Header.Set(CreatorSignatureHeader, signature)
	response, err := client.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, "", context.DeadlineExceeded
		case errors.Is(err, context.Canceled):
			return 0, "", context.Canceled
		default:
			// net/http errors usually embed the complete URL. Never propagate it:
			// a legacy callback token may be present in the query string.
			return 0, "", errors.New("callback request failed")
		}
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, callbackResponseBodyLimit+1))
	if readErr != nil {
		return response.StatusCode, "", errors.New("read callback response failed")
	}
	if len(raw) > callbackResponseBodyLimit {
		return response.StatusCode, "",
			fmt.Errorf("callback response exceeded %d bytes", callbackResponseBodyLimit)
	}
	return response.StatusCode, string(raw), nil
}

// parseCallbackReceipt 解析回执,并要求 code 与 accepted 都存在。
func parseCallbackReceipt(text string) (*CallbackReceipt, error) {
	var wire callbackReceiptWire
	if err := json.Unmarshal([]byte(text), &wire); err != nil {
		return nil, err
	}
	if wire.Code == nil {
		return nil, errors.New("missing field `code`")
	}
	if wire.Accepted == nil {
		return nil, errors.New("missing field `accepted`")
	}
	if safeCallbackCode(*wire.Code) == "invalid code" {
		return nil, errors.New("field `code` is invalid")
	}
	return &CallbackReceipt{
		Code:      *wire.Code,
		Accepted:  *wire.Accepted,
		RunStatus: wire.RunStatus,
		RunMsg:    wire.RunMsg,
	}, nil
}

// callbackResponseCode 取响应体里的 code 字段;读不到时返回 "no code"
// (与 rs 的 unwrap_or("no code") 一致,这个字符串会进错误文案)。
func callbackResponseCode(text string) string {
	var decoded map[string]any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return "no code"
	}
	if code, ok := decoded["code"].(string); ok {
		return safeCallbackCode(code)
	}
	return "no code"
}

// safeCallbackCode keeps an untrusted peer from writing an arbitrarily large
// or control-character-bearing value into structured logs and audit details.
func safeCallbackCode(code string) string {
	if code == "" || len(code) > 128 {
		return "invalid code"
	}
	for _, char := range code {
		if char < 0x20 || char > 0x7e {
			return "invalid code"
		}
	}
	return code
}

// httpStatusText 复现 Rust http::StatusCode 的 Display:"429 Too Many Requests"。
func httpStatusText(status int) string {
	reason := http.StatusText(status)
	if reason == "" {
		reason = "<unknown status code>"
	}
	return strconv.Itoa(status) + " " + reason
}

// appendCallbackToken 在 callback URL 上附加可选的回调令牌;token 为空时原样返回。
//
// 使用 net/url 编码 token,避免特殊字符改变 query 边界,并保留 fragment。
func appendCallbackToken(callbackURL, callbackToken, allowedOrigin string) (string, error) {
	if strings.TrimSpace(callbackToken) == "" {
		return callbackURL, nil
	}
	targetOrigin, err := callbackOriginKey(callbackURL)
	if err != nil {
		return "", errors.New("callbackUrl is not an absolute URL")
	}
	normalizedAllowed, err := validateConfiguredCallbackTokenOrigin(allowedOrigin)
	if err != nil {
		return "", errors.New("callback token origin is not configured")
	}
	if targetOrigin != normalizedAllowed {
		return "", errors.New("callbackUrl origin is not allowed to receive the callback token")
	}
	parsed, err := url.Parse(callbackURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", errors.New("callbackUrl is not an absolute URL")
	}
	query := parsed.Query()
	query.Add("token", callbackToken)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func callbackOriginKey(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("invalid callback origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func validateConfiguredCallbackTokenOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("must be an absolute HTTP(S) origin without credentials")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must contain only scheme and host")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", errors.New("must use HTTPS except for loopback development")
	}
	return callbackOriginKey(raw)
}

// callbackOrigin returns a credential-free log/audit target.
func callbackOrigin(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "<invalid callback URL>"
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// maskCallbackToken 把 URL 里的 token 查询参数掩码,避免明文令牌进日志。
func maskCallbackToken(target string) string {
	var out strings.Builder
	rest := target
	for {
		index := strings.Index(strings.ToLower(rest), "token=")
		if index < 0 {
			break
		}
		out.WriteString(rest[:index])
		out.WriteString("token=***")
		valueStart := index + len("token=")
		end := len(rest)
		if cut := strings.IndexAny(rest[valueStart:], "&#"); cut >= 0 {
			end = valueStart + cut
		}
		rest = rest[end:]
	}
	out.WriteString(rest)
	return out.String()
}

func callbackRequestErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "request"
	}
}

// validateCallbackInputs 在发请求之前挡掉明显不可用的入参。
//
// callbackUrl 必须是 HTTPS 绝对地址;HTTP 只对回环地址放行(本地集成测试用)。
func validateCallbackInputs(callbackURL, runID, signingSecret string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("callback runId is empty")
	}
	if strings.TrimSpace(signingSecret) == "" {
		return errors.New("callback signing secret is empty")
	}
	parsed, err := url.Parse(callbackURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return errors.New("callbackUrl is not an absolute URL")
	}
	if parsed.Scheme == "https" {
		if parsed.User != nil {
			return errors.New("callbackUrl must not contain credentials")
		}
		return nil
	}
	if parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		if parsed.User != nil {
			return errors.New("callbackUrl must not contain credentials")
		}
		return nil
	}
	return errors.New("callbackUrl must use HTTPS (HTTP is allowed only for loopback tests)")
}

// isLoopbackHost 判断主机名是否为回环地址。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// derefOrEmpty 便于把可选字符串写进结构化日志。
func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
