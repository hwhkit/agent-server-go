package execengine

// 按岗位归一数组项字段枚举与类型(与平台岗位契约严格对齐)。
// 模型常输出口语/中文/非枚举值,平台 schema 要求大写枚举与特定类型。
//
// 对应 Rust 侧 src/platform/executor/mod.rs 的 normalize_role_fields /
// normalize_top_level_fields / normalize_symbol / normalize_symbol_items。

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 数组与标量归一化辅助
// ---------------------------------------------------------------------------

// normalizeNumberArray 把数组里能解析为数字的元素收集成新数组,丢弃其余项。
func normalizeNumberArray(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	normalized := make([]any, 0, len(items))
	for _, item := range items {
		if number, ok := valueAsFloat(item); ok {
			normalized = append(normalized, number)
		}
	}
	return normalized
}

// normalizeStringArray 保留数组里非空的字符串项(去首尾空白)。
func normalizeStringArray(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	normalized := make([]any, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			normalized = append(normalized, text)
		}
	}
	return normalized
}

// normalizeHorizon 把投资期限归一到四档枚举;无法识别时默认 MEDIUM。
func normalizeHorizon(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "INTRADAY", "DAY", "日内":
		return "INTRADAY"
	case "SHORT", "SHORT_TERM", "短期":
		return "SHORT"
	case "LONG", "LONG_TERM", "长期":
		return "LONG"
	default:
		return "MEDIUM"
	}
}

// stringField 读取对象里的字符串字段。
func stringField(object map[string]any, key string) (string, bool) {
	value, ok := object[key].(string)
	return value, ok
}

// enumOrDefault 把 value 归一为大写;不在 allowed 内时返回 fallback。
func enumOrDefault(value string, allowed []string, fallback string) string {
	upper := strings.ToUpper(strings.TrimSpace(value))
	if slices.Contains(allowed, upper) {
		return upper
	}
	return fallback
}

// normalizeDirection 把方向归一为 BUY/SELL。以 SELL 开头或含「卖」判为卖出,
// 其余一律买入(模型常输出「减持」「Sell short」等变体)。
func normalizeDirection(value string) string {
	upper := strings.ToUpper(strings.TrimSpace(value))
	if strings.HasPrefix(upper, "SELL") || strings.Contains(upper, "卖") {
		return "SELL"
	}
	return "BUY"
}

// ---------------------------------------------------------------------------
// normalizeRoleFields:数组项级归一化
// ---------------------------------------------------------------------------

// riskDimensions 是风控检查维度的合法枚举。
var riskDimensions = []string{
	"SINGLE_NAME", "SECTOR_EXPOSURE", "FACTOR_EXPOSURE", "VAR", "VOLATILITY",
	"STYLE_FACTOR", "DRAWDOWN", "STRESS_TEST", "LIQUIDITY", "CONCENTRATION",
	"STOP_LOSS", "TAKE_PROFIT",
}

// coercedNumberFields 是允许「数字样式字符串 → 数字」的字段。
var coercedNumberFields = []string{
	"momentum_score", "confidence", "stop_loss", "target_weight_pct", "quantity",
	"price", "value", "threshold", "score", "upside_pct", "conviction", "fair_value",
	"limit_price", "weight_delta_pct", "current_weight_pct",
	"take_profit_trigger_price", "take_profit_limit_price",
	"stop_loss_trigger_price", "stop_loss_limit_price",
}

// normalizeRoleFields 就地归一化主数组里的一项。
func normalizeRoleFields(object map[string]any, role string) {
	switch role {
	case "fundamental_analyst":
		normalizeFundamentalItem(object)
	case "technical_analyst":
		normalizeTechnicalItem(object)
	case "strategist":
		normalizeStrategistItem(object)
	case "risk_manager":
		normalizeRiskItem(object)
	case "execution_trader", "portfolio_manager":
		normalizeOrderItem(object)
	}
	normalizeCommonNumbers(object)
}

