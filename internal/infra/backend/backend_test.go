package backend_test

// 执行后端存储与管理器集成测试（真 PostgreSQL，门禁 V2_TEST_DATABASE_URL /
// TEST_DATABASE_URL）。覆盖：加密组保存往返、密文落库、CAS 冲突、组别名
// revision 联动、同事务审计、Manager 列表/更新/密钥保留/热更新回调。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appbackend "ir-agent-server/internal/app/backend"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/infra/backend"
	"ir-agent-server/internal/infra/config"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/postgres"
)

func testStore(t *testing.T) (*backend.Store, *postgres.DB) {
	t.Helper()
	url := os.Getenv("V2_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no test database configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(db.Close)
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatal(err)
	}
	return backend.NewStore(db.Pool, cipher), db
}

func cleanupProviderKeys(t *testing.T, db *postgres.DB, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.Pool.Exec(context.Background(),
				`DELETE FROM platform_settings WHERE key IN ($1, $2)`,
				"executor_provider:"+id, "executor_provider_item_revision:"+id)
		}
	})
}

func TestProviderStoreGroupRoundTrip(t *testing.T) {
	store, db := testStore(t)
	cleanupProviderKeys(t, db, "gateway", "deepseek_harness")
	ctx := context.Background()
	secret := "gw-plain-secret-" + time.Now().Format("150405.000000000")

	values := map[string]map[string]any{
		"gateway": {
			"base_url": "https://gw.example.test", "api_key": secret,
			"model": "test-model", "protocol": "anthropic",
			"max_tokens": float64(4096), "max_attempts": float64(2), "max_response_bytes": float64(1024),
		},
		"deepseek_harness": {"binary": "/nonexistent/dsh-" + secret},
	}
	revision, err := store.PutExecutorProviderGroupIfRevision(ctx, "gateway", values, nil)
	if err != nil {
		t.Fatalf("put group: %v", err)
	}
	if revision < 1 {
		t.Errorf("revision = %d", revision)
	}

	// 往返一致。
	loaded, err := store.GetExecutorProvider(ctx, "gateway")
	if err != nil || loaded == nil {
		t.Fatalf("get gateway: %v %v", loaded, err)
	}
	if loaded["api_key"] != secret || loaded["base_url"] != "https://gw.example.test" {
		t.Errorf("round trip mismatch: %v", loaded)
	}

	// 密文落库：明文密钥不得出现在 platform_settings 原始值里。
	var raw string
	if err := db.Pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key = 'executor_provider:gateway'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, secret) {
		t.Error("provider value stored in plaintext")
	}

	// 组别名 revision 联动：两个入口都递增，任一表单都会因对方保存而失效。
	revisions, err := store.ListExecutorProviderRevisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if revisions["gateway"] != revision || revisions["deepseek_harness"] != revision {
		t.Errorf("item revisions = %v (primary %d)", revisions, revision)
	}
	globalBefore, err := store.ExecutorProviderRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// CAS 冲突：过期 expected revision 被拒绝且不写任何值。
	stale := revision + 100
	_, err = store.PutExecutorProviderGroupIfRevision(ctx, "gateway",
		map[string]map[string]any{"gateway": {"base_url": "https://evil.example.test"}}, &stale)
	if !errors.Is(err, backend.ErrStaleProviderRevision) {
		t.Fatalf("stale put err = %v", err)
	}
	after, err := store.GetExecutorProvider(ctx, "gateway")
	if err != nil || after["base_url"] != "https://gw.example.test" {
		t.Errorf("stale put mutated value: %v %v", after, err)
	}
	globalAfter, err := store.ExecutorProviderRevision(ctx)
	if err != nil || globalAfter != globalBefore {
		t.Errorf("stale put bumped global revision: %d -> %d (%v)", globalBefore, globalAfter, err)
	}

	// Audited 保存：审计行同事务落库；action/target 不符被拒绝。
	actor := "it-backend-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM audit_log WHERE actor = $1`, actor)
	})
	fresh := revision
	revision2, err := store.PutExecutorProviderGroupIfRevisionAudited(ctx, "gateway", values, &fresh,
		audit.Event{Actor: actor, Action: "provider.update", Target: "gateway",
			Detail: json.RawMessage(`{"providerId":"gateway"}`)})
	if err != nil {
		t.Fatalf("audited put: %v", err)
	}
	if revision2 != fresh+1 {
		t.Errorf("audited revision = %d want %d", revision2, fresh+1)
	}
	var auditCount int
	if err := db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE actor = $1 AND action = 'provider.update' AND target = 'gateway'`,
		actor).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Errorf("audit rows = %d", auditCount)
	}
	_, err = store.PutExecutorProviderGroupIfRevisionAudited(ctx, "gateway", values, nil,
		audit.Event{Actor: actor, Action: "agent.update", Target: "gateway"})
	if err == nil {
		t.Error("mismatched audit action must be rejected")
	}
}

func TestProviderManagerLifecycle(t *testing.T) {
	store, db := testStore(t)
	cleanupProviderKeys(t, db, "openai_agent")
	ctx := context.Background()

	initial, appliedRevision, err := backend.LoadInitial(ctx, store, &config.PlatformConfig{})
	if err != nil {
		t.Fatalf("load initial: %v", err)
	}
	applies := 0
	manager := backend.NewManager(store, initial, appliedRevision,
		func(*config.PlatformConfig) { applies++ })

	// List：全目录 + revision 盖章 + 热更新回调触发。
	views, err := manager.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 11 {
		t.Errorf("views = %d", len(views))
	}
	var current *int64
	for index := range views {
		if views[index].ID == "openai_agent" {
			revision := views[index].Revision
			current = &revision
			if views[index].Configured {
				t.Error("openai_agent must start unconfigured")
			}
		}
	}
	if current == nil {
		t.Fatal("openai_agent missing from list")
	}
	if applies == 0 {
		t.Error("list must apply the consistent snapshot")
	}

	// Update：保存 → 视图 configured，密钥永不回显。
	apiKey := "sk-it-" + time.Now().Format("150405.000000000")
	// 表单语义：全字段提交。base_url 必须非空——空值会被存库并在下一次
	// 快照重载时校验失败（与旧栈一致；真实部署由 default.toml 提供默认值）。
	revision, view, err := manager.Update(ctx, "openai_agent", map[string]any{
		"enabled": true, "api_key": apiKey, "default_model": "gpt-test",
		"base_url": "https://api.openai.example.test",
		// 重载校验要求这些字符串字段非空（assignString allowEmpty=false）。
		"reasoning_effort": "medium", "environment": "it-env",
		"mcp_server_label": "it_tools",
	}, current, "it-actor")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if revision != *current+1 || view.Revision != revision {
		t.Errorf("revision = %d %d (base %d)", revision, view.Revision, *current)
	}
	if !view.Configured || !view.SecretConfigured {
		t.Errorf("view = %+v", view)
	}
	if raw, _ := json.Marshal(view); strings.Contains(string(raw), apiKey) {
		t.Error("view must never echo the api key")
	}
	if manager.Snapshot().OpenAIAgents.APIKey != apiKey {
		t.Error("hot apply did not publish the new config")
	}

	// 空密钥 = 保留既有密文（表单不回显导致的空提交语义）。
	_, view, err = manager.Update(ctx, "openai_agent",
		map[string]any{"api_key": ""}, nil, "it-actor")
	if err != nil {
		t.Fatalf("empty-secret update: %v", err)
	}
	if !view.SecretConfigured || manager.Snapshot().OpenAIAgents.APIKey != apiKey {
		t.Error("empty secret must preserve the stored key")
	}

	// 显式 If-Match 冲突 → ErrStaleRevision。
	stale := revision + 100
	_, _, err = manager.Update(ctx, "openai_agent",
		map[string]any{"default_model": "x"}, &stale, "it-actor")
	if !errors.Is(err, backend.ErrStaleProviderRevision) {
		t.Errorf("stale update err = %v", err)
	}

	// 未知 Provider / 非法字段。
	_, _, err = manager.Update(ctx, "no_such_provider", map[string]any{}, nil, "it-actor")
	if !errors.Is(err, appbackend.ErrUnknownProvider) {
		t.Errorf("unknown provider err = %v", err)
	}
	_, _, err = manager.Update(ctx, "openai_agent", map[string]any{"bogus_field": 1}, nil, "it-actor")
	if err == nil || errors.Is(err, backend.ErrStaleProviderRevision) {
		t.Errorf("invalid field err = %v", err)
	}
}
