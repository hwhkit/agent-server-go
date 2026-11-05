package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseConfig 返回一份通过校验的最小 Push 配置。
func baseConfig() *Config {
	return &Config{
		PushEnabled:       true,
		PushPath:          "/ir/push",
		SigningSecret:     "a-real-secret-0123456789",
		SignatureEncoding: SignatureEncodingHex,
		PushMode:          PushModeSync,
		ClaimTimeout:      60 * time.Second,
		HeartbeatInterval: 60 * time.Second,
		RenewInterval:     30 * time.Second,
		TaskTimeout:       600 * time.Second,
	}
}

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error containing %q, got %v", want, err)
	}
}

// ---------------------------------------------------------------------------
// configFromFile 默认值(对应 rs from_file_defaults)
// ---------------------------------------------------------------------------

func TestConfigFromFileDefaults(t *testing.T) {
	secret := "a-real-secret-0123456789"
	cfg, err := configFromFile(&irFile{SigningSecret: &secret})
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if !cfg.PushEnabled {
		t.Error("push_enabled should default to true")
	}
	if cfg.PullEnabled {
		t.Error("pull_enabled should default to false")
	}
	if cfg.PushPath != "/ir/push" {
		t.Errorf("push_path = %q, want /ir/push", cfg.PushPath)
	}
	if cfg.SignatureEncoding != SignatureEncodingHex {
		t.Errorf("signature_encoding = %q, want hex", cfg.SignatureEncoding)
	}
	if cfg.PushMode != PushModeSync {
		t.Errorf("push_mode = %q, want sync", cfg.PushMode)
	}
	for _, item := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"claim_timeout", cfg.ClaimTimeout, 60 * time.Second},
		{"heartbeat_interval", cfg.HeartbeatInterval, 60 * time.Second},
		{"renew_interval", cfg.RenewInterval, 30 * time.Second},
		{"task_timeout", cfg.TaskTimeout, 600 * time.Second},
	} {
		if item.got != item.want {
			t.Errorf("%s = %s, want %s", item.name, item.got, item.want)
		}
	}
	if cfg.TrustedUsageReportingEnabled {
		t.Error("trusted_usage_reporting_enabled must default to false")
	}
	if cfg.DefaultAgentSlug != "" {
		t.Errorf("default_agent_slug = %q, want empty", cfg.DefaultAgentSlug)
	}
}

// ---------------------------------------------------------------------------
// Push 校验
// ---------------------------------------------------------------------------

func TestConfigPushRequiresSecret(t *testing.T) {
	_, err := configFromFile(&irFile{})
	requireErrContains(t, err, "[ir].signing_secret is empty")
	requireErrContains(t, err, "IR__SIGNING_SECRET")
}

func TestConfigPlaceholderSecretRejected(t *testing.T) {
	for _, placeholder := range []string{PlaceholderSecret, ExampleSigningSecret} {
		placeholder := placeholder
		t.Run(placeholder, func(t *testing.T) {
			_, err := configFromFile(&irFile{SigningSecret: &placeholder})
			requireErrContains(t, err, "placeholder")
		})
	}
}

func TestConfigPushPathMustStartWithSlash(t *testing.T) {
	cfg := baseConfig()
	cfg.PushPath = "ir/push"
	requireErrContains(t, cfg.Validate(), "must start with '/'")
}