func normalizeFundamentalItem(object map[string]any) {
	if horizon, ok := stringField(object, "horizon"); ok {
		object["horizon"] = normalizeHorizon(horizon)
	}
	for _, key := range []string{"key_drivers", "key_risks"} {
		if _, present := object[key]; present {
			object[key] = normalizeStringArray(object[key])
		}
	}
}

func normalizeTechnicalItem(object map[string]any) {
	if bias, ok := stringField(object, "bias"); ok {
		switch strings.ToUpper(strings.TrimSpace(bias)) {
		case "LONG_BIAS", "BULLISH", "看多", "偏多":
			object["bias"] = "LONG_BIAS"
		case "SHORT_BIAS", "BEARISH", "看空", "偏空":
			object["bias"] = "SHORT_BIAS"
		default:
			object["bias"] = "NEUTRAL"
		}
	}
	if trend, ok := stringField(object, "trend_state"); ok {
		switch strings.ToUpper(strings.TrimSpace(trend)) {
		case "UPTREND", "上升趋势", "上涨", "上升", "多头":
			object["trend_state"] = "UPTREND"
		case "DOWNTREND", "下降趋势", "下跌", "下降", "空头":
			object["trend_state"] = "DOWNTREND"
		case "SIDEWAYS", "横盘", "震荡", "盘整":
			object["trend_state"] = "SIDEWAYS"
		case "BREAKOUT", "突破":
			object["trend_state"] = "BREAKOUT"
		case "PULLBACK", "回调", "回撤":
			object["trend_state"] = "PULLBACK"
		case "MEAN_REVERSION", "均值回归":
			object["trend_state"] = "MEAN_REVERSION"
		default:
			object["trend_state"] = "OTHERS"
		}
	}

	// entry_zone:数组直接取数字;字符串按多种分隔符切分(「240-242」「240 至 242」)。
	var entry []any
	if _, isArray := object["entry_zone"].([]any); isArray {
		entry = normalizeNumberArray(object["entry_zone"])
	} else if text, ok := stringField(object, "entry_zone"); ok {
		entry = []any{}
		for _, part := range strings.FieldsFunc(text, func(r rune) bool {
			return r == '-' || r == '~' || r == '至' || r == '，' || r == ',' || r == ' '
		}) {
			// 这里用严格解析(不做字符过滤),与 rs 的 parse::<f64>() 一致。
			if number, err := strconv.ParseFloat(strings.TrimSpace(part), 64); err == nil {
				entry = append(entry, number)
			}
		}
	} else {
		entry = []any{}
	}
	if len(entry) != 2 {
		entry = []any{}
	} else {
		low, _ := valueAsFloat(entry[0])
		high, _ := valueAsFloat(entry[1])
		if low > high {
			entry[0], entry[1] = entry[1], entry[0]
		}
	}
	object["entry_zone"] = entry

	if _, present := object["targets"]; present {
		object["targets"] = normalizeNumberArray(object["targets"])
	}
	if levels, ok := asMapValue(object["key_levels"]); ok {
		retainKeys(levels, []string{"support", "resistance"})
		for _, key := range []string{"support", "resistance"} {
			if _, present := levels[key]; present {
				levels[key] = normalizeNumberArray(levels[key])
			}
		}
	}
}

func normalizeStrategistItem(object map[string]any) {
	if direction, ok := stringField(object, "direction"); ok {
		object["direction"] = normalizeDirection(direction)
	}
	if horizon, ok := stringField(object, "horizon"); ok {
		object["horizon"] = normalizeHorizon(horizon)
	}
}

