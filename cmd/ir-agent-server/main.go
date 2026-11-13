// ir-agent-server 组装根（composition root）。
//
// 整洁架构口径：本文件是全系统唯一允许「认识所有层」的地方——加载配置、
// 打开存储、构造 infra 适配器、注入 app 用例服务、拼装 api 路由、启动后台
// 循环。任何业务规则都不在这里实现。
//
// 对应旧栈根 main.go + ir/platform.go 的装配职责；旧栈的进程内 Agent
// Registry（2s 轮询刷新）被「按 slug 直读数据库」取代——仓储每次执行都
// 读到已提交的最新配置，不再有收敛窗口。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	hwconfig "github.com/hwhkit/hwhkit-go/config"
	"github.com/hwhkit/hwhkit-go/core/appctx"
	"github.com/hwhkit/hwhkit-go/core/health"
	"github.com/hwhkit/hwhkit-go/hwhkit"
	"github.com/hwhkit/hwhkit-go/hwhkit/production"
	"github.com/hwhkit/hwhkit-go/observability"
	"github.com/prometheus/client_golang/prometheus"

	apihttp "ir-agent-server/internal/api/http"
	"ir-agent-server/internal/app/a2a"
	"ir-agent-server/internal/app/admin"
	"ir-agent-server/internal/app/agentapi"
	"ir-agent-server/internal/app/callback"
	"ir-agent-server/internal/app/catalog"
	"ir-agent-server/internal/app/dashboard"
	"ir-agent-server/internal/app/dispatch"
	"ir-agent-server/internal/app/dynamic"
	"ir-agent-server/internal/app/push"
	"ir-agent-server/internal/app/share"
	"ir-agent-server/internal/app/teamrun"
	"ir-agent-server/internal/app/worker"
	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/platform"
	"ir-agent-server/internal/domain/team"
	infrabackend "ir-agent-server/internal/infra/backend"
	"ir-agent-server/internal/infra/config"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/execengine"
	"ir-agent-server/internal/infra/httpx"
	"ir-agent-server/internal/infra/platformhttp"
	"ir-agent-server/internal/infra/postgres"
	"ir-agent-server/web"
)

// version 由构建注入（Makefile ldflags）；未注入时保持 dev。
var version = "dev"

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

// runtime 持有组装完成的服务与需要在停机时收口的资源。
type runtime struct {
	router          chi.Router
	db              *postgres.DB
	providers       *infrabackend.Manager
	pushSvc         *push.Service
	dynamic         *dynamic.Service
	team            *teamrun.Engine
	connectors      *agentapi.Service
	agentAPIEnabled bool
	pullWorker      *worker.Worker
	heartbeat       *worker.PushHeartbeat
	metrics         *apihttp.MetricsRegistry
	pushIdempotency *postgres.PushIdempotencyStore
	retention       *postgres.RetentionStore
	retentionDays   int
	// healthChecks 由 buildRuntime 用其局部仓储构造，main 里注册到框架
	// 健康注册表（/health/ready checks 数组的数据源）。
	healthChecks []health.Check
}

// agentResolver 把智能体仓储适配为 dispatch.Resolver：停用与未命中一律
// ErrNotFound（旧栈内存 Registry 只登记启用智能体，语义一致），由 dispatch
// 决定默认智能体回落。
type agentResolver struct{ store *postgres.AgentStore }

func (r agentResolver) Resolve(ctx context.Context, slug string) (*agent.ResolvedConfig, error) {
	stored, err := r.store.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if stored == nil || !stored.Enabled {
		return nil, agent.ErrNotFound
	}
	return stored.Resolved(), nil
}

// accountLookup 把账号仓储适配为 admin.Auth 的查找端口：未命中返回
// (nil, nil)，让鉴权服务走 dummy-hash 防枚举路径。
type accountLookup struct{ store *postgres.AccountStore }

func (l accountLookup) Lookup(ctx context.Context, username string) (*account.Account, error) {
	stored, err := l.store.Get(ctx, username)
	if errors.Is(err, account.ErrNotFound) {
		return nil, nil
	}
	return stored, err
}

