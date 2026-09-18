package mota

// 单体多 Agent 平台配置。
//
// 加载顺序与 MOTA 配置一致:config/default.toml -> config/{env}.toml
// -> PLATFORM__* / ADMIN__* / CLAUDE__* / AGENT_GATEWAY__* / LOCAL_AGENT__* 环境变量。
// env 由 PLATFORM_ENV 指定,回退 MOTA_ENV,再回退 dev;等于 "default" 时跳过第二层。
//
// 对应 Rust 侧 src/platform/config.rs。

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// GatewayProtocol 是网关兼容协议。LiteLLM 同时支持 Anthropic Messages 与
// OpenAI Chat Completions。
type GatewayProtocol string

const (
	GatewayProtocolAnthropic GatewayProtocol = "anthropic"
	GatewayProtocolOpenAI    GatewayProtocol = "openai"
)

// ParseGatewayProtocol 接受 anthropic / openai / open_ai(大小写与首尾空白不敏感)。
func ParseGatewayProtocol(value string) (GatewayProtocol, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "anthropic":
		return GatewayProtocolAnthropic, true
	case "openai", "open_ai":
		return GatewayProtocolOpenAI, true
	default:
		return "", false
	}
}

// GatewayConfig 是经 litellm 网关直调 Agent 的配置。
type GatewayConfig struct {
	BaseURL          string
	APIKey           string
	Model            string
	Protocol         GatewayProtocol
	MaxTokens        uint64
	MaxAttempts      int
	MaxResponseBytes int
}

// LocalAgentConfig 是本机 agent CLI(pi / codex / reasonix)的配置。
type LocalAgentConfig struct {
	CodexBinary           string
	PiBinary              string
	ReasonixBinary        string
	DeepseekHarnessBinary string
	OpenCodeBinary        string
	OpenCodeDefaultModel  string
	QwenCodeBinary        string
	QwenCodeDefaultModel  string
	GeminiCLIBinary       string
	GeminiCLIDefaultModel string
	OpenHandsBinary       string
	OpenHandsDefaultModel string
	// FinanceExtensionPath 是 Pi 基本面/技术分析岗位的只读金融数据扩展;
	// 未配置时保持无工具模式。
	FinanceExtensionPath string
	// StrategySkillPaths 是策略分析师可用的标准 SKILL.md 路径;配置时必须为 2-3 个。
	StrategySkillPaths []string
	// AnalysisSkillPaths 是基本面/技术分析师可按 Agent 选择的标准 SKILL.md 路径。
	AnalysisSkillPaths []string
	// WorkflowSkillPaths 是组合/风控/执行岗位可按 Agent 选择的标准 SKILL.md 路径。
	WorkflowSkillPaths []string
	DefaultModel       string
	AllowedEnv         []string
	MaxOutputBytes     int
	MaxPromptBytes     int
}

// OpenAIAgentsConfig 是 OpenAI 托管 Agents API 配置。
type OpenAIAgentsConfig struct {
	Enabled          bool
	BaseURL          string
	APIKey           string
	DefaultModel     string
	ReasoningEffort  string
	Environment      string
	MCPServerURL     string
	MCPServerLabel   string
	MCPAuthorization string
	MCPRequired      bool
	DeleteSession    bool
	MaxResponseBytes int
}

// ClaudeManagedAgentsConfig 是 Anthropic Claude Managed Agents(beta)配置。
type ClaudeManagedAgentsConfig struct {
	Enabled          bool
	BaseURL          string
	APIKey           string
	WorkspaceID      string
	AgentID          string
	EnvironmentID    string
	VaultIDs         []string
	MaxListCostCents *uint64
	DeleteSession    bool
	MaxResponseBytes int
}