func normalizeRiskItem(object map[string]any) {
	if dimension, ok := stringField(object, "dimension"); ok {
		object["dimension"] = enumOrDefault(dimension, riskDimensions, "OTHERS")
	}
	if status, ok := stringField(object, "status"); ok {
		switch strings.ToUpper(strings.TrimSpace(status)) {
		case "OK", "PASS", "通过":
			object["status"] = "OK"
		case "WARN", "警告":
			object["status"] = "WARN"
		case "CRIT", "FAIL", "FAILED", "严重", "拦截":
			object["status"] = "CRIT"
		default:
			object["status"] = "UNKNOWN"
		}
	}
	// 缺数据/无适用阈值必须是 null,不能用 0 伪装。
	for _, key := range []string{"value", "threshold"} {
		if _, present := object[key]; present {
			if number, ok := valueAsFloat(object[key]); ok {
				object[key] = number
			} else {
				object[key] = nil
			}
		}
	}
}

// normalizeOrderItem 归一化执行交易员 / 组合经理的订单项。
//
// 这里最关键的是 order_type 与 limit_price 的联动:MARKET 单必须没有限价,
// LIMIT 单必须有正限价;拿不到正限价的 LIMIT 单会被降级成 MARKET,
// 避免产出平台会直接拒收的订单。
func normalizeOrderItem(object map[string]any) {
	object["market"] = resolveOrderMarket(object)

	if direction, ok := stringField(object, "direction"); ok {
		object["direction"] = normalizeDirection(direction)
	}
	if orderType, ok := stringField(object, "order_type"); ok {
		upper := strings.ToUpper(strings.TrimSpace(orderType))
		if strings.HasPrefix(upper, "MARKET") || strings.Contains(upper, "市价") {
			object["order_type"] = "MARKET"
		} else {
			object["order_type"] = "LIMIT"
		}
	}

	// 缺 order_type 时按 MARKET 处理(与 rs 的 unwrap_or("MARKET") 一致)。
	orderType, _ := stringField(object, "order_type")
	if orderType == "" {
		orderType = "MARKET"
	}
	if orderType == "MARKET" {
		object["limit_price"] = nil
	} else if price, ok := valueAsFloat(object["limit_price"]); ok && price > 0 {
		object["limit_price"] = price
	} else {
		// LIMIT 但没有可用限价 → 降级为 MARKET。
		object["order_type"] = "MARKET"
		object["limit_price"] = nil
	}

	// 止盈/止损的 style 与 limit_price 走同一套联动规则。
	for _, prefix := range []string{"take_profit", "stop_loss"} {
		styleKey := prefix + "_order_style"
		limitKey := prefix + "_limit_price"
		style, ok := stringField(object, styleKey)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(style), "LIMIT") {
			if price, ok := valueAsFloat(object[limitKey]); ok && price > 0 {
				object[styleKey] = "LIMIT"
				object[limitKey] = price
			} else {
				object[styleKey] = "MARKET"
				object[limitKey] = nil
			}
		} else {
			object[styleKey] = "MARKET"
			object[limitKey] = nil
		}
	}

	// 触发价必须是数字;缺失或无法解析时用 0(契约要求非 null)。
	for _, key := range []string{"take_profit_trigger_price", "stop_loss_trigger_price"} {
		if number, ok := valueAsFloat(object[key]); ok {
			object[key] = number
		} else {
			object[key] = 0.0
		}
	}

	if value, ok := stringField(object, "time_in_force"); ok {
		if strings.EqualFold(strings.TrimSpace(value), "GTC") {
			object["time_in_force"] = "GTC"
		} else {
			object["time_in_force"] = "DAY"
		}
	}
	if value, ok := stringField(object, "execution_instruction"); ok {
		object["execution_instruction"] = enumOrDefault(value, []string{"FAK", "FOK", "BOC"}, "GFD")
	}
	if value, ok := stringField(object, "algo_type"); ok {
		object["algo_type"] = enumOrDefault(value, []string{"TWAP", "VWAP", "POV", "ICEBERG"}, "DIRECT")
	}
}

