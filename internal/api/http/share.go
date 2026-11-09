package http

// 团队分享公开 API（/api/share/v1，无管理会话鉴权——token 即凭证）：
//
//	GET  /api/share/v1/{token}          团队公开视图（脱敏）
//	POST /api/share/v1/{token}/runs     触发运行（限流 429）
//	GET  /api/share/v1/{token}/runs/{id} 轮询状态与交易信号输出
//
// 错误信封与公开页一致：{"error":{"code","message"}}；链接无效/已撤销/
// 团队停用统一 404 share_not_found（不泄露失效原因）。

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/share"
)

// ShareHandler 是公开分享端点。
type ShareHandler struct{ Service *share.Service }

// ShareRouter 组装公开分享子路由（主路由直接挂载，无会话中间件）。
func ShareRouter(handler *ShareHandler) chi.Router {
	router := chi.NewRouter()
	router.Get("/{token}", handler.view)
	router.Post("/{token}/runs", handler.trigger)
	router.Get("/{token}/runs/{run_id}", handler.run)
	return router
}

func (h *ShareHandler) view(w http.ResponseWriter, r *http.Request) {
	view, err := h.Service.View(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": view})
}

func (h *ShareHandler) trigger(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Input     json.RawMessage `json:"input"`
		ClientKey string          `json:"client_key"`
	}
	if r.Body != nil {
		// 与管理端同口径的限长读取；空体视作 {}。
		body := make([]byte, 64*1024+1)
		n, _ := r.Body.Read(body)
		if n > 64*1024 {
			writeShareJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"input must be at most 64KiB")
			return
		}
		if n > 0 {
			if err := json.Unmarshal(body[:n], &payload); err != nil {
				writeShareJSONError(w, http.StatusBadRequest, "invalid_json",
					"request body must be valid JSON")
				return
			}
		}
	}
	if len(payload.Input) == 0 {
		payload.Input = json.RawMessage(`{}`)
	}
	if !json.Valid(payload.Input) {
		writeShareJSONError(w, http.StatusBadRequest, "invalid_input", "input must be valid JSON")
		return
	}
	run, err := h.Service.Trigger(r.Context(), chi.URLParam(r, "token"),
		payload.ClientKey, "share-visitor", payload.Input)
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"data": map[string]any{"id": run.ID, "status": string(run.Status)},
	})
}

func (h *ShareHandler) run(w http.ResponseWriter, r *http.Request) {
	view, err := h.Service.Run(r.Context(), chi.URLParam(r, "token"),
		chi.URLParam(r, "run_id"))
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": view})
}

func writeShareError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, share.ErrRateLimited):
		w.Header().Set("Retry-After", "10")
		writeShareJSONError(w, http.StatusTooManyRequests, "rate_limited",
			"too many triggers; retry later")
	case errors.Is(err, share.ErrNotFound), errors.Is(err, share.ErrNotShared):
		writeShareJSONError(w, http.StatusNotFound, "share_not_found", "share link is invalid")
	default:
		writeShareJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}

func writeShareJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}
