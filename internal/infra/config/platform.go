package config

// 单体多 Agent 平台配置。
//
// 加载顺序与 IR 配置一致:config/default.toml -> config/{env}.toml
// -> PLATFORM__* / ADMIN__* / CLAUDE__* / AGENT_GATEWAY__* / LOCAL_AGENT__* 环境变量。
// env 由 PLATFORM_ENV 指定,回退 IR_ENV,再回退 dev;等于 "default" 时跳过第二层。
//
// 对应 Rust 侧 src/platform/config.rs。

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	exampleAdminPassword      = "replace-with-a-strong-admin-password"
	exampleAdminSessionSecret = "replace-with-at-least-32-random-bytes"
	exampleProviderAPIKey     = "replace-me"
)

// PlatformConfig 是平台运行所需的全部配置。
type PlatformConfig struct {
	Enabled              bool
	PublicBaseURL        string
	MaxRequestBodyBytes  int
	GlobalMaxConcurrency int
	// AllowLegacyBodySignature 仅用于迁移旧动态 Agent 调用方。旧
	// X-IR-Signature 只覆盖 body、没有 timestamp/nonce，默认必须关闭。
	AllowLegacyBodySignature bool
	// RetentionDays 是历史数据保留窗口（天）：channel_events 与 audit_log
	// 超窗即被周期清扫。0（默认）= 关闭——保留期限是合规决策，由部署方
	// 显式开启；过期管理会话无论如何都会被清扫。
	RetentionDays int
	// DBMaxConns 是 PostgreSQL 连接池上限。0（默认）= 8；执行并发高或多
	// 副本共库时按需上调，上限 256（加载期校验，防误配打爆数据库）。
	DBMaxConns int

	AdminUsername          string
	AdminPassword          string
	AdminPasswordHash      string
	AdminSessionSecret     string
	AdminSessionTTL        time.Duration
	SecureCookies          bool
	AdminTrustedProxyCIDRs []string
}

// ---------------------------------------------------------------------------
// TOML 镜像结构:全部字段可选,用于分层合并
// ---------------------------------------------------------------------------

type PlatformRootFile struct {
	Platform      PlatformSectionFile `toml:"platform"`
	Admin         AdminSectionFile    `toml:"admin"`
	Claude        claudeSectionFile   `toml:"claude"`
	Gateway       gatewaySectionFile  `toml:"gateway"`
	LocalAgents   localAgentFile      `toml:"local_agents"`
	OpenAIAgents  openAIAgentsFile    `toml:"openai_agents"`
	ClaudeManaged claudeManagedFile   `toml:"claude_managed"`
}

type PlatformSectionFile struct {
	Enabled                  *bool   `toml:"enabled"`
	PublicBaseURL            *string `toml:"public_base_url"`
	MaxRequestBodyBytes      *int    `toml:"max_request_body_bytes"`
	GlobalMaxConcurrency     *int    `toml:"global_max_concurrency"`
	RetentionDays            *int    `toml:"retention_days"`
	DBMaxConns               *int    `toml:"db_max_conns"`
	AllowLegacyBodySignature *bool   `toml:"allow_legacy_body_signature"`
}

type AdminSectionFile struct {
	Username          *string   `toml:"username"`
	Password          *string   `toml:"password"`
	PasswordHash      *string   `toml:"password_hash"`
	SessionSecret     *string   `toml:"session_secret"`
	SessionTTLSecs    *uint64   `toml:"session_ttl_secs"`
	SecureCookies     *bool     `toml:"secure_cookies"`
	TrustedProxyCIDRs *[]string `toml:"trusted_proxy_cidrs"`
}

type claudeSectionFile struct {
	Enabled        *bool     `toml:"enabled"`
	WorkspaceRoot  *string   `toml:"workspace_root"`
	Binary         *string   `toml:"binary"`
	DefaultModel   *string   `toml:"default_model"`
	AllowedEnv     *[]string `toml:"allowed_env"`
	MaxOutputBytes *int      `toml:"max_output_bytes"`
}

type gatewaySectionFile struct {
	BaseURL          *string `toml:"base_url"`
	APIKey           *string `toml:"api_key"`
	Model            *string `toml:"model"`
	Protocol         *string `toml:"protocol"`
	MaxTokens        *uint64 `toml:"max_tokens"`
	MaxAttempts      *int    `toml:"max_attempts"`
	MaxResponseBytes *int    `toml:"max_response_bytes"`
}

