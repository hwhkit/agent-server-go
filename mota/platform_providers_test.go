package mota

import (
	"os"
	"path/filepath"
	"testing"
)

func executableFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderCatalogMatchesAgentCenterAssignments(t *testing.T) {
	cfg := testPlatformConfig(t)
	cfg.LocalAgents.OpenCodeBinary = executableFile(t, "opencode")
	cfg.LocalAgents.DeepseekHarnessBinary = executableFile(t, "dsh")
	cfg.Gateway.APIKey = "gateway-secret"
	cfg.OpenAIAgents.Enabled = true
	cfg.OpenAIAgents.APIKey = "openai-secret"
	cfg.ClaudeManaged.Enabled = true
	cfg.ClaudeManaged.APIKey = "claude-secret"
	cfg.ClaudeManaged.AgentID = "agent_test"
	cfg.ClaudeManaged.EnvironmentID = "env_test"

	providers := ListProviders(cfg)
	if len(providers) != len(providerIDs) {
		t.Fatalf("provider count = %d, want %d", len(providers), len(providerIDs))
	}
	byID := map[string]ProviderView{}
	for _, provider := range providers {
		byID[provider.ID] = provider
	}
	for _, id := range []string{"opencode_local", "deepseek_harness", "openai_agent", "claude_managed_agent"} {
		if !byID[id].Configured {
			t.Errorf("%s should be configured", id)
		}
		if len(byID[id].Assignments) != 1 {
			t.Errorf("%s assignments = %v", id, byID[id].Assignments)
		}
	}
	if byID["openai_agent"].Config["api_key"] != nil || byID["claude_managed_agent"].Config["api_key"] != nil {
		t.Fatal("provider views must never expose API keys")
	}
	if !byID["openai_agent"].SecretConfigured || !byID["claude_managed_agent"].SecretConfigured {
		t.Fatal("secret presence should be reported without exposing plaintext")
	}
}

func TestProviderUpdatePreservesEmptySecretAndValidatesAssignments(t *testing.T) {
	cfg := testPlatformConfig(t)
	cfg.OpenAIAgents.APIKey = "existing-secret"
	next, stored, err := UpdateProvider(cfg, "openai_agent", map[string]any{
		"enabled": true, "api_key": "", "base_url": "https://api.openai.com/v1",
		"default_model": "gpt-6-astra", "reasoning_effort": "medium",
		"environment": "none", "mcp_server_label": "mota_tools",
		"mcp_required": true, "delete_session": true, "max_response_bytes": float64(2 * 1024 * 1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.OpenAIAgents.APIKey != "existing-secret" || stored["api_key"] != "existing-secret" {
		t.Fatal("empty secret input must preserve the encrypted provider credential")
	}
	if !IsAssignmentConfigured(next, ExecutorKindOpenAIAgent, ExecutionModeManaged) {
		t.Fatal("configured OpenAI managed provider should be assignable")
	}
	if IsAssignmentConfigured(next, ExecutorKindOpenAIAgent, ExecutionModeLocal) {
		t.Fatal("OpenAI managed provider must not be assignable as local")
	}
}

func TestProviderUpdateRejectsUnknownFields(t *testing.T) {
	if _, _, err := UpdateProvider(testPlatformConfig(t), "gateway",
		map[string]any{"base_url": "https://gateway.example.test", "typo_key": true}); err == nil {
		t.Fatal("unknown Provider field must be rejected")
	}
}

func TestProviderSecretEnvironmentOverridesDatabaseSnapshot(t *testing.T) {
	t.Setenv("AGENT_GATEWAY__API_KEY", "gateway-env-secret")
	t.Setenv("OPENAI_AGENTS__API_KEY", "openai-env-secret")
	t.Setenv("OPENAI_AGENTS__MCP_AUTHORIZATION", "mcp-env-secret")
	t.Setenv("CLAUDE_MANAGED__API_KEY", "claude-env-secret")
	cfg := testPlatformConfig(t)
	cfg.Gateway.APIKey = "gateway-db-secret"
	cfg.OpenAIAgents.APIKey = "openai-db-secret"
	cfg.OpenAIAgents.MCPAuthorization = "mcp-db-secret"
	cfg.ClaudeManaged.APIKey = "claude-db-secret"
	applyProviderSecretEnvOverrides(cfg)
	if cfg.Gateway.APIKey != "gateway-env-secret" ||
		cfg.OpenAIAgents.APIKey != "openai-env-secret" ||
		cfg.OpenAIAgents.MCPAuthorization != "mcp-env-secret" ||
		cfg.ClaudeManaged.APIKey != "claude-env-secret" {
		t.Fatalf("environment secrets did not remain authoritative")
	}
}

func TestNewLocalAgentCommandsUseRealHarnessProtocols(t *testing.T) {
	cfg := executorTestConfig(t)
	cfg.LocalAgents.OpenCodeBinary = "opencode"
	cfg.LocalAgents.QwenCodeBinary = "qwen"
	cfg.LocalAgents.GeminiCLIBinary = "gemini"
	cfg.LocalAgents.OpenHandsBinary = "openhands"
	cfg.LocalAgents.DeepseekHarnessBinary = "dsh"
	runtime := executorRuntime("fundamental_analyst", ExecutorKindOpenCode, ExecutionModeLocal)
	workspace := t.TempDir()

	cases := []struct {
		kind     LocalCliKind
		contains []string
		mode     LocalOutputMode
	}{
		{LocalCliOpenCode, []string{"run", "--agent", "plan", "--format", "default"}, LocalOutputText},
		{LocalCliQwenCode, []string{"--output-format", "text", "--safe-mode", "--prompt"}, LocalOutputText},
		{LocalCliGeminiCLI, []string{"--output-format", "json", "--approval-mode", "plan"}, LocalOutputGeminiJSON},
		{LocalCliOpenHands, []string{"--headless", "--json", "--task"}, LocalOutputOpenHandsJSONLines},
		{LocalCliDeepseekHarness, []string{"--profile", "headless", "--patch"}, LocalOutputText},
	}
	for _, test := range cases {
		command := agentCommand(cfg, runtime, workspace, "prompt", "contract", test.kind)
		if command.OutputMode != test.mode {
			t.Errorf("%s output mode = %v, want %v", test.kind.Label(), command.OutputMode, test.mode)
		}
		joined := " " + joinArgs(command.Cmd.Args[1:]) + " "
		for _, expected := range test.contains {
			if !containsArg(command.Cmd.Args[1:], expected) {
				t.Errorf("%s args %q missing %q", test.kind.Label(), joined, expected)
			}
		}
		for _, path := range command.CleanupPaths {
			_ = os.Remove(path)
		}
	}
}

func containsArg(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func joinArgs(values []string) string {
	result := ""
	for _, value := range values {
		if result != "" {
			result += " "
		}
		result += value
	}
	return result
}
