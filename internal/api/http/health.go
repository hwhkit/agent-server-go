package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"ir-agent-server/internal/app/dashboard"
)

// HealthHandler 是存活探针（GET /health）。
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	}
}

// ReadinessCheck 是一条就绪检查：Essential 失败 → 整体 503（unavailable）；
// 非 Essential 失败 → 200 degraded（服务可用但该子系统降级）。
type ReadinessCheck struct {
	Name      string
	Essential bool
	Check     func(ctx context.Context) error
}

// ReadyHandler 是就绪探针（GET /health/ready）：逐项执行 checks 并填充
// checks 数组（name/status/detail/latency_ms，与框架端点形态一致）。
// health 为存量必需项（数据库探活，Essential），nil 时跳过。
func ReadyHandler(health dashboard.HealthSource, checks []ReadinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type checkResult struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Detail  string `json:"detail,omitempty"`
			Latency int64  `json:"latency_ms"`
		}
		results := make([]checkResult, 0, len(checks)+1)
		overall := "healthy"

		if health != nil {
			started := time.Now()
			result := checkResult{Name: "database"}
			if err := health.Ping(r.Context()); err != nil {
				result.Status = "unavailable"
				result.Detail = "ping failed"
				overall = "unavailable"
			} else {
				result.Status = "ok"
				result.Latency = time.Since(started).Milliseconds()
			}
			results = append(results, result)
		}
		for _, check := range checks {
			started := time.Now()
			result := checkResult{Name: check.Name}
			if err := check.Check(r.Context()); err != nil {
				result.Status = "unavailable"
				result.Detail = err.Error()
				if check.Essential {
					overall = "unavailable"
				} else if overall == "healthy" {
					overall = "degraded"
				}
			} else {
				result.Status = "ok"
			}
			result.Latency = time.Since(started).Milliseconds()
			results = append(results, result)
		}

		status := http.StatusOK
		if overall == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"status": overall, "checks": results})
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
