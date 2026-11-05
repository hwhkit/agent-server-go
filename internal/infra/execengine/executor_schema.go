package execengine

// 岗位结构校验与 schema 白名单。
// 对应 Rust 侧 src/platform/executor/mod.rs 的 required_for_role /
// missing_role_fields / role_schema / value_as_f64。

import (
	"encoding/json"
	"fmt"
	"ir-agent-server/internal/domain/position"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 岗位必填主字段
// ---------------------------------------------------------------------------

// requiredForRole 返回岗位必填主字段(缺任一即判定模型输出结构不符,替换为保守结构)。
// requiredForRole 委托 domain/position 的单一数据源。
func requiredForRole(role string, isFinal bool) []string {
	return position.RequiredFieldsFor(role, isFinal)
}

// ---------------------------------------------------------------------------
// 数组项必填字段集
// ---------------------------------------------------------------------------

var (
	assessmentRequired     = []string{"symbol", "rating", "fair_value", "reasoning"}
	signalRequired         = []string{"symbol", "bias", "entry_zone", "reasoning"}
	strategySignalRequired = []string{"symbol", "direction", "reasoning"}
	checkRequired          = []string{"dimension", "status", "reasoning"}
	executionOrderRequired = []string{
		"market", "symbol", "direction", "order_type", "limit_price", "quantity",
		"take_profit_trigger_price", "take_profit_order_style", "take_profit_limit_price",
		"stop_loss_trigger_price", "stop_loss_order_style", "stop_loss_limit_price",
		"reasoning",
	}
	pmOrderRequired      = []string{"symbol", "market", "direction", "order_type", "quantity", "reasoning"}
	pmFinalOrderRequired = []string{
		"time", "ticker", "name", "market", "direction", "quantity", "order_type",
		"limit_price", "take_profit_trigger_price", "take_profit_order_style",
		"take_profit_limit_price", "stop_loss_trigger_price", "stop_loss_order_style",
		"stop_loss_limit_price", "time_in_force", "execution_instruction", "reasoning",
	}
)

// nullableItemFields 是契约允许为 null 的订单字段(MARKET 单没有限价)。
var nullableItemFields = map[string]bool{
	"limit_price":             true,
	"take_profit_limit_price": true,
	"stop_loss_limit_price":   true,
}

// primaryArrayForRole 返回岗位主数组的键名与每项必填字段。ok 为 false 表示该岗位
// 不做数组项校验。
func primaryArrayForRole(role string, isFinal bool) (arrayKey string, required []string, ok bool) {
	switch role {
	case "fundamental_analyst", "":
		return "assessments", assessmentRequired, true
	case "technical_analyst":
		return "signals", signalRequired, true
	case "strategist":
		return "signals", strategySignalRequired, true
	case "risk_manager":
		return "checks", checkRequired, true
	case "execution_trader":
		return "orders", executionOrderRequired, true
	case "portfolio_manager":
		if isFinal {
			return "orders", pmFinalOrderRequired, true
		}
		return "orders", pmOrderRequired, true
	default:
		return "", nil, false
	}
}

// ---------------------------------------------------------------------------
// missingRoleFields
// ---------------------------------------------------------------------------

// missingRoleFields 在进入宽容归一化前检查模型原始结果是否至少满足岗位主结构。
//
// 缺字段或数组项结构错误必须重试;契约允许为空的主数组保留为空,作为可解释失败结果。
// 返回值已排序去重,可直接拼进错误信息。
func missingRoleFields(candidate any, role string, isFinal bool) []string {
	payload := candidate
	if object, ok := asMapValue(candidate); ok {
		if nested, present := object["payload_result"]; present {
			payload = nested
		}
	}

	object, ok := asMapValue(payload)
	if !ok {
		// 结果根本不是对象:全部必填字段都算缺失。
		return slices.Clone(requiredForRole(role, isFinal))
	}

	role = strings.TrimSpace(role)
	var missing []string

	for _, key := range requiredForRole(role, isFinal) {
		value, present := object[key]
		switch {
		case !present || value == nil:
			missing = append(missing, key)
		case isBlankString(value):
			missing = append(missing, key)
		default:
			// 空数组只在风控 checks 上算缺失;其它岗位的空主数组是合法失败结果。
			if items, isArray := value.([]any); isArray && len(items) == 0 &&
				role == "risk_manager" && key == "checks" {
				missing = append(missing, key)
			}
		}
	}

	missing = append(missing, missingPrimaryItemFields(object, role, isFinal)...)
	missing = append(missing, missingTechnicalEntryZones(object, role)...)
	missing = append(missing, missingPortfolioProcessFields(object, role, isFinal)...)
	missing = append(missing, missingRiskHedging(object, role)...)

	sort.Strings(missing)
	return slices.Compact(missing)
}

// missingPrimaryItemFields 校验主数组每一项的必填字段。
func missingPrimaryItemFields(object map[string]any, role string, isFinal bool) []string {
	arrayKey, required, ok := primaryArrayForRole(role, isFinal)
	if !ok {
		return nil
	}
	items, isArray := object[arrayKey].([]any)
	if !isArray {
		return nil
	}

	var missing []string
	for index, rawItem := range items {
		item, isObject := asMapValue(rawItem)
		if !isObject {
			missing = append(missing, fmt.Sprintf("%s[%d](object)", arrayKey, index))
			continue
		}
		for _, field := range required {
			value, present := item[field]
			invalid := false
			switch {
			case !present || value == nil:
				// 契约允许为 null 的限价字段不算缺失。
				invalid = !nullableItemFields[field]
			case isBlankString(value):
				invalid = true
			}
			if invalid {
				missing = append(missing, fmt.Sprintf("%s[%d].%s", arrayKey, index, field))
			}
		}
	}
	return missing
}

// missingTechnicalEntryZones 校验技术分析师的 entry_zone 形状:
// 只能是空数组或恰好两个可解析为数字的元素。
func missingTechnicalEntryZones(object map[string]any, role string) []string {
	if role != "technical_analyst" {
		return nil
	}
	signals, ok := object["signals"].([]any)
	if !ok {
		return nil
	}

	var missing []string
	for index, rawSignal := range signals {
		signal, isObject := asMapValue(rawSignal)
		if !isObject {
			continue
		}
		entryZone, present := signal["entry_zone"]
		if !present {
			continue
		}
		values, isArray := entryZone.([]any)
		valid := isArray && (len(values) == 0 || len(values) == 2)
		if valid {
			for _, value := range values {
				if _, ok := valueAsFloat(value); !ok {
					valid = false
					break
				}
			}
		}
		if !valid {
			missing = append(missing, fmt.Sprintf("signals[%d].entry_zone(shape)", index))
		}
	}
	return missing
}

// missingPortfolioProcessFields 校验组合经理职能一(非 final)的额外约束:
// orders 与 target_allocation 至少一项非空,且每条配置项必须有三要素。
func missingPortfolioProcessFields(object map[string]any, role string, isFinal bool) []string {
	if role != "portfolio_manager" || isFinal {
		return nil
	}

	var missing []string
	orders, _ := object["orders"].([]any)
	allocations, hasAllocationArray := object["target_allocation"].([]any)
	if len(orders) == 0 && len(allocations) == 0 {
		missing = append(missing, "orders|target_allocation(non-empty)")
	}
	if hasAllocationArray {
		for index, rawItem := range allocations {
			item, _ := asMapValue(rawItem)
			for _, field := range []string{"symbol", "target_weight_pct", "reasoning"} {
				// 这里只看缺失/为 null;空白字符串在配置项上是允许的。
				if value, present := item[field]; !present || value == nil {
					missing = append(missing, fmt.Sprintf("target_allocation[%d].%s", index, field))
				}
			}
		}
	}
	return missing
}

// missingRiskHedging 要求风控 gate 为 WARN/BREACH 时必须给出对冲建议。
func missingRiskHedging(object map[string]any, role string) []string {
	if role != "risk_manager" {
		return nil
	}
	gate, _ := object["gate"].(string)
	if gate != "WARN" && gate != "BREACH" {
		return nil
	}
	suggestions, isArray := object["hedging_suggestions"].([]any)
	if !isArray || len(suggestions) == 0 {
		return []string{"hedging_suggestions(non-empty)"}
	}
	return nil
}

// isBlankString 判断值是否为「只含空白的字符串」。
func isBlankString(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

// ---------------------------------------------------------------------------
// value_as_f64
// ---------------------------------------------------------------------------

// valueAsFloat 把数字或数字样式的字符串解析为 float64。
//
// 字符串会先过滤成只剩数字、小数点与负号(模型常输出 "245.0 美元" 这类值),
// 再解析;结果必须是有限数。
func valueAsFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return 0, false
		}
		return typed, true
	case float32:
		return valueAsFloat(float64(typed))
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return 0, false
		}
		return parsed, true
	case string:
		var cleaned strings.Builder
		for _, r := range strings.TrimSpace(typed) {
			if (r >= '0' && r <= '9') || r == '.' || r == '-' {
				cleaned.WriteRune(r)
			}
		}
		parsed, err := strconv.ParseFloat(cleaned.String(), 64)
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// roleSchema:additionalProperties:false 白名单
// ---------------------------------------------------------------------------

