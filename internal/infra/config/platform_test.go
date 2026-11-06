package config

import (
	"ir-agent-server/internal/infra/crypto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validPlatformRoot 返回一份最小的、能通过校验的 TOML 镜像。
func validPlatformRoot() *PlatformRootFile {
	password := "strong-test-password"
	secret := strings.Repeat("s", 32)
	return &PlatformRootFile{
		Admin: AdminSectionFile{Password: &password, SessionSecret: &secret},
	}
}

// 对应 rs execution_backends_have_safe_defaults。
func TestPlatformExecutionBackendsHaveSafeDefaults(t *testing.T) {
	cfg, err := PlatformConfigFromFile(validPlatformRoot())
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.Gateway.Protocol != GatewayProtocolAnthropic {
		t.Errorf("gateway.protocol = %q, want anthropic", cfg.Gateway.Protocol)
	}
	if cfg.AllowLegacyBodySignature {
		t.Error("legacy dynamic Agent body-only signature must be disabled by default")
	}
	if cfg.Gateway.MaxAttempts != 3 {
		t.Errorf("gateway.max_attempts = %d, want 3", cfg.Gateway.MaxAttempts)
	}
	if cfg.Gateway.Model != "dsk_4f" {
		t.Errorf("gateway.model = %q, want dsk_4f", cfg.Gateway.Model)
	}
	if cfg.Gateway.MaxTokens != 16384 {
		t.Errorf("gateway.max_tokens = %d, want 16384", cfg.Gateway.MaxTokens)
	}
	if cfg.Gateway.MaxResponseBytes != 2*1024*1024 {
		t.Errorf("gateway.max_response_bytes = %d, want 2097152", cfg.Gateway.MaxResponseBytes)
	}
	if cfg.LocalAgents.CodexBinary != "codex" {
		t.Errorf("local_agents.codex_binary = %q, want codex", cfg.LocalAgents.CodexBinary)
	}
	if cfg.LocalAgents.PiBinary != "/usr/local/bin/pi" {
		t.Errorf("local_agents.pi_binary = %q, want /usr/local/bin/pi", cfg.LocalAgents.PiBinary)
	}
	if cfg.LocalAgents.ReasonixBinary != "/usr/local/bin/reasonix" {
		t.Errorf("local_agents.reasonix_binary = %q, want /usr/local/bin/reasonix", cfg.LocalAgents.ReasonixBinary)
	}
	if cfg.LocalAgents.DeepseekHarnessBinary != "dsh" || cfg.LocalAgents.OpenCodeBinary != "opencode" || cfg.LocalAgents.QwenCodeBinary != "qwen" || cfg.LocalAgents.GeminiCLIBinary != "gemini" || cfg.LocalAgents.OpenHandsBinary != "openhands" {
		t.Errorf("new local Agent defaults are incomplete: %+v", cfg.LocalAgents)
	}
	if cfg.OpenAIAgents.Enabled || cfg.OpenAIAgents.BaseURL != "https://api.openai.com/v1" || cfg.OpenAIAgents.DefaultModel != "gpt-6-astra" {
		t.Errorf("unexpected OpenAI Agents defaults: %+v", cfg.OpenAIAgents)
	}
	if cfg.ClaudeManaged.Enabled || cfg.ClaudeManaged.BaseURL != "https://api.anthropic.com/v1" {
		t.Errorf("unexpected Claude Managed defaults: %+v", cfg.ClaudeManaged)
	}
	if cfg.LocalAgents.DefaultModel != "litellm/dsk_4f" {
		t.Errorf("local_agents.default_model = %q, want litellm/dsk_4f", cfg.LocalAgents.DefaultModel)
	}
	if cfg.LocalAgents.MaxPromptBytes != 128*1024 {
		t.Errorf("local_agents.max_prompt_bytes = %d, want 131072", cfg.LocalAgents.MaxPromptBytes)
	}
	if !containsString(cfg.LocalAgents.AllowedEnv, "CODEX_HOME") {
		t.Error("local_agents.allowed_env must include CODEX_HOME")
	}
	if !containsString(cfg.LocalAgents.AllowedEnv, "IR_FINANCE_USER_AGENT") {
		t.Error("local_agents.allowed_env must include IR_FINANCE_USER_AGENT")
	}
	if !containsString(cfg.ClaudeAllowedEnv, "ANTHROPIC_API_KEY") {
		t.Error("claude.allowed_env must include ANTHROPIC_API_KEY")
	}
	if cfg.ClaudeDefaultModel != "dsk_4f" {
		t.Errorf("claude.default_model = %q, want dsk_4f", cfg.ClaudeDefaultModel)
	}
	// 会话 TTL 默认 72 小时。
	if cfg.AdminSessionTTL != 72*time.Hour {
		t.Errorf("admin.session_ttl = %s, want 72h", cfg.AdminSessionTTL)
	}
}

func TestManagedProvidersRequireCredentialsWhenEnabled(t *testing.T) {
	root := validPlatformRoot()
	enabled := true
	root.OpenAIAgents.Enabled = &enabled
	_, err := PlatformConfigFromFile(root)
	requireErrContains(t, err, "openai_agents].enabled=true requires api_key")

	root = validPlatformRoot()
	root.ClaudeManaged.Enabled = &enabled
	_, err = PlatformConfigFromFile(root)
	requireErrContains(t, err, "claude_managed].enabled=true requires api_key, agent_id, and environment_id")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// 对应 rs invalid_gateway_protocol_is_rejected。
func TestPlatformInvalidGatewayProtocolRejected(t *testing.T) {
	root := validPlatformRoot()
	protocol := "grpc"
	root.Gateway.Protocol = &protocol
	_, err := PlatformConfigFromFile(root)
	requireErrContains(t, err, "anthropic or openai")
}

func TestPlatformGatewayProtocolAliases(t *testing.T) {
	for _, raw := range []string{"anthropic", "ANTHROPIC", "  Anthropic  "} {
		root := validPlatformRoot()
		value := raw
		root.Gateway.Protocol = &value
		cfg, err := PlatformConfigFromFile(root)
		if err != nil {
			t.Fatalf("protocol %q: %v", raw, err)
		}
		if cfg.Gateway.Protocol != GatewayProtocolAnthropic {
			t.Errorf("protocol %q => %q, want anthropic", raw, cfg.Gateway.Protocol)
		}
	}
	for _, raw := range []string{"openai", "open_ai", "OpenAI"} {
		root := validPlatformRoot()
		value := raw
		root.Gateway.Protocol = &value
		cfg, err := PlatformConfigFromFile(root)
		if err != nil {
			t.Fatalf("protocol %q: %v", raw, err)
		}
		if cfg.Gateway.Protocol != GatewayProtocolOpenAI {
			t.Errorf("protocol %q => %q, want openai", raw, cfg.Gateway.Protocol)
		}
	}
}

// 对应 rs unsafe_retry_and_output_limits_are_rejected。
func TestPlatformUnsafeRetryAndOutputLimitsRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PlatformRootFile)
		wantIn string
	}{
		{"gateway.max_attempts=0", func(r *PlatformRootFile) {
			value := 0
			r.Gateway.MaxAttempts = &value
		}, "[gateway].max_attempts"},
		{"gateway.max_attempts=7", func(r *PlatformRootFile) {
			value := 7
			r.Gateway.MaxAttempts = &value
		}, "[gateway].max_attempts"},
		{"gateway.max_tokens=255", func(r *PlatformRootFile) {
			value := uint64(255)
			r.Gateway.MaxTokens = &value
		}, "[gateway].max_tokens"},
		{"gateway.max_response_bytes=1024", func(r *PlatformRootFile) {
			value := 1024
			r.Gateway.MaxResponseBytes = &value
		}, "[gateway].max_response_bytes"},
		{"gateway.model empty", func(r *PlatformRootFile) {
			value := "   "
			r.Gateway.Model = &value
		}, "[gateway].model"},
		{"local_agents.max_output_bytes=1024", func(r *PlatformRootFile) {
			value := 1024
			r.LocalAgents.MaxOutputBytes = &value
		}, "[local_agents].max_output_bytes"},
		{"local_agents.max_prompt_bytes=1024", func(r *PlatformRootFile) {
			value := 1024
			r.LocalAgents.MaxPromptBytes = &value
		}, "[local_agents].max_prompt_bytes"},
		{"local_agents.max_prompt_bytes too large", func(r *PlatformRootFile) {
			value := 2 * 1024 * 1024
			r.LocalAgents.MaxPromptBytes = &value
		}, "[local_agents].max_prompt_bytes"},
		{"local_agents.pi_binary empty", func(r *PlatformRootFile) {
			value := "  "
			r.LocalAgents.PiBinary = &value
		}, "[local_agents].pi_binary"},
		{"claude.max_output_bytes=1024", func(r *PlatformRootFile) {
			value := 1024
			r.Claude.MaxOutputBytes = &value
		}, "[claude].max_output_bytes"},
		{"platform.max_request_body_bytes=512", func(r *PlatformRootFile) {
			value := 512
			r.Platform.MaxRequestBodyBytes = &value
		}, "[platform].max_request_body_bytes"},
		{"platform.global_max_concurrency=0", func(r *PlatformRootFile) {
			value := 0
			r.Platform.GlobalMaxConcurrency = &value
		}, "[platform].global_max_concurrency"},
		{"platform.global_max_concurrency=257", func(r *PlatformRootFile) {
			value := 257
			r.Platform.GlobalMaxConcurrency = &value
		}, "[platform].global_max_concurrency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			tc.mutate(root)
			_, err := PlatformConfigFromFile(root)
			requireErrContains(t, err, tc.wantIn)
		})
	}
}