// Push 关闭时不再要求签名密钥。
func TestConfigPushDisabledSkipsSecretChecks(t *testing.T) {
	cfg := baseConfig()
	cfg.PushEnabled = false
	cfg.SigningSecret = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config with push disabled, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 枚举校验
// ---------------------------------------------------------------------------

func TestConfigInvalidEncodingRejected(t *testing.T) {
	secret, encoding := "a-real-secret-0123456789", "bogus"
	_, err := configFromFile(&irFile{SigningSecret: &secret, SignatureEncoding: &encoding})
	requireErrContains(t, err, "[ir].signature_encoding")
}

func TestConfigSignatureEncodingIsCaseInsensitive(t *testing.T) {
	secret, encoding := "a-real-secret-0123456789", "BASE64"
	cfg, err := configFromFile(&irFile{SigningSecret: &secret, SignatureEncoding: &encoding})
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.SignatureEncoding != SignatureEncodingBase64 {
		t.Errorf("signature_encoding = %q, want base64", cfg.SignatureEncoding)
	}
}

func TestConfigInvalidPushModeRejected(t *testing.T) {
	secret, mode := "a-real-secret-0123456789", "bogus"
	_, err := configFromFile(&irFile{SigningSecret: &secret, PushMode: &mode})
	requireErrContains(t, err, "[ir].push_mode")
}

func TestConfigRejectsWeakSigningSecret(t *testing.T) {
	cfg := baseConfig()
	cfg.SigningSecret = "short"
	requireErrContains(t, cfg.Validate(), "16-512 bytes")
}

// Rust 侧自 2026-08-17 起 callback 令牌可选:async 不再强制要求 callback_token。
func TestConfigAsyncDoesNotRequireCallbackToken(t *testing.T) {
	cfg := baseConfig()
	cfg.PushMode = PushModeAsync
	if err := cfg.Validate(); err != nil {
		t.Fatalf("async push must be valid without a callback token, got %v", err)
	}
}

func TestConfigAsyncOKWithCallbackToken(t *testing.T) {
	cfg := baseConfig()
	cfg.PushMode = PushModeAsync
	cfg.CallbackToken = "mpk_test"
	cfg.CallbackTokenOrigin = "https://api.example.test"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid async config, got %v", err)
	}
}

func TestConfigCallbackTokenRequiresBoundOrigin(t *testing.T) {
	cfg := baseConfig()
	cfg.PushMode = PushModeAsync
	cfg.CallbackToken = "mpk_test"
	requireErrContains(t, cfg.Validate(), "callback_token_origin is required")
}

func TestConfigTrustedUsageReportingRequiresExplicitOptIn(t *testing.T) {
	secret, trusted := "a-real-secret-0123456789", true
	cfg, err := configFromFile(&irFile{
		SigningSecret:                &secret,
		TrustedUsageReportingEnabled: &trusted,
	})
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if !cfg.TrustedUsageReportingEnabled {
		t.Error("trusted_usage_reporting_enabled should be true when opted in")
	}
}

// ---------------------------------------------------------------------------
// default_agent_slug
// ---------------------------------------------------------------------------

func TestConfigDefaultAgentSlugIsNormalizedAndValidated(t *testing.T) {
	secret := "a-real-secret-0123456789"
	slug := "  Local-Pi  "
	cfg, err := configFromFile(&irFile{SigningSecret: &secret, DefaultAgentSlug: &slug})
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.DefaultAgentSlug != "local-pi" {
		t.Errorf("default_agent_slug = %q, want local-pi", cfg.DefaultAgentSlug)
	}

	for _, invalid := range []string{"invalid--slug", "-leading", "trailing-", "Has_Underscore", "has space"} {
		value := invalid
		_, err := configFromFile(&irFile{SigningSecret: &secret, DefaultAgentSlug: &value})
		requireErrContains(t, err, "[ir].default_agent_slug")
	}
}

// ---------------------------------------------------------------------------
// URL 校验
// ---------------------------------------------------------------------------

func TestConfigHeartbeatURLValidation(t *testing.T) {
	for _, test := range []struct {
		target string
		want   string
	}{
		{target: "not-a-url", want: "[ir].heartbeat_url"},
		{target: "ftp://example.com/hb", want: "[ir].heartbeat_url"},
		{target: "http://example.com/hb", want: "must use HTTPS"},
		{target: "https://user:password@example.com/hb", want: "embedded credentials"},
		{target: "http://user:password@127.0.0.1/hb", want: "embedded credentials"},
	} {
		t.Run("reject_"+strings.NewReplacer(":", "_", "/", "_").Replace(test.target), func(t *testing.T) {
			cfg := baseConfig()
			cfg.HeartbeatURL = test.target
			requireErrContains(t, cfg.Validate(), test.want)
		})
	}

	for _, target := range []string{
		"https://example.com/hb",
		"http://localhost:8080/hb",
		"http://127.1.2.3:8080/hb",
		"http://[::1]:8080/hb",
	} {
		t.Run("allow_"+strings.NewReplacer(":", "_", "/", "_").Replace(target), func(t *testing.T) {
			cfg := baseConfig()
			cfg.HeartbeatURL = target
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected valid heartbeat URL %q, got %v", target, err)
			}
		})
	}
}

func TestConfigPullRequiresQueueTokenWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		wantIn string
	}{
		{"queue", func(c *Config) {}, "[ir].queue_url is empty"},
		{"token", func(c *Config) { c.QueueURL = "http://q.example.com" }, "[ir].bind_token is empty"},
		{"worker", func(c *Config) {
			c.QueueURL = "http://q.example.com"
			c.BindToken = "t"
		}, "[ir].worker_id is empty"},
		{"relative-queue", func(c *Config) {
			c.QueueURL = "q.example.com"
			c.BindToken = "t"
			c.WorkerID = "w"
		}, "[ir].queue_url must be an absolute HTTPS URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.PullEnabled = true
			tc.mutate(cfg)
			requireErrContains(t, cfg.Validate(), tc.wantIn)
		})
	}
}

func TestConfigQueueURLTransportSecurity(t *testing.T) {
	build := func(target string) *Config {
		cfg := baseConfig()
		cfg.PullEnabled = true
		cfg.QueueURL = target
		cfg.BindToken = "bind-token"
		cfg.WorkerID = "worker-1"
		return cfg
	}

	for _, test := range []struct {
		target string
		want   string
	}{
		{target: "http://queue.example.com", want: "must use HTTPS"},
		{target: "https://user:password@queue.example.com", want: "embedded credentials"},
		{target: "http://user:password@127.0.0.1:8080", want: "embedded credentials"},
	} {
		t.Run("reject_"+strings.NewReplacer(":", "_", "/", "_").Replace(test.target), func(t *testing.T) {
			requireErrContains(t, build(test.target).Validate(), test.want)
		})
	}

	for _, target := range []string{
		"https://queue.example.com",
		"http://localhost:8080",
		"http://127.9.8.7:8080",
		"http://[::1]:8080",
	} {
		t.Run("allow_"+strings.NewReplacer(":", "_", "/", "_").Replace(target), func(t *testing.T) {
			if err := build(target).Validate(); err != nil {
				t.Fatalf("expected valid queue URL %q, got %v", target, err)
			}
		})
	}
}

func TestConfigPullRejectsPublicTemplateBindToken(t *testing.T) {
	cfg := baseConfig()
	cfg.PullEnabled = true
	cfg.QueueURL = "https://queue.example.com"
	cfg.BindToken = ExampleBindToken
	cfg.WorkerID = "worker-1"
	requireErrContains(t, cfg.Validate(), "public template placeholder")
}

func TestConfigPullOKWhenConfigured(t *testing.T) {
	cfg := baseConfig()
	cfg.PullEnabled = true
	cfg.QueueURL = "https://queue.example.com"
	cfg.BindToken = "bind-token"
	cfg.WorkerID = "worker-1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid pull config, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 时间参数
// ---------------------------------------------------------------------------

func TestConfigClaimTimeoutMustBeAtLeast35(t *testing.T) {
	cfg := baseConfig()
	cfg.ClaimTimeout = 10 * time.Second
	requireErrContains(t, cfg.Validate(), ">= 35")
}

func TestConfigZeroIntervalsRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"heartbeat_interval_secs", func(c *Config) { c.HeartbeatInterval = 0 }},
		{"renew_interval_secs", func(c *Config) { c.RenewInterval = 0 }},
		{"task_timeout_secs", func(c *Config) { c.TaskTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(cfg)
			requireErrContains(t, cfg.Validate(), tc.name+" must be greater than 0")
		})
	}
}

func TestConfigDurationOverflowIsRejected(t *testing.T) {
	tooLarge := ^uint64(0)
	secret := "a-real-secret-0123456789"
	_, err := configFromFile(&irFile{
		SigningSecret: &secret, HeartbeatIntervalSecs: &tooLarge,
	})
	requireErrContains(t, err, "heartbeat_interval_secs must not exceed 86400")
}

// ---------------------------------------------------------------------------
// 分层加载:config/default.toml -> config/{env}.toml -> IR__*
// ---------------------------------------------------------------------------

// chdirTemp 切到一个带 config/ 目录的临时工作目录,并在测试结束后切回。
func chdirTemp(t *testing.T) string {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o750); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	return dir
}

func writeConfigFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config", name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadConfigLayersDefaultThenEnvFileThenEnvVars(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[server]
port = 8080

[ir]
push_enabled = true
push_path = "/from/default"
signing_secret = "default-secret-0123456789"
signature_encoding = "hex"
claim_timeout_secs = 60
heartbeat_interval_secs = 60
renew_interval_secs = 30
task_timeout_secs = 600
default_agent_slug = "from-default"
`)
	writeConfigFile(t, dir, "staging.toml", `
