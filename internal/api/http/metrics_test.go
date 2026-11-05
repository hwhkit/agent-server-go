package http_test

// 指标载体测试：Instrument 中间件计数（路由模式进标签、unmatched 归并、
// 隐式/显式状态码）与抓取时回调采集器（const metric 推送）。渲染本身由
// promhttp 负责，这里用 client_golang 的 testutil 断言收集面。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	apihttp "ir-agent-server/internal/api/http"
)

func TestMetricsInstrument(t *testing.T) {
	reg := apihttp.NewMetricsRegistry()
	router := chi.NewRouter()
	router.Use(reg.Instrument)
	router.Get("/agents/{slug}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 显式状态码
	})
	router.Post("/teams", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) // 隐式 200
	})
	server := httptest.NewServer(router)
	defer server.Close()

	for _, request := range []struct{ method, path string }{
		{"GET", "/agents/smoke-1"}, {"GET", "/agents/smoke-2"}, {"POST", "/teams"}, {"GET", "/nope"},
	} {
		req, err := http.NewRequest(request.method, server.URL+request.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}

	// 路由模式进标签（具体 slug 不进——基数防护），隐式 200 与显式 418
	// 都被计数，未匹配路径归入 unmatched。
	expected := `
# HELP ir_http_requests_total HTTP requests handled.
# TYPE ir_http_requests_total counter
ir_http_requests_total{method="GET",path="/agents/{slug}",status="418"} 2
ir_http_requests_total{method="GET",path="unmatched",status="404"} 1
ir_http_requests_total{method="POST",path="/teams",status="200"} 1
`
	if err := testutil.CollectAndCompare(reg.RequestsCounterForTest(),
		strings.NewReader(expected)); err != nil {
		t.Error(err)
	}
}

// TestMetricsCollectorCallback 验证抓取时回调经 RegisterTo 注册后输出
// const metric；回调内部失败由回调自行降级（不推送即不出现）。
func TestMetricsCollectorCallback(t *testing.T) {
	reg := apihttp.NewMetricsRegistry()
	reg.RegisterCollector(func(ch chan<- prometheus.Metric) {
		desc := prometheus.NewDesc("ir_test_gauge", "Test gauge.", []string{"kind"}, nil)
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 7, "ok")
	})
	registry := prometheus.NewRegistry()
	reg.RegisterTo(registry)

	expected := `
# HELP ir_test_gauge Test gauge.
# TYPE ir_test_gauge gauge
ir_test_gauge{kind="ok"} 7
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected),
		"ir_test_gauge"); err != nil {
		t.Error(err)
	}
}

// TestMetricsContextFree 确认中间件在无路由上下文（直接 handler 调用）
// 时也安全（pattern 归入 unmatched）。
func TestMetricsContextFree(t *testing.T) {
	reg := apihttp.NewMetricsRegistry()
	handler := reg.Instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/anything", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	if count := testutil.ToFloat64(reg.RequestsCounterForTest().
		WithLabelValues("POST", "unmatched", "202")); count != 1 {
		t.Errorf("unmatched count = %v", count)
	}
}