// ---------------------------------------------------------------------------
// admin / public_base_url 安全规则
// ---------------------------------------------------------------------------

func TestPlatformAdminCredentialRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PlatformRootFile)
		wantIn string
	}{
		{"no password at all", func(r *PlatformRootFile) { r.Admin.Password = nil }, "admin login requires"},
		{"blank password treated as unset", func(r *PlatformRootFile) {
			value := "   "
			r.Admin.Password = &value
		}, "admin login requires"},
		{"placeholder admin", func(r *PlatformRootFile) {
			value := "admin"
			r.Admin.Password = &value
		}, "unsafe placeholder"},
		{"placeholder password", func(r *PlatformRootFile) {
			value := "password"
			r.Admin.Password = &value
		}, "unsafe placeholder"},
		{"placeholder change-me", func(r *PlatformRootFile) {
			value := "change-me"
			r.Admin.Password = &value
		}, "unsafe placeholder"},
		{"public example password", func(r *PlatformRootFile) {
			value := exampleAdminPassword
			r.Admin.Password = &value
		}, "unsafe placeholder"},
		{"password shorter than account policy", func(r *PlatformRootFile) {
			value := "too-short"
			r.Admin.Password = &value
		}, "password must contain 12-512 bytes"},
		{"short session secret", func(r *PlatformRootFile) {
			value := strings.Repeat("s", 31)
			r.Admin.SessionSecret = &value
		}, "[admin].session_secret"},
		{"public example session secret", func(r *PlatformRootFile) {
			value := exampleAdminSessionSecret
			r.Admin.SessionSecret = &value
		}, "public template placeholder"},
		{"session ttl too short", func(r *PlatformRootFile) {
			value := uint64(299)
			r.Admin.SessionTTLSecs = &value
		}, "[admin].session_ttl_secs"},
		{"session ttl too long", func(r *PlatformRootFile) {
			value := uint64(30*24*60*60 + 1)
			r.Admin.SessionTTLSecs = &value
		}, "[admin].session_ttl_secs"},
		{"invalid trusted proxy CIDR", func(r *PlatformRootFile) {
			value := []string{"not-a-cidr"}
			r.Admin.TrustedProxyCIDRs = &value
		}, "trusted_proxy_cidrs"},
		{"empty username", func(r *PlatformRootFile) {
			value := "   "
			r.Admin.Username = &value
		}, "[admin].username"},
		{"non-argon2 hash", func(r *PlatformRootFile) {
			r.Admin.Password = nil
			value := "$2b$12$notargon"
			r.Admin.PasswordHash = &value
		}, "Argon2 PHC string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			tc.mutate(root)
			_, err := PlatformConfigFromFile(root)
			requireErrContains(t, err, tc.wantIn)
		})
	}
}