func buildRuntime(ctx context.Context) (*runtime, *config.Config, error) {
	// 1. 四个配置子系统（启动期全量加载，配置错误立即失败）。
	cfgIR, err := config.LoadConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("ir config: %w", err)
	}
	cfgPlatform, err := config.LoadPlatformConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("platform config: %w", err)
	}
	cfgAgentAPI, err := config.LoadAgentAPIConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("agent api config: %w", err)
	}
	cfgA2A, err := config.LoadA2AConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("a2a config: %w", err)
	}
	if !cfgPlatform.Enabled {
		return nil, nil, errors.New("PLATFORM__ENABLED=true is required: the v2 binary is the " +
			"platform server itself (the legacy embedded placeholder mode was retired " +
			"with the ir/ package)")
	}
	cfgA2A.ResolveDefaultCardSlug(cfgIR.DefaultAgentSlug)

	// 2. 存储：连接串环境变量注入；Open 幂等迁移，Seed 幂等灌内置岗位。
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return nil, nil, errors.New("DATABASE_URL must be set for PostgreSQL storage")
	}
	// 连接池按配置调优（PLATFORM__DB_MAX_CONNS，默认 8）：执行并发大或
	// 多副本共库时按需上调，上限在配置加载期校验。
	db, err := postgres.OpenTuned(ctx, databaseURL, postgres.PoolConfig{
		MaxConns: int32(cfgPlatform.DBMaxConns),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect storage: %w", err)
	}
	if err := db.Seed(ctx); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("seed storage: %w", err)
	}

	// 3. 主密钥与加密仓储。会话主密钥同时是 Provider/签名密钥的 AES-GCM
	// 主密钥（与旧栈一致，密文互相可读）。
	cipher, err := crypto.NewSecretCipher(cfgPlatform.AdminSessionSecret)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("derive cipher: %w", err)
	}
	agents := postgres.NewAgentStore(db, cipher)
	accounts := postgres.NewAccountStore(db)
	sessions := postgres.NewSessionStore(db)
	audits := postgres.NewAuditStore(db)
	runs := postgres.NewRunStore(db)
	teams := postgres.NewTeamStore(db)
	teamRuns := postgres.NewTeamRunStore(db)
	positions := postgres.NewPositionStore(db)
	queue := postgres.NewTaskQueueStore(db)
	tokens := postgres.NewTokenStore(db)
	workers := postgres.NewWorkerStore(db)
	gateway := postgres.NewGatewayStore(db)
	a2aTasks := postgres.NewA2ATaskStore(db)
	channelEvents := postgres.NewChannelEventStore(db)
	teamShares := postgres.NewTeamShareStore(db)
	pushIdempotency, err := postgres.NewPushIdempotencyStore(ctx, db)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("push idempotency store: %w", err)
	}

	// 4. 鉴权与账号（必须在清空配置凭据副本之前构造）。
	dummyHash := cfgPlatform.AdminPasswordHash
	if strings.TrimSpace(dummyHash) == "" {
		dummyHash, err = crypto.HashPassword(cfgPlatform.AdminPassword)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("hash bootstrap admin password: %w", err)
		}
	}
	auth, err := admin.NewAuth(admin.Config{
		DummyPasswordHash: dummyHash,
		SessionTTL:        cfgPlatform.AdminSessionTTL,
		Verify:            crypto.VerifyPassword,
	}, accountLookup{store: accounts}, sessions)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("admin auth: %w", err)
	}
	accountService := admin.NewAccountService(accounts, crypto.HashPassword, audits)
	if err := accountService.EnsureBootstrap(ctx, cfgPlatform.AdminUsername, dummyHash); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("bootstrap admin account: %w", err)
	}

	// 5. Provider 快照：数据库是 Provider 字段的权威源，启动时做一次带
	// 全局 revision 栅栏的一致性加载，随后清空配置里的凭据副本（密码与
	// 主密钥已转入专用状态，不在共享配置里保留明文）。
	providerStore := infrabackend.NewStore(db.Pool, cipher)
	providerConfig, providerRevision, err := infrabackend.LoadInitial(ctx, providerStore, cfgPlatform)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("load agent center providers: %w", err)
	}
	providerConfig.AdminPassword = ""
	providerConfig.AdminPasswordHash = ""
	providerConfig.AdminSessionSecret = ""
	if providerConfig.ClaudeEnabled {
		slog.Warn("Claude Code executor enabled: current_dir is not an OS sandbox; " +
			"run this service under an isolated low-privilege identity or container")
	}

	// 6. 两个分工不同的 HTTP 客户端（与旧栈一致）：
	//   - executor 客户端给网关直调用：不设总超时（单一 deadline 由执行器
	//     控制），只限制建连并复用长连接；
	//   - callback 客户端给回传/心跳用：较短超时，避免下游拖住回调循环。
	executorHTTPClient := newExecutorHTTPClient()
	callbackHTTPClient := newCallbackHTTPClient()

	// 7. 执行引擎 + Provider 管理器（保存即热更新执行器配置快照）。
	executor, err := execengine.NewAgentExecutor(
		execengine.PlaceholderProcessor{}, executorHTTPClient, providerConfig)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("build executor: %w", err)
	}
	providers := infrabackend.NewManager(providerStore, providerConfig, providerRevision,
		func(next *config.PlatformConfig) { executor.SetConfig(next) })

	// 8. 分发服务：Push/Pull/动态入口/团队编排/A2A 共用同一套准入闸与
	// 执行闸。remote_pull 队列仅在 AGENT_API 启用时接入。
	var taskQueue *postgres.TaskQueueStore
	if cfgAgentAPI.Enabled {
		taskQueue = queue
	}
	dispatcher := dispatch.NewService(dispatch.Config{
		Resolver:             agentResolver{store: agents},
		Engine:               executor,
		Queue:                taskQueue,
		DefaultSlug:          cfgIR.DefaultAgentSlug,
		Remote:               dispatch.RemotePolicy{QueueTTL: seconds(cfgAgentAPI.QueueTTLSeconds), SyncHoldMax: seconds(cfgAgentAPI.SyncHoldMaxSeconds)},
		GlobalMaxConcurrency: cfgPlatform.GlobalMaxConcurrency,
	})

	callbackSender := callback.NewSender(callback.Config{
		SigningSecret: cfgIR.SigningSecret,
		Token:         cfgIR.CallbackToken,
		TokenOrigin:   cfgIR.CallbackTokenOrigin,
	}, httpx.CallbackPoster{Client: callbackHTTPClient})

	// 9. Push 用例（协议层硬约束：幂等状态必须落 PostgreSQL）。
	var pushService *push.Service
	if cfgIR.PushEnabled {
		pushService = push.NewService(push.Deps{
			Config: push.Config{
				SigningSecret:         cfgIR.SigningSecret,
				SignatureEncoding:     platform.SignatureEncoding(cfgIR.SignatureEncoding),
				Mode:                  push.Mode(cfgIR.PushMode),
				TaskTimeout:           cfgIR.TaskTimeout,
				TrustedUsageReporting: cfgIR.TrustedUsageReportingEnabled,
				MaxRequestBytes:       cfgPlatform.MaxRequestBodyBytes,
			},
			Idempotency: pushIdempotency,
			Dispatcher:  dispatcher,
			Callback:    callbackSender,
		})
		slog.Info("ir push endpoint enabled", "path", cfgIR.PushPath, "mode", cfgIR.PushMode)
	} else {
		slog.Warn("ir push endpoint disabled (IR__PUSH_ENABLED=false)")
	}

	// 10. Agent Pull Connector API。响应路由约束（AGENT-PULL-API §5.2）：
	// sync Push 与远端执行不兼容，启动期直接防呆。
	connectors := agentapi.NewService(agentapi.Deps{
		Config: agentapi.Config{
			LeaseSeconds:             cfgAgentAPI.LeaseSeconds,
			RenewInterval:            renewInterval(cfgAgentAPI.LeaseSeconds),
			HeartbeatIntervalSeconds: 60,
			ClaimMaxWait:             seconds(cfgAgentAPI.ClaimMaxWaitSeconds),
			MaxOutstandingLeases:     cfgAgentAPI.MaxOutstandingLeases,
			MaxAttempts:              cfgAgentAPI.MaxAttempts,
			QueueTTL:                 seconds(cfgAgentAPI.QueueTTLSeconds),
			WorkerOfflineAfterMiss:   cfgAgentAPI.WorkerOfflineAfterMiss,
		},
		Queue:   queue,
		Tokens:  tokens,
		Workers: workers,
		Remote:  agents,
		Secrets: agentSecretAdapter{agents},
		Denies:  postgres.NewClusterDenyStore(db),
	})
	if cfgAgentAPI.Enabled && cfgIR.PushMode == config.PushModeSync {
		slugs, err := agents.RemotePullSlugs(ctx)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("check remote-pull agents: %w", err)
		}
		if len(slugs) > 0 {
			db.Close()
			return nil, nil, fmt.Errorf("push_mode=sync cannot serve remote_pull agents %s; set IR__PUSH_MODE=async",
				strings.Join(slugs, ","))
		}
	}
	if cfgAgentAPI.Enabled {
		slog.Info("agent pull connector API enabled",
			"route", "/agent-api/v1/claim|renew|complete|runs|heartbeat",
			"lease_seconds", cfgAgentAPI.LeaseSeconds, "max_attempts", cfgAgentAPI.MaxAttempts)
	}

	// 11. 动态入口（/agent/{slug}）。
	dynamicService := dynamic.NewService(dynamic.Deps{
		Config: dynamic.Config{
			AllowLegacyBodySignature: providerConfig.AllowLegacyBodySignature,
			TrustedUsageReporting:    cfgIR.TrustedUsageReportingEnabled,
			MaxRequestBodyBytes:      cfgPlatform.MaxRequestBodyBytes,
			GlobalMaxConcurrency:     cfgPlatform.GlobalMaxConcurrency,
		},
		Store:    gateway,
		Agents:   agents,
		Engine:   executor,
		Callback: callbackSender,
		Events:   channelEvents,
	})

	// 12. 目录 / 团队编排 / 总览。
	catalogService := catalog.NewService(catalog.Deps{
		Positions: positions, Agents: agents, Teams: teams,
		Shares: teamShares, Audit: audits,
	})
	teamEngine := teamrun.NewEngine(teamrun.Deps{
		Teams: teams, TeamRuns: teamRuns, AgentRuns: runs,
		Agents: agents, Dispatcher: dispatcher,
	})
	dash := dashboard.NewService(dashboard.Deps{
		Version: version, Health: db, Runs: runs,
		Queue: queue, Workers: workers, TeamRuns: teamRuns,
		CacheTTL: 2 * time.Second,
	})

	// 13. HTTP 路由。
	adminRouter := apihttp.AdminRouter(apihttp.AdminDeps{
		Auth: auth, Catalog: catalogService, Dashboard: dash, Engine: teamEngine,
		Connectors: connectors, Accounts: accountService, Providers: providers,
		Runs: runs, Audits: audits, ChannelEvents: channelEvents,
		CookieSecure: cfgPlatform.SecureCookies, CookieTTL: cfgPlatform.AdminSessionTTL,
		PublicBaseURL: cfgPlatform.PublicBaseURL,
	})
	// 运维指标：HTTP 计数中间件随路由生效；域统计 gauge 以抓取时回调注入
	// （查询成本由抓取间隔承担；单项失败降级，绝不让指标端点 500）。
	// RegisterTo 在 main 里于 hwhkit.Run 之后绑定框架 Registry——框架的
	// production.Serve 已在外层 mux 挂 promhttp /metrics。
	metricsRegistry := apihttp.NewMetricsRegistry()
	metricsRegistry.RegisterCollector(func(ch chan<- prometheus.Metric) {
		collectDomainMetrics(ch, dash, db, agents)
	})
	// 公开分享服务：token 限流 + 脱敏视图 + 越权防护（app/share 口径）。
	shareService := share.NewService(teamShares, teams, shareRunner{engine: teamEngine},
		shareRuns{store: runs}, share.Config{})
	routerDeps := apihttp.RouterDeps{
		PushPath: cfgIR.PushPath,
		Share:    apihttp.ShareRouter(&apihttp.ShareHandler{Service: shareService}),
		Dynamic: &apihttp.DynamicHandler{
			Service: dynamicService, MaxRequestBytes: int64(cfgPlatform.MaxRequestBodyBytes),
		},
		Admin:   adminRouter,
		Health:  db,
		Metrics: metricsRegistry,
	}
	if pushService != nil {
		routerDeps.Push = &apihttp.PushHandler{
			Service: pushService, MaxRequestBytes: int64(cfgPlatform.MaxRequestBodyBytes),
		}
	}
	if cfgAgentAPI.Enabled {
		routerDeps.AgentAPI = &apihttp.AgentAPIHandler{Service: connectors}
	}
	if cfgA2A.Enabled {
		routerDeps.A2A = &apihttp.A2ADeps{
			Service: &a2a.Service{Tasks: a2aTasks, Dispatcher: dispatcher},
			Agents:  agents,
			Card: a2a.CardConfig{
				BasePath:             cfgA2A.BasePath,
				RequireBearerAuth:    cfgA2A.RequireBearerAuth,
				ProviderOrganization: cfgA2A.ProviderOrganization,
				ProviderURL:          cfgA2A.ProviderURL,
				DocumentationURL:     cfgA2A.DocumentationURL,
				AgentVersion:         cfgA2A.AgentVersion,
			},
			RequireBearerAuth:   cfgA2A.RequireBearerAuth,
			MaxRequestBodyBytes: cfgA2A.MaxRequestBodyBytes,
			DefaultCardSlug:     cfgA2A.DefaultCardSlug,
			PublicBaseURL:       providerConfig.PublicBaseURL,
		}
		slog.Info("A2A v1.0 endpoint enabled",
			"route", cfgA2A.BasePath+"/{slug}", "card", "/.well-known/agent-card.json")
	}
	router := apihttp.NewRouter(routerDeps)

	// 控制台（八模块 SPA，web/ 静态文件经 embed.FS 内嵌）。
	// /admin 与未知子路径都回落 index.html（hash 路由无需深链）；
	// /admin/ 前缀保证页面内相对资源路径（app.css 等）解析正确。
	// index.html 内的资源引用带构建版本戳（?v=…）：发版后 URL 变化，
	// 浏览器必然重新拉取——根治「页面还跑旧 JS」的缓存问题。
	console := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/admin/")
		data, err := web.Assets.ReadFile(name)
		if err != nil {
			if data, err = web.Assets.ReadFile("index.html"); err != nil {
				http.Error(w, "console assets unavailable", http.StatusInternalServerError)
				return
			}
			name = "index.html"
		}
		body := data
		if name == "index.html" {
			for _, asset := range []string{"app.css", "app.js", "i18n.js"} {
				body = bytes.ReplaceAll(body,
					[]byte(asset+"\""), []byte(asset+"?v="+version+"\""))
			}
		}
		w.Header().Set("Content-Type", consoleContentType(name))
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	router.Get("/admin", console)
	router.Get("/admin/*", console)

	// 公开分享子页面 /share/{token}（轻量独立页，无会话）。
	sharePage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := web.Assets.ReadFile("share.html")
		if err != nil {
			http.Error(w, "share page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
	router.Get("/share/{token}", sharePage)

	// 14. 后台执行体（在 main 里随停机信号启动/收口）。
	rt := &runtime{
		router: router, db: db, providers: providers, pushSvc: pushService,
		dynamic: dynamicService, team: teamEngine, connectors: connectors,
		agentAPIEnabled: cfgAgentAPI.Enabled, metrics: metricsRegistry,
		pushIdempotency: pushIdempotency, retention: postgres.NewRetentionStore(db),
		retentionDays: cfgPlatform.RetentionDays,
	}
	if cfgIR.PullEnabled {
		rt.pullWorker = worker.New(worker.Config{
			WorkerID:              cfgIR.WorkerID,
			TaskTimeout:           cfgIR.TaskTimeout,
			RenewInterval:         cfgIR.RenewInterval,
			HeartbeatInterval:     cfgIR.HeartbeatInterval,
			TrustedUsageReporting: cfgIR.TrustedUsageReportingEnabled,
		}, platformhttp.NewClient(platformhttp.Config{
			QueueURL:       cfgIR.QueueURL,
			BindToken:      cfgIR.BindToken,
			RequestTimeout: cfgIR.ClaimTimeout,
		}), dispatcher)
	} else {
		slog.Warn("pull worker disabled (IR__PULL_ENABLED=false)")
	}
	if strings.TrimSpace(cfgIR.HeartbeatURL) != "" {
		// 心跳通道走读缓存：5s tick 不再每轮全表解密（注册表版本栅栏 +
		// 10s TTL；换钥即版本变化，缓存立即失效）。
		channelLister := postgres.NewCachedChannelLister(agents, 10*time.Second)
		rt.heartbeat = worker.NewPushHeartbeat(worker.PushHeartbeatConfig{
			HeartbeatURL:    cfgIR.HeartbeatURL,
			DefaultInterval: cfgIR.HeartbeatInterval,
			TaskTimeout:     cfgIR.TaskTimeout,
		}, httpx.CallbackPoster{Client: callbackHTTPClient}, channelLister, channelEvents, dispatcher)
	}
	// B7：就绪检查闭包（迁移标记/注册表/队列；依赖本函数局部仓储），
	// main 在 hwhkit.Run 后注册到框架健康注册表。
	rt.healthChecks = append(rt.healthChecks, &health.CheckFunc{N: "migrations",
		F: func(ctx context.Context) health.Result {
			var marker int
			err := db.Pool.QueryRow(ctx,
				`SELECT 1 FROM platform_settings WHERE key = 'agent_registry_revision'`).Scan(&marker)
			return readinessResult("migrations", err)
		}},
		&health.CheckFunc{N: "agent_registry",
			F: func(ctx context.Context) health.Result {
				_, err := agents.RegistryVersion(ctx)
				return readinessResult("agent_registry", err)
			}})
	if cfgAgentAPI.Enabled {
		rt.healthChecks = append(rt.healthChecks, &health.CheckFunc{N: "queue",
			F: func(ctx context.Context) health.Result {
				_, err := queue.QueueTotals(ctx)
				return readinessResult("queue", err)
			}})
	}

	// B8：非敏感生效配置摘要（运维核对；凭据/密钥永不打印）。
	slog.Info("configuration summary",
		"push_enabled", cfgIR.PushEnabled, "push_path", cfgIR.PushPath,
		"push_mode", cfgIR.PushMode,
		"pull_enabled", cfgIR.PullEnabled,
		"agent_api_enabled", cfgAgentAPI.Enabled,
		"a2a_enabled", cfgA2A.Enabled, "a2a_base_path", cfgA2A.BasePath,
		"public_base_url", cfgPlatform.PublicBaseURL,
		"secure_cookies", cfgPlatform.SecureCookies,
		"global_max_concurrency", cfgPlatform.GlobalMaxConcurrency,
		"db_max_conns", cfgPlatform.DBMaxConns,
		"retention_days", cfgPlatform.RetentionDays,
		"workspace_root", cfgPlatform.WorkspaceRoot,
		"max_request_body_bytes", cfgPlatform.MaxRequestBodyBytes,
		"version", version)
	return rt, cfgIR, nil
}

// startLoops 在停机上下文中启动全部后台循环。
func (rt *runtime) startLoops(ctx context.Context) {
	go rt.providers.RefreshLoop(ctx)
	go rt.team.Run(ctx)
	if rt.agentAPIEnabled {
		go agentapi.RunReaper(ctx, rt.connectors)
	}
	if rt.pullWorker != nil {
		rt.pullWorker.Start(ctx)
	}
	if rt.heartbeat != nil {
		go rt.heartbeat.Run(ctx)
	}
	// 运行数据治理：push 陈旧 processing 租约 10min 一扫（与陈旧接管
	// 阈值同源）；保留策略 1h 一扫（窗口可配，0 = 只清过期会话）。
	go runPushLeaseGC(ctx, rt.pushIdempotency)
	go runRetentionSweep(ctx, rt.retention, rt.retentionDays)
}

// runPushLeaseGC 周期清扫崩溃遗留的陈旧 processing 租约。终态行按协议
// 永不清理；失败只记日志（下一 tick 重试）。
func runPushLeaseGC(ctx context.Context, store *postgres.PushIdempotencyStore) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if removed, err := store.SweepStaleProcessing(ctx); err != nil {
			slog.Warn("push lease GC failed", "err", err)
		} else if removed > 0 {
			slog.Info("push lease GC removed stale processing rows", "removed", removed)
		}
	}
}