// resolveOrderMarket 取合法的 market;非法或缺失时按 symbol/ticker 的交易所后缀推断。
func resolveOrderMarket(object map[string]any) string {
	if market, ok := stringField(object, "market"); ok {
		upper := strings.ToUpper(strings.TrimSpace(market))
		if upper == "US" || upper == "HK" || upper == "CN" {
			return upper
		}
	}
	symbol, ok := stringField(object, "symbol")
	if !ok {
		symbol, ok = stringField(object, "ticker")
	}
	if !ok {
		return "US"
	}
	return marketFromSymbolSuffix(symbol)
}

// marketFromSymbolSuffix 按最后一个 "." 之后的后缀推断市场;无法识别时归 US。
func marketFromSymbolSuffix(symbol string) string {
	suffix := symbol
	if index := strings.LastIndex(symbol, "."); index >= 0 {
		suffix = symbol[index+1:]
	}
	switch strings.ToUpper(suffix) {
	case "HK":
		return "HK"
	case "CN", "SH", "SZ":
		return "CN"
	default:
		return "US"
	}
}

// normalizeCommonNumbers 做跨岗位的数值归一:字符串转数字,再按契约区间收敛。
func normalizeCommonNumbers(object map[string]any) {
	// 数字样式字符串 → 数字。这里用严格解析(与 rs 的 parse::<f64>() 一致),
	// 带单位的字符串留给后续按字段的 valueAsFloat 处理。
	for _, key := range coercedNumberFields {
		text, ok := stringField(object, key)
		if !ok {
			continue
		}
		if number, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			object[key] = number
		}
	}

	if score, ok := valueAsFloat(object["score"]); ok {
		object["score"] = clampFloat(score, 0, 10)
	}
	if conviction, ok := valueAsFloat(object["conviction"]); ok {
		object["conviction"] = clampFloat(conviction, 0, 1)
	}
	// 模型常把动量写成 -100..100 的百分数;绝对值超过 1 时按百分数换算。
	if momentum, ok := valueAsFloat(object["momentum_score"]); ok {
		normalized := momentum
		if absolute := math.Abs(momentum); absolute > 1 && absolute <= 100 {
			normalized = momentum / 100
		}
		object["momentum_score"] = clampFloat(normalized, -1, 1)
	}
	// 置信度同理:72 / 85 这类百分数换算成 0.72 / 0.85。
	if confidence, ok := valueAsFloat(object["confidence"]); ok {
		normalized := confidence
		if confidence > 1 && confidence <= 100 {
			normalized = confidence / 100
		}
		object["confidence"] = clampFloat(normalized, 0, 1)
	}
	// 下单数量必须是正整数。
	if quantity, ok := valueAsFloat(object["quantity"]); ok {
		object["quantity"] = uint64(math.Round(math.Max(quantity, 1)))
	}
}

func clampFloat(value, low, high float64) float64 {
	return math.Min(math.Max(value, low), high)
}

// ---------------------------------------------------------------------------
// normalizeTopLevelFields:payload_result 顶层归一化
// ---------------------------------------------------------------------------

var (
	marketRegimes  = []string{"RISK_ON", "RISK_OFF", "SIDEWAYS", "ROTATION"}
	viewLenses     = []string{"STYLE_FACTOR", "MACRO", "TECHNICAL", "EVENT_DRIVEN", "SEASONALITY", "CROWDING", "SECTOR", "ASSET_CLASS", "OTHERS"}
	viewStances    = []string{"OVERWEIGHT", "NEUTRAL", "UNDERWEIGHT"}
	decisionTypes  = []string{"STANDALONE", "MULTI_SOURCE_CONSENSUS", "CONFLICT_RESOLUTION", "RISK_OVERRIDE", "REBALANCE", "NO_ACTION"}
	viewKeys       = []string{"lens", "subject", "stance"}
	allocationKeys = []string{"symbol", "target_weight_pct", "reasoning", "stance", "current_weight_pct"}
)

