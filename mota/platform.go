package mota

// 单体多 Agent 管理平台的共享状态与初始化。
// 对应 Rust 侧 src/platform/mod.rs。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// PlatformState 是管理平台的共享状态。
type PlatformState struct {
	Config    *PlatformConfig
	configMu  sync.RWMutex
	Store     *AgentStore
	Registry  *AgentRegistry
	Processor Processor
	// GlobalExecution 是跨 Agent 的全局并发闸。
	GlobalExecution *semaphore
	// Mutation 把 Agent 的存储变更与 Registry 应用串行化,避免持久配置与
	// 活动注册表不一致。
	Mutation sync.Mutex
	// AccountMutation 串行化账号角色、启停与删除，确保至少一个可用管理员。
	AccountMutation sync.Mutex
	// HTTPClient 是网关直调共用的 HTTP 客户端;为 nil 时用 http.DefaultClient。
	// 生产装配经 PlatformDependencies.ExecutorHTTPClient 注入,不设总超时。
	HTTPClient *http.Client
	// CallbackHTTPClient 是异步 callback 回传用的客户端;为 nil 时用 HTTPClient。
	// 生产装配经 PlatformDependencies.CallbackHTTPClient 注入,超时较短。
	CallbackHTTPClient *http.Client

	// 以下三项来自 MOTA 接入配置,经 PlatformDependencies 注入。
	//
	// SigningSecret 是本 Bound 的 mbt_ 密钥。注意异步 callback 用的是**每个 Agent
	// 自己的** SigningSecret,这里的值只给不区分 Agent 的通道使用。
	SigningSecret string
	// CallbackToken 是可选回调令牌(mpk_*);非空时附加到 callback URL 的 ?token=。
	CallbackToken string
	// CallbackTokenOrigin 限定 legacy token 只能发送到这一受信 origin。
	CallbackTokenOrigin string
	// TrustedUsageReportingEnabled 只有平台登记的受信 Investor Workflow Host 可开启;
	// 开启后拿不到 FINAL usage 的运行必须判失败,不能静默降级。
	TrustedUsageReportingEnabled bool

	// Auth 承载管理台鉴权(argon2 口令、登录并发闸与失败退避、会话 Cookie)。
	Auth *AdminAuth

	// DefaultAgentSlug 是 Push/Pull 的 agentCode 未命中 Registry 时使用的默认
	// 动态 Agent。这个值来自 MOTA 接入配置(mota.Config.DefaultAgentSlug),
	// 不是平台配置自身的字段,经 PlatformDependencies.DefaultAgentSlug 注入。
	// Dispatcher() 按这个字段组装分发器。
	DefaultAgentSlug string

	// 执行器按需构造:它要求 workspace 根目录可解析,而部分单元测试并不需要
	// 真实工作区。构造结果(含错误)缓存一次,避免每次请求都重做路径解析。
	executorOnce sync.Once
	executor     *AgentExecutor
	executorErr  error

	// 分发器同样按需构造并缓存;它包着执行器,所以复用 Executor() 的懒加载。
	dispatcherOnce sync.Once
	dispatcher     *AgentDispatcher
	dispatcherErr  error

	providerRefreshCancel context.CancelFunc
	providerRefreshDone   chan struct{}
	agentRefreshDone      chan struct{}
}

// Executor 返回共享的 Agent 执行器。首次调用时构造并缓存结果(含失败)。
func (s *PlatformState) Executor() (*AgentExecutor, error) {
	s.executorOnce.Do(func() {
		// 与 ApplyConfig 共用 configMu，避免首次懒构造正好撞上 Provider
		// 热更新时把旧快照永久装进执行器。
		s.configMu.Lock()
		s.executor, s.executorErr = NewAgentExecutor(s.Processor, s.HTTPClient, s.Config)
		s.configMu.Unlock()
	})
	s.configMu.RLock()
	executor, err := s.executor, s.executorErr
	s.configMu.RUnlock()
	return executor, err
}

// ConfigSnapshot 返回当前不可变配置快照。调用方不得修改返回值。
func (s *PlatformState) ConfigSnapshot() *PlatformConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.Config
}

// ApplyConfig 原子替换 Provider 配置，并让已构造执行器从下一次任务起使用新快照。
func (s *PlatformState) ApplyConfig(config *PlatformConfig) {
	s.configMu.Lock()
	s.Config = config
	if s.executor != nil {
		s.executor.SetConfig(config)
	}
	s.configMu.Unlock()
}

