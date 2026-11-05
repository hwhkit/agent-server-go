// 运维指标接入：基于 prometheus/client_golang（hwhkit 框架的 production.Serve
// 已在外层 mux 挂 promhttp /metrics 并持有框架 Registry——本文件只提供
// 指标载体与 HTTP 计数中间件，注册动作在装配根（cmd）于 hwhkit.Run 之后
// 执行 RegisterTo(frameworkRegistry)）。
//
// 整洁架构口径：指标是接入/运维关注点，载体留在 api 层；域统计（agents/
// queue/runs 等 gauge）以「抓取时回调」注入——查询成本由抓取间隔承担，
// 单项失败降级跳过，不让指标端点成为新故障面。
package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// MetricsRegistry 持有 HTTP 请求计数器与自定义采集器列表。零值不可用，
// 经 NewMetricsRegistry 构造；在 hwhkit.Run 之后 RegisterTo 框架注册表。
type MetricsRegistry struct {
	requests   *prometheus.CounterVec
	collectors []func(ch chan<- prometheus.Metric)
}

// NewMetricsRegistry 组装指标载体（计数器尚未注册，Inc 安全无副作用）。
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ir_http_requests_total",
			Help: "HTTP requests handled.",
		}, []string{"method", "path", "status"}),
	}
}

// RegisterTo 把计数器与全部自定义采集器注册到框架 Registry。
func (r *MetricsRegistry) RegisterTo(registry *prometheus.Registry) {
	registry.MustRegister(r.requests)
	for _, collect := range r.collectors {
		registry.MustRegister(&metricsCallbackCollector{fn: collect})
	}
}

// RegisterCollector 追加一个抓取时回调：每次 /metrics 抓取执行一次，
// 查询仓储/运行时后经 ch 推送 const metric。失败时跳过该批指标。
func (r *MetricsRegistry) RegisterCollector(collect func(ch chan<- prometheus.Metric)) {
	r.collectors = append(r.collectors, collect)
}

// metricsCallbackCollector 把抓取时回调适配为 prometheus.Collector
// （Describe 为空 = 无固定描述，配合 MustNewConstMetric 动态输出）。
type metricsCallbackCollector struct {
	fn func(ch chan<- prometheus.Metric)
}

func (c *metricsCallbackCollector) Describe(chan<- *prometheus.Desc) {}

func (c *metricsCallbackCollector) Collect(ch chan<- prometheus.Metric) {
	c.fn(ch)
}

// Instrument 为全部请求累加 ir_http_requests_total{method,path,status}。
// path 取 chi 路由模式；未匹配路由一律 "unmatched"——具体 id/slug 永不
// 进入标签，防止标签基数爆炸。
func (r *MetricsRegistry) Instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		stats := &metricsStatusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(stats, req)
		pattern := chi.RouteContext(req.Context()).RoutePattern()
		if pattern == "" {
			pattern = "unmatched"
		}
		r.requests.WithLabelValues(req.Method, pattern, itoa(stats.code)).Inc()
	})
}

// itoa 避免中间件热路径上的 fmt 开销。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	buf := [12]byte{}
	i := len(buf)
	for v != 0 {
		i--
		d := v % 10
		if negative {
			d = -d
		}
		buf[i] = byte('0' + d)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// metricsStatusWriter 捕获响应状态码（显式与隐式 200 都计入）。
type metricsStatusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *metricsStatusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.code = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *metricsStatusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// RequestsCounterForTest 暴露请求计数器给测试断言（testutil）。
func (r *MetricsRegistry) RequestsCounterForTest() *prometheus.CounterVec {
	return r.requests
}
