// Package callback 实现异步结果回传用例：签名、有界重试与回执分类。
//
// 重试策略（与旧栈一致）：网络异常、408、429 与 5xx 按 1/3/10/30/60 秒退避
// 重试（共 6 次尝试）；鉴权/参数类 4xx 与 accepted=false **不重试**——前者
// 重试无意义，后者是平台已经明确表态的业务结论，重复投递只会产生重复运行
// 记录。已收到 HTTP 响应但回执缺失/超限/不可解析属于**无法确认**，同样不
// 重试（可能已被接受，重投会造成重复副作用），以 Unconfirmed 错误交调用方
// 保守保持受理状态待对账。
package callback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ir-agent-server/internal/domain/platform"
)

// 投递预算（与旧栈一致）。
const (
	Attempts = 6
)

// DefaultRetryDelays 是第 1..5 次失败后的退避时长；最后一次之后不再等待。
var DefaultRetryDelays = [Attempts - 1]time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// DeliveryError 携带机器可读的投递元数据，且不暴露 callback URL
// （URL 可能含 legacy token）。
type DeliveryError struct {
	StatusCode int
	Attempts   int
	// Unconfirmed 表示 callback 可能已被接受但没有拿到可信回执；
	// 重新执行任务不安全。
	Unconfirmed bool
	Err         error
}

func (e *DeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "callback delivery failed"
	}
	return e.Err.Error()
}

