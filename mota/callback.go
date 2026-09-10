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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// callbackAttempts 是一次回传最多尝试的次数。
const callbackAttempts = 6

// callbackRetryDelays 是第 1..5 次失败后的退避时长;第 6 次(最后一次)之后不再等待。
var callbackRetryDelays = [callbackAttempts - 1]time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// callbackLogBodyLimit 是日志里保留的响应体上限。
const callbackLogBodyLimit = 1024

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
// 重试策略:网络异常、429 与 5xx 会按 1/3/10/30/60 秒退避重试(共 6 次尝试);
// 鉴权/参数类 4xx 与 accepted=false **不重试** —— 前者重试无意义,后者是平台
// 已经明确表态的业务结论,重复投递只会产生重复运行记录。
//
// callbackToken 非空时附加 ?token=(或 &token=)到 callback URL,兼容要求
// 「令牌 + 签名」双凭证的存量平台;新平台(2026-08-17 起)令牌可选,传空串即可。
func SendCallback(
	ctx context.Context,
	client *http.Client,
	callbackURL, runID, signingSecret, callbackToken string,
	result *PayloadV1,
) (*CallbackReceipt, error) {
	if err := validateCallbackInputs(callbackURL, runID, signingSecret); err != nil {
		return nil, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("serialize callback result failed: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	target := appendCallbackToken(callbackURL, callbackToken)
	masked := maskCallbackToken(target)

	for attempt := 0; attempt < callbackAttempts; attempt++ {
		// 最后一次尝试没有后续退避;retryDelay 为 0 表示「不再重试」。
		var retryDelay time.Duration
		if attempt < len(callbackRetryDelays) {
			retryDelay = callbackRetryDelays[attempt]
		}
		lastAttempt := attempt == callbackAttempts-1

		timestamp := time.Now().Unix()
		signature := CallbackSignatureV2(signingSecret, runID, body, timestamp)
		slog.Info("sending async callback",
			"run_id", runID, "attempt", attempt+1, "callback_url", masked)

		status, responseText, requestErr := postCallback(
			ctx, client, target, runID, signature, body)

		switch {
		case requestErr != nil:
			if lastAttempt {
				return nil, fmt.Errorf("callback request failed after %d attempts: %w",
					callbackAttempts, requestErr)
			}
			slog.Warn("callback request failed; retrying",
				"run_id", runID, "attempt", attempt+1, "error", requestErr)

		case status == http.StatusOK:
			receipt, parseErr := parseCallbackReceipt(responseText)
			if parseErr != nil {
				slog.Warn("callback returned an unparsable receipt",
					"run_id", runID, "status", status,
					"response_body", truncateForLog(responseText))
				return nil, fmt.Errorf("callback returned an invalid receipt: %w", parseErr)
			}
			slog.Info("callback response received",
				"run_id", runID, "status", status, "code", receipt.Code,
				"accepted", receipt.Accepted, "run_status", derefOrEmpty(receipt.RunStatus),
				"run_msg", derefOrEmpty(receipt.RunMsg),
				"response_body", truncateForLog(responseText))
			if receipt.Code != "OK" {
				return nil, fmt.Errorf("callback transport code was %s", receipt.Code)
			}
			if receipt.Accepted {
				slog.Info("callback result accepted",
					"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus))
			} else {
				slog.Warn("callback delivered but result was not accepted; not retrying",
					"run_id", runID, "run_status", derefOrEmpty(receipt.RunStatus),
					"run_msg", derefOrEmpty(receipt.RunMsg))
			}
			return receipt, nil

		default:
			retryable := status == http.StatusTooManyRequests || status >= 500
			responseCode := callbackResponseCode(responseText)
			slog.Warn("callback rejected",
				"run_id", runID, "status", status, "attempt", attempt+1,
				"response_code", responseCode, "response_body", truncateForLog(responseText))
			if !retryable || lastAttempt {
				return nil, fmt.Errorf("callback rejected with %s (%s)",
					httpStatusText(status), responseCode)
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
				return nil, fmt.Errorf("callback retry aborted: %w", ctx.Err())
			}
		}
	}
	return nil, errors.New("callback retry loop exhausted")
}

// postCallback 发一次回传请求并读回响应体。
func postCallback(
	ctx context.Context,
	client *http.Client,
	target, runID, signature string,
	body []byte,
) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(RunIDHeader, runID)
	request.Header.Set(CreatorSignatureHeader, signature)
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		// rs 侧读体失败会退化成一段占位文本而不是整体失败,保持一致。
		return response.StatusCode, fmt.Sprintf("<unreadable: %v>", readErr), nil
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
		return code
	}
	return "no code"
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
// 注意不对 token 做百分号转义 —— 与 rs 一致,平台下发的令牌本身就是 URL 安全的,
// 转义反而会导致校验失败。
func appendCallbackToken(callbackURL, callbackToken string) string {
	if strings.TrimSpace(callbackToken) == "" {
		return callbackURL
	}
	separator := "?"
	if strings.Contains(callbackURL, "?") {
		separator = "&"
	}
	return callbackURL + separator + "token=" + callbackToken
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

// truncateForLog 截断过长的响应体。
func truncateForLog(value string) string {
	if len(value) <= callbackLogBodyLimit {
		return value
	}
	return fmt.Sprintf("%s…<truncated %d bytes>",
		value[:callbackLogBodyLimit], len(value)-callbackLogBodyLimit)
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
		return nil
	}
	if parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return errors.New("callbackUrl must use HTTPS (HTTP is allowed only for loopback tests)")
}

// isLoopbackHost 判断主机名是否为回环地址。
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// derefOrEmpty 便于把可选字符串写进结构化日志。
func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