func TestManagedProvidersRejectPublicTemplateAPIKeys(t *testing.T) {
	enabled := true

	openAI := validPlatformRoot()
	openAI.OpenAIAgents.Enabled = &enabled
	openAIKey := exampleProviderAPIKey
	openAI.OpenAIAgents.APIKey = &openAIKey
	_, err := PlatformConfigFromFile(openAI)
	requireErrContains(t, err, "openai_agents].api_key is a public template placeholder")

	claude := validPlatformRoot()
	claude.ClaudeManaged.Enabled = &enabled
	claudeKey := exampleProviderAPIKey
	agentID, environmentID := "agent-test", "environment-test"
	claude.ClaudeManaged.APIKey = &claudeKey
	claude.ClaudeManaged.AgentID = &agentID
	claude.ClaudeManaged.EnvironmentID = &environmentID
	_, err = PlatformConfigFromFile(claude)
	requireErrContains(t, err, "claude_managed].api_key is a public template placeholder")
}

// password_hash 单独提供(不给明文密码)时应通过。
func TestPlatformArgon2HashAloneIsAccepted(t *testing.T) {
	root := validPlatformRoot()
	root.Admin.Password = nil
	hash, err := crypto.HashPassword("valid-test-password")
	if err != nil {
		t.Fatal(err)
	}
	root.Admin.PasswordHash = &hash
	cfg, err := PlatformConfigFromFile(root)
	if err != nil {
		t.Fatalf("argon2 hash alone must be accepted, got %v", err)
	}
	if cfg.AdminPasswordHash != hash {
		t.Errorf("admin_password_hash = %q, want %q", cfg.AdminPasswordHash, hash)
	}
}

