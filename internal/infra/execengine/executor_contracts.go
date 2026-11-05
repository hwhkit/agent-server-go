package execengine

// 岗位输出契约的查表、解析与提示词组装。
// 对应 Rust 侧 src/platform/executor/mod.rs 中的 ROLE_CONTRACTS /
// resolve_role_contract / build_prompt / output_language_instruction。
//
// 契约正文已迁至 internal/domain/position（golden 快照随迁），本文件只保留
// 查表与提示词组装逻辑，P4 随应用层一并迁移。

import (
	"fmt"
	"strings"

	"ir-agent-server/internal/domain/position"
)

// roleContract 是一个预置岗位:标识、中文名与契约正文。
//
// AgentConfig.SystemPrompt 存的是岗位标识(id),执行时映射到对应契约 prompt;
// 非预置标识时视为自定义提示词(向后兼容已有 Agent)。
type roleContract struct {
	id     string
	label  string
	prompt string
}

// roleContracts 是预置岗位表(与 ir-creator 评测 schema 严格对齐)。
// 顺序有意义:空 system_prompt 会回退到第一项。
var roleContracts = []roleContract{
	{id: "fundamental_analyst", label: "基本面分析师", prompt: position.FundamentalAnalystContract},
	{id: "technical_analyst", label: "技术分析师", prompt: position.TechnicalAnalystContract},
	{id: "strategist", label: "策略分析师", prompt: position.StrategistContract},
	{id: "risk_manager", label: "风控经理", prompt: position.RiskManagerContract},
	{id: "execution_trader", label: "执行交易员", prompt: position.ExecutionTraderContract},
	{id: "portfolio_manager", label: "组合经理", prompt: position.PortfolioManagerContract},
}

// customRoleLabel 是自定义提示词在提示词模板里显示的岗位名。
const customRoleLabel = "自定义"

// resolveRoleContract 把 system_prompt 解析为(岗位中文名, 契约 prompt):
// 命中预置岗位标识 → 对应契约;空 → 默认基本面;其余 → 自定义文本(向后兼容)。
func resolveRoleContract(systemPrompt string) (label, contract string) {
	trimmed := strings.TrimSpace(systemPrompt)
	for _, role := range roleContracts {
		if trimmed == role.id {
			return role.label, role.prompt
		}
	}
	if trimmed == "" {
		return roleContracts[0].label, roleContracts[0].prompt
	}
	// 注意这里返回未 trim 的原文,与 Rust 侧一致。
	return customRoleLabel, systemPrompt
}

// RoleDescriptor 已归位 domain/position（Descriptor）；迁移期保留别名。
type RoleDescriptor = position.Descriptor

var (
	RoleDescriptors       = position.Descriptors
	ResolveRoleDescriptor = position.ResolveDescriptor
)

// buildPrompt 组装交给模型的完整提示词:岗位契约 + 任务输入 + 输出约束 + 语言硬约束。
func buildPrompt(runtime *AgentConfig, rawInput any) string {
	input := jsonString(rawInput)
	roleLabel, contract := resolveRoleContract(runtime.SystemPrompt)
	languageInstruction := outputLanguageInstruction(rawInput)

	return fmt.Sprintf(
		"执行 Agent `%s` 收到的任务。请完成任务并按下面的输出契约返回最终结果。\n\n"+
			"输出契约(%s):\n%s\n\n"+
			"任务输入(JSON):\n%s\n\n"+
			"输出约束:如提供顶层 payload_info,它必须是 50 字以内的纯文本摘要,严禁把完整 JSON 再次序列化后放入其中;整个输出控制在 2000 token 内,避免截断。\n"+
			"输出语言硬约束:%s\n"+
			"投资范围(globalParameters.universeMode): RESTRICTED(缺省)只评估 symbolPool 内的标的;"+
			"SEED 时池子只是起点,可在 targetMarkets 允许的市场内自主发现池外机会并给出评估。",
		runtime.Slug, roleLabel, contract, input, languageInstruction,
	)
}

// defaultOutputLanguage 是 globalParameters.language 缺省时的输出语言。
const defaultOutputLanguage = "en-US"

// outputLanguageInstruction 生成输出语言硬约束。
//
// 按 globalParameters.language 的前缀分三档:zh* 要求全简体中文,en* 要求全英文,
// 其余要求「统一使用所请求的语言」。三者都会回显实际生效的 language 值。
func outputLanguageInstruction(rawInput any) string {
	language := defaultOutputLanguage
	if object, ok := asMapValue(rawInput); ok {
		if global, ok := asMapValue(object["globalParameters"]); ok {
			if raw, ok := global["language"].(string); ok {
				if trimmed := strings.TrimSpace(raw); trimmed != "" {
					language = trimmed
				}
			}
		}
	}

	lower := strings.ToLower(language)
	switch {
	case strings.HasPrefix(lower, "zh"):
		return fmt.Sprintf("globalParameters.language=%s;payload_info、summary、reasoning、rationale、report_markdown、interpretation、execution_summary、detail、风险说明和建议等所有人类可读文本必须统一使用简体中文,不得混用中英文句子,不得原样复制英文 taskBrief、irPlatformRules、Skill 或工具说明。JSON 键名、契约枚举、Ticker、公司专名及必要金融缩写(如 SEC/OHLCV/RSI/ATR/SMA)保持原格式。", language)
	case strings.HasPrefix(lower, "en"):
		return fmt.Sprintf("globalParameters.language=%s;all human-readable text in payload_info, summary, reasoning, rationale, report_markdown, interpretation, execution_summary, detail, risks, and suggestions must be English only. Do not mix Chinese and English sentences or copy Chinese task instructions, Skills, or tool guidance verbatim. Keep JSON keys, contract enums, tickers, company names, and necessary financial abbreviations in their required form.", language)
	default:
		return fmt.Sprintf("globalParameters.language=%s;all human-readable output fields must use the requested language consistently. Do not mix languages or copy task, Skill, or tool instructions written in another language. Keep JSON keys, contract enums, tickers, company names, and required financial abbreviations unchanged.", language)
	}
}
