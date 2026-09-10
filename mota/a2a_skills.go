package mota

// 岗位契约 -> A2A skill 的静态映射。对应 rs 侧 src/a2a/skills.rs。
//
// agents 表目前没有 skills / capabilities 列,能描述能力的只有
// AgentConfig.SystemPrompt(命中预置岗位时存的是岗位标识)。这里把
// roleContracts 的六个岗位映射成规范的 A2AAgentSkill。
//
// 注意:v1.0 起 AgentSkill.tags 是必填且不能为空数组——skill 级发现依赖
// tags 做过滤,所以每个岗位都必须给出可检索的关键词。

import "strings"

// A2AInputModes 是所有 skill 都接受的输入类型:结构化 JSON 与纯文本。
var A2AInputModes = []string{"application/json", "text/plain"}

// A2AOutputModes 是输出类型:既有结构化 payload_result,也有人读摘要。
var A2AOutputModes = []string{"application/json", "text/plain"}

// a2aSkillProfile 是岗位的对外展示信息。中文岗位名来自 RoleDescriptor.Label,
// 英文名与 tags 在这里补齐(Agent Card 面向的是跨组织调用方)。
type a2aSkillProfile struct {
	roleID   string
	nameEn   string
	summary  string
	tags     []string
	examples []string
}

var a2aSkillProfiles = []a2aSkillProfile{
	{
		roleID:  "fundamental_analyst",
		nameEn:  "Fundamental Analysis",
		summary: "Rates listed equities on fundamentals and returns a per-symbol assessment with a fair value estimate and reasoning.",
		tags: []string{
			"finance", "equity-research", "fundamental-analysis", "valuation", "stock-rating",
		},
		examples: []string{
			"Assess AAPL and MSFT on fundamentals and give a fair value for each.",
			"Rate the securities in the provided symbol pool using the latest financial statements.",
		},
	},
	{
		roleID:  "technical_analyst",
		nameEn:  "Technical Analysis",
		summary: "Derives directional bias and entry zones for listed equities from price and volume structure.",
		tags: []string{
			"finance", "technical-analysis", "price-action", "trading-signals", "entry-zone",
		},
		examples: []string{
			"Give me the technical bias and entry zone for NVDA.",
			"Screen the symbol pool for breakout setups confirmed by volume.",
		},
	},
	{
		roleID:  "strategist",
		nameEn:  "Strategy Research",
		summary: "Turns a market view into directional signals with an explicit rationale for the overall strategy.",
		tags: []string{
			"finance", "investment-strategy", "trading-signals", "market-view", "research",
		},
		examples: []string{
			"Build a mean-reversion signal set for liquid US large caps.",
			"Which symbols in the pool fit a momentum rotation this quarter, and why?",
		},
	},
	{
		roleID:  "risk_manager",
		nameEn:  "Risk Review",
		summary: "Reviews a proposed trade or portfolio against risk dimensions and returns a pass/warn/block gate with per-dimension checks.",
		tags: []string{
			"finance", "risk-management", "compliance-gate", "portfolio-risk", "pre-trade-check",
		},
		examples: []string{
			"Run a risk gate over these proposed orders and flag any concentration breach.",
			"Check the upstream allocation against drawdown and liquidity limits.",
		},
	},
	{
		roleID:  "execution_trader",
		nameEn:  "Order Construction",
		summary: "Converts upstream trade intent into fully specified orders with limit price, sizing, take-profit and stop-loss levels.",
		tags: []string{
			"finance", "trade-execution", "order-generation", "order-routing", "position-sizing",
		},
		examples: []string{
			"Turn the upstream buy intent on TSLA into a limit order with stop loss and take profit.",
			"Produce executable orders for the approved allocation.",
		},
	},
	{
		roleID:  "portfolio_manager",
		nameEn:  "Portfolio Decision",
		summary: "Makes the portfolio-level call: a decision type with rationale, plus orders or a target allocation, and a final report when requested.",
		tags: []string{
			"finance", "portfolio-management", "asset-allocation", "investment-decision", "rebalancing",
		},
		examples: []string{
			"Decide whether to rebalance given the analyst and risk inputs, and explain the call.",
			"Produce the final portfolio report with the resulting orders.",
		},
	},
}