// normalizeTopLevelFields 就地归一化 payload_result 的顶层字段。
func normalizeTopLevelFields(object map[string]any, role string, isFinal bool) {
	switch strings.TrimSpace(role) {
	case "strategist":
		if regime, ok := stringField(object, "market_regime"); ok {
			object["market_regime"] = enumOrDefault(regime, marketRegimes, "OTHERS")
		}
		if views, ok := object["views"].([]any); ok {
			object["views"] = normalizeStrategistViews(views)
		}
		if _, present := object["key_risks"]; present {
			object["key_risks"] = normalizeStringArray(object["key_risks"])
		}

	case "risk_manager":
		for _, key := range []string{"hedging_suggestions", "gating_actions"} {
			if _, present := object[key]; present {
				object[key] = normalizeStringArray(object[key])
			}
		}

	case "portfolio_manager":
		if isFinal {
			return
		}
		if decision, ok := stringField(object, "decision_type"); ok {
			object["decision_type"] = enumOrDefault(decision, decisionTypes, "OTHERS")
		}
		if items, ok := object["target_allocation"].([]any); ok {
			object["target_allocation"] = normalizeTargetAllocation(items)
		}
		if _, present := object["constraints_applied"]; present {
			object["constraints_applied"] = normalizeStringArray(object["constraints_applied"])
		}
	}
}

// normalizeStrategistViews 裁剪并归一策略观点;subject 为空的条目会被丢弃。
func normalizeStrategistViews(views []any) []any {
	kept := make([]any, 0, len(views))
	for _, rawView := range views {
		view, ok := asMapValue(rawView)
		if !ok {
			continue
		}
		retainKeys(view, viewKeys)

		lens, _ := stringField(view, "lens")
		view["lens"] = enumOrDefault(lens, viewLenses, "OTHERS")
		stance, _ := stringField(view, "stance")
		view["stance"] = enumOrDefault(stance, viewStances, "NEUTRAL")

		// 没有主题的观点没有信息量,直接丢弃。
		if subject, ok := stringField(view, "subject"); ok && strings.TrimSpace(subject) != "" {
			kept = append(kept, view)
		}
	}
	return kept
}

