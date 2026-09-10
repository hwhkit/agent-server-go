package mota

// A2A 接入配置。对应 rs 侧 src/a2a/config.rs。
//
// 独立解析 config/default.toml -> config/{env}.toml -> A2A__* 环境变量的
// [a2a] 段。之所以不挂到 mota.Config:既有测试用结构体字面量构造 Config,
// 加字段会打断全部字面量。TOML 未知段互不干扰,hwhkit 的 AppConfig 解析
// 也会忽略 [a2a]。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// a2aAppVersion 是 Cargo.toml 里 env!("CARGO_PKG_VERSION") 的 Go 侧对应常量,
// 用作 agent_version 缺省值。
const a2aAppVersion = "0.1.0"

// A2AConfig 是解析后的 A2A 配置。
type A2AConfig struct {
	// Enabled 默认关闭。A2A 入口能触发本机 CLI 子进程执行,必须先确认认证
	// 配置到位再显式开启。
	Enabled bool
	// BasePath 是 JSON-RPC 端点前缀,实际路由为 {base_path}/{slug}。
	BasePath string
	// StreamEnabled 表示是否声明并提供 SendStreamingMessage / SubscribeToTask。
	StreamEnabled bool
	// RequireBearerAuth 表示是否强制 Authorization: Bearer。
	// 仅在受信内网且有其他鉴权时才可关闭。
	RequireBearerAuth bool
	// AgentVersion 是对外 Agent Card 的 version 字段(Agent 自身版本,不是
	// 协议版本)。
	AgentVersion         string
	ProviderOrganization string
	ProviderURL          string
	DocumentationURL     string
	MaxRequestBodyBytes  int
	// DefaultCardSlug 是 /.well-known/agent-card.json 使用的 Agent slug;
	// 留空时回落到 mota.Config.DefaultAgentSlug。
	DefaultCardSlug string
}

// DefaultA2AConfig 返回内置默认值。
func DefaultA2AConfig() *A2AConfig {
	return &A2AConfig{
		Enabled:             false,
		BasePath:            "/a2a",
		StreamEnabled:       true,
		RequireBearerAuth:   true,
		AgentVersion:        a2aAppVersion,
		MaxRequestBodyBytes: 1024 * 1024,
		DefaultCardSlug:     "",
	}
}

// a2aFile 是 [a2a] 段的镜像,全部字段可选,用于分层合并。
type a2aFile struct {
	Enabled              *bool   `toml:"enabled"`
	BasePath             *string `toml:"base_path"`
	StreamEnabled        *bool   `toml:"stream_enabled"`
	RequireBearerAuth    *bool   `toml:"require_bearer_auth"`
	AgentVersion         *string `toml:"agent_version"`
	ProviderOrganization *string `toml:"provider_organization"`
	ProviderURL          *string `toml:"provider_url"`
	DocumentationURL     *string `toml:"documentation_url"`
	MaxRequestBodyBytes  *int    `toml:"max_request_body_bytes"`
	DefaultCardSlug      *string `toml:"default_card_slug"`
}

type a2aRootFile struct {
	A2A a2aFile `toml:"a2a"`
}

func (f *a2aFile) merge(other *a2aFile) {
	if other == nil {
		return
	}
	mergePtr(&f.Enabled, other.Enabled)
	mergePtr(&f.BasePath, other.BasePath)
	mergePtr(&f.StreamEnabled, other.StreamEnabled)
	mergePtr(&f.RequireBearerAuth, other.RequireBearerAuth)
	mergePtr(&f.AgentVersion, other.AgentVersion)
	mergePtr(&f.ProviderOrganization, other.ProviderOrganization)
	mergePtr(&f.ProviderURL, other.ProviderURL)
	mergePtr(&f.DocumentationURL, other.DocumentationURL)
	mergePtr(&f.MaxRequestBodyBytes, other.MaxRequestBodyBytes)
	mergePtr(&f.DefaultCardSlug, other.DefaultCardSlug)
}

// LoadA2AConfig 加载 A2A 配置:config/default.toml -> config/{env}.toml ->
// A2A__* 环境变量。env 由 A2A_ENV 指定,回退 MOTA_ENV,再回退 dev;
// 等于 "default" 时跳过第二层。
func LoadA2AConfig() (*A2AConfig, error) {
	file := &a2aFile{}

	root, err := readA2ARoot("config/default.toml")
	if err != nil {
		return nil, err
	}
	if root != nil {
		file.merge(&root.A2A)
	}

	env := os.Getenv("A2A_ENV")
	if env == "" {
		env = os.Getenv("MOTA_ENV")
	}
	if env == "" {
		env = "dev"
	}
	if env != "default" {
		root, err = readA2ARoot(fmt.Sprintf("config/%s.toml", env))
		if err != nil {
			return nil, err
		}
		if root != nil {
			file.merge(&root.A2A)
		}
	}

	applyA2AEnv(file)
	return a2aConfigFromFile(file)
}

func readA2ARoot(path string) (*a2aRootFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	root := &a2aRootFile{}
	if err := toml.Unmarshal(content, root); err != nil {
		return nil, fmt.Errorf("parse %s as TOML: %w", path, err)
	}
	return root, nil
}

