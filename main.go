package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hwhkit/hwhkit-go/config"
	"github.com/hwhkit/hwhkit-go/core/appctx"
	"github.com/hwhkit/hwhkit-go/hwhkit"
	"github.com/hwhkit/hwhkit-go/hwhkit/production"
	"github.com/hwhkit/hwhkit-go/observability"

	"mota-agent-server/mota"
)

// App 组装 HTTP 路由。Push 与动态 Agent 网关(/agent/{slug})、管理台三组路由
// 共用同一份 MOTA 配置与(如启用平台)同一个执行后端。
// 对应 Rust 侧 src/app.rs 的 MotaAgentApp。
type App struct {
	processor mota.Processor
	config    *mota.Config
	platform  *mota.PlatformState
	// execution 是 Push 使用的统一执行后端:平台开启时指向共享的
	// AgentDispatcher(与 Pull worker/Push 心跳共用同一套准入闸与执行闸);
	// 平台关闭时回落到嵌入模式的进程内 Processor。
	execution *mota.TaskExecutionBackend
}

func (a App) BuildRouter(
	ctx context.Context,
	app *appctx.Context,
	_ *config.AppConfig,
) (http.Handler, error) {
	motaCfg := a.config
	var err error
	if motaCfg == nil {
		motaCfg, err = mota.LoadConfig()
	}
	if err != nil {
		return nil, err
	}
	appctx.Insert(app, motaCfg)

	r := chi.NewRouter()
	if motaCfg.PushEnabled {
		slog.Info("mota push endpoint enabled", "path", motaCfg.PushPath)
		handler, err := a.pushHandler(ctx, motaCfg)
		if err != nil {
			return nil, fmt.Errorf("build push handler: %w", err)
		}
		r.Post(motaCfg.PushPath, handler)
	} else {
		slog.Warn("mota push endpoint disabled (MOTA_PUSH_ENABLED=false)")
	}
	if a.platform != nil {
		r.Post("/agent/{slug}", a.platform.Gateway)
		admin := &mota.AdminHandler{State: a.platform}
		// AdminHandler dispatches on the complete path. Mount would strip the
		// prefix before forwarding and make every management route look unknown.
		r.Handle("/admin", admin)
		r.Handle("/admin/*", admin)
		r.Handle("/api/admin/*", admin)
	}
	return r, nil
}

// pushHandler 组装生产 Push 入口:统一执行后端 + PostgreSQL 幂等存储。
//
// Push 的跨进程幂等状态必须落 PostgreSQL(与 rs 一致)——内存后端只给单元
// 测试用,起两个副本或重启都会重复执行一次有副作用的任务。平台关闭时也一样
// 要求 DATABASE_URL,这是协议层的硬约束,不是平台特性的附带条件。
func (a App) pushHandler(ctx context.Context, cfg *mota.Config) (http.HandlerFunc, error) {
	execution := a.execution
	if execution == nil {
		execution = mota.NewProcessorBackend(a.processor)
	}

	var callbackClient *http.Client
	var idempotency *mota.PushIdempotencyStore
	var err error
	if a.platform != nil {
		callbackClient = a.platform.CallbackHTTPClient
		idempotency, err = mota.NewPushIdempotencyStoreFromPool(ctx, a.platform.Store.Pool())
	} else {
		databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if databaseURL == "" {
			return nil, fmt.Errorf("DATABASE_URL must be set for PostgreSQL storage")
		}
		idempotency, err = mota.ConnectPushIdempotencyStore(ctx, databaseURL)
	}
	if err != nil {
		return nil, err
	}
	return mota.NewPushHandlerWithBackend(cfg, execution, idempotency, callbackClient), nil
}

func main() {
	processor := mota.PlaceholderProcessor{}
	motaCfg, err := mota.LoadConfig()
	if err != nil {
		slog.Error("mota config failed", "err", err)
		os.Exit(1)
	}

	var platform *mota.PlatformState
	platformFlag := strings.TrimSpace(strings.ToLower(os.Getenv("PLATFORM__ENABLED")))
	platformDisabled := platformFlag == "false" || platformFlag == "0" || platformFlag == "off" || platformFlag == "no"
	platformRequested := platformFlag != "" || os.Getenv("ADMIN__SESSION_SECRET") != "" || os.Getenv("ADMIN__PASSWORD") != "" || os.Getenv("ADMIN__PASSWORD_HASH") != ""
	if platformRequested && !platformDisabled {
		platformCfg, loadErr := mota.LoadPlatformConfig()
		if loadErr != nil {
			slog.Error("platform config failed", "err", loadErr)
			os.Exit(1)
		}
		// 两个分工不同的 HTTP 客户端(与 rs main.rs 一致):
		//   - executorHTTPClient 给网关直调(litellm 等)用:不设总超时,
		//     总时限由 AgentExecutor 的单一 deadline 控制;Client 只限制
		//     建连并复用长连接,避免全局超时截断长推理请求。
		//   - callbackHTTPClient 给异步 callback 回传与心跳用:较短的
		//     连接/总超时,避免下游异常拖住整条回调/心跳循环。
		executorHTTPClient := &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 60 * time.Second,
				}).DialContext,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 32,
			},
		}
		callbackHTTPClient := &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
			Timeout: 30 * time.Second,
		}

		platform, loadErr = mota.NewPlatformStateWithDependencies(
			context.Background(), platformCfg, processor, mota.PlatformDependencies{
				ExecutorHTTPClient:           executorHTTPClient,
				CallbackHTTPClient:           callbackHTTPClient,
				SigningSecret:                motaCfg.SigningSecret,
				CallbackToken:                motaCfg.CallbackToken,
				TrustedUsageReportingEnabled: motaCfg.TrustedUsageReportingEnabled,
				DefaultAgentSlug:             motaCfg.DefaultAgentSlug,
			})
		if loadErr != nil {
			slog.Error("platform initialization failed", "err", loadErr)
			os.Exit(1)
		}
		defer platform.Close()
	}

	// Pull worker 与 Push 心跳与 HTTP 服务共用同一执行后端、配置快照与退出
	// 信号(与 rs main.rs 的启动顺序一致)。平台开启时三者都走同一个共享
	// Dispatcher();关闭时回落到进程内 Processor(嵌入模式)。
	execution := mota.NewProcessorBackend(processor)
	var heartbeatStore *mota.AgentStore
	if platform != nil {
		dispatcher, dispatchErr := platform.Dispatcher()
		if dispatchErr != nil {
			slog.Error("build agent dispatcher failed", "err", dispatchErr)
			os.Exit(1)
		}
		execution = mota.NewAgentBackend(dispatcher)
		heartbeatStore = platform.Store
	}

	built, err := hwhkit.Run(context.Background(),
		App{processor: processor, config: motaCfg, platform: platform, execution: execution},
		config.DefaultBootstrap())
	if err != nil {
		slog.Error("bootstrap failed", "err", err)
		os.Exit(1)
	}

	logger := observability.InitLogging(built.Config().Observability.Log)

	if cfg, ok := appctx.Get[mota.Config](built.AppContext()); ok {
		if cfg.PullEnabled {
			worker := mota.NewWorkerWithBackend(cfg, execution, built.Shutdown())
			worker.Start()
			defer worker.Stop()
		} else {
			slog.Warn("pull worker disabled (MOTA_PULL_ENABLED=false)")
		}
		if strings.TrimSpace(cfg.HeartbeatURL) != "" {
			go mota.RunPushHeartbeat(cfg, execution, built.Shutdown(), heartbeatStore)
		}
	}

	if err := production.Serve(context.Background(), built, logger); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