// runRetentionSweep 周期执行历史数据保留清扫。days <= 0 时只清过期会话。
func runRetentionSweep(ctx context.Context, store *postgres.RetentionStore, days int) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		events, audits, sessions, err := store.Sweep(ctx, days)
		if err != nil {
			slog.Warn("retention sweep failed", "err", err)
			continue
		}
		if events > 0 || audits > 0 || sessions > 0 {
			slog.Info("retention sweep", "channel_events", events,
				"audit_rows", audits, "expired_sessions", sessions)
		}
	}
}

// drain 等待在飞任务收尾并释放存储（优雅停机体面）。
func (rt *runtime) drain() {
	if rt.pullWorker != nil {
		rt.pullWorker.Stop()
	}
	if rt.pushSvc != nil {
		rt.pushSvc.WaitBackground()
	}
	if rt.dynamic != nil {
		rt.dynamic.WaitBackground()
	}
	if rt.team != nil {
		rt.team.Wait()
	}
	if rt.db != nil {
		rt.db.Close()
	}
}

// ---------------------------------------------------------------------------
// HTTP 客户端
// ---------------------------------------------------------------------------

// sameOriginRedirectPolicy 禁止携带 Provider API key、回调签名或回调令牌
// 的 HTTP 客户端把请求转发到不同 origin。原 origin 上的相对重定向保持兼容；
// 跨主机与 HTTPS→HTTP 重定向一律失败关闭。
func sameOriginRedirectPolicy(req *http.Request, via []*http.Request) error {
	return platformhttp.CredentialedRedirectPolicy(req, via)
}

func newExecutorHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 32,
		},
		CheckRedirect: sameOriginRedirectPolicy,
	}
}

func newCallbackHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		},
		Timeout:       30 * time.Second,
		CheckRedirect: sameOriginRedirectPolicy,
	}
}

// consoleContentType 按扩展名给控制台静态资源定 MIME（集合固定且极小）。
func consoleContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		return "text/html; charset=utf-8"
	}
}

// shareRunner 把 *teamrun.Engine 适配为 share.Runner（输入形态转换）。
type shareRunner struct{ engine *teamrun.Engine }

func (r shareRunner) Submit(ctx context.Context, s share.Submission) (*team.TeamRun, error) {
	return r.engine.Submit(ctx, teamrun.Submission{
		TeamID: s.TeamID, Input: s.Input, IdempotencyKey: s.IdempotencyKey, Actor: s.Actor,
	})
}

func (r shareRunner) Get(ctx context.Context, id string) (*team.TeamRun, error) {
	return r.engine.Get(ctx, id)
}

// shareRuns 把 *postgres.RunStore 适配为 share.ChildRunReader。
type shareRuns struct{ store *postgres.RunStore }

func (r shareRuns) Get(ctx context.Context, runID string) (*share.ChildRun, error) {
	run, err := r.store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &share.ChildRun{RunID: run.RunID, Status: string(run.Status), Output: run.Output}, nil
}

