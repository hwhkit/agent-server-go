package http

import (
	"errors"
	"io"
	"net/http"

	"github.com/hwhkit/hwhkit-go/core/apierror"

	"ir-agent-server/internal/app/push"
	"ir-agent-server/internal/domain/platform"
)

// PushHandler 是 POST {pushPath} 的 HTTP 适配：拆头、限长读体、调用
// app/push 用例、渲染 problem+json（与旧栈 apierror 线格式一致）。
type PushHandler struct {
	Service *push.Service
	// MaxRequestBytes 是请求体上限（超限 400；<=0 表示不限）。
	MaxRequestBytes int64
}

// ServeHTTP 处理一次 Push 请求。
func (h PushHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, apierror.New(http.StatusMethodNotAllowed, "Method Not Allowed",
			"push endpoint accepts POST only"))
		return
	}
	body, readErr := readBoundedBody(r.Body, h.MaxRequestBytes)
	request := push.Request{
		ContentType:     r.Header.Get("Content-Type"),
		RunIDHeader:     r.Header.Values(platform.HeaderRunID),
		IdempotencyKey:  r.Header.Values(platform.HeaderIdempotencyKey),
		SignatureHeader: r.Header.Values(platform.HeaderSignature),
		Body:            body,
		BodyReadFailed:  readErr != nil,
	}
	response, err := h.Service.Handle(r.Context(), request)
	if err != nil {
		var requestErr *push.RequestError
		if errors.As(err, &requestErr) {
			writeAPIError(w, toAPIError(requestErr))
			return
		}
		writeAPIError(w, apierror.Internal("push processing failed"))
		return
	}
	if response.Status == http.StatusAccepted {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.Status)
	_, _ = io.WriteString(w, response.Body)
}

// toAPIError 把用例层结构化拒绝映射为线格式（字段一一对应）。
func toAPIError(requestErr *push.RequestError) *apierror.ApiError {
	out := &apierror.ApiError{
		Status: requestErr.Status,
		Type:   requestErr.Type,
		Title:  requestErr.Title,
		Detail: requestErr.Detail,
	}
	if requestErr.Title == "" {
		out.Title = http.StatusText(requestErr.Status)
	}
	for _, field := range requestErr.Fields {
		out.Fields = append(out.Fields, apierror.FieldError{Field: field.Field, Message: field.Message})
	}
	return out
}

func writeAPIError(w http.ResponseWriter, apiErr *apierror.ApiError) {
	apiErr.WriteJSON(w)
}

// readBoundedBody 限长读取请求体。超限时返回 limit+1 字节且不报错——
// 由用例层按 MaxRequestBytes 判定并给出稳定的 400 文案；这里只报告
// 真实的读取失败。
func readBoundedBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer func() { _ = body.Close() }()
	if limit <= 0 {
		return io.ReadAll(body)
	}
	return io.ReadAll(io.LimitReader(body, limit+1))
}
