package http

import (
	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/dashboard"
)

// RouterDeps 是 HTTP 装配依赖。可选项为 nil 时对应路由不挂载
// （与旧栈「按配置启用」的语义一致）。
type RouterDeps struct {
	// PushPath 是 Push 端点路径（配置 [ir].push_path，缺省 /ir/push）。
	PushPath string
	Push     *PushHandler
	// AgentAPI 为 nil 表示 AGENT_API__ENABLED=false。
	AgentAPI *AgentAPIHandler
	// Admin 为 nil 表示 PLATFORM__ENABLED=false。
	Admin chi.Router
	// Dynamic 为动态 Agent 入口（/agent/{slug} 与别名路径）。
	Dynamic *DynamicHandler
	// A2A 为 nil 表示 [a2a].enabled=false。
	A2A *A2ADeps
	// Health 是就绪探针的必需项（数据库探活）。
	Health dashboard.HealthSource
	// Readiness 是就绪探针的补充检查项（B7：迁移标记/队列/注册表）。
	Readiness []ReadinessCheck
	// Metrics 非 nil 时挂 GET /metrics（Prometheus 文本格式）并为全部
	// 路由启用请求计数中间件。
	Metrics *MetricsRegistry
}

// NewRouter 组装服务的完整 HTTP 路由。
func NewRouter(deps RouterDeps) chi.Router {
	router := chi.NewRouter()
	router.Use(SecurityHeaders)
	// chi 要求：中间件必须在任何路由注册之前 Use。/metrics 端点由 hwhkit
	// 框架在外层 mux 提供（promhttp），这里只做请求计数。
	if deps.Metrics != nil {
		router.Use(deps.Metrics.Instrument)
	}

	router.Get("/health", HealthHandler())
	router.Get("/health/ready", ReadyHandler(deps.Health, deps.Readiness))

	if deps.Push != nil {
		pushPath := deps.PushPath
		if pushPath == "" {
			pushPath = "/ir/push"
		}
		router.Post(pushPath, deps.Push.ServeHTTP)
	}
	if deps.AgentAPI != nil {
		router.Mount("/agent-api/v1", deps.AgentAPI.Router())
	}
	if deps.Dynamic != nil {
		MountDynamic(router, deps.Dynamic)
	}
	if deps.A2A != nil {
		MountA2A(router, *deps.A2A)
	}
	if deps.Admin != nil {
		router.Mount("/", deps.Admin)
	}
	return router
}