type localAgentFile struct {
	CodexBinary           *string   `toml:"codex_binary"`
	PiBinary              *string   `toml:"pi_binary"`
	ReasonixBinary        *string   `toml:"reasonix_binary"`
	DeepseekHarnessBinary *string   `toml:"deepseek_harness_binary"`
	OpenCodeBinary        *string   `toml:"opencode_binary"`
	OpenCodeDefaultModel  *string   `toml:"opencode_default_model"`
	QwenCodeBinary        *string   `toml:"qwen_code_binary"`
	QwenCodeDefaultModel  *string   `toml:"qwen_code_default_model"`
	GeminiCLIBinary       *string   `toml:"gemini_cli_binary"`
	GeminiCLIDefaultModel *string   `toml:"gemini_cli_default_model"`
	OpenHandsBinary       *string   `toml:"openhands_binary"`
	OpenHandsDefaultModel *string   `toml:"openhands_default_model"`
	FinanceExtensionPath  *string   `toml:"finance_extension_path"`
	StrategySkillPaths    *[]string `toml:"strategy_skill_paths"`
	AnalysisSkillPaths    *[]string `toml:"analysis_skill_paths"`
	WorkflowSkillPaths    *[]string `toml:"workflow_skill_paths"`
	DefaultModel          *string   `toml:"default_model"`
	AllowedEnv            *[]string `toml:"allowed_env"`
	MaxOutputBytes        *int      `toml:"max_output_bytes"`
	MaxPromptBytes        *int      `toml:"max_prompt_bytes"`
}

type openAIAgentsFile struct {
	Enabled          *bool   `toml:"enabled"`
	BaseURL          *string `toml:"base_url"`
	APIKey           *string `toml:"api_key"`
	DefaultModel     *string `toml:"default_model"`
	ReasoningEffort  *string `toml:"reasoning_effort"`
	Environment      *string `toml:"environment"`
	MCPServerURL     *string `toml:"mcp_server_url"`
	MCPServerLabel   *string `toml:"mcp_server_label"`
	MCPAuthorization *string `toml:"mcp_authorization"`
	MCPRequired      *bool   `toml:"mcp_required"`
	DeleteSession    *bool   `toml:"delete_session"`
	MaxResponseBytes *int    `toml:"max_response_bytes"`
}

type claudeManagedFile struct {
	Enabled          *bool     `toml:"enabled"`
	BaseURL          *string   `toml:"base_url"`
	APIKey           *string   `toml:"api_key"`
	WorkspaceID      *string   `toml:"workspace_id"`
	AgentID          *string   `toml:"agent_id"`
	EnvironmentID    *string   `toml:"environment_id"`
	VaultIDs         *[]string `toml:"vault_ids"`
	MaxListCostCents *uint64   `toml:"max_list_cost_cents"`
	DeleteSession    *bool     `toml:"delete_session"`
	MaxResponseBytes *int      `toml:"max_response_bytes"`
}