func TestPlatformPublicBaseURLRules(t *testing.T) {
	for _, tc := range []struct {
		name          string
		baseURL       string
		secureCookies bool
		wantIn        string
	}{
		{"https requires secure cookies", "https://agents.example.com", false, "secure_cookies"},
		{"non-loopback http rejected", "http://agents.example.com", false, "loopback"},
		{"bad scheme rejected", "ftp://127.0.0.1:8080", false, "must use http or https"},
		{"relative rejected", "127.0.0.1:8080", false, "public_base_url"},
		{"credentials rejected", "https://user:pass@agents.example.com", true, "must not contain credentials"},
		{"query rejected", "https://agents.example.com?token=secret", true, "query string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			baseURL := tc.baseURL
			root.Platform.PublicBaseURL = &baseURL
			secure := tc.secureCookies
			root.Admin.SecureCookies = &secure
			_, err := PlatformConfigFromFile(root)
			requireErrContains(t, err, tc.wantIn)
		})
	}

	for _, tc := range []struct {
		baseURL       string
		secureCookies bool
	}{
		{"http://127.0.0.1:8080", false},
		{"http://localhost:8080", false},
		{"https://agents.example.com", true},
	} {
		root := validPlatformRoot()
		baseURL := tc.baseURL
		root.Platform.PublicBaseURL = &baseURL
		secure := tc.secureCookies
		root.Admin.SecureCookies = &secure
		if _, err := PlatformConfigFromFile(root); err != nil {
			t.Errorf("%s should be accepted, got %v", tc.baseURL, err)
		}
	}
}

func TestProviderURLsRejectEmbeddedCredentialsAndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PlatformRootFile)
	}{
		{"gateway userinfo", func(root *PlatformRootFile) {
			value := "https://user:pass@gateway.example.com/v1"
			root.Gateway.BaseURL = &value
		}},
		{"openai query", func(root *PlatformRootFile) {
			value := "https://api.example.com/v1?api_key=secret"
			root.OpenAIAgents.BaseURL = &value
		}},
		{"mcp fragment", func(root *PlatformRootFile) {
			value := "https://mcp.example.com/sse#secret"
			root.OpenAIAgents.MCPServerURL = &value
		}},
		{"claude userinfo", func(root *PlatformRootFile) {
			value := "https://token@api.example.com/v1"
			root.ClaudeManaged.BaseURL = &value
		}},
		{"gateway remote http", func(root *PlatformRootFile) {
			value := "http://gateway.example.com/v1"
			root.Gateway.BaseURL = &value
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			tc.mutate(root)
			if _, err := PlatformConfigFromFile(root); err == nil {
				t.Fatal("credential-bearing Provider URL must be rejected")
			}
		})
	}
}

