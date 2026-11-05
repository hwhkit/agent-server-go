package config

// 对应 rs src/a2a/config.rs 的单测。

import (
	"strings"
	"testing"
)

func TestA2AConfigDisabledByDefault(t *testing.T) {
	cfg, err := a2aConfigFromFile(&a2aFile{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Error("A2A must stay opt-in")
	}
	if cfg.BasePath != "/a2a" {
		t.Errorf("base_path = %q, want /a2a", cfg.BasePath)
	}
	if !cfg.RequireBearerAuth {
		t.Error("require_bearer_auth should default to true")
	}
	if cfg.AgentVersion != "1.2.0" {
		t.Errorf("agent_version = %q, want Rust package version 1.2.0", cfg.AgentVersion)
	}
}

func TestA2AConfigBasePathTrailingSlashIsTrimmed(t *testing.T) {
	enabled := true
	basePath := "/a2a/"
	cfg, err := a2aConfigFromFile(&a2aFile{Enabled: &enabled, BasePath: &basePath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BasePath != "/a2a" {
		t.Errorf("base_path = %q, want /a2a", cfg.BasePath)
	}
	if got := cfg.AgentPath("demo"); got != "/a2a/demo" {
		t.Errorf("AgentPath = %q, want /a2a/demo", got)
	}
}

func TestA2AConfigBasePathMustBeAbsolute(t *testing.T) {
	enabled := true
	basePath := "a2a"
	_, err := a2aConfigFromFile(&a2aFile{Enabled: &enabled, BasePath: &basePath})
	if err == nil || !strings.Contains(err.Error(), "base_path") {
		t.Fatalf("expected a base_path error, got %v", err)
	}
}

func TestA2AConfigProviderFieldsMustBePaired(t *testing.T) {
	enabled := true
	organization := "Intelligent Router"
	_, err := a2aConfigFromFile(&a2aFile{Enabled: &enabled, ProviderOrganization: &organization})
	if err == nil || !strings.Contains(err.Error(), "provider_url") {
		t.Fatalf("expected a provider_url error, got %v", err)
	}
}

func TestA2AConfigRejectsUnimplementedStreamingCapability(t *testing.T) {
	enabled, streaming := true, true
	_, err := a2aConfigFromFile(&a2aFile{Enabled: &enabled, StreamEnabled: &streaming})
	if err == nil || !strings.Contains(err.Error(), "stream_enabled") {
		t.Fatalf("expected a stream_enabled error, got %v", err)
	}
}

func TestA2AConfigDefaultCardSlugFallsBackToIRDefault(t *testing.T) {
	enabled := true
	cfg, err := a2aConfigFromFile(&a2aFile{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResolveDefaultCardSlug("  Local-Pi ")
	if cfg.DefaultCardSlug != "local-pi" {
		t.Errorf("default_card_slug = %q, want local-pi", cfg.DefaultCardSlug)
	}
}

// 守住 config/default.toml 的 [a2a] 段:字段名写错会被 TOML 反序列化静默忽略,
// 段本身写坏则会让服务启动失败。只断言"能加载",不断言具体值,因为环境里
// 可能设置了 A2A__* 覆盖。
func TestA2AConfigShippedDefaultConfigIsLoadable(t *testing.T) {
	if _, err := LoadA2AConfig(); err != nil {
		t.Fatalf("config/default.toml [a2a] must load: %v", err)
	}
}

// 关闭时不校验,避免默认配置里的空值阻塞启动。
func TestA2AConfigValidationIsSkippedWhileDisabled(t *testing.T) {
	basePath := "bogus"
	cfg, err := a2aConfigFromFile(&a2aFile{BasePath: &basePath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled config")
	}
}
