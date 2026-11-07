// Package agent 定义智能体：可以干活的 AI。
//
// 智能体 = 执行绑定（kind × mode）+ 岗位聘任（多对多）+ 运行约束
// （并发/队列/墙钟）。它是 wire 身份：push 的 agentCode、动态入口
// /agent/{slug}、连接器 claim 的 agentSlug 都指向 Slug。
package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"ir-agent-server/internal/domain/exec"
)

// 缺省运行约束，与既有系统对齐。
const (
	DefaultTimeoutSecs    = 600
	DefaultMaxConcurrency = 1
	DefaultQueueCapacity  = 10
	DefaultPermissionMode = "dontAsk"
)

// Agent 是智能体实体。
// json 标签服务于批量迁移格式（/agents/export|import）——管理 API 的
// 展示态另由视图层组装，不受此影响。
type Agent struct {
	// ID 是存储主键（网关幂等记录键的组成部分）。
	ID          int64  `json:"id,omitempty"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Binding 是执行绑定；remote-pull 绑定的智能体由连接器队列派发，
	// 不占用全局执行并发闸。
	Binding exec.Binding `json:"binding"`
	// Positions 是聘任的岗位 Code 集合（去重、非空才可启用）。
	Positions    []string `json:"positions,omitempty"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	// WorkspacePath 是本地 CLI 执行器的工作目录。
	WorkspacePath string   `json:"workspace_path,omitempty"`
	Model         *string  `json:"claude_model,omitempty"`
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	// SignatureEncoding 是动态入口签名编码（hex/base64）。
	SignatureEncoding string `json:"signature_encoding,omitempty"`
	// PermissionMode 是本地 CLI 执行器的权限口径。
	PermissionMode string `json:"permission_mode,omitempty"`
	TimeoutSecs    uint64 `json:"timeout_secs,omitempty"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	QueueCapacity  int    `json:"queue_capacity,omitempty"`
	Enabled        bool   `json:"enabled"`
	// SigningSecret 是动态入口的 HMAC 密钥（AES-256-GCM 加密存储，不回显）。
	// 仅执行路径（GetBySlug）填充；任何序列化永不携带明文。
	SigningSecret []byte `json:"-"`
	// SecretConfigured 是列表/视图用的派生态：签名密钥是否已配置。列表查询
	// 不解密（单个坏密文不影响浏览整页），执行路径始终解密并严格校验。
	SecretConfigured bool      `json:"secret_configured,omitempty"`
	Revision         int64     `json:"revision,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

// Export 是智能体导出记录（迁移格式）：携带签名密钥密文（AAD=slug，
// 主密钥一致的目标环境可直接落库，明文密钥永不出现在导出流）。
type Export struct {
	Agent
	// SecretEnc 是 AES-GCM 密文（AAD=slug）；明文密钥永不序列化。
	SecretEnc string `json:"secret_enc"`
}

// ImportEntry 是批量导入条目：SigningSecret（明文，新环境录入）与
// SecretEnc（密文，同主密钥迁移）二选一，都空且目标已存在 = 保留既有密钥。
type ImportEntry struct {
	Agent
	SecretEnc string `json:"secret_enc"`
}

// ListQuery 是管理台规模的智能体列表查询条件。
type ListQuery struct {
	// Query 模糊匹配 slug 与 name（大小写不敏感）；空 = 全部。
	Query string
	// IncludeDisabled 为 true 时包含停用智能体。
	IncludeDisabled bool
	// Limit/Offset 分页；Limit<=0 视作 50，上限 500。
	Limit  int
	Offset int
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// ErrNotFound 是仓储未命中哨兵。
var ErrNotFound = errors.New("agent not found")

// ErrRevisionConflict 是 If-Match 乐观并发冲突哨兵。
var ErrRevisionConflict = errors.New("revision conflict")

// ApplyDefaults 填充缺省值。
func (a *Agent) ApplyDefaults() {
	a.Slug = strings.ToLower(strings.TrimSpace(a.Slug))
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.SystemPrompt = strings.TrimSpace(a.SystemPrompt)
	a.WorkspacePath = strings.TrimSpace(a.WorkspacePath)
	if a.WorkspacePath == "" {
		a.WorkspacePath = "."
	}
	a.SignatureEncoding = strings.ToLower(strings.TrimSpace(a.SignatureEncoding))
	if a.SignatureEncoding == "" {
		a.SignatureEncoding = "hex"
	}
	a.PermissionMode = strings.TrimSpace(a.PermissionMode)
	if a.PermissionMode == "" {
		a.PermissionMode = DefaultPermissionMode
	}
	if a.TimeoutSecs == 0 {
		a.TimeoutSecs = DefaultTimeoutSecs
	}
	if a.MaxConcurrency == 0 {
		a.MaxConcurrency = DefaultMaxConcurrency
	}
	// QueueCapacity 保留显式 0（无排队，准入=并发）；「未提供」的缺省
	// 语义（10）属于 API 输入层，不在实体上表达。
	seen := make(map[string]bool, len(a.Positions))
	clean := a.Positions[:0]
	for _, code := range a.Positions {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code != "" && !seen[code] {
			seen[code] = true
			clean = append(clean, code)
		}
	}
	a.Positions = clean
	tools := make([]string, 0, len(a.AllowedTools))
	for _, tool := range a.AllowedTools {
		if tool = strings.TrimSpace(tool); tool != "" {
			tools = append(tools, tool)
		}
	}
	a.AllowedTools = tools
	// 签名密钥是不透明字节，不 trim；显式空串视为未提供。
	if a.SigningSecret != nil && len(a.SigningSecret) == 0 {
		a.SigningSecret = nil
	}
}

// Validate 规范化并校验。requireSecret 为真时（创建场景）必须提供签名密钥。
func (a *Agent) Validate(requireSecret bool) error {
	a.ApplyDefaults()
	if !slugPattern.MatchString(a.Slug) {
		return errors.New("slug must be 2-64 chars of [a-z0-9-], not starting or ending with '-'")
	}
	if n := utf8.RuneCountInString(a.Name); n < 1 || n > 30 {
		return errors.New("name must be 1-30 characters")
	}
	if err := a.Binding.Validate(); err != nil {
		return err
	}
	if a.Enabled && len(a.Positions) == 0 {
		return errors.New("an enabled agent must staff at least one position")
	}
	if a.SignatureEncoding != "hex" && a.SignatureEncoding != "base64" {
		return errors.New("signature_encoding must be hex or base64")
	}
	if requireSecret && len(a.SigningSecret) == 0 {
		return errors.New("signing_secret is required on create")
	}
	if n := len(a.SigningSecret); n != 0 && (n < 16 || n > 512) {
		return fmt.Errorf("signing_secret must be 16-512 bytes, got %d", n)
	}
	return nil
}

// ServesPosition 报告智能体是否聘任了指定岗位。push 的 bound-task 同时
// 携带 positionType 与 agentCode 时，调度层据此做聘任校验。
func (a *Agent) ServesPosition(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range a.Positions {
		if c == code {
			return true
		}
	}
	return false
}

// Repository 是智能体仓储端口。Upsert 在事务中递增 revision 与全局注册表
// 版本（多实例经版本轮询收敛停用/删除/密钥轮换）。
type Repository interface {
	GetBySlug(ctx context.Context, slug string) (*Agent, error)
	List(ctx context.Context, includeDisabled bool) ([]Agent, error)
	// Search 是分页/搜索列表（千级智能体规模）：结果不携带解密密钥，
	// 只置 SecretConfigured 标记。
	Search(ctx context.Context, q ListQuery) ([]Agent, int64, error)
	Upsert(ctx context.Context, a *Agent, ifMatchRevision *int64) (int64, error)
	// ImportUpsert 与 Upsert 同语义，但签名密钥直接落传入密文（批量
	// 导入的密文迁移路径；明文永不过手）。
	ImportUpsert(ctx context.Context, a *Agent, secretEnc string,
		ifMatchRevision *int64) (int64, error)
	// ExportAll 导出全部智能体（含密文，不解密）——迁移与备份格式。
	ExportAll(ctx context.Context) ([]Export, error)
	Delete(ctx context.Context, slug string) error
	// RegistryVersion 返回全局注册表版本，用于多实例收敛轮询。
	RegistryVersion(ctx context.Context) (int64, error)
}
