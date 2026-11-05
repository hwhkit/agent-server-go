package position

import "strings"

// ---------------------------------------------------------------------------
// 岗位公开描述符（A2A Agent Card skills、输出 schema 校验共用）
// ---------------------------------------------------------------------------

// Descriptor 是预置岗位的公开元数据。只暴露对外协议（A2A Agent Card）与
// 校验需要的信息，不暴露契约 prompt 正文。
type Descriptor struct {
	// ID 是岗位标识，即智能体 SystemPrompt 命中预置岗位时存的值。
	ID string
	// Label 是岗位中文名。
	Label string
	// RequiredFields 是岗位输出契约的必填主字段（非 final 形态）。
	RequiredFields []string
}

// builtinRoles 是预置岗位标识与中文名（顺序有意义：空 system_prompt 回退
// 到第一项，与契约解析语义一致）。
var builtinRoles = []struct{ id, label string }{
	{"fundamental_analyst", "基本面分析师"},
	{"technical_analyst", "技术分析师"},
	{"strategist", "策略分析师"},
	{"risk_manager", "风控经理"},
	{"execution_trader", "执行交易员"},
	{"portfolio_manager", "组合经理"},
}

// Descriptors 返回全部预置岗位的公开元数据。
func Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(builtinRoles))
	for _, role := range builtinRoles {
		out = append(out, Descriptor{
			ID: role.id, Label: role.label,
			RequiredFields: RequiredFieldsFor(role.id, false),
		})
	}
	return out
}

// ResolveDescriptor 把 system_prompt 解析为预置岗位描述，语义与契约解析
// 一致：命中预置标识 → 该岗位；空 → 默认基本面；其余（自定义提示词）→
// 第二个返回值为 false。
func ResolveDescriptor(systemPrompt string) (Descriptor, bool) {
	trimmed := strings.TrimSpace(systemPrompt)
	if trimmed == "" {
		first := builtinRoles[0]
		return Descriptor{ID: first.id, Label: first.label,
			RequiredFields: RequiredFieldsFor(first.id, false)}, true
	}
	for _, role := range builtinRoles {
		if trimmed == role.id {
			return Descriptor{ID: role.id, Label: role.label,
				RequiredFields: RequiredFieldsFor(role.id, false)}, true
		}
	}
	return Descriptor{}, false
}

// RequiredFieldsFor 返回岗位输出契约的必填主字段（输出 schema 校验与
// A2A skills 描述共用的唯一数据源）。isFinal 仅对组合经理有意义
// （职能二评测形态）。
func RequiredFieldsFor(roleID string, isFinal bool) []string {
	switch strings.TrimSpace(roleID) {
	case "risk_manager":
		return []string{"gate", "checks"}
	case "execution_trader":
		return []string{"orders"}
	case "portfolio_manager":
		if isFinal {
			// 职能二（平台评测）：必填为 timestamp / orders / report_markdown。
			return []string{"timestamp", "orders", "report_markdown"}
		}
		return []string{"decision_type", "rationale"}
	case "strategist":
		return []string{"signals", "rationale"}
	case "technical_analyst":
		return []string{"signals"}
	default:
		return []string{"assessments"}
	}
}