// 数组项允许键(平台 schema 为 additionalProperties:false,多余键会被裁掉)。
var (
	assessmentKeys = []string{
		"symbol", "rating", "fair_value", "reasoning", "score", "conviction",
		"horizon", "upside_pct", "valuation", "key_drivers", "sector", "key_risks",
	}
	signalKeys = []string{
		"symbol", "trend_state", "bias", "entry_zone", "reasoning", "interval",
		"momentum_score", "confidence", "stop_loss", "targets", "key_levels",
		"pattern", "indicators", "volatility_regime", "relative_strength", "invalidation",
	}
	strategistSignalKeys = []string{
		"symbol", "direction", "reasoning", "target_weight_pct", "quantity", "price", "horizon",
	}
	checkKeys = []string{"dimension", "status", "reasoning", "value", "threshold", "detail"}
	orderKeys = []string{
		"market", "symbol", "direction", "order_type", "limit_price", "quantity",
		"take_profit_trigger_price", "take_profit_order_style", "take_profit_limit_price",
		"stop_loss_trigger_price", "stop_loss_order_style", "stop_loss_limit_price",
		"reasoning", "time_in_force", "execution_instruction", "algo_type", "algo_params",
	}
	// pmProcessOrderKeys 是 PM 职能一订单字段(8 字段)。
	pmProcessOrderKeys = []string{
		"symbol", "market", "direction", "order_type", "quantity", "reasoning",
		"limit_price", "weight_delta_pct",
	}
	// pmFinalOrderKeys 是 PM 职能二订单字段(19 字段)。
	pmFinalOrderKeys = []string{
		"time", "ticker", "name", "market", "direction", "quantity", "order_type",
		"limit_price", "take_profit_trigger_price", "take_profit_order_style",
		"take_profit_limit_price", "stop_loss_trigger_price", "stop_loss_order_style",
		"stop_loss_limit_price", "time_in_force", "execution_instruction", "reasoning",
		"algo_type", "algo_params",
	}
)

