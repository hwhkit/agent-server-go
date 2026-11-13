package config

// Intelligent Router 接入配置。加载层级为
//
//	config/default.toml 的 [ir] 段
//	-> config/{env}.toml 的 [ir] 段(env 由 IR_ENV 指定,默认 dev)
//	-> 环境变量 IR__*(空字符串视为未设置,不覆盖)
//
// [ir] 段是 ir-agent-server 自己的配置段;hwhkit 的 AppConfig 解析会忽略
// 未知段,两者互不干扰。
//
// 对应 Rust 侧 src/config.rs。

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const irEnvPrefix = "IR__"

const maxIRDuration = 24 * time.Hour

// PlaceholderSecret 是 config/default.toml 中的占位签名密钥。启动校验会拒绝
// 它,避免开箱即可伪造 HMAC 认证的部署。生产必须用 IR__SIGNING_SECRET
// 注入真实密钥。
const PlaceholderSecret = "change-me"

// ExampleSigningSecret / ExampleBindToken are public values printed in
// .env.example. Treating a copied template as a real credential would make
// authentication forgeable, so enabled channels reject them at startup.
const (
	ExampleSigningSecret = "replace-with-ir-signing-secret"
	ExampleBindToken     = "replace-with-bind-token"
)

// PushMode 是 Push 响应形态。
type PushMode string

const (
	// PushModeSync 同步:200 + 结果信封。
	PushModeSync PushMode = "sync"
	// PushModeAsync 异步:202 + callback 回传。
	PushModeAsync PushMode = "async"
)

// ParsePushMode 大小写不敏感地解析 push_mode;无法识别时返回 false。
func ParsePushMode(value string) (PushMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sync":
		return PushModeSync, true
	case "async":
		return PushModeAsync, true
	default:
		return "", false
	}
}

// Config 是解析后的 IR 接入配置(结构与 Rust 侧接入配置对齐)。
type Config struct {
	// Push
	PushEnabled       bool
	PushPath          string
	SigningSecret     string
	SignatureEncoding SignatureEncoding

	// Pull
	PullEnabled       bool
	QueueURL          string
	BindToken         string
	WorkerID          string
	ClaimTimeout      time.Duration
	HeartbeatInterval time.Duration
	RenewInterval     time.Duration
	TaskTimeout       time.Duration

	// 新协议(ir-agent-bound-task v1)

	// PushMode "sync" = 同步(200 + 结果信封);"async" = 异步(202 + callback 回传)。
	PushMode PushMode
	// HeartbeatURL 是 Push 形态的心跳地址(平台绑定信息下发);配了才启用 Push 心跳。
	HeartbeatURL string
	// CallbackToken 是可选的回调令牌(mpk_*);非空时附加到 callback URL 的 ?token=。
	CallbackToken string
	// CallbackTokenOrigin 把 legacy token 绑定到唯一受信 origin，避免签名调用方
	// 在 callbackUrl 中指定第三方地址并窃取全局 token。
	CallbackTokenOrigin string
	// TrustedUsageReportingEnabled 向结果信封写入供应商 FINAL usage;
	// 普通 Bound 创作者必须保持 false。
	TrustedUsageReportingEnabled bool
	// DefaultAgentSlug 是 Push/Pull 的 agentCode 未命中 Registry 时使用的默认动态 Agent。
	DefaultAgentSlug string
}

