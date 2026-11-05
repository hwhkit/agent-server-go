package platform

// taskBrief 文本提取（自旧栈 processor.go 迁入，语义逐字保留）：
// 目标条数、失败路径关键词与失败原因的识别，供执行引擎的保守缺省
// 输出（FAILURE_DEFAULT 口径）与提示词组装使用。

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

// briefText 汇总 taskBrief 中用于关键词/数量识别的文本。
// 对象形态只读 userCustomInstructions 与 upstreamRequirements;字符串形态直接用。
func BriefText(taskBrief any) string {
	var builder strings.Builder
	if object, ok := AsMapValue(taskBrief); ok {
		for _, key := range []string{"userCustomInstructions", "upstreamRequirements"} {
			if value, ok := object[key].(string); ok {
				builder.WriteString(value)
			}
		}
	} else if text, ok := taskBrief.(string); ok {
		builder.WriteString(text)
	}
	return builder.String()
}

// hanNumerals 是中文数字到数值的映射(一 … 十)。
var hanNumerals = []struct {
	glyph rune
	value int
}{
	{'一', 1}, {'二', 2}, {'两', 2}, {'三', 3}, {'四', 4},
	{'五', 5}, {'六', 6}, {'七', 7}, {'八', 8}, {'九', 9}, {'十', 10},
}

// ExtractTargetCount 从 taskBrief 文本解析目标标的数量(如「精选 5 只」「恰好 5 条」
// 「覆盖全部 5 只」)。用于评测断言 EXACT_ITEMS 对齐:全覆盖用例数量 = pool 大小,
// 精选用例数量 < pool 大小。无法识别时返回 (0, false)。
func ExtractTargetCount(taskBrief any) (int, bool) {
	text := BriefText(taskBrief)
	if text == "" {
		return 0, false
	}
	runes := []rune(text)
	for i := 0; i < len(runes); {
		if runes[i] >= '0' && runes[i] <= '9' {
			start := i
			for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
				i++
			}
			number, err := parseDecimalRunes(runes[start:i])
			if err != nil {
				// Rust 侧解析失败会直接放弃整段扫描。
				return 0, false
			}
			if value, ok := afterCountUnit(runes, i, number); ok {
				return value, true
			}
			i++
			continue
		}
		if number, ok := hanNumeralAt(runes[i]); ok {
			if value, ok := afterCountUnit(runes, i+1, number); ok {
				return value, true
			}
		}
		i++
	}
	return 0, false
}

func hanNumeralAt(glyph rune) (int, bool) {
	for _, item := range hanNumerals {
		if item.glyph == glyph {
			return item.value, true
		}
	}
	return 0, false
}

func parseDecimalRunes(runes []rune) (int, error) {
	value := 0
	for _, r := range runes {
		digit := int(r - '0')
		if value > (1<<31-1-digit)/10 {
			return 0, fmt.Errorf("numeric overflow")
		}
		value = value*10 + digit
	}
	return value, nil
}

// afterCountUnit 判断数字后是否紧跟计量单位(只/条/个/项/维),是则返回该数量。
// 只接受 1..=100。
func afterCountUnit(runes []rune, index, number int) (int, bool) {
	j := index
	for j < len(runes) && unicode.IsSpace(runes[j]) {
		j++
	}
	if j >= len(runes) {
		return 0, false
	}
	switch runes[j] {
	case '只', '条', '个', '项', '维':
	default:
		return 0, false
	}
	if number < 1 || number > 100 {
		return 0, false
	}
	// 「六维」/「6 个维度」等情况:单位后可再跟 度/数/目。
	k := j + 1
	for k < len(runes) && unicode.IsSpace(runes[k]) {
		k++
	}
	if k < len(runes) && (runes[k] == '度' || runes[k] == '数' || runes[k] == '目') {
		return number, true
	}
	return number, true
}

