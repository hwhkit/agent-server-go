package postgres_test

// 心跳通道凭据与通道事件存储的集成测试。门禁同 store_test.go。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/infra/crypto"
	"ir-agent-server/internal/infra/postgres"
)

func TestListChannelsAndRecordChannelEvent(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	agents := postgres.NewAgentStore(db, cipher)
	events := postgres.NewChannelEventStore(db)

	slug := "hb-desk-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	channelAgent := &agent.Agent{
		Slug: slug, Name: "心跳台", Enabled: true,
		Binding:       exec.Binding{Kind: exec.KindPi, Mode: exec.ModeLocal},
		Positions:     []string{"FUNDAMENTAL_ANALYST"},
		SigningSecret: []byte("channel-secret-32-bytes-long!!!"),
	}
	if _, err := agents.Upsert(ctx, channelAgent, nil); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = agents.Delete(context.Background(), slug) })

	channels, err := agents.ListChannels(ctx)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	found := false
	for _, channel := range channels {
		if channel.Slug == slug {
			found = true
			if channel.SigningSecret != "channel-secret-32-bytes-long!!!" {
				t.Error("channel secret must round-trip through AES-GCM")
			}
		}
	}
	if !found {
		t.Fatal("enabled agent must appear in heartbeat channels")
	}

	// 禁用后通道消失。
	channelAgent.Enabled = false
	rev := channelAgent.Revision
	if _, err := agents.Upsert(ctx, channelAgent, &rev); err != nil {
		t.Fatalf("disable agent: %v", err)
	}
	channels, err = agents.ListChannels(ctx)
	if err != nil {
		t.Fatalf("list channels after disable: %v", err)
	}
	for _, channel := range channels {
		if channel.Slug == slug {
			t.Error("disabled agent must not keep a heartbeat channel")
		}
	}

	// 通道事件写入。
	status := int64(200)
	pass := true
	latency := int64(42)
	slugCopy := slug
	if err := events.RecordChannelEvent(ctx, audit.ChannelEvent{
		EventType: "heartbeat", AgentSlug: &slugCopy,
		URL: "https://platform.example.com", StatusCode: &status,
		Pass: &pass, LatencyMs: &latency,
	}); err != nil {
		t.Fatalf("record channel event: %v", err)
	}
}

// TestCachedChannelLister 覆盖心跳缓存装饰器：TTL 内复用缓存不重读、
// 注册表版本变化立即失效重读（换钥即刻生效）、缓存值随重读更新。
func TestCachedChannelLister(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	cipher, err := crypto.NewSecretCipher("it-shared-master")
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewAgentStore(db, cipher)
	slug := "it-chan-cache-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a := &agent.Agent{
		Slug: slug, Name: "缓存通道", Binding: exec.Binding{Kind: "codex", Mode: exec.ModeLocal},
		Positions:     []string{"FUNDAMENTAL_ANALYST"},
		SigningSecret: []byte("cache-channel-secret-01"), Enabled: true,
	}
	if _, err := store.Upsert(ctx, a, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), slug) })

	lister := postgres.NewCachedChannelLister(store, time.Hour) // TTL 长到测试内不会自然过期

	first, err := lister.ListChannels(ctx)
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	found := false
	for _, channel := range first {
		if channel.Slug == slug && channel.SigningSecret == "cache-channel-secret-01" {
			found = true
		}
	}
	if !found {
		t.Fatalf("channel missing from first list: %v", first)
	}

	// 换钥：Upsert 自身会递增注册表版本 → 缓存立即失效（版本栅栏优先于
	// TTL），下一次心跳即用新钥——换钥不断心跳。
	if _, err := store.Upsert(ctx, &agent.Agent{
		Slug: slug, Name: "缓存通道", Binding: exec.Binding{Kind: "codex", Mode: exec.ModeLocal},
		Positions:     []string{"FUNDAMENTAL_ANALYST"},
		SigningSecret: []byte("cache-channel-secret-02"), Enabled: true,
	}, nil); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	refreshed, err := lister.ListChannels(ctx)
	if err != nil {
		t.Fatalf("refreshed list: %v", err)
	}
	found = false
	for _, channel := range refreshed {
		if channel.Slug == slug && channel.SigningSecret == "cache-channel-secret-02" {
			found = true
		}
	}
	if !found {
		t.Error("registry version change must invalidate the cache immediately")
	}
}