// roleSchemaSpec 是一个岗位的裁剪规则。
type roleSchemaSpec struct {
	// resultKeys 是 payload_result 允许的顶层键。
	resultKeys []string
	// itemField 是主数组的字段名。
	itemField string
	// itemKeys 是数组每项允许的键。
	itemKeys []string
}

// roleSchema 返回岗位的裁剪规则。ok 为 false 表示未收录该岗位:保持原样,
// 只做 evidenceRefs / extensions 规范化。
func roleSchema(role string, isFinal bool) (roleSchemaSpec, bool) {
	// 空 = 默认基本面,与 resolveRoleContract 的空值默认保持一致,
	// 否则占位/未配置 system_prompt 的 Agent 会跳过裁剪与归一化。
	role = strings.TrimSpace(role)
	if role == "" {
		role = "fundamental_analyst"
	}

	switch role {
	case "fundamental_analyst":
		return roleSchemaSpec{
			resultKeys: []string{"assessments", "evidenceRefs", "summary", "extensions"},
			itemField:  "assessments",
			itemKeys:   assessmentKeys,
		}, true
	case "technical_analyst":
		return roleSchemaSpec{
			resultKeys: []string{"signals", "summary", "evidenceRefs", "extensions"},
			itemField:  "signals",
			itemKeys:   signalKeys,
		}, true
	case "strategist":
		return roleSchemaSpec{
			resultKeys: []string{
				"signals", "rationale", "strategy_code", "market_regime", "views",
				"backtest", "key_risks", "evidenceRefs", "extensions",
			},
			itemField: "signals",
			itemKeys:  strategistSignalKeys,
		}, true
	case "risk_manager":
		return roleSchemaSpec{
			resultKeys: []string{
				"gate", "checks", "hedging_suggestions", "exposures", "gating_actions",
				"interpretation", "evidenceRefs", "extensions",
			},
			itemField: "checks",
			itemKeys:  checkKeys,
		}, true
	case "execution_trader":
		return roleSchemaSpec{
			resultKeys: []string{
				"orders", "timestamp", "execution_summary", "evidenceRefs", "extensions",
			},
			itemField: "orders",
			itemKeys:  orderKeys,
		}, true
	case "portfolio_manager":
		if isFinal {
			return roleSchemaSpec{
				resultKeys: []string{
					"timestamp", "orders", "report_markdown", "evidenceRefs", "extensions",
				},
				itemField: "orders",
				itemKeys:  pmFinalOrderKeys,
			}, true
		}
		return roleSchemaSpec{
			resultKeys: []string{
				"decision_type", "orders", "target_allocation", "rationale",
				"constraints_applied", "risk_handoff", "evidenceRefs", "extensions",
			},
			itemField: "orders",
			itemKeys:  pmProcessOrderKeys,
		}, true
	default:
		return roleSchemaSpec{}, false
	}
}

// retainKeys 就地裁掉 object 中不在 allowed 里的键(additionalProperties:false)。
func retainKeys(object map[string]any, allowed []string) {
	for key := range object {
		if !slices.Contains(allowed, key) {
			delete(object, key)
		}
	}
}