func a2aConfigFromFile(file *a2aFile) (*A2AConfig, error) {
	defaults := DefaultA2AConfig()

	agentVersion := strings.TrimSpace(derefOr(file.AgentVersion, defaults.AgentVersion))
	if agentVersion == "" {
		agentVersion = a2aAppVersion
	}

	config := &A2AConfig{
		Enabled: derefBoolOr(file.Enabled, defaults.Enabled),
		BasePath: strings.TrimRight(
			strings.TrimSpace(derefOr(file.BasePath, defaults.BasePath)), "/"),
		StreamEnabled:        derefBoolOr(file.StreamEnabled, defaults.StreamEnabled),
		RequireBearerAuth:    derefBoolOr(file.RequireBearerAuth, defaults.RequireBearerAuth),
		AgentVersion:         agentVersion,
		ProviderOrganization: strings.TrimSpace(derefOr(file.ProviderOrganization, defaults.ProviderOrganization)),
		ProviderURL:          strings.TrimSpace(derefOr(file.ProviderURL, defaults.ProviderURL)),
		DocumentationURL:     strings.TrimSpace(derefOr(file.DocumentationURL, defaults.DocumentationURL)),
		MaxRequestBodyBytes:  derefIntOr(file.MaxRequestBodyBytes, defaults.MaxRequestBodyBytes),
		DefaultCardSlug: strings.ToLower(strings.TrimSpace(
			derefOr(file.DefaultCardSlug, defaults.DefaultCardSlug))),
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return config, nil
}

func (c *A2AConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if !strings.HasPrefix(c.BasePath, "/") || len(c.BasePath) < 2 {
		return fmt.Errorf(
			"[a2a].base_path must start with '/' and not be the root path, got %q", c.BasePath)
	}
	if strings.Contains(c.BasePath, "{") || strings.Contains(c.BasePath, "}") {
		return errors.New("[a2a].base_path must not contain path parameters")
	}
	if c.MaxRequestBodyBytes < 1024 || c.MaxRequestBodyBytes > 16*1024*1024 {
		return errors.New("[a2a].max_request_body_bytes must be between 1024 and 16777216")
	}
	if c.ProviderURL != "" {
		if err := validateAbsoluteHTTPURL(c.ProviderURL); err != nil {
			return fmt.Errorf("[a2a].provider_url must be an absolute HTTP(S) URL: %w", err)
		}
	}
	if c.DocumentationURL != "" {
		if err := validateAbsoluteHTTPURL(c.DocumentationURL); err != nil {
			return fmt.Errorf("[a2a].documentation_url must be an absolute HTTP(S) URL: %w", err)
		}
	}
	// provider 是 AgentCard 的可选对象,但一旦出现,organization 与 url 都是必填。
	if (c.ProviderOrganization == "") != (c.ProviderURL == "") {
		return errors.New(
			"[a2a].provider_organization and [a2a].provider_url must be set together or both left empty")
	}
	return nil
}

// ResolveDefaultCardSlug 把 [a2a].default_card_slug 留空时回落到
// [mota].default_agent_slug。
func (c *A2AConfig) ResolveDefaultCardSlug(fallback string) {
	if c.DefaultCardSlug == "" {
		c.DefaultCardSlug = strings.ToLower(strings.TrimSpace(fallback))
	}
}

// AgentPath 返回 {base_path}/{slug} 的 JSON-RPC 端点路径。
func (c *A2AConfig) AgentPath(slug string) string {
	return c.BasePath + "/" + slug
}

// ---------------------------------------------------------------------------
// A2A__* 环境变量覆盖;空字符串视为未设置
// ---------------------------------------------------------------------------

func applyA2AEnv(file *a2aFile) {
	setEnvBoolA2A("A2A__ENABLED", &file.Enabled)
	setEnvStringA2A("A2A__BASE_PATH", &file.BasePath)
	setEnvBoolA2A("A2A__STREAM_ENABLED", &file.StreamEnabled)
	setEnvBoolA2A("A2A__REQUIRE_BEARER_AUTH", &file.RequireBearerAuth)
	setEnvStringA2A("A2A__AGENT_VERSION", &file.AgentVersion)
	setEnvStringA2A("A2A__PROVIDER_ORGANIZATION", &file.ProviderOrganization)
	setEnvStringA2A("A2A__PROVIDER_URL", &file.ProviderURL)
	setEnvStringA2A("A2A__DOCUMENTATION_URL", &file.DocumentationURL)
	setEnvIntA2A("A2A__MAX_REQUEST_BODY_BYTES", &file.MaxRequestBodyBytes)
	setEnvStringA2A("A2A__DEFAULT_CARD_SLUG", &file.DefaultCardSlug)
}

func setEnvStringA2A(name string, target **string) {
	if raw, ok := os.LookupEnv(name); ok && strings.TrimSpace(raw) != "" {
		*target = &raw
	}
}

func setEnvBoolA2A(name string, target **bool) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		value := true
		*target = &value
	case "false", "0", "no", "off":
		value := false
		*target = &value
	}
}

func setEnvIntA2A(name string, target **int) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	if value, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		*target = &value
	}
}