// collectDomainMetrics 在每次 /metrics 抓取时经 const metric 推送域统计。
// 单项失败降级为跳过（不推送该样本），指标端点永远 200——监控数据源
// 不该成为新的故障面。
func collectDomainMetrics(ch chan<- prometheus.Metric, dash *dashboard.Service,
	db *postgres.DB, agents *postgres.AgentStore,
) {
	gauge := func(name, help string, labelNames, labelValues []string, value float64) {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(name, help, labelNames, nil),
			prometheus.GaugeValue, value, labelValues...)
	}
	snap := dash.Snapshot(context.Background())
	gauge("ir_db_up", "Database reachability (1 = ping ok).", nil, nil,
		boolGauge(snap.DatabaseOK))
	gauge("ir_workers_online", "Connectors with a fresh heartbeat.", nil, nil,
		float64(snap.OnlineWorkers))
	gauge("ir_queue_tasks", "Connector queue tasks.", []string{"state"},
		[]string{"queued"}, float64(snap.Queue.Queued))
	gauge("ir_queue_tasks", "Connector queue tasks.", []string{"state"},
		[]string{"leased"}, float64(snap.Queue.Leased))
	gauge("ir_team_runs_active", "Team runs in queued/running state.", nil, nil,
		float64(snap.ActiveTeamRuns))
	gauge("ir_runs_today", "Runs since midnight UTC.", []string{"status"},
		[]string{"completed"}, float64(snap.RunsToday.Completed))
	gauge("ir_runs_today", "Runs since midnight UTC.", []string{"status"},
		[]string{"failed"}, float64(snap.RunsToday.Failed))
	gauge("ir_runs_today", "Runs since midnight UTC.", []string{"status"},
		[]string{"canceled"}, float64(snap.RunsToday.Canceled))
	gauge("ir_runs_today", "Runs since midnight UTC.", []string{"status"},
		[]string{"in_progress"}, float64(snap.RunsToday.InProgress))
	gauge("ir_runs_today", "Runs since midnight UTC.", []string{"status"},
		[]string{"queued"}, float64(snap.RunsToday.Queued))
	// 智能体总数：列表 Search 的 LIMIT 1 + 窗口 COUNT，成本是一次索引扫描。
	if _, total, err := agents.Search(context.Background(),
		agent.ListQuery{Limit: 1}); err == nil {
		gauge("ir_agents_total", "Registered agents.", []string{"enabled"},
			[]string{"all"}, float64(total))
	}
	if _, enabled, err := agents.Search(context.Background(),
		agent.ListQuery{Limit: 1}); err == nil {
		gauge("ir_agents_total", "Registered agents.", []string{"enabled"},
			[]string{"true"}, float64(enabled))
	}
	stat := db.Pool.Stat()
	gauge("ir_db_pool_conns", "pgxpool connections.", []string{"state"},
		[]string{"total"}, float64(stat.TotalConns()))
	gauge("ir_db_pool_conns", "pgxpool connections.", []string{"state"},
		[]string{"acquired"}, float64(stat.AcquiredConns()))
	gauge("ir_db_pool_conns", "pgxpool connections.", []string{"state"},
		[]string{"idle"}, float64(stat.IdleConns()))
	gauge("ir_db_pool_conns", "pgxpool connections.", []string{"state"},
		[]string{"max"}, float64(stat.MaxConns()))
	gauge("ir_go_goroutines", "Number of goroutines.", nil, nil,
		float64(goruntime.NumGoroutine()))
	var mem goruntime.MemStats
	goruntime.ReadMemStats(&mem)
	gauge("ir_go_heap_bytes", "Heap bytes in use.", nil, nil, float64(mem.HeapAlloc))
	gauge("ir_build_info", "Build metadata; value is always 1.", []string{"version"},
		[]string{version}, 1)
}