// public_base_url 的尾部斜杠会被去掉。
func TestPlatformPublicBaseURLTrailingSlashTrimmed(t *testing.T) {
	root := validPlatformRoot()
	baseURL := "http://127.0.0.1:8080///"
	root.Platform.PublicBaseURL = &baseURL
	cfg, err := PlatformConfigFromFile(root)
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.PublicBaseURL != "http://127.0.0.1:8080" {
		t.Errorf("public_base_url = %q, want http://127.0.0.1:8080", cfg.PublicBaseURL)
	}
}

// 平台禁用时跳过全部校验。
func TestPlatformDisabledSkipsValidation(t *testing.T) {
	root := &PlatformRootFile{}
	disabled := false
	root.Platform.Enabled = &disabled
	if _, err := PlatformConfigFromFile(root); err != nil {
		t.Fatalf("disabled platform must skip validation, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// skill / extension 路径校验
// ---------------------------------------------------------------------------

func TestPlatformSkillPathRules(t *testing.T) {
	dir := t.TempDir()
	makeSkill := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("# skill"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	one, two, three := makeSkill("a.md"), makeSkill("b.md"), makeSkill("c.md")

	// strategy 必须是 2-3 个。
	for _, tc := range []struct {
		name  string
		paths []string
		valid bool
	}{
		{"one strategy skill rejected", []string{one}, false},
		{"two accepted", []string{one, two}, true},
		{"three accepted", []string{one, two, three}, true},
		{"four rejected", []string{one, two, three, one}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			paths := tc.paths
			root.LocalAgents.StrategySkillPaths = &paths
			_, err := PlatformConfigFromFile(root)
			if tc.valid && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.valid {
				requireErrContains(t, err, "[local_agents].strategy_skill_paths")
			}
		})
	}

	// 不存在的路径一律拒绝。
	missing := filepath.Join(dir, "missing.md")
	for _, tc := range []struct {
		name   string
		mutate func(*PlatformRootFile)
		wantIn string
	}{
		{"strategy missing file", func(r *PlatformRootFile) {
			paths := []string{one, missing}
			r.LocalAgents.StrategySkillPaths = &paths
		}, "[local_agents].strategy_skill_paths"},
		{"analysis missing file", func(r *PlatformRootFile) {
			paths := []string{missing}
			r.LocalAgents.AnalysisSkillPaths = &paths
		}, "[local_agents].analysis_skill_paths"},
		{"workflow missing file", func(r *PlatformRootFile) {
			paths := []string{missing}
			r.LocalAgents.WorkflowSkillPaths = &paths
		}, "[local_agents].workflow_skill_paths"},
		{"finance extension missing file", func(r *PlatformRootFile) {
			r.LocalAgents.FinanceExtensionPath = &missing
		}, "[local_agents].finance_extension_path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := validPlatformRoot()
			tc.mutate(root)
			_, err := PlatformConfigFromFile(root)
			requireErrContains(t, err, tc.wantIn)
		})
	}
}

// ---------------------------------------------------------------------------
// 环境变量覆盖
// ---------------------------------------------------------------------------

func TestApplyPlatformEnvOverrides(t *testing.T) {
	t.Setenv("PLATFORM__GLOBAL_MAX_CONCURRENCY", "32")
	t.Setenv("AGENT_GATEWAY__PROTOCOL", "openai")
	t.Setenv("AGENT_GATEWAY__MAX_ATTEMPTS", "5")
	t.Setenv("AGENT_GATEWAY__MAX_TOKENS", "8192")
	t.Setenv("LOCAL_AGENT__PI_BINARY", "/opt/pi")
	t.Setenv("LOCAL_AGENT__ALLOWED_ENV", " HOME , PATH ,, CUSTOM ")
	t.Setenv("ADMIN__SECURE_COOKIES", "yes")

	root := validPlatformRoot()
	if err := applyPlatformEnv(root); err != nil {
		t.Fatalf("applyPlatformEnv: %v", err)
	}
	// secure_cookies=true 时 http loopback 仍然合法。
	cfg, err := PlatformConfigFromFile(root)
	if err != nil {
		t.Fatalf("PlatformConfigFromFile: %v", err)
	}
	if cfg.GlobalMaxConcurrency != 32 {
		t.Errorf("global_max_concurrency = %d, want 32", cfg.GlobalMaxConcurrency)
	}
	if cfg.Gateway.Protocol != GatewayProtocolOpenAI {
		t.Errorf("gateway.protocol = %q, want openai", cfg.Gateway.Protocol)
	}
	if cfg.Gateway.MaxAttempts != 5 {
		t.Errorf("gateway.max_attempts = %d, want 5", cfg.Gateway.MaxAttempts)
	}
	if cfg.Gateway.MaxTokens != 8192 {
		t.Errorf("gateway.max_tokens = %d, want 8192", cfg.Gateway.MaxTokens)
	}
	if cfg.LocalAgents.PiBinary != "/opt/pi" {
		t.Errorf("local_agents.pi_binary = %q, want /opt/pi", cfg.LocalAgents.PiBinary)
	}
	want := []string{"HOME", "PATH", "CUSTOM"}
	if len(cfg.LocalAgents.AllowedEnv) != len(want) {
		t.Fatalf("allowed_env = %v, want %v", cfg.LocalAgents.AllowedEnv, want)
	}
	for i, value := range want {
		if cfg.LocalAgents.AllowedEnv[i] != value {
			t.Errorf("allowed_env[%d] = %q, want %q", i, cfg.LocalAgents.AllowedEnv[i], value)
		}
	}
	if !cfg.SecureCookies {
		t.Error("ADMIN__SECURE_COOKIES=yes should parse as true")
	}
}

// 数值/布尔环境变量无法解析时报错(rs set_parsed 语义)。
func TestApplyPlatformEnvRejectsUnparsableValues(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		t.Setenv("PLATFORM__GLOBAL_MAX_CONCURRENCY", "many")
		requireErrContains(t, applyPlatformEnv(validPlatformRoot()),
			"invalid PLATFORM__GLOBAL_MAX_CONCURRENCY value")
	})
	t.Run("bool", func(t *testing.T) {
		t.Setenv("CLAUDE__ENABLED", "maybe")
		requireErrContains(t, applyPlatformEnv(validPlatformRoot()), "invalid CLAUDE__ENABLED value")
	})
	t.Run("uint", func(t *testing.T) {
		t.Setenv("ADMIN__SESSION_TTL_SECS", "-1")
		requireErrContains(t, applyPlatformEnv(validPlatformRoot()), "invalid ADMIN__SESSION_TTL_SECS value")
	})
}