// SetExecutor 注入一个已构造好的执行器(装配与测试用),并抑制按需构造。
func (s *PlatformState) SetExecutor(executor *AgentExecutor) {
	s.executorOnce.Do(func() {})
	s.configMu.Lock()
	s.executor = executor
	s.executorErr = nil
	if executor != nil {
		executor.SetConfig(s.Config)
	}
	s.configMu.Unlock()
}

// Dispatcher 返回共享的 AgentDispatcher。首次调用时构造并缓存结果(含失败),
// 与 Executor() 的懒加载模式一致。
//
// 生产装配把 Push/Pull 的 TaskExecutionBackend 都指向同一个 Dispatcher()
// 实例,让两条协议入口共享同一套准入闸与执行闸,与 rs 侧 task_execution 的
// 构造顺序一致。
func (s *PlatformState) Dispatcher() (*AgentDispatcher, error) {
	executor, err := s.Executor()
	if err != nil {
		return nil, err
	}
	s.dispatcherOnce.Do(func() {
		s.dispatcher = NewAgentDispatcher(s.Registry, executor, s.GlobalExecution, s.DefaultAgentSlug)
	})
	return s.dispatcher, s.dispatcherErr
}

// SetDispatcher 注入一个已构造好的分发器(测试用),并抑制按需构造。
func (s *PlatformState) SetDispatcher(dispatcher *AgentDispatcher) {
	s.dispatcherOnce.Do(func() {})
	s.dispatcher = dispatcher
	s.dispatcherErr = nil
}

// PlatformDependencies 是生产装配(#12)注入的外部依赖:两个分工不同的 HTTP
// 客户端,以及来自 MOTA 接入配置的密钥/开关。对应 rs 侧
// src/platform/mod.rs::PlatformDependencies。
type PlatformDependencies struct {
	// ExecutorHTTPClient 给网关直调(litellm 等)用:不设总超时(由
	// AgentExecutor 的单一 deadline 控制),但复用长连接池,避免全局超时
	// 截断长推理请求。
	ExecutorHTTPClient *http.Client
	// CallbackHTTPClient 给异步 callback 回传与 Push 心跳用:较短的连接/
	// 总超时,避免下游异常拖住整条心跳循环。
	CallbackHTTPClient *http.Client
	// SigningSecret 是本 Bound 的 mbt_ 密钥,供不区分 Agent 的通道使用。
	SigningSecret string
	// CallbackToken 是可选回调令牌(mpk_*)。
	CallbackToken string
	// CallbackTokenOrigin 把 token 绑定到唯一受信 callback origin。
	CallbackTokenOrigin string
	// TrustedUsageReportingEnabled 仅平台登记的受信 Investor Workflow Host 可开启。
	TrustedUsageReportingEnabled bool
	// DefaultAgentSlug 是 Push/Pull 的 agentCode 未命中 Registry 时使用的默认
	// 动态 Agent(来自 mota.Config.DefaultAgentSlug)。
	DefaultAgentSlug string
}

// NewPlatformState 连接存储、载入 Agent 并组装平台状态。
//
// 这是嵌入/测试用的便捷构造:不注入任何外部 HTTP 客户端或 MOTA 密钥,网关
// 直调与 callback 回传都会退回 http.DefaultClient。生产装配(#12)应使用
// NewPlatformStateWithDependencies,让网关直调与回调/心跳使用分工不同、
// 参数各自调优的 HTTP 客户端。
func NewPlatformState(ctx context.Context, cfg *PlatformConfig, processor Processor) (*PlatformState, error) {
	return NewPlatformStateWithDependencies(ctx, cfg, processor, PlatformDependencies{})
}