func (e *DeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func deliveryError(status, attempts int, err error) error {
	return &DeliveryError{StatusCode: status, Attempts: attempts, Err: err}
}

func unconfirmedError(status, attempts int, err error) error {
	return &DeliveryError{StatusCode: status, Attempts: attempts, Unconfirmed: true, Err: err}
}

// Poster 是单次 HTTP 投递端口（infra/httpx 实现：限时、有界读取、
// 禁重定向、错误脱敏）。
type Poster interface {
	Post(ctx context.Context, target string, headers map[string]string, body []byte) (status int, responseText string, err error)
}

// Config 是回传凭证配置。
type Config struct {
	SigningSecret string
	// Token / TokenOrigin 是 legacy 平台的「令牌 + 签名」双凭证兼容：
	// Token 非空时必须同时配置 TokenOrigin，且只发给该 origin。
	Token       string
	TokenOrigin string
}

// Sender 是回传投递服务。满足 app/push 的 CallbackSender 端口。
type Sender struct {
	config Config
	poster Poster
	// retryDelays 可注入（测试用短退避）；nil 时用 DefaultRetryDelays。
	retryDelays []time.Duration
}

// NewSender 组装回传服务。
func NewSender(config Config, poster Poster) *Sender {
	return &Sender{config: config, poster: poster, retryDelays: DefaultRetryDelays[:]}
}

// Send 回传结果信封（push.CallbackSender 端口实现），使用配置的签名密钥。
func (s *Sender) Send(ctx context.Context, callbackURL, runID string, result *platform.PayloadV1) (*platform.CallbackReceipt, error) {
	return s.SendAs(ctx, callbackURL, runID, s.config.SigningSecret, result)
}

// SendAs 以指定密钥回传结果信封（动态 Agent 入口使用**每个智能体自己的**
// signing secret 签名回调；令牌/令牌 origin 仍取服务级配置）。
func (s *Sender) SendAs(ctx context.Context, callbackURL, runID, signingSecret string, result *platform.PayloadV1) (receipt *platform.CallbackReceipt, deliveryErr error) {
	attemptsMade := 0
	lastStatus := 0
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("callback delivery panicked",
				"run_id", runID, "attempt", attemptsMade, "status", lastStatus)
			receipt = nil
			deliveryErr = deliveryError(lastStatus, attemptsMade,
				errors.New("callback delivery panicked"))
		}
	}()

	if err := ValidateInputs(callbackURL, runID, signingSecret); err != nil {
		return nil, deliveryError(0, 0, err)
	}
	if result == nil {
		return nil, deliveryError(0, 0, errors.New("callback result is nil"))
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, deliveryError(0, 0,
			fmt.Errorf("serialize callback result failed: %w", err))
	}
	target, err := platform.AppendCallbackToken(callbackURL, s.config.Token, s.config.TokenOrigin)
	if err != nil {
		return nil, deliveryError(0, 0, err)
	}
	logTarget := platform.CallbackOrigin(target)

	for attempt := 0; attempt < Attempts; attempt++ {
		// 最后一次尝试没有后续退避；retryDelay 为 0 表示「不再重试」。
		var retryDelay time.Duration
		if attempt < len(s.retryDelays) {
			retryDelay = s.retryDelays[attempt]
		}
		lastAttempt := attempt == Attempts-1

		timestamp := time.Now().Unix()
		signature := platform.CallbackSignatureV2(signingSecret, runID, body, timestamp)
		attemptsMade = attempt + 1
		slog.Info("sending async callback",
			"run_id", runID, "attempt", attempt+1, "callback_origin", logTarget)

		status, responseText, requestErr := s.poster.Post(ctx, target, map[string]string{
			"Content-Type":                  "application/json",
			platform.HeaderRunID:            runID,
			platform.CreatorSignatureHeader: signature,
		}, body)
		lastStatus = status

		switch {
		case requestErr != nil:
			// 已收到 HTTP 响应后的回执缺失/超限属于无法确认：重试可能重复
			// 已接受的结果，带观测到的状态码交调用方处理。
			if status != 0 && status < 500 && status != http.StatusRequestTimeout &&
				status != http.StatusTooManyRequests {
				return nil, unconfirmedError(status, attemptsMade, requestErr)
			}
			if lastAttempt {
				return nil, unconfirmedError(status, attemptsMade,
					fmt.Errorf("callback request failed after %d attempts: %w",
						Attempts, requestErr))
			}
			slog.Warn("callback request failed; retrying",
				"run_id", runID, "attempt", attempt+1,
				"status", status, "error_kind", requestErrorKind(requestErr))

		case status == http.StatusOK:
			receipt, parseErr := platform.ParseCallbackReceipt(responseText)
			if parseErr != nil {
				slog.Warn("callback returned an unparsable receipt",
					"run_id", runID, "status", status, "response_bytes", len(responseText))
				return nil, unconfirmedError(status, attemptsMade,
					fmt.Errorf("callback returned an invalid receipt: %w", parseErr))
			}
			slog.Info("callback response received",
				"run_id", runID, "status", status, "code", receipt.Code,
				"accepted", receipt.Accepted, "run_status", deref(receipt.RunStatus),
				"response_bytes", len(responseText))
			if receipt.Code != "OK" {
				return nil, unconfirmedError(status, attemptsMade,
					fmt.Errorf("callback transport code was %s", receipt.Code))
			}
			if receipt.Accepted {
				slog.Info("callback result accepted",
					"run_id", runID, "run_status", deref(receipt.RunStatus))
			} else {
				slog.Warn("callback delivered but result was not accepted; not retrying",
					"run_id", runID, "run_status", deref(receipt.RunStatus))
			}
			return receipt, nil

		default:
			retryable := status == http.StatusRequestTimeout ||
				status == http.StatusTooManyRequests || status >= 500
			responseCode := platform.CallbackResponseCode(responseText)
			slog.Warn("callback rejected",
				"run_id", runID, "status", status, "attempt", attempt+1,
				"response_code", responseCode, "response_bytes", len(responseText))
			if !retryable || lastAttempt {
				build := deliveryError
				if retryable {
					build = unconfirmedError
				}
				return nil, build(status, attemptsMade,
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
				return nil, unconfirmedError(lastStatus, attemptsMade,
					fmt.Errorf("callback retry aborted: %w", ctx.Err()))
			}
		}
	}
	return nil, unconfirmedError(lastStatus, attemptsMade,
		errors.New("callback retry loop exhausted"))
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("not an absolute URL")
	}
	return parsed, nil
}

// ValidateInputs 在发请求之前挡掉明显不可用的入参。
//
// callbackUrl 必须是 HTTPS 绝对地址；HTTP 只对回环地址放行（本地集成测试用）。
func ValidateInputs(callbackURL, runID, signingSecret string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("callback runId is empty")
	}
	if strings.TrimSpace(signingSecret) == "" {
		return errors.New("callback signing secret is empty")
	}
	parsed, err := parseAbsoluteURL(callbackURL)
	if err != nil {
		return errors.New("callbackUrl is not an absolute URL")
	}
	if parsed.User != nil {
		return errors.New("callbackUrl must not contain credentials")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && platform.IsLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return errors.New("callbackUrl must use HTTPS (HTTP is allowed only for loopback tests)")
}

func requestErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "request"
	}
}

// httpStatusText 复现 Rust http::StatusCode 的 Display："429 Too Many Requests"。
func httpStatusText(status int) string {
	reason := http.StatusText(status)
	if reason == "" {
		reason = "<unknown status code>"
	}
	return strconv.Itoa(status) + " " + reason
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
