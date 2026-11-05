package agent

import (
	"time"

	"ir-agent-server/internal/domain/exec"
	"ir-agent-server/internal/domain/platform"
)

// ResolvedConfig 是一个智能体的存储态运行快照（含解密后的明文签名密钥，
// 禁止序列化外发）。旧栈名 AgentConfig；执行引擎与调度层共享此值对象。
type ResolvedConfig struct {
	ID                int64
	Slug              string
	Name              string
	Description       string
	Enabled           bool
	ExecutorKind      exec.Kind
	ExecutionMode     exec.Mode
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

// SignatureEncodingOrHex 解析存储的编码名，无法识别时回退 hex。
func (a *ResolvedConfig) SignatureEncodingOrHex() platform.SignatureEncoding {
	if encoding, err := platform.ParseEncoding(a.SignatureEncoding); err == nil {
		return encoding
	}
	return platform.EncodingHex
}

// SecretHint 是管理台展示用的密钥占位文案；永不回显真实密钥。
func (a *ResolvedConfig) SecretHint() string { return "已加密配置" }

// Timeout 返回该智能体的单次执行总时限。
func (a *ResolvedConfig) Timeout() time.Duration {
	return time.Duration(a.TimeoutSecs) * time.Second
}

// Channel 是一个启用智能体的心跳通道凭据（slug + 解密后的签名密钥）。
// Push 形态心跳按通道独立发送 Bearer <该智能体的 signing_secret>。
type Channel struct {
	Slug          string
	SigningSecret string
}

// Resolved 把智能体实体转换为执行快照（执行引擎与网关入口消费的形态）。
func (a *Agent) Resolved() *ResolvedConfig {
	model := a.Model
	secret := ""
	if len(a.SigningSecret) > 0 {
		secret = string(a.SigningSecret)
	}
	return &ResolvedConfig{
		ID: a.ID, Slug: a.Slug, Name: a.Name, Description: a.Description,
		Enabled: a.Enabled, ExecutorKind: a.Binding.Kind, ExecutionMode: a.Binding.Mode,
		SigningSecret: secret, SignatureEncoding: a.SignatureEncoding,
		SystemPrompt: a.SystemPrompt, WorkspacePath: a.WorkspacePath,
		ClaudeModel: model, AllowedTools: a.AllowedTools,
		PermissionMode: a.PermissionMode, TimeoutSecs: a.TimeoutSecs,
		MaxConcurrency: a.MaxConcurrency, QueueCapacity: a.QueueCapacity,
		Revision:  a.Revision,
		CreatedAt: a.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		UpdatedAt: a.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
}
