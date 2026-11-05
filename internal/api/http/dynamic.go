package http

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/dynamic"
	"ir-agent-server/internal/domain/platform"
)

// DynamicHandler 是 POST /agent/{slug} 与 /agent-center-go/{slug} 的
// HTTP 适配：路由参数/多值头提取、限长读体、溯源头与鉴权挑战渲染。
type DynamicHandler struct {
	Service *dynamic.Service
	// MaxRequestBytes 是请求体上限（与用例层配置同值；api 层负责限长读取）。
	MaxRequestBytes int64
}

// ServeHTTP 处理一次动态入口请求。
func (h DynamicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		// 直接单测/嵌入调用没有 chi 路由上下文时仍兼容按路径解析。
		for _, prefix := range []string{"/agent/", "/agent-center-go/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				slug = strings.TrimPrefix(r.URL.Path, prefix)
				break
			}
		}
	}
	body, readErr := readBoundedBody(r.Body, h.MaxRequestBytes)
	if readErr != nil {
		body = nil
	}
	response := h.Service.Handle(r.Context(), dynamic.Request{
		Slug:            slug,
		RunIDHeader:     r.Header.Values(platform.HeaderAgentRunID),
		IdempotencyKey:  r.Header.Values(platform.HeaderIdempotencyKeyAgent),
		AgentSignature:  r.Header.Values(platform.HeaderAgentSignature),
		LegacySignature: r.Header.Values(platform.HeaderSignature),
		Timestamp:       r.Header.Values(platform.HeaderAgentTimestamp),
		Nonce:           r.Header.Values(platform.HeaderAgentNonce),
		Body:            body,
	})

	if response.AuthError {
		w.Header().Set("WWW-Authenticate", `HMAC realm="agent"`)
	}
	if response.RetryAfter != "" {
		w.Header().Set("Retry-After", response.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if response.Slug != "" && !response.Accepted {
		// 成功与缓存回放都带 Agent 溯源头。
		w.Header().Set("X-Agent-Slug", response.Slug)
		w.Header().Set("X-Agent-Revision", strconv.FormatInt(response.Revision, 10))
		w.Header().Set("X-Agent-Run-Id", response.RunID)
		if response.Replayed {
			w.Header().Set("X-Idempotent-Replay", "true")
		}
	}
	if response.Accepted {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(response.Status)
	_, _ = w.Write([]byte(response.Body))
}

// MountDynamic 把动态入口挂到两个别名路径（与既有平台调用方形态一致）。
func MountDynamic(router chi.Router, handler *DynamicHandler) {
	router.Post("/agent/{slug}", handler.ServeHTTP)
	router.Post("/agent-center-go/{slug}", handler.ServeHTTP)
}