// merge 用 other 中已设置的字段覆盖 r。
func (r *PlatformRootFile) merge(other *PlatformRootFile) {
	if other == nil {
		return
	}
	mergePtr(&r.Platform.Enabled, other.Platform.Enabled)
	mergePtr(&r.Platform.PublicBaseURL, other.Platform.PublicBaseURL)
	mergePtr(&r.Platform.MaxRequestBodyBytes, other.Platform.MaxRequestBodyBytes)
	mergePtr(&r.Platform.GlobalMaxConcurrency, other.Platform.GlobalMaxConcurrency)
	mergePtr(&r.Platform.RetentionDays, other.Platform.RetentionDays)
	mergePtr(&r.Platform.DBMaxConns, other.Platform.DBMaxConns)
	mergePtr(&r.Platform.AllowLegacyBodySignature, other.Platform.AllowLegacyBodySignature)

	mergePtr(&r.Admin.Username, other.Admin.Username)
	mergePtr(&r.Admin.Password, other.Admin.Password)
	mergePtr(&r.Admin.PasswordHash, other.Admin.PasswordHash)
	mergePtr(&r.Admin.SessionSecret, other.Admin.SessionSecret)
	mergePtr(&r.Admin.SessionTTLSecs, other.Admin.SessionTTLSecs)
	mergePtr(&r.Admin.SecureCookies, other.Admin.SecureCookies)
	mergePtr(&r.Admin.TrustedProxyCIDRs, other.Admin.TrustedProxyCIDRs)

	mergePtr(&r.Claude.Enabled, other.Claude.Enabled)
	mergePtr(&r.Claude.WorkspaceRoot, other.Claude.WorkspaceRoot)
	mergePtr(&r.Claude.Binary, other.Claude.Binary)
	mergePtr(&r.Claude.DefaultModel, other.Claude.DefaultModel)
	mergePtr(&r.Claude.AllowedEnv, other.Claude.AllowedEnv)
	mergePtr(&r.Claude.MaxOutputBytes, other.Claude.MaxOutputBytes)

	mergePtr(&r.Gateway.BaseURL, other.Gateway.BaseURL)
	mergePtr(&r.Gateway.APIKey, other.Gateway.APIKey)
	mergePtr(&r.Gateway.Model, other.Gateway.Model)
	mergePtr(&r.Gateway.Protocol, other.Gateway.Protocol)
	mergePtr(&r.Gateway.MaxTokens, other.Gateway.MaxTokens)
	mergePtr(&r.Gateway.MaxAttempts, other.Gateway.MaxAttempts)
	mergePtr(&r.Gateway.MaxResponseBytes, other.Gateway.MaxResponseBytes)

	mergePtr(&r.LocalAgents.CodexBinary, other.LocalAgents.CodexBinary)
	mergePtr(&r.LocalAgents.PiBinary, other.LocalAgents.PiBinary)
	mergePtr(&r.LocalAgents.ReasonixBinary, other.LocalAgents.ReasonixBinary)
	mergePtr(&r.LocalAgents.DeepseekHarnessBinary, other.LocalAgents.DeepseekHarnessBinary)
	mergePtr(&r.LocalAgents.OpenCodeBinary, other.LocalAgents.OpenCodeBinary)
	mergePtr(&r.LocalAgents.OpenCodeDefaultModel, other.LocalAgents.OpenCodeDefaultModel)
	mergePtr(&r.LocalAgents.QwenCodeBinary, other.LocalAgents.QwenCodeBinary)
	mergePtr(&r.LocalAgents.QwenCodeDefaultModel, other.LocalAgents.QwenCodeDefaultModel)
	mergePtr(&r.LocalAgents.GeminiCLIBinary, other.LocalAgents.GeminiCLIBinary)
	mergePtr(&r.LocalAgents.GeminiCLIDefaultModel, other.LocalAgents.GeminiCLIDefaultModel)
	mergePtr(&r.LocalAgents.OpenHandsBinary, other.LocalAgents.OpenHandsBinary)
	mergePtr(&r.LocalAgents.OpenHandsDefaultModel, other.LocalAgents.OpenHandsDefaultModel)
	mergePtr(&r.LocalAgents.FinanceExtensionPath, other.LocalAgents.FinanceExtensionPath)
	mergePtr(&r.LocalAgents.StrategySkillPaths, other.LocalAgents.StrategySkillPaths)
	mergePtr(&r.LocalAgents.AnalysisSkillPaths, other.LocalAgents.AnalysisSkillPaths)
	mergePtr(&r.LocalAgents.WorkflowSkillPaths, other.LocalAgents.WorkflowSkillPaths)
	mergePtr(&r.LocalAgents.DefaultModel, other.LocalAgents.DefaultModel)
	mergePtr(&r.LocalAgents.AllowedEnv, other.LocalAgents.AllowedEnv)
	mergePtr(&r.LocalAgents.MaxOutputBytes, other.LocalAgents.MaxOutputBytes)
	mergePtr(&r.LocalAgents.MaxPromptBytes, other.LocalAgents.MaxPromptBytes)

	mergePtr(&r.OpenAIAgents.Enabled, other.OpenAIAgents.Enabled)
	mergePtr(&r.OpenAIAgents.BaseURL, other.OpenAIAgents.BaseURL)
	mergePtr(&r.OpenAIAgents.APIKey, other.OpenAIAgents.APIKey)
	mergePtr(&r.OpenAIAgents.DefaultModel, other.OpenAIAgents.DefaultModel)
	mergePtr(&r.OpenAIAgents.ReasoningEffort, other.OpenAIAgents.ReasoningEffort)
	mergePtr(&r.OpenAIAgents.Environment, other.OpenAIAgents.Environment)
	mergePtr(&r.OpenAIAgents.MCPServerURL, other.OpenAIAgents.MCPServerURL)
	mergePtr(&r.OpenAIAgents.MCPServerLabel, other.OpenAIAgents.MCPServerLabel)
	mergePtr(&r.OpenAIAgents.MCPAuthorization, other.OpenAIAgents.MCPAuthorization)
	mergePtr(&r.OpenAIAgents.MCPRequired, other.OpenAIAgents.MCPRequired)
	mergePtr(&r.OpenAIAgents.DeleteSession, other.OpenAIAgents.DeleteSession)
	mergePtr(&r.OpenAIAgents.MaxResponseBytes, other.OpenAIAgents.MaxResponseBytes)

	mergePtr(&r.ClaudeManaged.Enabled, other.ClaudeManaged.Enabled)
	mergePtr(&r.ClaudeManaged.BaseURL, other.ClaudeManaged.BaseURL)
	mergePtr(&r.ClaudeManaged.APIKey, other.ClaudeManaged.APIKey)
	mergePtr(&r.ClaudeManaged.WorkspaceID, other.ClaudeManaged.WorkspaceID)
	mergePtr(&r.ClaudeManaged.AgentID, other.ClaudeManaged.AgentID)
	mergePtr(&r.ClaudeManaged.EnvironmentID, other.ClaudeManaged.EnvironmentID)
	mergePtr(&r.ClaudeManaged.VaultIDs, other.ClaudeManaged.VaultIDs)
	mergePtr(&r.ClaudeManaged.MaxListCostCents, other.ClaudeManaged.MaxListCostCents)
	mergePtr(&r.ClaudeManaged.DeleteSession, other.ClaudeManaged.DeleteSession)
	mergePtr(&r.ClaudeManaged.MaxResponseBytes, other.ClaudeManaged.MaxResponseBytes)
}