// NewPlatformStateWithDependencies 是生产装配入口。
func NewPlatformStateWithDependencies(
	ctx context.Context, cfg *PlatformConfig, processor Processor, deps PlatformDependencies,
) (*PlatformState, error) {
	if !cfg.Enabled {
		return nil, errors.New("platform is disabled")
	}

	cipher, err := NewSecretCipher(cfg.AdminSessionSecret)
	if err != nil {
		return nil, err
	}

	// PostgreSQL 存储:连接串由环境变量注入(DATABASE_URL)。
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL must be set for PostgreSQL storage")
	}
	store, err := ConnectAgentStore(ctx, databaseURL, cipher)
	if err != nil {
		return nil, err
	}
	loadedConfig, _, providerRevision, err := LoadConsistentProviderSnapshot(ctx, store, cfg)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("load Agent Center providers: %w", err)
	}
	cfg = loadedConfig
	agents, agentRevision, err := LoadConsistentAgentSnapshot(ctx, store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("load Agent registry: %w", err)
	}
	registry := NewAgentRegistry(agents)

	// 鉴权状态必须在清空配置里的凭证副本**之前**构造。
	auth, err := NewAdminAuth(cfg, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	// 密码与加密主密钥已转入专用状态,不在共享配置里保留明文副本。
	cfg.AdminPassword = ""
	cfg.AdminPasswordHash = ""
	cfg.AdminSessionSecret = ""

	if cfg.ClaudeEnabled {
		slog.Warn("Claude Code executor enabled: current_dir is not an OS sandbox; "+
			"run this service under an isolated low-privilege identity or container",
			"workspace_root", cfg.WorkspaceRoot)
	}
	slog.Info("multi-agent platform initialized", "storage", "postgresql", "agents", registry.Len())

	state := &PlatformState{
		Config:                       cfg,
		Store:                        store,
		Registry:                     registry,
		Processor:                    processor,
		GlobalExecution:              newSemaphore(cfg.GlobalMaxConcurrency),
		Auth:                         auth,
		HTTPClient:                   deps.ExecutorHTTPClient,
		CallbackHTTPClient:           deps.CallbackHTTPClient,
		SigningSecret:                deps.SigningSecret,
		CallbackToken:                deps.CallbackToken,
		CallbackTokenOrigin:          deps.CallbackTokenOrigin,
		TrustedUsageReportingEnabled: deps.TrustedUsageReportingEnabled,
		DefaultAgentSlug:             deps.DefaultAgentSlug,
	}
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	state.providerRefreshCancel = cancelRefresh
	state.providerRefreshDone = make(chan struct{})
	state.agentRefreshDone = make(chan struct{})
	go state.runProviderRefresh(refreshCtx, providerRevision)
	go state.runAgentRefresh(refreshCtx, agentRevision)
	return state, nil
}

// Close 释放存储连接。
func (s *PlatformState) Close() error {
	if s.providerRefreshCancel != nil {
		s.providerRefreshCancel()
	}
	if s.providerRefreshDone != nil {
		<-s.providerRefreshDone
	}
	if s.agentRefreshDone != nil {
		<-s.agentRefreshDone
	}
	return s.Store.Close()
}

// LoadConsistentAgentSnapshot binds a decrypted registry snapshot to the same
// global revision. A concurrent CRUD commit causes a bounded retry.
func LoadConsistentAgentSnapshot(
	ctx context.Context, store *AgentStore,
) ([]*AgentConfig, int64, error) {
	for range 3 {
		before, err := store.AgentRegistryRevision(ctx)
		if err != nil {
			return nil, 0, err
		}
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return nil, 0, err
		}
		after, err := store.AgentRegistryRevision(ctx)
		if err != nil {
			return nil, 0, err
		}
		if before == after {
			return agents, after, nil
		}
	}
	return nil, 0, errors.New("Agent registry changed during snapshot read")
}

func (s *PlatformState) runAgentRefresh(ctx context.Context, appliedRevision int64) {
	defer close(s.agentRefreshDone)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		revision, err := s.Store.AgentRegistryRevision(pollCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("read Agent registry revision failed", "error", err)
			}
			continue
		}
		if revision == appliedRevision {
			continue
		}
		s.Mutation.Lock()
		pollCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		agents, snapshotRevision, loadErr := LoadConsistentAgentSnapshot(pollCtx, s.Store)
		cancel()
		if loadErr == nil {
			s.Registry.ReplaceAll(agents)
			appliedRevision = snapshotRevision
			slog.Info("Agent registry refreshed from PostgreSQL",
				"revision", snapshotRevision, "agents", len(agents))
		} else if ctx.Err() == nil {
			slog.Error("refresh Agent registry failed", "error", loadErr)
		}
		s.Mutation.Unlock()
	}
}

// runProviderRefresh 通过轻量全局 revision 轮询让同库多副本最终收敛。只有
// revision 改变时才解密并校验完整 Provider 快照。
func (s *PlatformState) runProviderRefresh(ctx context.Context, appliedRevision int64) {
	defer close(s.providerRefreshDone)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		revision, err := s.Store.ExecutorProviderRevision(pollCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("read Provider revision failed", "error", err)
			}
			continue
		}
		if revision == appliedRevision {
			continue
		}
		s.Mutation.Lock()
		pollCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		next, _, snapshotRevision, loadErr := LoadConsistentProviderSnapshot(
			pollCtx, s.Store, s.ConfigSnapshot())
		cancel()
		if loadErr == nil {
			s.ApplyConfig(next)
			appliedRevision = snapshotRevision
			slog.Info("Provider configuration refreshed from PostgreSQL", "revision", snapshotRevision)
		} else if ctx.Err() == nil {
			slog.Error("refresh Provider configuration failed", "error", loadErr)
		}
		s.Mutation.Unlock()
	}
}
