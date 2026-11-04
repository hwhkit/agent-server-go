package mota

// Agent 配置的输入校验、存储态与展示态。
// 对应 Rust 侧 src/platform/model.rs。

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// ExecutorKind
// ---------------------------------------------------------------------------

// ExecutorKind 是执行该 Agent 的后端种类。
type ExecutorKind string

const (
	// ExecutorKindRust 是进程内 Processor(Rust 版名称保留,Go 版即内置 Processor)。
	ExecutorKindRust            ExecutorKind = "rust"
	ExecutorKindClaudeCode      ExecutorKind = "claude_code"
	ExecutorKindCodex           ExecutorKind = "codex"
	ExecutorKindReasonix        ExecutorKind = "reasonix"
	ExecutorKindPi              ExecutorKind = "pi"
	ExecutorKindOpenCode        ExecutorKind = "opencode"
	ExecutorKindQwenCode        ExecutorKind = "qwen_code"
	ExecutorKindGeminiCLI       ExecutorKind = "gemini_cli"
	ExecutorKindOpenHands       ExecutorKind = "openhands"
	ExecutorKindDeepseekHarness ExecutorKind = "deepseek_harness"
	ExecutorKindGoose           ExecutorKind = "goose"
	ExecutorKindOpenAIAgent     ExecutorKind = "openai_agent"
	ExecutorKindClaudeManaged   ExecutorKind = "claude_managed_agent"
)

// ParseExecutorKind 严格解析(不做大小写折叠),与 Rust 侧 ExecutorKind::parse 一致。
func ParseExecutorKind(value string) (ExecutorKind, error) {
	switch ExecutorKind(value) {
	case ExecutorKindRust, ExecutorKindClaudeCode, ExecutorKindCodex, ExecutorKindReasonix,
		ExecutorKindPi, ExecutorKindOpenCode, ExecutorKindQwenCode, ExecutorKindGeminiCLI,
		ExecutorKindOpenHands, ExecutorKindDeepseekHarness, ExecutorKindGoose,
		ExecutorKindOpenAIAgent, ExecutorKindClaudeManaged:
		return ExecutorKind(value), nil
	default:
		return "", errors.New("unsupported executor kind")
	}
}

func (k ExecutorKind) String() string { return string(k) }

// SupportsLocalCLI 表示该执行器是否支持本机 CLI 进程调用。
func (k ExecutorKind) SupportsLocalCLI() bool {
	switch k {
	case ExecutorKindClaudeCode, ExecutorKindCodex, ExecutorKindReasonix, ExecutorKindPi,
		ExecutorKindOpenCode, ExecutorKindQwenCode, ExecutorKindGeminiCLI,
		ExecutorKindOpenHands, ExecutorKindDeepseekHarness:
		return true
	default:
		return false
	}
}

// SupportsGateway 表示该执行器是否可经 litellm 网关直调。
func (k ExecutorKind) SupportsGateway() bool {
	switch k {
	case ExecutorKindRust, ExecutorKindOpenCode, ExecutorKindQwenCode,
		ExecutorKindGeminiCLI, ExecutorKindOpenHands, ExecutorKindOpenAIAgent,
		ExecutorKindClaudeManaged:
		return false
	default:
		return true
	}
}