// AGENT_GATEWAY__* 优先于兼容的 ANTHROPIC_* 变量。
func TestApplyPlatformEnvGatewayFallbacks(t *testing.T) {
	t.Run("anthropic fallback used when gateway unset", func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", "https://litellm.internal/")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-fallback")
		root := validPlatformRoot()
		if err := applyPlatformEnv(root); err != nil {
			t.Fatal(err)
		}
		cfg, err := PlatformConfigFromFile(root)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Gateway.BaseURL != "https://litellm.internal" {
			t.Errorf("gateway.base_url = %q, want https://litellm.internal", cfg.Gateway.BaseURL)
		}
		if cfg.Gateway.APIKey != "sk-fallback" {
			t.Errorf("gateway.api_key = %q, want sk-fallback", cfg.Gateway.APIKey)
		}
	})

	t.Run("gateway env wins", func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", "https://fallback.internal")
		t.Setenv("AGENT_GATEWAY__BASE_URL", "https://primary.internal")
		root := validPlatformRoot()
		if err := applyPlatformEnv(root); err != nil {
			t.Fatal(err)
		}
		cfg, err := PlatformConfigFromFile(root)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Gateway.BaseURL != "https://primary.internal" {
			t.Errorf("gateway.base_url = %q, want https://primary.internal", cfg.Gateway.BaseURL)
		}
	})

	t.Run("gateway model falls back to claude default model", func(t *testing.T) {
		t.Setenv("CLAUDE__DEFAULT_MODEL", "custom-model")
		root := validPlatformRoot()
		if err := applyPlatformEnv(root); err != nil {
			t.Fatal(err)
		}
		cfg, err := PlatformConfigFromFile(root)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Gateway.Model != "custom-model" {
			t.Errorf("gateway.model = %q, want custom-model", cfg.Gateway.Model)
		}
		if cfg.ClaudeDefaultModel != "custom-model" {
			t.Errorf("claude.default_model = %q, want custom-model", cfg.ClaudeDefaultModel)
		}
	})
}