[ir]
push_path = "/from/staging"
signature_encoding = "base64"
`)
	t.Setenv("IR_ENV", "staging")
	t.Setenv("IR__SIGNING_SECRET", "env-secret-0123456789")
	t.Setenv("IR__DEFAULT_AGENT_SLUG", "From-Env")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// default.toml 提供的、后续层未覆盖的值保留。
	if cfg.ClaimTimeout != 60*time.Second {
		t.Errorf("claim_timeout = %s, want 60s", cfg.ClaimTimeout)
	}
	// {env}.toml 覆盖 default.toml。
	if cfg.PushPath != "/from/staging" {
		t.Errorf("push_path = %q, want /from/staging", cfg.PushPath)
	}
	if cfg.SignatureEncoding != SignatureEncodingBase64 {
		t.Errorf("signature_encoding = %q, want base64", cfg.SignatureEncoding)
	}
	// 环境变量覆盖两个文件层,并做规范化。
	if cfg.SigningSecret != "env-secret-0123456789" {
		t.Errorf("signing_secret = %q, want env-secret-0123456789", cfg.SigningSecret)
	}
	if cfg.DefaultAgentSlug != "from-env" {
		t.Errorf("default_agent_slug = %q, want from-env", cfg.DefaultAgentSlug)
	}
}

// IR_ENV=default 时跳过 {env}.toml 这一层。
func TestLoadConfigSkipsEnvFileWhenEnvIsDefault(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[ir]
signing_secret = "default-secret-0123456789"
push_path = "/from/default"
`)
	writeConfigFile(t, dir, "default.toml.should-not-load", ``)
	t.Setenv("IR_ENV", "default")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PushPath != "/from/default" {
		t.Errorf("push_path = %q, want /from/default", cfg.PushPath)
	}
}

// 空白环境变量视为未设置,不覆盖文件层。
func TestLoadConfigBlankEnvVarDoesNotOverride(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[ir]
signing_secret = "default-secret-0123456789"
push_path = "/from/default"
`)
	t.Setenv("IR_ENV", "default")
	t.Setenv("IR__PUSH_PATH", "   ")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PushPath != "/from/default" {
		t.Errorf("push_path = %q, want /from/default (blank env must not override)", cfg.PushPath)
	}
}

// 缺少 config/ 目录时用内置默认值,不报错。
func TestLoadConfigWorksWithoutConfigFiles(t *testing.T) {
	chdirTemp(t)
	t.Setenv("IR_ENV", "default")
	t.Setenv("IR__SIGNING_SECRET", "env-secret-0123456789")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PushPath != "/ir/push" {
		t.Errorf("push_path = %q, want /ir/push", cfg.PushPath)
	}
}

func TestLoadConfigRejectsMalformedToml(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", "[ir]\nthis is not toml")
	t.Setenv("IR_ENV", "default")

	_, err := LoadConfig()
	requireErrContains(t, err, "as TOML")
}

func TestApplyIREnvParsesBoolsAndUints(t *testing.T) {
	for _, raw := range []string{"true", "1", "yes", "on"} {
		t.Setenv("IR__PULL_ENABLED", raw)
		file := &irFile{}
		applyIREnv(file)
		if file.PullEnabled == nil || !*file.PullEnabled {
			t.Errorf("IR__PULL_ENABLED=%q should parse as true", raw)
		}
	}
	for _, raw := range []string{"false", "0", "no", "off"} {
		t.Setenv("IR__PULL_ENABLED", raw)
		file := &irFile{}
		applyIREnv(file)
		if file.PullEnabled == nil || *file.PullEnabled {
			t.Errorf("IR__PULL_ENABLED=%q should parse as false", raw)
		}
	}
	// 无法识别的布尔值保持未设置,让文件层的值生效。
	t.Setenv("IR__PULL_ENABLED", "maybe")
	file := &irFile{}
	applyIREnv(file)
	if file.PullEnabled != nil {
		t.Error("unparsable bool must leave the field unset")
	}

	t.Setenv("IR__CLAIM_TIMEOUT_SECS", "90")
	file = &irFile{}
	applyIREnv(file)
	if file.ClaimTimeoutSecs == nil || *file.ClaimTimeoutSecs != 90 {
		t.Error("IR__CLAIM_TIMEOUT_SECS=90 should parse as 90")
	}

	t.Setenv("IR__CLAIM_TIMEOUT_SECS", "not-a-number")
	file = &irFile{}
	applyIREnv(file)
	if file.ClaimTimeoutSecs != nil {
		t.Error("unparsable uint must leave the field unset")
	}
}