// PlatformConfig 是平台运行所需的全部配置。
type PlatformConfig struct {
	Enabled              bool
	PublicBaseURL        string
	MaxRequestBodyBytes  int
	GlobalMaxConcurrency int

	AdminUsername      string
	AdminPassword      string
	AdminPasswordHash  string
	AdminSessionSecret string
	AdminSessionTTL    time.Duration
	SecureCookies      bool

	WorkspaceRoot string
	ClaudeEnabled bool
	ClaudeBinary  string
	// ClaudeDefaultModel 是 Agent 未显式指定 claude_model 时使用的默认模型。
	// 经 litellm 网关时须填网关模型名(如 dsk_4f),否则会回退到 Claude 内置默认模型名。
	ClaudeDefaultModel   string
	ClaudeAllowedEnv     []string
	MaxClaudeOutputBytes int

	Gateway       GatewayConfig
	LocalAgents   LocalAgentConfig
	OpenAIAgents  OpenAIAgentsConfig
	ClaudeManaged ClaudeManagedAgentsConfig
}

// ---------------------------------------------------------------------------
// TOML 镜像结构:全部字段可选,用于分层合并
// ---------------------------------------------------------------------------

type platformRootFile struct {
	Platform      platformSectionFile `toml:"platform"`
	Admin         adminSectionFile    `toml:"admin"`
	Claude        claudeSectionFile   `toml:"claude"`
	Gateway       gatewaySectionFile  `toml:"gateway"`
	LocalAgents   localAgentFile      `toml:"local_agents"`
	OpenAIAgents  openAIAgentsFile    `toml:"openai_agents"`
	ClaudeManaged claudeManagedFile   `toml:"claude_managed"`
}

type platformSectionFile struct {
	Enabled              *bool   `toml:"enabled"`
	PublicBaseURL        *string `toml:"public_base_url"`
	MaxRequestBodyBytes  *int    `toml:"max_request_body_bytes"`
	GlobalMaxConcurrency *int    `toml:"global_max_concurrency"`
}

