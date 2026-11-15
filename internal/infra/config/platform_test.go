package config

import (
	"ir-agent-server/internal/infra/crypto"
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
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// 对应 rs invalid_gateway_protocol_is_rejected。
// 对应 rs unsafe_retry_and_output_limits_are_rejected。
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
			value := "short7!"
			r.Admin.Password = &value
		}, "password must contain 8-512 bytes"},
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

// ---------------------------------------------------------------------------
// 环境变量覆盖
// ---------------------------------------------------------------------------

func TestApplyPlatformEnvOverrides(t *testing.T) {
	t.Setenv("PLATFORM__GLOBAL_MAX_CONCURRENCY", "32")
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
		t.Setenv("PLATFORM__ENABLED", "maybe")
		requireErrContains(t, applyPlatformEnv(validPlatformRoot()), "invalid PLATFORM__ENABLED value")
	})
	t.Run("uint", func(t *testing.T) {
		t.Setenv("ADMIN__SESSION_TTL_SECS", "-1")
		requireErrContains(t, applyPlatformEnv(validPlatformRoot()), "invalid ADMIN__SESSION_TTL_SECS value")
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

	cfg, err := LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if cfg.GlobalMaxConcurrency != 8 {
		t.Errorf("global_max_concurrency = %d, want 8 (from default.toml)", cfg.GlobalMaxConcurrency)
	}
	if cfg.AdminSessionTTL != 259200*time.Second {
		t.Errorf("admin.session_ttl = %s, want 72h", cfg.AdminSessionTTL)
	}
}

// PLATFORM_ENV 未设置时回退 IR_ENV。
func TestLoadPlatformConfigFallsBackToIREnv(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, dir, "default.toml", `
[platform]
public_base_url = "http://127.0.0.1:1"
`)
	writeConfigFile(t, dir, "qa.toml", `
[platform]
public_base_url = "http://127.0.0.1:2"
`)
	t.Setenv("IR_ENV", "qa")
	t.Setenv("ADMIN__PASSWORD", "strong-test-password")
	t.Setenv("ADMIN__SESSION_SECRET", strings.Repeat("s", 32))

	cfg, err := LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if cfg.PublicBaseURL != "http://127.0.0.1:2" {
		t.Errorf("public_base_url = %q, want loopback qa", cfg.PublicBaseURL)
	}
}

// WorkspaceRoot 校验通过后被规范化为绝对路径。
// TestPlatformRetentionDays 覆盖保留窗口的默认关闭与 env 注入。
// 负值不阻断加载：Sweep 对 <=0 一律视为关闭（保守语义在存储层收口）。
func TestPlatformRetentionDays(t *testing.T) {
	cfg, err := PlatformConfigFromFile(validPlatformRoot())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("retention days default = %d, want 90 (production default)", cfg.RetentionDays)
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