func mergePtr[T any](target **T, source *T) {
	if source != nil {
		*target = source
	}
}

// ---------------------------------------------------------------------------
// 加载与默认值
// ---------------------------------------------------------------------------

// LoadPlatformConfig 按文档顺序加载并合并平台配置,然后做启动校验。
func LoadPlatformConfig() (*PlatformConfig, error) {
	root := &PlatformRootFile{}

	file, err := readPlatformRoot("config/default.toml")
	if err != nil {
		return nil, err
	}
	root.merge(file)

	env := os.Getenv("PLATFORM_ENV")
	if env == "" {
		env = os.Getenv("IR_ENV")
	}
	if env == "" {
		env = "dev"
	}
	if env != "default" {
		file, err = readPlatformRoot(fmt.Sprintf("config/%s.toml", env))
		if err != nil {
			return nil, err
		}
		root.merge(file)
	}

	if err := applyPlatformEnv(root); err != nil {
		return nil, err
	}
	return PlatformConfigFromFile(root)
}

// PlatformConfigFromFile 把合并后的可选字段落成带默认值的 PlatformConfig 并校验。
func PlatformConfigFromFile(root *PlatformRootFile) (*PlatformConfig, error) {
	config := &PlatformConfig{
		Enabled:                  derefBoolOr(root.Platform.Enabled, true),
		PublicBaseURL:            strings.TrimRight(derefOr(root.Platform.PublicBaseURL, "http://127.0.0.1:8080"), "/"),
		MaxRequestBodyBytes:      derefIntOr(root.Platform.MaxRequestBodyBytes, 2*1024*1024),
		GlobalMaxConcurrency:     derefIntOr(root.Platform.GlobalMaxConcurrency, 16),
		RetentionDays:            derefIntOr(root.Platform.RetentionDays, 0),
		DBMaxConns:               derefIntOr(root.Platform.DBMaxConns, 0),
		AllowLegacyBodySignature: derefBoolOr(root.Platform.AllowLegacyBodySignature, false),

		AdminUsername:          strings.TrimSpace(derefOr(root.Admin.Username, "admin")),
		AdminPassword:          nonEmpty(derefOr(root.Admin.Password, "")),
		AdminPasswordHash:      nonEmpty(derefOr(root.Admin.PasswordHash, "")),
		AdminSessionSecret:     derefOr(root.Admin.SessionSecret, ""),
		AdminSessionTTL:        secondsOr(root.Admin.SessionTTLSecs, 72*60*60),
		SecureCookies:          derefBoolOr(root.Admin.SecureCookies, false),
		AdminTrustedProxyCIDRs: derefSliceOr(root.Admin.TrustedProxyCIDRs, nil),
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// defaultClaudeAllowedEnv 是 Claude Code 子进程可继承的环境变量白名单。
func defaultClaudeAllowedEnv() []string {
	return []string{
		"HOME",
		"PATH",
		"USER",
		"TMPDIR",
		"XDG_CONFIG_HOME",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"AWS_PROFILE",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
	}
}

// defaultLocalAgentEnv 是 pi / codex / reasonix 子进程可继承的环境变量白名单。
func defaultLocalAgentEnv() []string {
	return []string{
		"HOME",
		"PATH",
		"USER",
		"TMPDIR",
		"XDG_CONFIG_HOME",
		"CODEX_HOME",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"LITELLM_API_KEY",
		"OPENAI_API_KEY",
		"DEEPSEEK_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"GOOGLE_CLOUD_PROJECT",
		"DASHSCOPE_API_KEY",
		"QWEN_API_KEY",
		"LLM_API_KEY",
		"LLM_BASE_URL",
		"LLM_MODEL",
		"OPENCODE_CONFIG",
		"OPENCODE_CONFIG_CONTENT",
		"IR_FINANCE_USER_AGENT",
		"LOCAL_AGENT__STRATEGY_SKILL_PATHS",
		"LOCAL_AGENT__ANALYSIS_SKILL_PATHS",
		"LOCAL_AGENT__WORKFLOW_SKILL_PATHS",
	}
}

// readPlatformRoot 读取整个 TOML 文件;文件不存在返回 (nil, nil)。
func readPlatformRoot(path string) (*PlatformRootFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	root := &PlatformRootFile{}
	if err := toml.Unmarshal(content, root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return root, nil
}

// ---------------------------------------------------------------------------
// 环境变量覆盖
// ---------------------------------------------------------------------------

// applyPlatformEnv 用环境变量覆盖配置;空字符串视为未设置。数值/布尔值无法
// 解析时报错(与 Rust 侧 set_parsed 一致,不静默忽略)。
func applyPlatformEnv(root *PlatformRootFile) error {
	setEnvString("PLATFORM__PUBLIC_BASE_URL", &root.Platform.PublicBaseURL)
	setEnvString("ADMIN__USERNAME", &root.Admin.Username)
	setEnvString("ADMIN__PASSWORD", &root.Admin.Password)
	setEnvString("ADMIN__PASSWORD_HASH", &root.Admin.PasswordHash)
	setEnvString("ADMIN__SESSION_SECRET", &root.Admin.SessionSecret)
	setEnvCSV("ADMIN__TRUSTED_PROXY_CIDRS", &root.Admin.TrustedProxyCIDRs)

	if root.Gateway.APIKey == nil || *root.Gateway.APIKey == "" {
		setEnvString("ANTHROPIC_AUTH_TOKEN", &root.Gateway.APIKey)
	}
	if root.Gateway.Model == nil {
	}

	if err := setEnvBool("PLATFORM__ENABLED", &root.Platform.Enabled); err != nil {
		return err
	}
	if err := setEnvBool("PLATFORM__ALLOW_LEGACY_BODY_SIGNATURE", &root.Platform.AllowLegacyBodySignature); err != nil {
		return err
	}
	if err := setEnvBool("ADMIN__SECURE_COOKIES", &root.Admin.SecureCookies); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__MAX_REQUEST_BODY_BYTES", &root.Platform.MaxRequestBodyBytes); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__GLOBAL_MAX_CONCURRENCY", &root.Platform.GlobalMaxConcurrency); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__RETENTION_DAYS", &root.Platform.RetentionDays); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__DB_MAX_CONNS", &root.Platform.DBMaxConns); err != nil {
		return err
	}
	if err := setEnvUint("ADMIN__SESSION_TTL_SECS", &root.Admin.SessionTTLSecs); err != nil {
		return err
	}

	if root.LocalAgents.FinanceExtensionPath == nil {
	}
	if root.LocalAgents.StrategySkillPaths == nil {
	}
	if root.LocalAgents.AnalysisSkillPaths == nil {
	}
	if root.LocalAgents.WorkflowSkillPaths == nil {
	}
	// 这两个白名单允许环境变量无条件覆盖文件层。
	return nil
}

// lookupNonBlankEnv 返回环境变量的值;未设置或仅含空白时返回 false。
func LookupNonBlankEnv(name string) (string, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

func setEnvString(name string, target **string) {
	if raw, ok := LookupNonBlankEnv(name); ok {
		value := raw
		*target = &value
	}
}

func setEnvBool(name string, target **bool) error {
	raw, ok := LookupNonBlankEnv(name)
	if !ok {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		value := true
		*target = &value
	case "false", "0", "no", "off":
		value := false
		*target = &value
	default:
		return fmt.Errorf("invalid %s value", name)
	}
	return nil
}

func setEnvInt(name string, target **int) error {
	raw, ok := LookupNonBlankEnv(name)
	if !ok {
		return nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid %s value", name)
	}
	*target = &value
	return nil
}

func setEnvUint(name string, target **uint64) error {
	raw, ok := LookupNonBlankEnv(name)
	if !ok {
		return nil
	}
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid %s value", name)
	}
	*target = &value
	return nil
}

// setEnvCSV 解析逗号分隔列表;去掉首尾空白并丢弃空项。全空时不覆盖。
func setEnvCSV(name string, target **[]string) {
	raw, ok := LookupNonBlankEnv(name)
	if !ok {
		return
	}
	values := parseCSV(raw)
	if len(values) == 0 {
		return
	}
	*target = &values
}

func parseCSV(raw string) []string {
	var values []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func nonEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return value
}

func derefBoolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func derefIntOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func derefUintOr(value *uint64, fallback uint64) uint64 {
	if value == nil {
		return fallback
	}
	return *value
}

func derefSliceOr(value *[]string, fallback func() []string) []string {
	if value == nil {
		if fallback == nil {
			return nil
		}
		return fallback()
	}
	return *value
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

// Validate 做启动校验。平台禁用时直接放行(不会有任何组件被构造)。
// 成功时把 WorkspaceRoot 规范化为绝对路径。
func (c *PlatformConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	baseURL, err := url.Parse(c.PublicBaseURL)
	if err != nil {
		return fmt.Errorf("[platform].public_base_url must be an absolute HTTP(S) URL: %w", err)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return errors.New("[platform].public_base_url must use http or https")
	}
	if baseURL.Host == "" {
		return errors.New("[platform].public_base_url must be an absolute HTTP(S) URL: missing host")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return errors.New("[platform].public_base_url must not contain credentials, query string, or fragment")
	}
	if baseURL.Scheme == "https" && !c.SecureCookies {
		return errors.New("HTTPS public_base_url requires [admin].secure_cookies=true (ADMIN__SECURE_COOKIES=true)")
	}
	if baseURL.Scheme == "http" {
		switch baseURL.Hostname() {
		case "127.0.0.1", "localhost", "::1":
		default:
			return errors.New("HTTP public_base_url is only allowed for loopback development")
		}
	}

	if c.MaxRequestBodyBytes < 1024 {
		return errors.New("[platform].max_request_body_bytes must be at least 1024")
	}
	if c.GlobalMaxConcurrency < 1 || c.GlobalMaxConcurrency > 256 {
		return errors.New("[platform].global_max_concurrency must be between 1 and 256")
	}
	if c.DBMaxConns < 0 || c.DBMaxConns > 256 {
		return errors.New("[platform].db_max_conns must be between 0 (default 8) and 256")
	}

	if c.AdminUsername == "" || len(c.AdminUsername) > 120 {
		return errors.New("[admin].username must contain 1-120 bytes after trimming")
	}
	if c.AdminPassword == "" && c.AdminPasswordHash == "" {
		return errors.New("admin login requires [admin].password or [admin].password_hash; " +
			"prefer ADMIN__PASSWORD or ADMIN__PASSWORD_HASH")
	}
	if c.AdminPasswordHash != "" && !strings.HasPrefix(c.AdminPasswordHash, "$argon2") {
		return errors.New("[admin].password_hash must be an Argon2 PHC string")
	}
	switch c.AdminPassword {
	case "admin", "password", "change-me", exampleAdminPassword:
		return errors.New("[admin].password is an unsafe placeholder; configure a strong password")
	}
	if c.AdminPassword != "" && (len(c.AdminPassword) < 8 || len(c.AdminPassword) > 512) {
		return errors.New("[admin].password must contain 8-512 bytes")
	}
	if len(c.AdminSessionSecret) < 32 {
		return errors.New("[admin].session_secret must contain at least 32 bytes; set ADMIN__SESSION_SECRET")
	}
	if c.AdminSessionSecret == exampleAdminSessionSecret {
		return errors.New("[admin].session_secret is a public template placeholder; configure a random secret")
	}
	if c.AdminSessionTTL < 300*time.Second || c.AdminSessionTTL > 30*24*time.Hour {
		return errors.New("[admin].session_ttl_secs must be between 300 and 2592000")
	}
	for _, raw := range c.AdminTrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("[admin].trusted_proxy_cidrs contains invalid CIDR %q", raw)
		}
	}

	return nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
