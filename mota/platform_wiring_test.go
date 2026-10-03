package mota

// 装配层(#12)测试:PlatformDependencies 注入是否落到 PlatformState 的对应
// 字段,以及 Dispatcher() 是否与 Push/Pull 共享同一套准入闸/执行闸。

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestPlatformDependenciesFlowIntoState 验证 NewPlatformStateWithDependencies
// 把两个 HTTP 客户端与三个 MOTA 密钥/开关正确落到 PlatformState 字段上。
//
// rs 侧这三项密钥/开关来自 MotaConfig,不属于平台自身配置;Go 侧同理经
// PlatformDependencies 注入,这里守住「装配阶段真的把值传进去了」这条链路。
func TestPlatformDependenciesFlowIntoState(t *testing.T) {
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)

	executorClient := &http.Client{}
	callbackClient := &http.Client{}
	state, err := NewPlatformStateWithDependencies(context.Background(), cfg, PlaceholderProcessor{},
		PlatformDependencies{
			ExecutorHTTPClient:           executorClient,
			CallbackHTTPClient:           callbackClient,
			SigningSecret:                "mbt_test_secret",
			CallbackToken:                "mpk_test_token",
			CallbackTokenOrigin:          "https://api.example.test",
			TrustedUsageReportingEnabled: true,
			DefaultAgentSlug:             "default-agent",
		})
	if err != nil {
		t.Fatalf("NewPlatformStateWithDependencies: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })

	if state.HTTPClient != executorClient {
		t.Error("HTTPClient must be the injected executor HTTP client")
	}
	if state.CallbackHTTPClient != callbackClient {
		t.Error("CallbackHTTPClient must be the injected callback HTTP client")
	}
	if state.SigningSecret != "mbt_test_secret" {
		t.Errorf("SigningSecret = %q", state.SigningSecret)
	}
	if state.CallbackToken != "mpk_test_token" {
		t.Errorf("CallbackToken = %q", state.CallbackToken)
	}
	if state.CallbackTokenOrigin != "https://api.example.test" {
		t.Errorf("CallbackTokenOrigin = %q", state.CallbackTokenOrigin)
	}
	if !state.TrustedUsageReportingEnabled {
		t.Error("TrustedUsageReportingEnabled must be true")
	}
	if state.DefaultAgentSlug != "default-agent" {
		t.Errorf("DefaultAgentSlug = %q", state.DefaultAgentSlug)
	}
}

// NewPlatformState(不带依赖) 必须等价于传空 PlatformDependencies —— 嵌入/
// 测试用的便捷构造不能悄悄改变行为。
func TestNewPlatformStateIsEquivalentToEmptyDependencies(t *testing.T) {
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)
	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatalf("NewPlatformState: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })

	if state.HTTPClient != nil {
		t.Error("HTTPClient should be nil without injected dependencies")
	}
	if state.SigningSecret != "" || state.CallbackToken != "" ||
		state.CallbackTokenOrigin != "" || state.DefaultAgentSlug != "" {
		t.Error("MOTA-derived fields should be empty without injected dependencies")
	}
	if state.TrustedUsageReportingEnabled {
		t.Error("TrustedUsageReportingEnabled should default to false")
	}
}

// ---------------------------------------------------------------------------
// Dispatcher()
// ---------------------------------------------------------------------------

func TestPlatformDispatcherResolvesRegisteredAgent(t *testing.T) {
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)
	cfg.WorkspaceRoot = t.TempDir()
	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })

	slug := uniqueSlug("dispatch")
	secret := unique("dispatch-secret")
	agent, err := state.Store.CreateAgent(context.Background(), &AgentInput{
		Slug: slug, Name: "dispatcher test agent", ExecutorKind: ExecutorKindRust,
		ExecutionMode: ExecutionModeLocal, SigningSecret: &secret,
		SignatureEncoding: "hex", WorkspacePath: "agents",
		PermissionMode: "dontAsk", TimeoutSecs: uint64Ptr(30),
		MaxConcurrency: intPtr(2), QueueCapacity: intPtr(4),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = state.Store.DeleteAgent(context.Background(), slug) })
	state.Registry.Replace(agent)

	dispatcher, err := state.Dispatcher()
	if err != nil {
		t.Fatalf("Dispatcher(): %v", err)
	}
	out, err := dispatcher.Execute(context.Background(), slug, &TaskContext{Params: map[string]any{}},
		map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatalf("dispatcher.Execute: %v", err)
	}
	if out == nil {
		t.Fatal("expected a non-nil output from the registered agent")
	}
}

// Dispatcher() 必须缓存结果:两次调用拿到同一个实例,否则 Push/Pull/心跳三条
// 路径各自持有独立的准入闸/执行闸,容量控制形同虚设。
func TestPlatformDispatcherIsCached(t *testing.T) {
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)
	cfg.WorkspaceRoot = t.TempDir()
	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })

	first, err := state.Dispatcher()
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.Dispatcher()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("Dispatcher() must return the same cached instance across calls")
	}
}

// SetDispatcher 抑制按需构造,供需要注入自定义分发器的测试使用。
func TestPlatformSetDispatcherSuppressesLazyBuild(t *testing.T) {
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)
	cfg.WorkspaceRoot = t.TempDir()
	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })

	injected := NewAgentDispatcher(state.Registry, nil, state.GlobalExecution, "")
	state.SetDispatcher(injected)

	got, err := state.Dispatcher()
	if err != nil {
		t.Fatal(err)
	}
	if got != injected {
		t.Fatal("Dispatcher() must return the injected instance after SetDispatcher")
	}
}