// normalizeTargetAllocation 裁剪并归一目标配置;缺三要素的条目会被丢弃。
func normalizeTargetAllocation(items []any) []any {
	kept := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, ok := asMapValue(rawItem)
		if !ok {
			continue
		}
		retainKeys(item, allocationKeys)

		if symbol, ok := stringField(item, "symbol"); ok {
			item["symbol"] = trimUpper(symbol)
		}
		for _, key := range []string{"target_weight_pct", "current_weight_pct"} {
			if number, ok := valueAsFloat(item[key]); ok {
				item[key] = number
			}
		}
		stance, _ := stringField(item, "stance")
		item["stance"] = enumOrDefault(stance, viewStances, "NEUTRAL")

		// 三要素齐全才保留:标的非空、权重是数字、理由非空白。
		symbol, hasSymbol := stringField(item, "symbol")
		if !hasSymbol || symbol == "" {
			continue
		}
		if !isJSONNumber(item["target_weight_pct"]) {
			continue
		}
		reasoning, hasReasoning := stringField(item, "reasoning")
		if !hasReasoning || strings.TrimSpace(reasoning) == "" {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// isJSONNumber 判断值是否已经是数字(而不是数字样式的字符串)。
func isJSONNumber(value any) bool {
	switch value.(type) {
	case float64, float32, int, int64, uint64, json.Number:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// symbol 标准化与占位补全
// ---------------------------------------------------------------------------

// normalizeSymbol 把模型写的标的匹配回标的池里的标准写法。
//
// 大小写不敏感,并容忍交易所后缀省略(如 aapl → 池中的 AAPL.US)。
// 池里找不到时返回 ("", false)。
func normalizeSymbol(value string, pool []string) (string, bool) {
	upper := trimUpper(value)
	for _, candidate := range pool {
		if strings.EqualFold(candidate, upper) {
			return candidate, true
		}
	}
	// 退一步:只比对第一个 "." 之前的基础代码。
	base := upper
	if index := strings.Index(upper, "."); index >= 0 {
		base = upper[:index]
	}
	for _, candidate := range pool {
		candidateBase := candidate
		if index := strings.Index(candidate, "."); index >= 0 {
			candidateBase = candidate[:index]
		}
		if strings.EqualFold(candidateBase, base) {
			return candidate, true
		}
	}
	return "", false
}

// placeholderReasoning 是各类占位项的固定 reasoning 文案。
const (
	placeholderNoReasoning  = "conservative placeholder: no model reasoning returned"
	placeholderNoAssessment = "conservative placeholder: no model assessment produced for this symbol"
	placeholderNoSignal     = "conservative placeholder: no model signal produced for this symbol"
	placeholderNoOrder      = "conservative placeholder: no model order produced for this symbol"
)

// normalizeSymbolItems 就地规范化主数组:
//   - rating 归一到五档大写枚举;
//   - fair_value 字符串/非法值归一到数字;
//   - reasoning 非空兜底;
//   - 有 symbol 语义且给出 symbolPool 时:过滤池外标的、标准化写法、按池顺序补全缺失标的。
//
// hasTicker 必须由调用方在裁剪 itemKeys **之前**探测,否则 ticker 已被裁掉。
func normalizeSymbolItems(
	items []any,
	symbolPool []string,
	role string,
	targetCount int,
	hasTargetCount bool,
	universeMode *string,
	isFinal bool,
	hasTicker bool,
) []any {
	// SEED 模式:池子只是起点,允许在目标市场发现池外机会 —— 不做池内强制过滤/补全。
	seed := universeMode != nil && strings.EqualFold(*universeMode, "SEED")

	for _, rawItem := range items {
		object, ok := asMapValue(rawItem)
		if !ok {
			continue
		}
		normalizeRoleFields(object, role)
		if rating, ok := stringField(object, "rating"); ok {
			object["rating"] = normalizeRating(rating)
		}
		if _, present := object["fair_value"]; present {
			// 无法解析时归 0.0;契约要求 fair_value 是数字,不能是 null。
			number, _ := valueAsFloat(object["fair_value"])
			object["fair_value"] = number
		}
		if reasoning, ok := stringField(object, "reasoning"); !ok || strings.TrimSpace(reasoning) == "" {
			object["reasoning"] = placeholderNoReasoning
		}
	}

	// 风控 checks 无 symbol 语义(键为 dimension/status/reasoning),符号池过滤会
	// 因缺少 symbol 字段把整条 check 丢弃,导致平台收到 checks:[] → minItems:1 拒收。
	// checks 的条数对齐在 output_from_claude 的 risk_manager 分支单独处理。
	if strings.TrimSpace(role) == "risk_manager" {
		return items
	}
	if symbolPool == nil {
		return items
	}

	// 注意:不要按「数组里已有 symbol」判断 —— 空数组(模型未产出)同样要补全占位,
	// 否则 PRESERVE_UPSTREAM / EXACT_ITEMS 断言会挂。
	seen := map[string]bool{}
	kept := make([]any, 0, len(symbolPool))
	for _, rawItem := range items {
		object, isObject := asMapValue(rawItem)
		symbol := ""
		if isObject {
			key := "symbol"
			if hasTicker {
				key = "ticker"
			}
			symbol, _ = stringField(object, key)
		}

		var standard string
		var ok bool
		if seed {
			standard, ok = trimUpper(symbol), true
		} else {
			standard, ok = normalizeSymbol(symbol, symbolPool)
		}
		if !ok || seen[standard] {
			continue
		}
		seen[standard] = true
		if isObject {
			if isFinal {
				object["ticker"] = standard
			} else {
				// 职能一用 symbol;若模型输出了 ticker,移除它改用 symbol。
				delete(object, "ticker")
				object["symbol"] = standard
			}
		}
		kept = append(kept, rawItem)
	}

	if seed {
		// SEED:保留去重后的全部标的(含池外),不补全、不截断。
		return kept
	}

	// 目标条数:taskBrief 明确「精选/恰好 N 只」时按 N 对齐,否则默认全覆盖。
	target := len(symbolPool)
	if hasTargetCount && targetCount >= 1 && targetCount <= len(symbolPool) {
		target = targetCount
	}
	for _, symbol := range symbolPool {
		if len(kept) >= target {
			break
		}
		if seen[symbol] {
			continue
		}
		placeholder, ok := placeholderItem(role, symbol, isFinal)
		if !ok {
			continue
		}
		kept = append(kept, placeholder)
		seen[symbol] = true
	}
	if len(kept) > target {
		kept = kept[:target]
	}
	return kept
}

// normalizeRating 把评级归一到五档大写枚举;无法识别时保守取 HOLD。
func normalizeRating(rating string) string {
	switch strings.ToUpper(strings.TrimSpace(rating)) {
	case "STRONG_BUY", "STRONG BUY", "SB":
		return "STRONG_BUY"
	case "BUY", "买入", "买":
		return "BUY"
	case "HOLD", "NEUTRAL", "持有", "中性":
		return "HOLD"
	case "SELL", "卖出", "卖":
		return "SELL"
	case "STRONG_SELL", "STRONG SELL", "SS":
		return "STRONG_SELL"
	default:
		return "HOLD"
	}
}

// placeholderItem 按岗位生成一条保守占位项。ok 为 false 表示该岗位不补位。
func placeholderItem(role, symbol string, isFinal bool) (map[string]any, bool) {
	market := marketFromSymbolSuffix(symbol)
	switch role {
	case "fundamental_analyst":
		return map[string]any{
			"symbol":     symbol,
			"rating":     "HOLD",
			"fair_value": 0.0,
			"reasoning":  placeholderNoAssessment,
		}, true
	case "technical_analyst":
		return map[string]any{
			"symbol":     symbol,
			"bias":       "NEUTRAL",
			"entry_zone": []any{},
			"reasoning":  placeholderNoSignal,
		}, true
	case "strategist":
		return map[string]any{
			"symbol":    symbol,
			"direction": "BUY",
			"reasoning": placeholderNoSignal,
		}, true
	case "execution_trader":
		return map[string]any{
			"symbol":                    symbol,
			"market":                    market,
			"direction":                 "BUY",
			"order_type":                "MARKET",
			"limit_price":               nil,
			"quantity":                  1,
			"take_profit_trigger_price": 0,
			"take_profit_order_style":   "MARKET",
			"take_profit_limit_price":   nil,
			"stop_loss_trigger_price":   0,
			"stop_loss_order_style":     "MARKET",
			"stop_loss_limit_price":     nil,
			"reasoning":                 placeholderNoOrder,
		}, true
	case "portfolio_manager":
		if isFinal {
			// 职能二:ticker + 19 字段。
			return map[string]any{
				"time":                      time.Now().UTC().Format("15:04:05"),
				"ticker":                    symbol,
				"name":                      symbol,
				"market":                    market,
				"direction":                 "BUY",
				"order_type":                "MARKET",
				"limit_price":               nil,
				"quantity":                  1,
				"take_profit_trigger_price": 0,
				"take_profit_order_style":   "MARKET",
				"take_profit_limit_price":   nil,
				"stop_loss_trigger_price":   0,
				"stop_loss_order_style":     "MARKET",
				"stop_loss_limit_price":     nil,
				"time_in_force":             "DAY",
				"execution_instruction":     "GFD",
				"reasoning":                 placeholderNoOrder,
			}, true
		}
		// 职能一(平台评测):只允许 8 字段,用 symbol 不是 ticker。
		return map[string]any{
			"symbol":      symbol,
			"market":      market,
			"direction":   "BUY",
			"order_type":  "MARKET",
			"limit_price": nil,
			"quantity":    1,
			"reasoning":   placeholderNoOrder,
		}, true
	default:
		return nil, false
	}
}