// a2aCustomSkillID 是自定义 system_prompt(非预置岗位)时的兜底 skill。
const a2aCustomSkillID = "custom"

var a2aCustomTags = []string{"finance", "custom-agent", "structured-analysis"}

// A2ASkillsFor 为一个 Agent 生成它的 skill 列表。
//
// 当前是一个 Agent 一个 skill:AgentConfig.SystemPrompt 决定它承担哪个岗位。
// description 优先使用管理端为该 Agent 填写的说明,为空时回落到岗位摘要。
func A2ASkillsFor(systemPrompt, agentDescription string) []A2AAgentSkill {
	descriptor, ok := ResolveRoleDescriptor(systemPrompt)
	if ok {
		return []A2AAgentSkill{a2aPresetSkill(descriptor, agentDescription)}
	}
	return []A2AAgentSkill{a2aCustomSkill(agentDescription)}
}

func a2aPresetSkill(descriptor RoleDescriptor, agentDescription string) A2AAgentSkill {
	var profile *a2aSkillProfile
	for i := range a2aSkillProfiles {
		if a2aSkillProfiles[i].roleID == descriptor.ID {
			profile = &a2aSkillProfiles[i]
			break
		}
	}
	var nameEn, summary string
	var tags, examples []string
	if profile != nil {
		nameEn, summary, tags, examples = profile.nameEn, profile.summary, profile.tags, profile.examples
	} else {
		// 新增岗位但忘了补 profile 时不至于产出非法 card(tags 必填非空)。
		nameEn, summary, tags, examples = descriptor.ID, "", a2aCustomTags, nil
	}
	return A2AAgentSkill{
		ID:          descriptor.ID,
		Name:        nameEn + " (" + descriptor.Label + ")",
		Description: a2aComposeDescription(agentDescription, summary, descriptor.RequiredFields),
		Tags:        append([]string{}, tags...),
		Examples:    append([]string{}, examples...),
		InputModes:  append([]string{}, A2AInputModes...),
		OutputModes: append([]string{}, A2AOutputModes...),
	}
}

func a2aCustomSkill(agentDescription string) A2AAgentSkill {
	return A2AAgentSkill{
		ID:   a2aCustomSkillID,
		Name: "Custom Analysis (自定义)",
		Description: a2aComposeDescription(agentDescription,
			"Runs this agent's configured prompt and returns a structured result envelope.",
			[]string{"assessments"}),
		Tags:        append([]string{}, a2aCustomTags...),
		InputModes:  append([]string{}, A2AInputModes...),
		OutputModes: append([]string{}, A2AOutputModes...),
	}
}

// a2aComposeDescription 组装 skill 描述 = Agent 自述(有则优先) + 岗位摘要 +
// 输出契约必填字段。把必填字段写进描述,是为了让调用方知道结构化输出长
// 什么样,因为 AgentSkill 没有 schema 字段可用。
func a2aComposeDescription(agentDescription, summary string, requiredFields []string) string {
	var parts []string
	agentDescription = strings.TrimSpace(agentDescription)
	if agentDescription != "" {
		parts = append(parts, agentDescription)
	}
	if summary != "" && summary != agentDescription {
		parts = append(parts, summary)
	}
	if len(requiredFields) > 0 {
		parts = append(parts, "Structured output is returned as a JSON data part whose payloadResult always contains: "+
			strings.Join(requiredFields, ", ")+".")
	}
	if len(parts) == 0 {
		// AgentSkill.Description 是必填,不能留空。
		return "Structured financial analysis agent."
	}
	return strings.Join(parts, " ")
}