// FailurePathKeywords 是「任务明确要求保守缺省」的关键词。
var FailurePathKeywords = []string{
	"不足以支撑",
	"无法完成",
	"保守缺省",
	"无法落地",
	"无法产出",
	"无法执行",
}

// ExtractFailurePath 检测任务简报是否为失败路径(平台要求给保守缺省结构)。
func ExtractFailurePath(taskBrief any) bool {
	_, ok := ExtractFailureReason(taskBrief)
	return ok
}

// ExtractFailureReason 返回失败路径的安全、可展示原因。
// 只回显命中的规则,不回显完整用户输入。
func ExtractFailureReason(taskBrief any) (string, bool) {
	if taskBrief == nil {
		return "", false
	}
	text := BriefText(taskBrief)
	for _, keyword := range FailurePathKeywords {
		if strings.Contains(text, keyword) {
			return fmt.Sprintf("任务简报命中失败路径规则“%s”，按要求不生成主动决策", keyword), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// globalParameters / upstream 提取

// IsLoopbackHost 判断主机名是否为回环地址。
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// globalParameters / upstream 提取（自旧栈 processor.go 迁入）
// ---------------------------------------------------------------------------

// 支持两种上游结构:
//  1. upstreamPayload: [{agentInstanceId, payloadResult: {orders: [...]}}]
//  2. upstream(DAG): {edges, nodes: [{agentInstanceId, output: {orders: [...]}}], self}
//
// 只从上游的 orders 提取标的(不收集 target_allocation):
// PRESERVE_UPSTREAM_ORDER_INTENT 只校验 orders;target_allocation 里的 CASH 等是
// 现金/配置占位,不是可成交订单,收集后补全会越出标的池。
func ExtractUpstreamSymbols(upstream any, positionType *string) []string {
	_ = positionType

	var symbols []string
	seen := map[string]bool{}
	collectOrders := func(object map[string]any) {
		orders, ok := object["orders"].([]any)
		if !ok {
			return
		}
		for _, order := range orders {
			entry, ok := AsMapValue(order)
			if !ok {
				continue
			}
			raw, ok := entry["symbol"].(string)
			if !ok {
				continue
			}
			if symbol := TrimUpper(raw); symbol != "" && !seen[symbol] {
				seen[symbol] = true
				symbols = append(symbols, symbol)
			}
		}
	}

	// 先尝试 upstreamPayload 数组形态。
	if items, ok := upstream.([]any); ok {
		for _, item := range items {
			entry, ok := AsMapValue(item)
			if !ok {
				continue
			}
			if result, ok := AsMapValue(entry["payloadResult"]); ok {
				collectOrders(result)
			}
		}
	}

	// 再尝试 upstream DAG 结构 (nodes[].output)。
	if len(symbols) == 0 {
		if object, ok := AsMapValue(upstream); ok {
			if nodes, ok := object["nodes"].([]any); ok {
				for _, node := range nodes {
					entry, ok := AsMapValue(node)
					if !ok {
						continue
					}
					if output, ok := AsMapValue(entry["output"]); ok {
						collectOrders(output)
					}
				}
			}
		}
	}
	return symbols
}

// ExtractUniverseMode 从 globalParameters.universeMode 提取投资范围模式;
// 缺失或非法返回 nil(按 RESTRICTED 处理)。
func ExtractUniverseMode(global any) *string {
	object, ok := AsMapValue(global)
	if !ok {
		return nil
	}
	raw, ok := object["universeMode"].(string)
	if !ok {
		return nil
	}
	mode := TrimUpper(raw)
	if mode == "RESTRICTED" || mode == "SEED" {
		return &mode
	}
	return nil
}

// ExtractSymbolPool 从 globalParameters.symbolPool 提取标的池;缺失/空返回 nil。
func ExtractSymbolPool(global any) []string {
	object, ok := AsMapValue(global)
	if !ok {
		return nil
	}
	return AsStringSlice(object["symbolPool"])
}
