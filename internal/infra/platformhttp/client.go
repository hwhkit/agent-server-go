// Package platformhttp 实现上游平台队列的出站 HTTP 客户端
// （app/worker 的 PlatformClient 端口）：Bearer bind token、URL 拼接、
// 有界响应读取与同源重定向策略。
package platformhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config 是客户端配置。
type Config struct {
	// QueueURL 是平台队列基址（绝对 HTTPS；回环开发允许 HTTP，由配置层校验）。
	QueueURL string
	// BindToken 是平台签发的绑定令牌，以 Authorization: Bearer 携带。
	BindToken string
	// RequestTimeout 约束单次请求（claim 长轮询需要覆盖服务端等待窗口）。
	RequestTimeout time.Duration
}

// Client 是平台队列客户端。
type Client struct {
	config Config
	http   *http.Client
}

// NewClient 组装客户端。
func NewClient(config Config) *Client {
	return &Client{
		config: config,
		http: &http.Client{
			Timeout:       config.RequestTimeout,
			CheckRedirect: CredentialedRedirectPolicy,
		},
	}
}

// Post 发送一次队列请求并读回有界响应体。
func (c *Client) Post(ctx context.Context, path string, query map[string]string,
	body []byte, headers map[string]string, responseLimit int) (int, []byte, error) {
	url := strings.TrimRight(c.config.QueueURL, "/") + path
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if len(query) > 0 {
		values := request.URL.Query()
		for key, value := range query {
			values.Set(key, value)
		}
		request.URL.RawQuery = values.Encode()
	}
	request.Header.Set("Authorization", "Bearer "+c.config.BindToken)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if responseLimit <= 0 {
		return response.StatusCode, nil, errors.New("response body limit must be positive")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil {
		return response.StatusCode, nil, errors.New("read response body failed")
	}
	if len(raw) > responseLimit {
		return response.StatusCode, nil, fmt.Errorf("response body exceeded %d bytes", responseLimit)
	}
	return response.StatusCode, raw, nil
}

// CredentialedRedirectPolicy 只允许同源重定向（scheme+host 一致），最多 10 跳。
// 携带凭据的请求跟随跨源重定向等于把 bind token 交给第三方。
func CredentialedRedirectPolicy(request *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if request == nil || request.URL == nil || via[0] == nil || via[0].URL == nil {
		return errors.New("refusing redirect with an invalid origin")
	}
	original := via[0].URL
	if !strings.EqualFold(request.URL.Scheme, original.Scheme) ||
		!strings.EqualFold(request.URL.Host, original.Host) {
		return errors.New("refusing cross-origin redirect for a credentialed request")
	}
	return nil
}