// irFile 是 [ir] 段的扁平镜像,全部字段可选,用于分层合并。
type irFile struct {
	PushEnabled           *bool   `toml:"push_enabled"`
	PushPath              *string `toml:"push_path"`
	SigningSecret         *string `toml:"signing_secret"`
	SignatureEncoding     *string `toml:"signature_encoding"`
	PullEnabled           *bool   `toml:"pull_enabled"`
	QueueURL              *string `toml:"queue_url"`
	BindToken             *string `toml:"bind_token"`
	WorkerID              *string `toml:"worker_id"`
	ClaimTimeoutSecs      *uint64 `toml:"claim_timeout_secs"`
	HeartbeatIntervalSecs *uint64 `toml:"heartbeat_interval_secs"`
	RenewIntervalSecs     *uint64 `toml:"renew_interval_secs"`
	TaskTimeoutSecs       *uint64 `toml:"task_timeout_secs"`

	// 新协议(ir-agent-bound-task v1)
	PushMode     *string `toml:"push_mode"`
	HeartbeatURL *string `toml:"heartbeat_url"`
	// CallbackToken 可选回调令牌(mpk_*)。配置了就在 callback URL 上附加 ?token=,
	// 兼容要求双凭证的存量平台;新平台(2026-08-17 起)令牌可选,可留空。
	CallbackToken       *string `toml:"callback_token"`
	CallbackTokenOrigin *string `toml:"callback_token_origin"`
	// TrustedUsageReportingEnabled 仅平台登记的受信 Investor Workflow Host 可开启。
	TrustedUsageReportingEnabled *bool   `toml:"trusted_usage_reporting_enabled"`
	DefaultAgentSlug             *string `toml:"default_agent_slug"`
}

// merge 用 other 中已设置的字段覆盖 f。
func (f *irFile) merge(other *irFile) {
	if other == nil {
		return
	}
	if other.PushEnabled != nil {
		f.PushEnabled = other.PushEnabled
	}
	if other.PushPath != nil {
		f.PushPath = other.PushPath
	}
	if other.SigningSecret != nil {
		f.SigningSecret = other.SigningSecret
	}
	if other.SignatureEncoding != nil {
		f.SignatureEncoding = other.SignatureEncoding
	}
	if other.PullEnabled != nil {
		f.PullEnabled = other.PullEnabled
	}
	if other.QueueURL != nil {
		f.QueueURL = other.QueueURL
	}
	if other.BindToken != nil {
		f.BindToken = other.BindToken
	}
	if other.WorkerID != nil {
		f.WorkerID = other.WorkerID
	}
	if other.ClaimTimeoutSecs != nil {
		f.ClaimTimeoutSecs = other.ClaimTimeoutSecs
	}
	if other.HeartbeatIntervalSecs != nil {
		f.HeartbeatIntervalSecs = other.HeartbeatIntervalSecs
	}
	if other.RenewIntervalSecs != nil {
		f.RenewIntervalSecs = other.RenewIntervalSecs
	}
	if other.TaskTimeoutSecs != nil {
		f.TaskTimeoutSecs = other.TaskTimeoutSecs
	}
	if other.PushMode != nil {
		f.PushMode = other.PushMode
	}
	if other.HeartbeatURL != nil {
		f.HeartbeatURL = other.HeartbeatURL
	}
	if other.CallbackToken != nil {
		f.CallbackToken = other.CallbackToken
	}
	if other.CallbackTokenOrigin != nil {
		f.CallbackTokenOrigin = other.CallbackTokenOrigin
	}
	if other.TrustedUsageReportingEnabled != nil {
		f.TrustedUsageReportingEnabled = other.TrustedUsageReportingEnabled
	}
	if other.DefaultAgentSlug != nil {
		f.DefaultAgentSlug = other.DefaultAgentSlug
	}
}

// LoadConfig 按文档顺序加载并合并配置,然后做启动校验。
func LoadConfig() (*Config, error) {
	file := &irFile{}

	// 1) config/default.toml 的 [ir]
	table, err := readIRTable("config/default.toml")
	if err != nil {
		return nil, err
	}
	file.merge(table)

	// 2) config/{env}.toml 的 [ir]
	env := os.Getenv("IR_ENV")
	if env == "" {
		env = "dev"
	}
	if env != "default" {
		table, err = readIRTable(fmt.Sprintf("config/%s.toml", env))
		if err != nil {
			return nil, err
		}
		file.merge(table)
	}

	// 3) 环境变量 IR__* 覆盖
	applyIREnv(file)

	return configFromFile(file)
}