// readinessResult 把检查错误折叠为框架健康结果（失败=unhealthy+稳定文案）。
func readinessResult(name string, err error) health.Result {
	result := health.Result{Name: name, Status: health.StatusHealthy}
	if err != nil {
		result.Status = health.StatusUnhealthy
		result.Detail = name + " check failed"
	}
	return result
}

func boolGauge(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// 框架壳
// ---------------------------------------------------------------------------

// App 把组装好的路由交给 hwhkit 框架托管（服务器绑定、优雅停机、
// 可观测性初始化都来自框架配置）。
type App struct {
	router chi.Router
	irCfg  *config.Config
}

// BuildRouter 返回预组装的路由（装配在 main 里提前完成，配置错误早于
// 框架启动暴露）。
func (a App) BuildRouter(_ context.Context, app *appctx.Context, _ *hwconfig.AppConfig) (http.Handler, error) {
	if a.irCfg != nil {
		appctx.Insert(app, a.irCfg)
	}
	return a.router, nil
}

// applicationBootstrap 让仓库内 config/default.toml 成为本地基础配置；
// 显式 HWHKIT_ENV 仍会选择 config/{env}.toml。
func applicationBootstrap() *hwconfig.BootstrapConfig {
	bootstrap := hwconfig.DefaultBootstrap()
	if strings.TrimSpace(os.Getenv("HWHKIT_ENV")) == "" {
		bootstrap.Env = "default"
	}
	return bootstrap
}

func seconds(value int) time.Duration { return time.Duration(value) * time.Second }

// renewInterval 计算建议续租间隔（lease/3，与租约协议一致），下限 1s。
func renewInterval(leaseSeconds int) time.Duration {
	if leaseSeconds <= 0 {
		leaseSeconds = 90
	}
	interval := time.Duration(leaseSeconds/3) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

func main() {
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBoot()
	rt, irCfg, err := buildRuntime(bootCtx)
	if err != nil {
		slog.Error("initialization failed", "err", err)
		os.Exit(1)
	}
	defer rt.drain()

	built, err := hwhkit.Run(context.Background(), App{router: rt.router, irCfg: irCfg},
		applicationBootstrap())
	if err != nil {
		slog.Error("bootstrap failed", "err", err)
		os.Exit(1)
	}
	logger := observability.InitLogging(built.Config().Observability.Log)
	// 域统计指标绑定框架 Registry：框架 Serve() 只在未设置时才自建——预先
	// SetMetricsRegistry 让 promhttp /metrics 同时暴露 ir_* 指标与框架的
	// HTTP 中间件指标（Serve 内 RegisterHTTPMetrics 会补注册到同一 registry）。
	frameworkReg := prometheus.NewRegistry()
	built.SetMetricsRegistry(frameworkReg)
	rt.metrics.RegisterTo(frameworkReg)

	// B7：就绪检查注册到框架健康注册表（/health/ready 由框架外层 mux
	// 提供，checks 数组逐项可见；数据库探活已由框架装配）。
	for _, check := range rt.healthChecks {
		built.Health().Register(check)
	}
	rt.startLoops(built.Shutdown().Context())

	slog.Info("ir-agent-server ready", "version", version)
	if err := production.Serve(context.Background(), built, logger); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// agentSecretAdapter 把 AgentStore 的解密密钥适配为 agentapi.AgentSecretSource。
// prev 仅在轮换宽限窗口内有效（domain 层判定），窗外一律置空。
type agentSecretAdapter struct{ agents *postgres.AgentStore }

func (a agentSecretAdapter) SigningSecrets(ctx context.Context, slug string) ([]byte, []byte, error) {
	record, err := a.agents.GetBySlug(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	prev := record.PrevSigningSecret
	if !record.PrevSecretActive(time.Now()) {
		prev = nil
	}
	return record.SigningSecret, prev, nil
}