// SupportsExecutionMode 判断执行器与执行方式的组合是否可用。
func (k ExecutorKind) SupportsExecutionMode(mode ExecutionMode) bool {
	switch mode {
	case ExecutionModeGateway:
		return k == ExecutorKindRust || k.SupportsGateway()
	case ExecutionModeLocal:
		return k == ExecutorKindRust || k.SupportsLocalCLI()
	case ExecutionModeManaged:
		return k == ExecutorKindOpenAIAgent || k == ExecutorKindClaudeManaged
	case ExecutionModeRemotePull:
		// kind 仅描述远端连接器名义上运行的执行器,平台不本机执行,
		// 因此不做 kind 组合限制。
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ExecutionMode
// ---------------------------------------------------------------------------

// ExecutionMode 是 Agent 执行方式:经 litellm 网关直调(默认,保留既有行为),
// 调用本机安装的 agent CLI，或调用远端托管智能体运行时。
type ExecutionMode string

const (
	// ExecutionModeGateway 经 litellm 网关 HTTP 直调。
	ExecutionModeGateway ExecutionMode = "gateway"
	// ExecutionModeLocal 调用本机安装的 agent CLI 进程。
	ExecutionModeLocal ExecutionMode = "local"
	// ExecutionModeManaged 由 OpenAI / Anthropic 托管 Agent 管理会话与上下文。
	ExecutionModeManaged ExecutionMode = "managed"
	// ExecutionModeRemotePull 任务进入 Pull 队列,由远端连接器认领执行;
	// 平台不做本机执行,容量由 agent_pull_tasks 的 SQL 计数约束。
	ExecutionModeRemotePull ExecutionMode = "remote_pull"
)

// ParseExecutionMode 严格解析,与 Rust 侧 ExecutionMode::parse 一致。
func ParseExecutionMode(value string) (ExecutionMode, error) {
	switch ExecutionMode(value) {
	case ExecutionModeGateway, ExecutionModeLocal, ExecutionModeManaged, ExecutionModeRemotePull:
		return ExecutionMode(value), nil
	default:
		return "", errors.New("execution_mode must be gateway, local, managed, or remote_pull")
	}
}

func (m ExecutionMode) String() string { return string(m) }

// ---------------------------------------------------------------------------
// AgentInput
// ---------------------------------------------------------------------------

// AgentInput 是管理 API 收到的 Agent 配置。指针字段用于区分「未提供」与「显式置空」。
type AgentInput struct {
	Slug          string        `json:"slug"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Enabled       *bool         `json:"enabled"`
	ExecutorKind  ExecutorKind  `json:"executor_kind"`
	ExecutionMode ExecutionMode `json:"execution_mode"`
	// SigningSecret 创建时必填;更新时留空表示保留原密钥。
	SigningSecret     *string  `json:"signing_secret"`
	SignatureEncoding string   `json:"signature_encoding"`
	SystemPrompt      string   `json:"system_prompt"`
	WorkspacePath     string   `json:"workspace_path"`
	ClaudeModel       *string  `json:"claude_model"`
	AllowedTools      []string `json:"allowed_tools"`
	PermissionMode    string   `json:"permission_mode"`
	TimeoutSecs       *uint64  `json:"timeout_secs"`
	MaxConcurrency    *int     `json:"max_concurrency"`
	QueueCapacity     *int     `json:"queue_capacity"`
}

// EnabledOrDefault 返回 enabled,未提供时默认 true。
func (i *AgentInput) EnabledOrDefault() bool { return i.Enabled == nil || *i.Enabled }

// TimeoutSecsOrDefault 返回 timeout_secs,未提供时默认 600。
func (i *AgentInput) TimeoutSecsOrDefault() uint64 {
	if i.TimeoutSecs == nil {
		return 600
	}
	return *i.TimeoutSecs
}

// MaxConcurrencyOrDefault 返回 max_concurrency,未提供时默认 1。
func (i *AgentInput) MaxConcurrencyOrDefault() int {
	if i.MaxConcurrency == nil {
		return 1
	}
	return *i.MaxConcurrency
}

// QueueCapacityOrDefault 返回 queue_capacity,未提供时默认 10。
func (i *AgentInput) QueueCapacityOrDefault() int {
	if i.QueueCapacity == nil {
		return 10
	}
	return *i.QueueCapacity
}

// applyDefaults 把未提供的字段填成 Rust 侧 AgentInput::default() 的值。
func (i *AgentInput) applyDefaults() {
	if i.ExecutorKind == "" {
		i.ExecutorKind = ExecutorKindPi
	}
	if i.ExecutionMode == "" {
		i.ExecutionMode = ExecutionModeGateway
	}
	if i.SignatureEncoding == "" {
		i.SignatureEncoding = "hex"
	}
	if i.WorkspacePath == "" {
		i.WorkspacePath = "."
	}
	if i.PermissionMode == "" {
		i.PermissionMode = "dontAsk"
	}
}

// Validate 规范化并校验输入。requireSecret 为真时(创建场景)必须提供 signing_secret。
func (i *AgentInput) Validate(requireSecret bool) error {
	i.applyDefaults()

	i.Slug = strings.ToLower(strings.TrimSpace(i.Slug))
	i.Name = strings.TrimSpace(i.Name)
	i.Description = strings.TrimSpace(i.Description)
	i.SystemPrompt = strings.TrimSpace(i.SystemPrompt)
	i.WorkspacePath = strings.TrimSpace(i.WorkspacePath)
	i.SignatureEncoding = strings.ToLower(strings.TrimSpace(i.SignatureEncoding))
	i.PermissionMode = strings.TrimSpace(i.PermissionMode)

	if i.ClaudeModel != nil {
		trimmed := strings.TrimSpace(*i.ClaudeModel)
		if trimmed == "" {
			i.ClaudeModel = nil
		} else {
			i.ClaudeModel = &trimmed
		}
	}
	// 签名密钥是不透明字节。不要 trim,否则会静默改掉调用方配置的密钥。
	if i.SigningSecret != nil && *i.SigningSecret == "" {
		i.SigningSecret = nil
	}

	cleanTools := make([]string, 0, len(i.AllowedTools))
	for _, tool := range i.AllowedTools {
		if tool = strings.TrimSpace(tool); tool != "" {
			cleanTools = append(cleanTools, tool)
		}
	}
	i.AllowedTools = cleanTools

	if err := ValidateSlug(i.Slug); err != nil {
		return err
	}
	if nameLen := utf8.RuneCountInString(i.Name); nameLen < 1 || nameLen > 30 {
		return errors.New("name must contain 1-30 characters")
	}
	if utf8.RuneCountInString(i.Description) > 500 {
		return errors.New("description must not exceed 500 characters")
	}
	if requireSecret && i.SigningSecret == nil {
		return errors.New("signing_secret is required when creating an Agent")
	}
	if i.SigningSecret != nil {
		if length := len(*i.SigningSecret); length < 16 || length > 512 {
			return errors.New("signing_secret must contain 16-512 bytes")
		}
	}
	if _, ok := ParseSignatureEncoding(i.SignatureEncoding); !ok {
		return errors.New("signature_encoding must be hex or base64")
	}
	if _, err := ParseExecutionMode(string(i.ExecutionMode)); err != nil {
		return err
	}
	if _, err := ParseExecutorKind(string(i.ExecutorKind)); err != nil {
		return err
	}
	if !i.ExecutorKind.SupportsExecutionMode(i.ExecutionMode) {
		return fmt.Errorf("executor %s does not support %s execution",
			i.ExecutorKind, i.ExecutionMode)
	}
	if utf8.RuneCountInString(i.SystemPrompt) > 32000 {
		return errors.New("system_prompt must not exceed 32000 characters")
	}
	if err := validateWorkspacePath(i.WorkspacePath); err != nil {
		return err
	}
	if i.ClaudeModel != nil && len(*i.ClaudeModel) > 120 {
		return errors.New("claude_model must not exceed 120 characters")
	}
	if len(i.AllowedTools) > 64 {
		return errors.New("allowed_tools contains too many, overly long, or invalid values")
	}
	for _, tool := range i.AllowedTools {
		if len(tool) > 200 || strings.Contains(tool, ",") || strings.ContainsFunc(tool, unicode.IsControl) {
			return errors.New("allowed_tools contains too many, overly long, or invalid values")
		}
	}
	switch i.PermissionMode {
	case "dontAsk", "plan", "acceptEdits", "manual":
	default:
		// permission_mode 已不再由管理台暴露；存量或第三方客户端传入未知值时
		// 与 Rust 版一致归一为安全默认值。
		i.PermissionMode = "dontAsk"
	}
	if timeout := i.TimeoutSecsOrDefault(); timeout < 5 || timeout > 3600 {
		return errors.New("timeout_secs must be between 5 and 3600")
	}
	if concurrency := i.MaxConcurrencyOrDefault(); concurrency < 1 || concurrency > 32 {
		return errors.New("max_concurrency must be between 1 and 32")
	}
	if capacity := i.QueueCapacityOrDefault(); capacity < 0 || capacity > 1000 {
		return errors.New("queue_capacity must not exceed 1000")
	}
	return nil
}

// ValidateSlug 校验 Agent slug:2-64 个字符,首尾为字母或数字,只含小写字母、
// 数字与单个连字符。
func ValidateSlug(slug string) error {
	if len(slug) < 2 || len(slug) > 64 {
		return errors.New("slug must contain 2-64 characters")
	}
	isAlnum := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	if !isAlnum(slug[0]) || !isAlnum(slug[len(slug)-1]) {
		return errors.New("slug must start and end with a letter or number")
	}
	for i := 0; i < len(slug); i++ {
		b := slug[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' {
			continue
		}
		return errors.New("slug may only contain lowercase letters, numbers, and hyphens")
	}
	if strings.Contains(slug, "--") {
		return errors.New("slug must not contain consecutive hyphens")
	}
	return nil
}

// validateWorkspacePath 要求相对路径且不含 `..` 段。这里检查的是原始路径段,
// 不做 Clean 折叠,因此 "a/../b" 也会被拒绝(与 Rust 侧组件检查一致)。
func validateWorkspacePath(value string) error {
	if value == "" {
		return errors.New("workspace_path must not be empty")
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) {
		return errors.New("workspace_path must stay within the configured workspace root")
	}
	// Windows 盘符前缀(rs 的 Component::Prefix)。
	if len(value) >= 2 && value[1] == ':' {
		return errors.New("workspace_path must stay within the configured workspace root")
	}
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	}) {
		if segment == ".." {
			return errors.New("workspace_path must stay within the configured workspace root")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// AgentConfig / AgentView
// ---------------------------------------------------------------------------

// AgentConfig 是一个 Agent 的存储态(含解密后的明文签名密钥,禁止序列化外发)。
type AgentConfig struct {
	ID                int64
	Slug              string
	Name              string
	Description       string
	Enabled           bool
	ExecutorKind      ExecutorKind
	ExecutionMode     ExecutionMode
	SigningSecret     string
	SignatureEncoding string
	SystemPrompt      string
	WorkspacePath     string
	ClaudeModel       *string
	AllowedTools      []string
	PermissionMode    string
	TimeoutSecs       uint64
	MaxConcurrency    int
	QueueCapacity     int
	Revision          int64
	CreatedAt         string
	UpdatedAt         string
}

// SignatureEncodingOrHex 解析存储的编码名,无法识别时回退 hex。
// 对应 Rust 侧 AgentConfig::signature_encoding()。
func (a *AgentConfig) SignatureEncodingOrHex() SignatureEncoding {
	if encoding, ok := ParseSignatureEncoding(a.SignatureEncoding); ok {
		return encoding
	}
	return SignatureEncodingHex
}

// SecretHint 是管理台展示用的密钥占位文案;永不回显真实密钥。
func (a *AgentConfig) SecretHint() string { return "已加密配置" }

// Timeout 返回该 Agent 的单次执行总时限。
func (a *AgentConfig) Timeout() time.Duration {
	return time.Duration(a.TimeoutSecs) * time.Second
}

// AgentView 是管理 API 返回的 Agent 展示态。JSON 键为 snake_case,与 Rust 侧
// AgentView 的默认序列化一致。
type AgentView struct {
	ID           int64        `json:"id"`
	Slug         string       `json:"slug"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Enabled      bool         `json:"enabled"`
	ExecutorKind ExecutorKind `json:"executor_kind"`
	// ExecutionMode 执行方式:gateway 或 local。
	ExecutionMode           ExecutionMode `json:"execution_mode"`
	SignatureEncoding       string        `json:"signature_encoding"`
	SigningSecretConfigured bool          `json:"signing_secret_configured"`
	SigningSecretHint       string        `json:"signing_secret_hint"`
	Endpoint                string        `json:"endpoint"`
	SystemPrompt            string        `json:"system_prompt"`
	WorkspacePath           string        `json:"workspace_path"`
	ClaudeModel             *string       `json:"claude_model"`
	AllowedTools            []string      `json:"allowed_tools"`
	PermissionMode          string        `json:"permission_mode"`
	TimeoutSecs             uint64        `json:"timeout_secs"`
	MaxConcurrency          int           `json:"max_concurrency"`
	QueueCapacity           int           `json:"queue_capacity"`
	Revision                int64         `json:"revision"`
	CreatedAt               string        `json:"created_at"`
	UpdatedAt               string        `json:"updated_at"`
}

// ToView 生成展示态。endpoint 由平台公开基址推导。
func (a *AgentConfig) ToView(publicBaseURL string) AgentView {
	tools := a.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	return AgentView{
		ID:                      a.ID,
		Slug:                    a.Slug,
		Name:                    a.Name,
		Description:             a.Description,
		Enabled:                 a.Enabled,
		ExecutorKind:            a.ExecutorKind,
		ExecutionMode:           a.ExecutionMode,
		SignatureEncoding:       a.SignatureEncoding,
		SigningSecretConfigured: true,
		SigningSecretHint:       a.SecretHint(),
		Endpoint:                strings.TrimRight(publicBaseURL, "/") + "/agent/" + a.Slug,
		SystemPrompt:            a.SystemPrompt,
		WorkspacePath:           a.WorkspacePath,
		ClaudeModel:             a.ClaudeModel,
		AllowedTools:            tools,
		PermissionMode:          a.PermissionMode,
		TimeoutSecs:             a.TimeoutSecs,
		MaxConcurrency:          a.MaxConcurrency,
		QueueCapacity:           a.QueueCapacity,
		Revision:                a.Revision,
		CreatedAt:               a.CreatedAt,
		UpdatedAt:               a.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// RunView / ChannelEventView / SessionRecord
// ---------------------------------------------------------------------------

// RunView 是一次 Agent 运行的展示态。JSON 键为 snake_case。
type RunView struct {
	RunID         string `json:"run_id"`
	AgentSlug     string `json:"agent_slug"`
	AgentRevision int64  `json:"agent_revision"`
	// ExecutorKind 是运行开始时的执行器快照,避免 Agent 后续编辑改写历史语义。
	ExecutorKind  *string `json:"executor_kind"`
	ExecutionMode *string `json:"execution_mode"`
	Status        string  `json:"status"`
	DurationMs    *int64  `json:"duration_ms"`
	ErrorCode     *string `json:"error_code"`
	// ResponseCode 是动态 Agent HTTP 入口实际返回给调用方的状态码。
	ResponseCode *int64 `json:"response_code"`
	// Input 是请求体原文(JSON 字符串);未记录为 nil。
	Input *string `json:"input"`
	// Output 是结果体原文(JSON 字符串);未记录为 nil。
	Output *string `json:"output"`
	// ExecutorOutput 是执行节点直接返回的最终结果;早期记录或执行器未产生结果时为 nil。
	ExecutorOutput *string `json:"executor_output"`
	CreatedAt      string  `json:"created_at"`
	CompletedAt    *string `json:"completed_at"`
}

// ChannelEventView 是通道事件记录(callback 回传 / heartbeat 心跳)的展示态,
// 供管理台运行记录页使用。JSON 键为 camelCase。
type ChannelEventView struct {
	EventType  string  `json:"eventType"`
	AgentSlug  *string `json:"agentSlug"`
	RunID      *string `json:"runId"`
	URL        string  `json:"url"`
	StatusCode *int64  `json:"statusCode"`
	Accepted   *bool   `json:"accepted"`
	RunStatus  *string `json:"runStatus"`
	Pass       *bool   `json:"pass"`
	LatencyMs  *int64  `json:"latencyMs"`
	Detail     *string `json:"detail"`
	Input      *string `json:"input"`
	Output     *string `json:"output"`
	CreatedAt  string  `json:"createdAt"`
}

// AuditLogView 是管理台用户操作审计记录。Details 不包含密钥明文。
type AuditLogView struct {
	ID        int64  `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Details   any    `json:"details"`
	CreatedAt string `json:"createdAt"`
}

// UserRole 是管理台账号角色。
type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

func ParseUserRole(value string) (UserRole, bool) {
	switch UserRole(value) {
	case UserRoleAdmin, UserRoleUser:
		return UserRole(value), true
	default:
		return "", false
	}
}

type UserView struct {
	Username  string   `json:"username"`
	Role      UserRole `json:"role"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

type UserAuthRecord struct {
	Username     string
	PasswordHash string
	Role         UserRole
	Enabled      bool
}

// SessionRecord 是一条有效的管理会话。
type SessionRecord struct {
	Username  string
	Role      UserRole
	CSRFToken string
	ExpiresAt int64
}