// configFromFile 把合并后的可选字段落成带默认值的 Config 并校验。
func configFromFile(f *irFile) (*Config, error) {
	pushEnabled := true
	if f.PushEnabled != nil {
		pushEnabled = *f.PushEnabled
	}
	pullEnabled := false
	if f.PullEnabled != nil {
		pullEnabled = *f.PullEnabled
	}

	signatureEncoding := SignatureEncodingHex
	if f.SignatureEncoding != nil {
		parsed, ok := ParseSignatureEncoding(*f.SignatureEncoding)
		if !ok {
			return nil, fmt.Errorf(
				"invalid [ir].signature_encoding: %q (expected \"hex\" or \"base64\"); "+
					"set IR__SIGNATURE_ENCODING", *f.SignatureEncoding)
		}
		signatureEncoding = parsed
	}

	pushMode := PushModeAsync
	if f.PushMode != nil {
		parsed, ok := ParsePushMode(*f.PushMode)
		if !ok {
			return nil, fmt.Errorf(
				"invalid [ir].push_mode: %q (expected \"sync\" or \"async\"); "+
					"set IR__PUSH_MODE", *f.PushMode)
		}
		pushMode = parsed
	}

	cfg := &Config{
		PushEnabled:                  pushEnabled,
		PushPath:                     derefOr(f.PushPath, "/ir/push"),
		SigningSecret:                derefOr(f.SigningSecret, ""),
		SignatureEncoding:            signatureEncoding,
		PullEnabled:                  pullEnabled,
		QueueURL:                     derefOr(f.QueueURL, ""),
		BindToken:                    derefOr(f.BindToken, ""),
		WorkerID:                     derefOr(f.WorkerID, ""),
		ClaimTimeout:                 irSecondsOr(f.ClaimTimeoutSecs, 60),
		HeartbeatInterval:            irSecondsOr(f.HeartbeatIntervalSecs, 60),
		RenewInterval:                irSecondsOr(f.RenewIntervalSecs, 30),
		TaskTimeout:                  irSecondsOr(f.TaskTimeoutSecs, 600),
		PushMode:                     pushMode,
		HeartbeatURL:                 derefOr(f.HeartbeatURL, ""),
		CallbackToken:                derefOr(f.CallbackToken, ""),
		CallbackTokenOrigin:          strings.TrimSpace(derefOr(f.CallbackTokenOrigin, "")),
		TrustedUsageReportingEnabled: f.TrustedUsageReportingEnabled != nil && *f.TrustedUsageReportingEnabled,
		DefaultAgentSlug:             strings.ToLower(strings.TrimSpace(derefOr(f.DefaultAgentSlug, ""))),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 做启动校验:启用 Push 必须有签名密钥(且不得为占位值);启用 Pull
// 必须有队列/令牌/workerId;claim 读超时不得低于 35 秒(协议要求)。
func (c *Config) Validate() error {
	if c.SignatureEncoding == "" {
		c.SignatureEncoding = SignatureEncodingHex
	}
	if _, ok := ParseSignatureEncoding(string(c.SignatureEncoding)); !ok {
		return fmt.Errorf(
			"invalid [ir].signature_encoding: %q (expected \"hex\" or \"base64\"); "+
				"set IR__SIGNATURE_ENCODING", c.SignatureEncoding)
	}
	if c.PushMode == "" {
		c.PushMode = PushModeAsync
	}
	if _, ok := ParsePushMode(string(c.PushMode)); !ok {
		return fmt.Errorf(
			"invalid [ir].push_mode: %q (expected \"sync\" or \"async\"); "+
				"set IR__PUSH_MODE", c.PushMode)
	}

	if c.PushEnabled {
		if strings.TrimSpace(c.SigningSecret) == "" {
			return errors.New(
				"push is enabled but [ir].signing_secret is empty — " +
					"set IR__SIGNING_SECRET or fill config/default.toml")
		}
		if secret := strings.TrimSpace(c.SigningSecret); secret == PlaceholderSecret || secret == ExampleSigningSecret {
			return fmt.Errorf(
				"push is enabled but [ir].signing_secret is still a public template placeholder — " +
					"forgeable HMAC auth; set IR__SIGNING_SECRET to the key issued by " +
					"the Intelligent Router platform")
		}
		if len(c.SigningSecret) < 16 || len(c.SigningSecret) > 512 {
			return errors.New("[ir].signing_secret must contain 16-512 bytes")
		}
		if !strings.HasPrefix(c.PushPath, "/") {
			return fmt.Errorf("[ir].push_path must start with '/', got %q", c.PushPath)
		}
	}

	if c.DefaultAgentSlug != "" && !validDefaultAgentSlug(c.DefaultAgentSlug) {
		return errors.New("[ir].default_agent_slug must be a lowercase Agent slug")
	}

	if strings.TrimSpace(c.HeartbeatURL) != "" {
		if err := validateAbsoluteHTTPURL(c.HeartbeatURL); err != nil {
			return fmt.Errorf("[ir].heartbeat_url must be an absolute HTTPS URL "+
				"(HTTP is allowed only for loopback development; set IR__HEARTBEAT_URL): %w", err)
		}
	}
	if strings.TrimSpace(c.CallbackToken) != "" {
		if strings.TrimSpace(c.CallbackTokenOrigin) == "" {
			return errors.New("[ir].callback_token_origin is required when callback_token is configured")
		}
		normalized, err := ValidateCallbackTokenOrigin(c.CallbackTokenOrigin)
		if err != nil {
			return fmt.Errorf("[ir].callback_token_origin is invalid: %w", err)
		}
		c.CallbackTokenOrigin = normalized
	}

	if c.PullEnabled {
		if strings.TrimSpace(c.QueueURL) == "" {
			return errors.New("pull is enabled but [ir].queue_url is empty — set IR__QUEUE_URL")
		}
		if strings.TrimSpace(c.BindToken) == "" {
			return errors.New("pull is enabled but [ir].bind_token is empty — set IR__BIND_TOKEN")
		}
		if strings.TrimSpace(c.BindToken) == ExampleBindToken {
			return errors.New("pull is enabled but [ir].bind_token is still a public template placeholder")
		}
		if strings.TrimSpace(c.WorkerID) == "" {
			return errors.New("pull is enabled but [ir].worker_id is empty — set IR__WORKER_ID")
		}
		if err := validateAbsoluteHTTPURL(c.QueueURL); err != nil {
			return fmt.Errorf("[ir].queue_url must be an absolute HTTPS URL "+
				"(HTTP is allowed only for loopback development; set IR__QUEUE_URL): %w", err)
		}
	}

	if c.ClaimTimeout < 35*time.Second {
		return fmt.Errorf(
			"claim_timeout_secs must be >= 35 (long-polling read timeout), got %d",
			int64(c.ClaimTimeout/time.Second))
	}
	if c.ClaimTimeout > maxIRDuration {
		return errors.New("claim_timeout_secs must not exceed 86400")
	}
	for _, item := range []struct {
		name     string
		duration time.Duration
	}{
		{"heartbeat_interval_secs", c.HeartbeatInterval},
		{"renew_interval_secs", c.RenewInterval},
		{"task_timeout_secs", c.TaskTimeout},
	} {
		if item.duration <= 0 {
			return fmt.Errorf("%s must be greater than 0", item.name)
		}
		if item.duration > maxIRDuration {
			return fmt.Errorf("%s must not exceed 86400", item.name)
		}
	}
	return nil
}

// validDefaultAgentSlug 只接受小写字母/数字/连字符,且不以连字符开头或结尾、
// 不含连续连字符。
func validDefaultAgentSlug(slug string) bool {
	if len(slug) < 2 || len(slug) > 64 {
		return false
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return false
	}
	for i := 0; i < len(slug); i++ {
		b := slug[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' {
			continue
		}
		return false
	}
	return true
}

// validateAbsoluteHTTPURL 要求带 host 的 HTTPS 绝对 URL。HTTP 只对回环
// 地址放行，便于本地集成测试，同时禁止 URL 内嵌凭据。
func validateAbsoluteHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("missing host")
	}
	if parsed.User != nil {
		return errors.New("must not contain embedded credentials")
	}
	if parsed.Scheme == "http" && !IsLoopbackHost(parsed.Hostname()) {
		return errors.New("must use HTTPS except for loopback development")
	}
	return nil
}

