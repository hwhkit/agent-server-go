package exec

import "errors"

// 执行器种类常量（值与存量数据库/线协议逐字一致）。
const (
	// KindRust 是进程内 Processor（Rust 版名称保留，Go 版即内置 Processor）。
	KindRust            Kind = "rust"
	KindClaudeCode      Kind = "claude_code"
	KindCodex           Kind = "codex"
	KindReasonix        Kind = "reasonix"
	KindPi              Kind = "pi"
	KindOpenCode        Kind = "opencode"
	KindQwenCode        Kind = "qwen_code"
	KindGeminiCLI       Kind = "gemini_cli"
	KindOpenHands       Kind = "openhands"
	KindDeepseekHarness Kind = "deepseek_harness"
	KindGoose           Kind = "goose"
	KindOpenAIAgent     Kind = "openai_agent"
	KindClaudeManaged   Kind = "claude_managed_agent"
)

// ParseKind 严格解析执行器种类（不做大小写折叠），与旧栈 ParseExecutorKind 一致。
func ParseKind(value string) (Kind, error) {
	switch Kind(value) {
	case KindRust, KindClaudeCode, KindCodex, KindReasonix,
		KindPi, KindOpenCode, KindQwenCode, KindGeminiCLI,
		KindOpenHands, KindDeepseekHarness, KindGoose,
		KindOpenAIAgent, KindClaudeManaged:
		return Kind(value), nil
	default:
		return "", errors.New("unsupported executor kind")
	}
}

// String 返回种类名。
func (k Kind) String() string { return string(k) }

// SupportsLocalCLI 表示该执行器是否支持本机 CLI 进程调用。
func (k Kind) SupportsLocalCLI() bool {
	switch k {
	case KindClaudeCode, KindCodex, KindReasonix, KindPi,
		KindOpenCode, KindQwenCode, KindGeminiCLI,
		KindOpenHands, KindDeepseekHarness:
		return true
	default:
		return false
	}
}

// SupportsGateway 表示该执行器是否可经 litellm 网关直调。
func (k Kind) SupportsGateway() bool {
	switch k {
	case KindRust, KindOpenCode, KindQwenCode,
		KindGeminiCLI, KindOpenHands, KindOpenAIAgent,
		KindClaudeManaged:
		return false
	default:
		return true
	}
}

// SupportsExecutionMode 判断执行器与执行方式的组合是否可用。
func (k Kind) SupportsExecutionMode(mode Mode) bool {
	switch mode {
	case ModeGateway:
		return k == KindRust || k.SupportsGateway()
	case ModeLocal:
		return k == KindRust || k.SupportsLocalCLI()
	case ModeManaged:
		return k == KindOpenAIAgent || k == KindClaudeManaged
	case ModeRemotePull:
		// kind 仅描述远端连接器名义上运行的执行器，平台不本机执行，
		// 因此不做 kind 组合限制。
		return true
	default:
		return false
	}
}

// ParseMode 严格解析执行模式，与旧栈 ParseExecutionMode 一致。
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case ModeGateway, ModeLocal, ModeManaged, ModeRemotePull:
		return Mode(value), nil
	default:
		return "", errors.New("execution_mode must be gateway, local, managed, or remote_pull")
	}
}