// ---------------------------------------------------------------------------
// 分层加载
// ---------------------------------------------------------------------------

func TestLoadPlatformConfigLayersFilesThenEnv(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[platform]
enabled = true
public_base_url = "http://127.0.0.1:8080"
global_max_concurrency = 8

[admin]
username = "admin"
session_ttl_secs = 259200

[gateway]
model = "from-default"
max_attempts = 2

[local_agents]
pi_binary = "/from/default/pi"
`)
	writeConfigFile(t, dir, "staging.toml", `
[gateway]
max_attempts = 4

[local_agents]
pi_binary = "/from/staging/pi"
`)
	t.Setenv("PLATFORM_ENV", "staging")
	t.Setenv("ADMIN__PASSWORD", "strong-test-password")
	t.Setenv("ADMIN__SESSION_SECRET", strings.Repeat("s", 32))
	t.Setenv("LOCAL_AGENT__PI_BINARY", "/from/env/pi")

	cfg, err := LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if cfg.GlobalMaxConcurrency != 8 {
		t.Errorf("global_max_concurrency = %d, want 8 (from default.toml)", cfg.GlobalMaxConcurrency)
	}
	if cfg.Gateway.Model != "from-default" {
		t.Errorf("gateway.model = %q, want from-default", cfg.Gateway.Model)
	}
	if cfg.Gateway.MaxAttempts != 4 {
		t.Errorf("gateway.max_attempts = %d, want 4 (from staging.toml)", cfg.Gateway.MaxAttempts)
	}
	if cfg.LocalAgents.PiBinary != "/from/env/pi" {
		t.Errorf("local_agents.pi_binary = %q, want /from/env/pi", cfg.LocalAgents.PiBinary)
	}
	if cfg.AdminSessionTTL != 259200*time.Second {
		t.Errorf("admin.session_ttl = %s, want 72h", cfg.AdminSessionTTL)
	}
}

// PLATFORM_ENV 未设置时回退 IR_ENV。
func TestLoadPlatformConfigFallsBackToIREnv(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[gateway]
model = "from-default"
`)
	writeConfigFile(t, dir, "qa.toml", `
[gateway]
model = "from-qa"
`)
	t.Setenv("IR_ENV", "qa")
	t.Setenv("ADMIN__PASSWORD", "strong-test-password")
	t.Setenv("ADMIN__SESSION_SECRET", strings.Repeat("s", 32))

	cfg, err := LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if cfg.Gateway.Model != "from-qa" {
		t.Errorf("gateway.model = %q, want from-qa", cfg.Gateway.Model)
	}
}

// WorkspaceRoot 校验通过后被规范化为绝对路径。
func TestLoadPlatformConfigResolvesWorkspaceRoot(t *testing.T) {
	dir := chdirTemp(t)
	t.Setenv("IR_ENV", "default")
	t.Setenv("ADMIN__PASSWORD", "strong-test-password")
	t.Setenv("ADMIN__SESSION_SECRET", strings.Repeat("s", 32))
	t.Setenv("CLAUDE__WORKSPACE_ROOT", ".")

	cfg, err := LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if !filepath.IsAbs(cfg.WorkspaceRoot) {
		t.Errorf("workspace_root = %q, want an absolute path", cfg.WorkspaceRoot)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedRoot != resolvedDir {
		t.Errorf("workspace_root = %q, want %q", resolvedRoot, resolvedDir)
	}
}

// TestPlatformRetentionDays 覆盖保留窗口的默认关闭与 env 注入。
// 负值不阻断加载：Sweep 对 <=0 一律视为关闭（保守语义在存储层收口）。
func TestPlatformRetentionDays(t *testing.T) {
	cfg, err := PlatformConfigFromFile(validPlatformRoot())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RetentionDays != 0 {
		t.Errorf("retention days default = %d, want 0 (disabled)", cfg.RetentionDays)
	}
	t.Setenv("PLATFORM__RETENTION_DAYS", "45")
	root := validPlatformRoot()
	if err := applyPlatformEnv(root); err != nil {
		t.Fatalf("apply env: %v", err)
	}
	cfg, err = PlatformConfigFromFile(root)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if cfg.RetentionDays != 45 {
		t.Errorf("retention days = %d, want 45", cfg.RetentionDays)
	}
}
