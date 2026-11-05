package http

import (
	"encoding/json"
	"net/http"

	"ir-agent-server/internal/app/dashboard"
)

// HealthHandler 是存活探针（GET /health）。
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	}
}

// ReadyHandler 是就绪探针（GET /health/ready）：存储探活失败返回 503。
func ReadyHandler(health dashboard.HealthSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if health != nil {
			if err := health.Ping(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]string{"status": "unavailable", "detail": "database ping failed"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "checks": []any{}})
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