type adminSectionFile struct {
	Username       *string `toml:"username"`
	Password       *string `toml:"password"`
	PasswordHash   *string `toml:"password_hash"`
	SessionSecret  *string `toml:"session_secret"`
	SessionTTLSecs *uint64 `toml:"session_ttl_secs"`
	SecureCookies  *bool   `toml:"secure_cookies"`
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
func (r *platformRootFile) merge(other *platformRootFile) {
	if other == nil {
		return
	}
	mergePtr(&r.Platform.Enabled, other.Platform.Enabled)
	mergePtr(&r.Platform.PublicBaseURL, other.Platform.PublicBaseURL)
	mergePtr(&r.Platform.MaxRequestBodyBytes, other.Platform.MaxRequestBodyBytes)
	mergePtr(&r.Platform.GlobalMaxConcurrency, other.Platform.GlobalMaxConcurrency)

	mergePtr(&r.Admin.Username, other.Admin.Username)
	mergePtr(&r.Admin.Password, other.Admin.Password)
	mergePtr(&r.Admin.PasswordHash, other.Admin.PasswordHash)
	mergePtr(&r.Admin.SessionSecret, other.Admin.SessionSecret)
	mergePtr(&r.Admin.SessionTTLSecs, other.Admin.SessionTTLSecs)
	mergePtr(&r.Admin.SecureCookies, other.Admin.SecureCookies)

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
	root := &platformRootFile{}

	file, err := readPlatformRoot("config/default.toml")
	if err != nil {
		return nil, err
	}
	root.merge(file)

	env := os.Getenv("PLATFORM_ENV")
	if env == "" {
		env = os.Getenv("MOTA_ENV")
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
	return platformConfigFromFile(root)
}

// platformConfigFromFile 把合并后的可选字段落成带默认值的 PlatformConfig 并校验。
func platformConfigFromFile(root *platformRootFile) (*PlatformConfig, error) {
	protocol := GatewayProtocolAnthropic
	if root.Gateway.Protocol != nil {
		parsed, ok := ParseGatewayProtocol(*root.Gateway.Protocol)
		if !ok {
			return nil, errors.New("[gateway].protocol must be anthropic or openai")
		}
		protocol = parsed
	}

	config := &PlatformConfig{
		Enabled:              derefBoolOr(root.Platform.Enabled, true),
		PublicBaseURL:        strings.TrimRight(derefOr(root.Platform.PublicBaseURL, "http://127.0.0.1:8080"), "/"),
		MaxRequestBodyBytes:  derefIntOr(root.Platform.MaxRequestBodyBytes, 2*1024*1024),
		GlobalMaxConcurrency: derefIntOr(root.Platform.GlobalMaxConcurrency, 16),

		AdminUsername:      strings.TrimSpace(derefOr(root.Admin.Username, "admin")),
		AdminPassword:      nonEmpty(derefOr(root.Admin.Password, "")),
		AdminPasswordHash:  nonEmpty(derefOr(root.Admin.PasswordHash, "")),
		AdminSessionSecret: derefOr(root.Admin.SessionSecret, ""),
		AdminSessionTTL:    secondsOr(root.Admin.SessionTTLSecs, 72*60*60),
		SecureCookies:      derefBoolOr(root.Admin.SecureCookies, false),

		WorkspaceRoot:        derefOr(root.Claude.WorkspaceRoot, "."),
		ClaudeEnabled:        derefBoolOr(root.Claude.Enabled, false),
		ClaudeBinary:         derefOr(root.Claude.Binary, "claude"),
		ClaudeDefaultModel:   derefOr(root.Claude.DefaultModel, "dsk_4f"),
		ClaudeAllowedEnv:     derefSliceOr(root.Claude.AllowedEnv, defaultClaudeAllowedEnv),
		MaxClaudeOutputBytes: derefIntOr(root.Claude.MaxOutputBytes, 1024*1024),

		Gateway: GatewayConfig{
			BaseURL:          strings.TrimRight(derefOr(root.Gateway.BaseURL, ""), "/"),
			APIKey:           derefOr(root.Gateway.APIKey, ""),
			Model:            strings.TrimSpace(derefOr(root.Gateway.Model, "dsk_4f")),
			Protocol:         protocol,
			MaxTokens:        derefUintOr(root.Gateway.MaxTokens, 16384),
			MaxAttempts:      derefIntOr(root.Gateway.MaxAttempts, 3),
			MaxResponseBytes: derefIntOr(root.Gateway.MaxResponseBytes, 2*1024*1024),
		},

		LocalAgents: LocalAgentConfig{
			CodexBinary:           derefOr(root.LocalAgents.CodexBinary, "codex"),
			PiBinary:              derefOr(root.LocalAgents.PiBinary, "/usr/local/bin/pi"),
			ReasonixBinary:        derefOr(root.LocalAgents.ReasonixBinary, "/usr/local/bin/reasonix"),
			DeepseekHarnessBinary: derefOr(root.LocalAgents.DeepseekHarnessBinary, "dsh"),
			OpenCodeBinary:        derefOr(root.LocalAgents.OpenCodeBinary, "opencode"),
			OpenCodeDefaultModel:  derefOr(root.LocalAgents.OpenCodeDefaultModel, ""),
			QwenCodeBinary:        derefOr(root.LocalAgents.QwenCodeBinary, "qwen"),
			QwenCodeDefaultModel:  derefOr(root.LocalAgents.QwenCodeDefaultModel, ""),
			GeminiCLIBinary:       derefOr(root.LocalAgents.GeminiCLIBinary, "gemini"),
			GeminiCLIDefaultModel: derefOr(root.LocalAgents.GeminiCLIDefaultModel, "auto"),
			OpenHandsBinary:       derefOr(root.LocalAgents.OpenHandsBinary, "openhands"),
			OpenHandsDefaultModel: derefOr(root.LocalAgents.OpenHandsDefaultModel, ""),
			FinanceExtensionPath:  nonEmpty(derefOr(root.LocalAgents.FinanceExtensionPath, "")),
			StrategySkillPaths:    derefSliceOr(root.LocalAgents.StrategySkillPaths, nil),
			AnalysisSkillPaths:    derefSliceOr(root.LocalAgents.AnalysisSkillPaths, nil),
			WorkflowSkillPaths:    derefSliceOr(root.LocalAgents.WorkflowSkillPaths, nil),
			DefaultModel:          derefOr(root.LocalAgents.DefaultModel, "litellm/dsk_4f"),
			AllowedEnv:            derefSliceOr(root.LocalAgents.AllowedEnv, defaultLocalAgentEnv),
			MaxOutputBytes:        derefIntOr(root.LocalAgents.MaxOutputBytes, 1024*1024),
			MaxPromptBytes:        derefIntOr(root.LocalAgents.MaxPromptBytes, 128*1024),
		},
		OpenAIAgents: OpenAIAgentsConfig{
			Enabled:          derefBoolOr(root.OpenAIAgents.Enabled, false),
			BaseURL:          strings.TrimRight(derefOr(root.OpenAIAgents.BaseURL, "https://api.openai.com/v1"), "/"),
			APIKey:           derefOr(root.OpenAIAgents.APIKey, ""),
			DefaultModel:     strings.TrimSpace(derefOr(root.OpenAIAgents.DefaultModel, "gpt-6-astra")),
			ReasoningEffort:  strings.TrimSpace(derefOr(root.OpenAIAgents.ReasoningEffort, "medium")),
			Environment:      strings.TrimSpace(derefOr(root.OpenAIAgents.Environment, "none")),
			MCPServerURL:     nonEmpty(derefOr(root.OpenAIAgents.MCPServerURL, "")),
			MCPServerLabel:   strings.TrimSpace(derefOr(root.OpenAIAgents.MCPServerLabel, "mota_tools")),
			MCPAuthorization: derefOr(root.OpenAIAgents.MCPAuthorization, ""),
			MCPRequired:      derefBoolOr(root.OpenAIAgents.MCPRequired, true),
			DeleteSession:    derefBoolOr(root.OpenAIAgents.DeleteSession, true),
			MaxResponseBytes: derefIntOr(root.OpenAIAgents.MaxResponseBytes, 2*1024*1024),
		},
		ClaudeManaged: ClaudeManagedAgentsConfig{
			Enabled:          derefBoolOr(root.ClaudeManaged.Enabled, false),
			BaseURL:          strings.TrimRight(derefOr(root.ClaudeManaged.BaseURL, "https://api.anthropic.com/v1"), "/"),
			APIKey:           derefOr(root.ClaudeManaged.APIKey, ""),
			WorkspaceID:      nonEmpty(derefOr(root.ClaudeManaged.WorkspaceID, "")),
			AgentID:          strings.TrimSpace(derefOr(root.ClaudeManaged.AgentID, "")),
			EnvironmentID:    strings.TrimSpace(derefOr(root.ClaudeManaged.EnvironmentID, "")),
			VaultIDs:         derefSliceOr(root.ClaudeManaged.VaultIDs, nil),
			MaxListCostCents: root.ClaudeManaged.MaxListCostCents,
			DeleteSession:    derefBoolOr(root.ClaudeManaged.DeleteSession, true),
			MaxResponseBytes: derefIntOr(root.ClaudeManaged.MaxResponseBytes, 2*1024*1024),
		},
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
		"MOTA_FINANCE_USER_AGENT",
		"LOCAL_AGENT__STRATEGY_SKILL_PATHS",
		"LOCAL_AGENT__ANALYSIS_SKILL_PATHS",
		"LOCAL_AGENT__WORKFLOW_SKILL_PATHS",
	}
}

// readPlatformRoot 读取整个 TOML 文件;文件不存在返回 (nil, nil)。
func readPlatformRoot(path string) (*platformRootFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	root := &platformRootFile{}
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
func applyPlatformEnv(root *platformRootFile) error {
	setEnvString("PLATFORM__PUBLIC_BASE_URL", &root.Platform.PublicBaseURL)
	setEnvString("ADMIN__USERNAME", &root.Admin.Username)
	setEnvString("ADMIN__PASSWORD", &root.Admin.Password)
	setEnvString("ADMIN__PASSWORD_HASH", &root.Admin.PasswordHash)
	setEnvString("ADMIN__SESSION_SECRET", &root.Admin.SessionSecret)
	setEnvString("CLAUDE__WORKSPACE_ROOT", &root.Claude.WorkspaceRoot)
	setEnvString("CLAUDE__BINARY", &root.Claude.Binary)
	setEnvString("CLAUDE__DEFAULT_MODEL", &root.Claude.DefaultModel)
	setEnvString("AGENT_GATEWAY__BASE_URL", &root.Gateway.BaseURL)
	setEnvString("AGENT_GATEWAY__API_KEY", &root.Gateway.APIKey)
	setEnvString("AGENT_GATEWAY__MODEL", &root.Gateway.Model)
	setEnvString("AGENT_GATEWAY__PROTOCOL", &root.Gateway.Protocol)
	setEnvString("LOCAL_AGENT__CODEX_BINARY", &root.LocalAgents.CodexBinary)
	setEnvString("LOCAL_AGENT__PI_BINARY", &root.LocalAgents.PiBinary)
	setEnvString("LOCAL_AGENT__REASONIX_BINARY", &root.LocalAgents.ReasonixBinary)
	setEnvString("LOCAL_AGENT__DEEPSEEK_HARNESS_BINARY", &root.LocalAgents.DeepseekHarnessBinary)
	setEnvString("LOCAL_AGENT__OPENCODE_BINARY", &root.LocalAgents.OpenCodeBinary)
	setEnvString("LOCAL_AGENT__OPENCODE_DEFAULT_MODEL", &root.LocalAgents.OpenCodeDefaultModel)
	setEnvString("LOCAL_AGENT__QWEN_CODE_BINARY", &root.LocalAgents.QwenCodeBinary)
	setEnvString("LOCAL_AGENT__QWEN_CODE_DEFAULT_MODEL", &root.LocalAgents.QwenCodeDefaultModel)
	setEnvString("LOCAL_AGENT__GEMINI_CLI_BINARY", &root.LocalAgents.GeminiCLIBinary)
	setEnvString("LOCAL_AGENT__GEMINI_CLI_DEFAULT_MODEL", &root.LocalAgents.GeminiCLIDefaultModel)
	setEnvString("LOCAL_AGENT__OPENHANDS_BINARY", &root.LocalAgents.OpenHandsBinary)
	setEnvString("LOCAL_AGENT__OPENHANDS_DEFAULT_MODEL", &root.LocalAgents.OpenHandsDefaultModel)
	setEnvString("LOCAL_AGENT__DEFAULT_MODEL", &root.LocalAgents.DefaultModel)
	setEnvString("OPENAI_AGENTS__BASE_URL", &root.OpenAIAgents.BaseURL)
	setEnvString("OPENAI_AGENTS__API_KEY", &root.OpenAIAgents.APIKey)
	setEnvString("OPENAI_AGENTS__DEFAULT_MODEL", &root.OpenAIAgents.DefaultModel)
	setEnvString("OPENAI_AGENTS__REASONING_EFFORT", &root.OpenAIAgents.ReasoningEffort)
	setEnvString("OPENAI_AGENTS__ENVIRONMENT", &root.OpenAIAgents.Environment)
	setEnvString("OPENAI_AGENTS__MCP_SERVER_URL", &root.OpenAIAgents.MCPServerURL)
	setEnvString("OPENAI_AGENTS__MCP_SERVER_LABEL", &root.OpenAIAgents.MCPServerLabel)
	setEnvString("OPENAI_AGENTS__MCP_AUTHORIZATION", &root.OpenAIAgents.MCPAuthorization)
	setEnvString("CLAUDE_MANAGED__BASE_URL", &root.ClaudeManaged.BaseURL)
	setEnvString("CLAUDE_MANAGED__API_KEY", &root.ClaudeManaged.APIKey)
	setEnvString("CLAUDE_MANAGED__WORKSPACE_ID", &root.ClaudeManaged.WorkspaceID)
	setEnvString("CLAUDE_MANAGED__AGENT_ID", &root.ClaudeManaged.AgentID)
	setEnvString("CLAUDE_MANAGED__ENVIRONMENT_ID", &root.ClaudeManaged.EnvironmentID)

	// 兼容现网原有 Anthropic/LiteLLM 环境变量;新 AGENT_GATEWAY__* 优先。
	if root.Gateway.BaseURL == nil || strings.TrimSpace(*root.Gateway.BaseURL) == "" {
		setEnvString("ANTHROPIC_BASE_URL", &root.Gateway.BaseURL)
	}
	if root.Gateway.APIKey == nil || *root.Gateway.APIKey == "" {
		setEnvString("ANTHROPIC_AUTH_TOKEN", &root.Gateway.APIKey)
	}
	if root.Gateway.Model == nil {
		setEnvString("CLAUDE__DEFAULT_MODEL", &root.Gateway.Model)
	}

	if err := setEnvBool("CLAUDE__ENABLED", &root.Claude.Enabled); err != nil {
		return err
	}
	if err := setEnvBool("PLATFORM__ENABLED", &root.Platform.Enabled); err != nil {
		return err
	}
	if err := setEnvBool("ADMIN__SECURE_COOKIES", &root.Admin.SecureCookies); err != nil {
		return err
	}
	if err := setEnvBool("OPENAI_AGENTS__ENABLED", &root.OpenAIAgents.Enabled); err != nil {
		return err
	}
	if err := setEnvBool("OPENAI_AGENTS__MCP_REQUIRED", &root.OpenAIAgents.MCPRequired); err != nil {
		return err
	}
	if err := setEnvBool("OPENAI_AGENTS__DELETE_SESSION", &root.OpenAIAgents.DeleteSession); err != nil {
		return err
	}
	if err := setEnvBool("CLAUDE_MANAGED__ENABLED", &root.ClaudeManaged.Enabled); err != nil {
		return err
	}
	if err := setEnvBool("CLAUDE_MANAGED__DELETE_SESSION", &root.ClaudeManaged.DeleteSession); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__MAX_REQUEST_BODY_BYTES", &root.Platform.MaxRequestBodyBytes); err != nil {
		return err
	}
	if err := setEnvInt("PLATFORM__GLOBAL_MAX_CONCURRENCY", &root.Platform.GlobalMaxConcurrency); err != nil {
		return err
	}
	if err := setEnvUint("ADMIN__SESSION_TTL_SECS", &root.Admin.SessionTTLSecs); err != nil {
		return err
	}
	if err := setEnvInt("CLAUDE__MAX_OUTPUT_BYTES", &root.Claude.MaxOutputBytes); err != nil {
		return err
	}
	if err := setEnvUint("AGENT_GATEWAY__MAX_TOKENS", &root.Gateway.MaxTokens); err != nil {
		return err
	}
	if err := setEnvInt("AGENT_GATEWAY__MAX_ATTEMPTS", &root.Gateway.MaxAttempts); err != nil {
		return err
	}
	if err := setEnvInt("AGENT_GATEWAY__MAX_RESPONSE_BYTES", &root.Gateway.MaxResponseBytes); err != nil {
		return err
	}
	if err := setEnvInt("LOCAL_AGENT__MAX_OUTPUT_BYTES", &root.LocalAgents.MaxOutputBytes); err != nil {
		return err
	}
	if err := setEnvInt("LOCAL_AGENT__MAX_PROMPT_BYTES", &root.LocalAgents.MaxPromptBytes); err != nil {
		return err
	}
	if err := setEnvInt("OPENAI_AGENTS__MAX_RESPONSE_BYTES", &root.OpenAIAgents.MaxResponseBytes); err != nil {
		return err
	}
	if err := setEnvInt("CLAUDE_MANAGED__MAX_RESPONSE_BYTES", &root.ClaudeManaged.MaxResponseBytes); err != nil {
		return err
	}
	if err := setEnvUint("CLAUDE_MANAGED__MAX_LIST_COST_CENTS", &root.ClaudeManaged.MaxListCostCents); err != nil {
		return err
	}
	setEnvCSV("CLAUDE_MANAGED__VAULT_IDS", &root.ClaudeManaged.VaultIDs)

	if root.LocalAgents.FinanceExtensionPath == nil {
		setEnvString("LOCAL_AGENT__FINANCE_EXTENSION_PATH", &root.LocalAgents.FinanceExtensionPath)
	}
	if root.LocalAgents.StrategySkillPaths == nil {
		setEnvCSV("LOCAL_AGENT__STRATEGY_SKILL_PATHS", &root.LocalAgents.StrategySkillPaths)
	}
	if root.LocalAgents.AnalysisSkillPaths == nil {
		setEnvCSV("LOCAL_AGENT__ANALYSIS_SKILL_PATHS", &root.LocalAgents.AnalysisSkillPaths)
	}
	if root.LocalAgents.WorkflowSkillPaths == nil {
		setEnvCSV("LOCAL_AGENT__WORKFLOW_SKILL_PATHS", &root.LocalAgents.WorkflowSkillPaths)
	}
	// 这两个白名单允许环境变量无条件覆盖文件层。
	setEnvCSV("CLAUDE__ALLOWED_ENV", &root.Claude.AllowedEnv)
	setEnvCSV("LOCAL_AGENT__ALLOWED_ENV", &root.LocalAgents.AllowedEnv)
	return nil
}

// lookupNonBlankEnv 返回环境变量的值;未设置或仅含空白时返回 false。
func lookupNonBlankEnv(name string) (string, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

func setEnvString(name string, target **string) {
	if raw, ok := lookupNonBlankEnv(name); ok {
		value := raw
		*target = &value
	}
}

func setEnvBool(name string, target **bool) error {
	raw, ok := lookupNonBlankEnv(name)
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
	raw, ok := lookupNonBlankEnv(name)
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
	raw, ok := lookupNonBlankEnv(name)
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
	raw, ok := lookupNonBlankEnv(name)
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
	case "admin", "password", "change-me":
		return errors.New("[admin].password is an unsafe placeholder; configure a strong password")
	}
	if len(c.AdminSessionSecret) < 32 {
		return errors.New("[admin].session_secret must contain at least 32 bytes; set ADMIN__SESSION_SECRET")
	}
	if c.AdminSessionTTL < 300*time.Second || c.AdminSessionTTL > 30*24*time.Hour {
		return errors.New("[admin].session_ttl_secs must be between 300 and 2592000")
	}

	if strings.TrimSpace(c.WorkspaceRoot) == "" {
		return errors.New("[claude].workspace_root must not be empty")
	}
	if strings.TrimSpace(c.ClaudeBinary) == "" {
		return errors.New("[claude].binary must not be empty")
	}
	if c.MaxClaudeOutputBytes < 4096 {
		return errors.New("[claude].max_output_bytes must be at least 4096")
	}

	if strings.TrimSpace(c.Gateway.BaseURL) != "" {
		gatewayURL, err := url.Parse(c.Gateway.BaseURL)
		if err != nil {
			return fmt.Errorf("[gateway].base_url must be an absolute HTTP(S) URL: %w", err)
		}
		if gatewayURL.Scheme != "http" && gatewayURL.Scheme != "https" {
			return errors.New("[gateway].base_url must use http or https")
		}
		if gatewayURL.Host == "" {
			return errors.New("[gateway].base_url must be an absolute HTTP(S) URL: missing host")
		}
	}
	if strings.TrimSpace(c.Gateway.Model) == "" {
		return errors.New("[gateway].model must not be empty")
	}
	if c.Gateway.MaxTokens < 256 || c.Gateway.MaxTokens > 131072 {
		return errors.New("[gateway].max_tokens must be between 256 and 131072")
	}
	if c.Gateway.MaxAttempts < 1 || c.Gateway.MaxAttempts > 6 {
		return errors.New("[gateway].max_attempts must be between 1 and 6")
	}
	if c.Gateway.MaxResponseBytes < 4096 || c.Gateway.MaxResponseBytes > 16*1024*1024 {
		return errors.New("[gateway].max_response_bytes must be between 4096 and 16777216")
	}

	for _, item := range []struct {
		name   string
		binary string
	}{
		{"codex_binary", c.LocalAgents.CodexBinary},
		{"pi_binary", c.LocalAgents.PiBinary},
		{"reasonix_binary", c.LocalAgents.ReasonixBinary},
		{"deepseek_harness_binary", c.LocalAgents.DeepseekHarnessBinary},
		{"opencode_binary", c.LocalAgents.OpenCodeBinary},
		{"qwen_code_binary", c.LocalAgents.QwenCodeBinary},
		{"gemini_cli_binary", c.LocalAgents.GeminiCLIBinary},
		{"openhands_binary", c.LocalAgents.OpenHandsBinary},
	} {
		if strings.TrimSpace(item.binary) == "" {
			return fmt.Errorf("[local_agents].%s must not be empty", item.name)
		}
	}
	if c.LocalAgents.FinanceExtensionPath != "" && !isRegularFile(c.LocalAgents.FinanceExtensionPath) {
		return errors.New("[local_agents].finance_extension_path must point to a readable file")
	}
	if len(c.LocalAgents.StrategySkillPaths) > 0 {
		if len(c.LocalAgents.StrategySkillPaths) < 2 || len(c.LocalAgents.StrategySkillPaths) > 3 {
			return errors.New("[local_agents].strategy_skill_paths must contain 2 or 3 SKILL.md files")
		}
		for _, path := range c.LocalAgents.StrategySkillPaths {
			if !isRegularFile(path) {
				return errors.New("[local_agents].strategy_skill_paths must point to readable files")
			}
		}
	}
	if len(c.LocalAgents.AnalysisSkillPaths) > 16 {
		return errors.New("[local_agents].analysis_skill_paths must contain at most 16 SKILL.md files")
	}
	for _, path := range c.LocalAgents.AnalysisSkillPaths {
		if !isRegularFile(path) {
			return errors.New("[local_agents].analysis_skill_paths must point to readable files")
		}
	}
	if len(c.LocalAgents.WorkflowSkillPaths) > 24 {
		return errors.New("[local_agents].workflow_skill_paths must contain at most 24 SKILL.md files")
	}
	for _, path := range c.LocalAgents.WorkflowSkillPaths {
		if !isRegularFile(path) {
			return errors.New("[local_agents].workflow_skill_paths must point to readable files")
		}
	}
	if strings.TrimSpace(c.LocalAgents.DefaultModel) == "" {
		return errors.New("[local_agents].default_model must not be empty")
	}
	if c.LocalAgents.MaxOutputBytes < 4096 {
		return errors.New("[local_agents].max_output_bytes must be at least 4096")
	}
	if c.LocalAgents.MaxPromptBytes < 4096 || c.LocalAgents.MaxPromptBytes > 1024*1024 {
		return errors.New("[local_agents].max_prompt_bytes must be between 4096 and 1048576")
	}

	if err := validateManagedURL("[openai_agents].base_url", c.OpenAIAgents.BaseURL); err != nil {
		return err
	}
	if c.OpenAIAgents.Enabled && strings.TrimSpace(c.OpenAIAgents.APIKey) == "" {
		return errors.New("[openai_agents].enabled=true requires api_key")
	}
	if c.OpenAIAgents.DefaultModel == "" {
		return errors.New("[openai_agents].default_model must not be empty")
	}
	if c.OpenAIAgents.ReasoningEffort == "" {
		return errors.New("[openai_agents].reasoning_effort must not be empty")
	}
	if c.OpenAIAgents.Environment != "none" {
		return errors.New("[openai_agents].environment currently must be none")
	}
	if c.OpenAIAgents.MCPServerURL != "" {
		if err := validateManagedURL("[openai_agents].mcp_server_url", c.OpenAIAgents.MCPServerURL); err != nil {
			return err
		}
		if c.OpenAIAgents.MCPServerLabel == "" {
			return errors.New("[openai_agents].mcp_server_label must not be empty")
		}
	}
	if c.OpenAIAgents.MaxResponseBytes < 4096 || c.OpenAIAgents.MaxResponseBytes > 16*1024*1024 {
		return errors.New("[openai_agents].max_response_bytes must be between 4096 and 16777216")
	}

	if err := validateManagedURL("[claude_managed].base_url", c.ClaudeManaged.BaseURL); err != nil {
		return err
	}
	if c.ClaudeManaged.Enabled {
		if strings.TrimSpace(c.ClaudeManaged.APIKey) == "" || c.ClaudeManaged.AgentID == "" || c.ClaudeManaged.EnvironmentID == "" {
			return errors.New("[claude_managed].enabled=true requires api_key, agent_id, and environment_id")
		}
	}
	if c.ClaudeManaged.MaxResponseBytes < 4096 || c.ClaudeManaged.MaxResponseBytes > 16*1024*1024 {
		return errors.New("[claude_managed].max_response_bytes must be between 4096 and 16777216")
	}

	// Rust 侧在 PlatformState::initialize 里 canonicalize workspace_root;
	// 这里提前解析为绝对路径,后续执行器只做 workspace 逃逸检查。
	root, err := filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve [claude].workspace_root %s: %w", c.WorkspaceRoot, err)
	}
	c.WorkspaceRoot = root
	return nil
}

func validateManagedURL(name, value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
	}
	return nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