// readIRTable 读取 TOML 文件中的 [ir] 段;文件不存在返回 (nil, nil)。
func readIRTable(path string) (*irFile, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var document struct {
		IR *irFile `toml:"ir"`
	}
	if err := toml.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("parse %s as TOML: %w", path, err)
	}
	return document.IR, nil
}

// applyIREnv 用 IR__* 环境变量覆盖配置;空字符串视为未设置。
// 值无法解析时保持原值不动(与 Rust 侧一致,不报错)。
func applyIREnv(f *irFile) {
	envString("PUSH_PATH", &f.PushPath)
	envString("SIGNING_SECRET", &f.SigningSecret)
	envString("SIGNATURE_ENCODING", &f.SignatureEncoding)
	envString("QUEUE_URL", &f.QueueURL)
	envString("BIND_TOKEN", &f.BindToken)
	envString("WORKER_ID", &f.WorkerID)
	envString("PUSH_MODE", &f.PushMode)
	envString("HEARTBEAT_URL", &f.HeartbeatURL)
	envString("CALLBACK_TOKEN", &f.CallbackToken)
	envString("CALLBACK_TOKEN_ORIGIN", &f.CallbackTokenOrigin)
	envString("DEFAULT_AGENT_SLUG", &f.DefaultAgentSlug)

	envBoolPtr("PUSH_ENABLED", &f.PushEnabled)
	envBoolPtr("PULL_ENABLED", &f.PullEnabled)
	envBoolPtr("TRUSTED_USAGE_REPORTING_ENABLED", &f.TrustedUsageReportingEnabled)

	envUint("CLAIM_TIMEOUT_SECS", &f.ClaimTimeoutSecs)
	envUint("HEARTBEAT_INTERVAL_SECS", &f.HeartbeatIntervalSecs)
	envUint("RENEW_INTERVAL_SECS", &f.RenewIntervalSecs)
	envUint("TASK_TIMEOUT_SECS", &f.TaskTimeoutSecs)
}

