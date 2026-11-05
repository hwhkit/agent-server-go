// Package httpx 提供出站 HTTP 机制：有界读取、禁跟随重定向、错误脱敏。
// 签名/重试等策略在 app 层；本包只做安全的传输原语。
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CallbackRequestTimeout 约束一次完整网络尝试（含读取回执）。调用方注入的
// client 可能没有 Timeout，投递 helper 必须自带截止。
const CallbackRequestTimeout = 30 * time.Second

// CallbackResponseBodyLimit 防止 callback 端点让服务缓冲无界回执。
const CallbackResponseBodyLimit = 64 * 1024

// CallbackPoster 是回传投递的 HTTP 实现（满足 app/callback 的 Poster 端口）。
type CallbackPoster struct {
	// Client 为 nil 时使用 http.DefaultClient 的拷贝。
	Client *http.Client
}

// Post 发一次请求并读回响应体。
//
// 安全口径与旧栈一致：签名请求绝不跟随重定向（net/http 会把自定义签名头
// 复制到重定向请求，URL 里还可能有 legacy token）；net/http 错误通常内嵌
// 完整 URL，绝不外传——统一替换为稳定文案。
func (p CallbackPoster) Post(ctx context.Context, target string, headers map[string]string, body []byte) (int, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, CallbackRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	client := p.Client
	safe := http.Client{}
	if client != nil {
		safe = *client
	}
	safe.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	response, err := safe.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, "", context.DeadlineExceeded
		case errors.Is(err, context.Canceled):
			return 0, "", context.Canceled
		default:
			return 0, "", errors.New("callback request failed")
		}
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, CallbackResponseBodyLimit+1))
	if readErr != nil {
		return response.StatusCode, "", errors.New("read callback response failed")
	}
	if len(raw) > CallbackResponseBodyLimit {
		return response.StatusCode, "",
			fmt.Errorf("callback response exceeded %d bytes", CallbackResponseBodyLimit)
	}
	return response.StatusCode, string(raw), nil
}
