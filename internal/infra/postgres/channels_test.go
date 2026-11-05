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