// lookupIREnv 返回 IR__<name> 的值;未设置或仅含空白时返回 false。
func lookupIREnv(name string) (string, bool) {
	raw, ok := os.LookupEnv(irEnvPrefix + name)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return raw, true
}

func envString(name string, target **string) {
	if raw, ok := lookupIREnv(name); ok {
		value := raw
		*target = &value
	}
}

func envBoolPtr(name string, target **bool) {
	raw, ok := lookupIREnv(name)
	if !ok {
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

func envUint(name string, target **uint64) {
	raw, ok := lookupIREnv(name)
	if !ok {
		return
	}
	if value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64); err == nil {
		*target = &value
	}
}

func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func irSecondsOr(value *uint64, fallback uint64) time.Duration {
	seconds := fallback
	if value != nil {
		seconds = *value
	}
	if seconds > uint64(maxIRDuration/time.Second) {
		// Return a stable out-of-range sentinel instead of overflowing
		// time.Duration before Validate can reject it.
		return maxIRDuration + time.Second
	}
	return time.Duration(seconds) * time.Second
}

func secondsOr(value *uint64, fallback uint64) time.Duration {
	seconds := fallback
	if value != nil {
		seconds = *value
	}
	const maxDurationSeconds = uint64((1<<63 - 1) / int64(time.Second))
	if seconds > maxDurationSeconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(seconds) * time.Second
}
