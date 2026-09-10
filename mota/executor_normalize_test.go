package mota

import (
	"reflect"
	"strings"
	"testing"
)

// normalizeItem 用 JSON 文本构造一个数组项,归一化后返回。
func normalizeItem(t *testing.T, role, raw string) map[string]any {
	t.Helper()
	object, ok := asMapValue(decodeJSONValue(t, raw))
	if !ok {
		t.Fatalf("test input must be an object: %s", raw)
	}
	normalizeRoleFields(object, role)
	return object
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func TestNormalizeNumberArray(t *testing.T) {
	got := normalizeNumberArray(decodeJSONValue(t, `[1, "2.5", "bad", null, 3]`))
	want := []any{1.0, 2.5, 3.0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// 非数组输入返回空数组(而不是 nil),避免 JSON 里出现 null。
	for _, input := range []any{nil, "text", 42.0, map[string]any{}} {
		if got := normalizeNumberArray(input); got == nil || len(got) != 0 {
			t.Errorf("normalizeNumberArray(%v) = %v, want an empty slice", input, got)
		}
	}
}

func TestNormalizeStringArray(t *testing.T) {
	got := normalizeStringArray(decodeJSONValue(t, `["  keep ", "", "   ", 42, null, "also"]`))
	want := []any{"keep", "also"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := normalizeStringArray("not an array"); got == nil || len(got) != 0 {
		t.Errorf("got %v, want an empty slice", got)
	}
}

func TestNormalizeHorizon(t *testing.T) {
	for input, want := range map[string]string{
		"INTRADAY": "INTRADAY", "DAY": "INTRADAY", "日内": "INTRADAY", "intraday": "INTRADAY",
		"SHORT": "SHORT", "SHORT_TERM": "SHORT", "短期": "SHORT",
		"LONG": "LONG", "LONG_TERM": "LONG", "长期": "LONG",
		"MEDIUM": "MEDIUM", "中期": "MEDIUM", "": "MEDIUM", "garbage": "MEDIUM",
		"  short  ": "SHORT",
	} {
		if got := normalizeHorizon(input); got != want {
			t.Errorf("normalizeHorizon(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeDirection(t *testing.T) {
	for input, want := range map[string]string{
		"SELL": "SELL", "sell": "SELL", "SELL_SHORT": "SELL", "卖出": "SELL", "减持卖": "SELL",
		"BUY": "BUY", "buy": "BUY", "增持": "BUY", "": "BUY", "garbage": "BUY",
	} {
		if got := normalizeDirection(input); got != want {
			t.Errorf("normalizeDirection(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMarketFromSymbolSuffix(t *testing.T) {
	for input, want := range map[string]string{
		"AAPL.US": "US", "0700.HK": "HK", "600519.CN": "CN",
		"600519.SH": "CN", "000001.SZ": "CN", "aapl.us": "US",
		"NOSUFFIX": "US", "": "US", "X.UNKNOWN": "US",
	} {
		if got := marketFromSymbolSuffix(input); got != want {
			t.Errorf("marketFromSymbolSuffix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEnumOrDefault(t *testing.T) {
	allowed := []string{"A", "B"}
	for input, want := range map[string]string{
		"A": "A", "b": "B", "  a  ": "A", "C": "FALLBACK", "": "FALLBACK",
	} {
		if got := enumOrDefault(input, allowed, "FALLBACK"); got != want {
			t.Errorf("enumOrDefault(%q) = %q, want %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 基本面 / 策略
// ---------------------------------------------------------------------------

func TestNormalizeFundamentalItem(t *testing.T) {
	got := normalizeItem(t, "fundamental_analyst", `{
		"horizon": "long_term",
		"key_drivers": ["  服务收入  ", "", 42],
		"key_risks": "not an array"
	}`)
	if got["horizon"] != "LONG" {
		t.Errorf("horizon = %v, want LONG", got["horizon"])
	}
	if !reflect.DeepEqual(got["key_drivers"], []any{"服务收入"}) {
		t.Errorf("key_drivers = %v", got["key_drivers"])
	}
	// 非数组的 key_risks 会被归一成空数组。
	if !reflect.DeepEqual(got["key_risks"], []any{}) {
		t.Errorf("key_risks = %v, want an empty array", got["key_risks"])
	}
}

// 缺失的字段不应被凭空创建。
func TestNormalizeFundamentalItemLeavesAbsentFieldsAlone(t *testing.T) {
	got := normalizeItem(t, "fundamental_analyst", `{"symbol":"AAPL.US"}`)
	for _, key := range []string{"horizon", "key_drivers", "key_risks"} {
		if _, present := got[key]; present {
			t.Errorf("absent field %q must not be created", key)
		}
	}
}

func TestNormalizeStrategistItem(t *testing.T) {
	got := normalizeItem(t, "strategist", `{"direction":"卖出","horizon":"day"}`)
	if got["direction"] != "SELL" {
		t.Errorf("direction = %v, want SELL", got["direction"])
	}
	if got["horizon"] != "INTRADAY" {
		t.Errorf("horizon = %v, want INTRADAY", got["horizon"])
	}
}

// ---------------------------------------------------------------------------
// 技术分析
// ---------------------------------------------------------------------------

func TestNormalizeTechnicalBias(t *testing.T) {
	for input, want := range map[string]string{
		"LONG_BIAS": "LONG_BIAS", "BULLISH": "LONG_BIAS", "看多": "LONG_BIAS", "偏多": "LONG_BIAS",
		"SHORT_BIAS": "SHORT_BIAS", "BEARISH": "SHORT_BIAS", "看空": "SHORT_BIAS", "偏空": "SHORT_BIAS",
		"NEUTRAL": "NEUTRAL", "garbage": "NEUTRAL", "": "NEUTRAL",
	} {
		got := normalizeItem(t, "technical_analyst", `{"bias":"`+input+`"}`)
		if got["bias"] != want {
			t.Errorf("bias %q => %v, want %q", input, got["bias"], want)
		}
	}
}

func TestNormalizeTechnicalTrendState(t *testing.T) {
	for input, want := range map[string]string{
		"UPTREND": "UPTREND", "上升趋势": "UPTREND", "上涨": "UPTREND", "上升": "UPTREND", "多头": "UPTREND",
		"DOWNTREND": "DOWNTREND", "下降趋势": "DOWNTREND", "下跌": "DOWNTREND", "空头": "DOWNTREND",
		"SIDEWAYS": "SIDEWAYS", "横盘": "SIDEWAYS", "震荡": "SIDEWAYS", "盘整": "SIDEWAYS",
		"BREAKOUT": "BREAKOUT", "突破": "BREAKOUT",
		"PULLBACK": "PULLBACK", "回调": "PULLBACK", "回撤": "PULLBACK",
		"MEAN_REVERSION": "MEAN_REVERSION", "均值回归": "MEAN_REVERSION",
		"garbage": "OTHERS", "": "OTHERS",
	} {
		got := normalizeItem(t, "technical_analyst", `{"trend_state":"`+input+`"}`)
		if got["trend_state"] != want {
			t.Errorf("trend_state %q => %v, want %q", input, got["trend_state"], want)
		}
	}
}

func TestNormalizeTechnicalEntryZone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []any
	}{
		{"two numbers", `[240, 242]`, []any{240.0, 242.0}},
		{"numeric strings", `["240", "242"]`, []any{240.0, 242.0}},
		{"out of order is sorted", `[242, 240]`, []any{240.0, 242.0}},
		{"one number is cleared", `[240]`, []any{}},
		{"three numbers cleared", `[1,2,3]`, []any{}},
		{"empty stays empty", `[]`, []any{}},
		{"hyphen string", `"240-242"`, []any{240.0, 242.0}},
		{"tilde string", `"240~242"`, []any{240.0, 242.0}},
		{"chinese 至", `"240至242"`, []any{240.0, 242.0}},
		{"comma string", `"240,242"`, []any{240.0, 242.0}},
		{"fullwidth comma", `"240，242"`, []any{240.0, 242.0}},
		{"space string", `"240 242"`, []any{240.0, 242.0}},
		{"prose is cleared", `"看情况"`, []any{}},
		{"object is cleared", `{"low":240}`, []any{}},
		{"null is cleared", `null`, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeItem(t, "technical_analyst", `{"entry_zone":`+tc.input+`}`)
			if !reflect.DeepEqual(got["entry_zone"], tc.want) {
				t.Errorf("entry_zone %s => %v, want %v", tc.input, got["entry_zone"], tc.want)
			}
		})
	}
}

// entry_zone 缺失时也会被补成空数组(契约要求该字段存在)。
func TestNormalizeTechnicalEntryZoneAlwaysPresent(t *testing.T) {
	got := normalizeItem(t, "technical_analyst", `{"symbol":"AAPL.US"}`)
	if !reflect.DeepEqual(got["entry_zone"], []any{}) {
		t.Errorf("entry_zone = %v, want an empty array", got["entry_zone"])
	}
}

func TestNormalizeTechnicalKeyLevels(t *testing.T) {
	got := normalizeItem(t, "technical_analyst", `{
		"key_levels": {"support": [235, "236"], "resistance": ["250"], "extra": "dropped"},
		"targets": [250, "260", "bad"]
	}`)
	levels, ok := asMapValue(got["key_levels"])
	if !ok {
		t.Fatalf("key_levels = %v", got["key_levels"])
	}
	if _, present := levels["extra"]; present {
		t.Error("key_levels must only keep support/resistance")
	}
	if !reflect.DeepEqual(levels["support"], []any{235.0, 236.0}) {
		t.Errorf("support = %v", levels["support"])
	}
	if !reflect.DeepEqual(levels["resistance"], []any{250.0}) {
		t.Errorf("resistance = %v", levels["resistance"])
	}
	if !reflect.DeepEqual(got["targets"], []any{250.0, 260.0}) {
		t.Errorf("targets = %v", got["targets"])
	}
}

// ---------------------------------------------------------------------------
// 风控
// ---------------------------------------------------------------------------

func TestNormalizeRiskDimension(t *testing.T) {
	for _, dimension := range riskDimensions {
		got := normalizeItem(t, "risk_manager", `{"dimension":"`+strings.ToLower(dimension)+`"}`)
		if got["dimension"] != dimension {
			t.Errorf("dimension %q => %v, want %q", dimension, got["dimension"], dimension)
		}
	}
	for _, input := range []string{"UNKNOWN_DIM", "", "行业暴露"} {
		got := normalizeItem(t, "risk_manager", `{"dimension":"`+input+`"}`)
		if got["dimension"] != "OTHERS" {
			t.Errorf("dimension %q => %v, want OTHERS", input, got["dimension"])
		}
	}
}

func TestNormalizeRiskStatus(t *testing.T) {
	for input, want := range map[string]string{
		"OK": "OK", "PASS": "OK", "通过": "OK", "ok": "OK",
		"WARN": "WARN", "警告": "WARN",
		"CRIT": "CRIT", "FAIL": "CRIT", "FAILED": "CRIT", "严重": "CRIT", "拦截": "CRIT",
		"UNKNOWN": "UNKNOWN", "garbage": "UNKNOWN", "": "UNKNOWN",
	} {
		got := normalizeItem(t, "risk_manager", `{"status":"`+input+`"}`)
		if got["status"] != want {
			t.Errorf("status %q => %v, want %q", input, got["status"], want)
		}
	}
}

// 缺数据必须是 null,不能用 0 伪装。
func TestNormalizeRiskValueAndThreshold(t *testing.T) {
	got := normalizeItem(t, "risk_manager", `{"value":"6.2","threshold":"n/a"}`)
	if got["value"] != 6.2 {
		t.Errorf("value = %v, want 6.2", got["value"])
	}
	if got["threshold"] != nil {
		t.Errorf("threshold = %v, want nil", got["threshold"])
	}

	// 字段缺失时不创建。
	got = normalizeItem(t, "risk_manager", `{"dimension":"VAR"}`)
	if _, present := got["value"]; present {
		t.Error("absent value must not be created")
	}
}

// ---------------------------------------------------------------------------
// 订单:market / order_type 与 limit_price 联动
// ---------------------------------------------------------------------------

func TestNormalizeOrderMarket(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"explicit US", `{"market":"us"}`, "US"},
		{"explicit HK", `{"market":"HK"}`, "HK"},
		{"explicit CN", `{"market":"cn"}`, "CN"},
		{"illegal falls back to symbol suffix", `{"market":"JP","symbol":"0700.HK"}`, "HK"},
		{"absent uses symbol suffix", `{"symbol":"600519.SH"}`, "CN"},
		{"absent uses ticker suffix", `{"ticker":"AAPL.US"}`, "US"},
		{"no symbol at all defaults US", `{}`, "US"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeItem(t, "execution_trader", tc.input)
			if got["market"] != tc.want {
				t.Errorf("market = %v, want %q", got["market"], tc.want)
			}
		})
	}
}

func TestNormalizeOrderTypeAndLimitPrice(t *testing.T) {
	for _, tc := range []struct {
		name         string
		input        string
		wantType     string
		wantLimitNil bool
		wantLimitVal float64
	}{
		{"MARKET clears limit", `{"order_type":"MARKET","limit_price":245}`, "MARKET", true, 0},
		{"市价 maps to MARKET", `{"order_type":"市价单","limit_price":245}`, "MARKET", true, 0},
		{"LIMIT keeps positive price", `{"order_type":"LIMIT","limit_price":245}`, "LIMIT", false, 245},
		{"LIMIT parses string price", `{"order_type":"limit","limit_price":"245.5"}`, "LIMIT", false, 245.5},
		{"LIMIT without price degrades", `{"order_type":"LIMIT"}`, "MARKET", true, 0},
		{"LIMIT with zero price degrades", `{"order_type":"LIMIT","limit_price":0}`, "MARKET", true, 0},
		{"LIMIT with negative price degrades", `{"order_type":"LIMIT","limit_price":-5}`, "MARKET", true, 0},
		{"LIMIT with null price degrades", `{"order_type":"LIMIT","limit_price":null}`, "MARKET", true, 0},
		{"unknown type becomes LIMIT then degrades", `{"order_type":"weird"}`, "MARKET", true, 0},
		{"absent order_type behaves as MARKET", `{"limit_price":245}`, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeItem(t, "execution_trader", tc.input)
			if tc.wantType != "" && got["order_type"] != tc.wantType {
				t.Errorf("order_type = %v, want %q", got["order_type"], tc.wantType)
			}
			if tc.wantLimitNil {
				if got["limit_price"] != nil {
					t.Errorf("limit_price = %v, want nil", got["limit_price"])
				}
			} else if got["limit_price"] != tc.wantLimitVal {
				t.Errorf("limit_price = %v, want %v", got["limit_price"], tc.wantLimitVal)
			}
		})
	}
}

func TestNormalizeOrderTakeProfitAndStopLoss(t *testing.T) {
	for _, prefix := range []string{"take_profit", "stop_loss"} {
		styleKey, limitKey := prefix+"_order_style", prefix+"_limit_price"
		t.Run(prefix+" LIMIT keeps price", func(t *testing.T) {
			got := normalizeItem(t, "execution_trader",
				`{"`+styleKey+`":"limit","`+limitKey+`":259.5}`)
			if got[styleKey] != "LIMIT" || got[limitKey] != 259.5 {
				t.Errorf("%s=%v %s=%v", styleKey, got[styleKey], limitKey, got[limitKey])
			}
		})
		t.Run(prefix+" MARKET clears price", func(t *testing.T) {
			got := normalizeItem(t, "execution_trader",
				`{"`+styleKey+`":"MARKET","`+limitKey+`":259.5}`)
			if got[styleKey] != "MARKET" || got[limitKey] != nil {
				t.Errorf("%s=%v %s=%v", styleKey, got[styleKey], limitKey, got[limitKey])
			}
		})
		t.Run(prefix+" LIMIT without price degrades", func(t *testing.T) {
			got := normalizeItem(t, "execution_trader", `{"`+styleKey+`":"LIMIT"}`)
			if got[styleKey] != "MARKET" || got[limitKey] != nil {
				t.Errorf("%s=%v %s=%v", styleKey, got[styleKey], limitKey, got[limitKey])
			}
		})
		t.Run(prefix+" absent style is untouched", func(t *testing.T) {
			got := normalizeItem(t, "execution_trader", `{"symbol":"AAPL.US"}`)
			if _, present := got[styleKey]; present {
				t.Errorf("absent %s must not be created", styleKey)
			}
		})
	}
}

// 触发价必须是数字,缺失或无法解析时归 0。
func TestNormalizeOrderTriggerPrices(t *testing.T) {
	got := normalizeItem(t, "execution_trader",
		`{"take_profit_trigger_price":"260.5","stop_loss_trigger_price":"n/a"}`)
	if got["take_profit_trigger_price"] != 260.5 {
		t.Errorf("take_profit_trigger_price = %v, want 260.5", got["take_profit_trigger_price"])
	}
	if got["stop_loss_trigger_price"] != 0.0 {
		t.Errorf("stop_loss_trigger_price = %v, want 0", got["stop_loss_trigger_price"])
	}
	// 缺失时也补 0(契约要求非 null)。
	got = normalizeItem(t, "execution_trader", `{"symbol":"AAPL.US"}`)
	for _, key := range []string{"take_profit_trigger_price", "stop_loss_trigger_price"} {
		if got[key] != 0.0 {
			t.Errorf("%s = %v, want 0", key, got[key])
		}
	}
}

func TestNormalizeOrderEnums(t *testing.T) {
	for input, want := range map[string]string{"GTC": "GTC", "gtc": "GTC", "DAY": "DAY", "weird": "DAY"} {
		got := normalizeItem(t, "execution_trader", `{"time_in_force":"`+input+`"}`)
		if got["time_in_force"] != want {
			t.Errorf("time_in_force %q => %v, want %q", input, got["time_in_force"], want)
		}
	}
	for input, want := range map[string]string{
		"FAK": "FAK", "FOK": "FOK", "BOC": "BOC", "fak": "FAK", "GFD": "GFD", "weird": "GFD",
	} {
		got := normalizeItem(t, "execution_trader", `{"execution_instruction":"`+input+`"}`)
		if got["execution_instruction"] != want {
			t.Errorf("execution_instruction %q => %v, want %q", input, got["execution_instruction"], want)
		}
	}
	for input, want := range map[string]string{
		"TWAP": "TWAP", "VWAP": "VWAP", "POV": "POV", "ICEBERG": "ICEBERG",
		"vwap": "VWAP", "DIRECT": "DIRECT", "weird": "DIRECT",
	} {
		got := normalizeItem(t, "execution_trader", `{"algo_type":"`+input+`"}`)
		if got["algo_type"] != want {
			t.Errorf("algo_type %q => %v, want %q", input, got["algo_type"], want)
		}
	}
}

// 组合经理走同一套订单归一化。
func TestNormalizeOrderItemAppliesToPortfolioManager(t *testing.T) {
	got := normalizeItem(t, "portfolio_manager", `{"symbol":"0700.HK","direction":"卖","order_type":"LIMIT"}`)
	if got["market"] != "HK" {
		t.Errorf("market = %v, want HK", got["market"])
	}
	if got["direction"] != "SELL" {
		t.Errorf("direction = %v, want SELL", got["direction"])
	}
	if got["order_type"] != "MARKET" {
		t.Errorf("order_type = %v, want MARKET (degraded)", got["order_type"])
	}
}

// ---------------------------------------------------------------------------
// 通用数值归一与收敛
// ---------------------------------------------------------------------------

func TestNormalizeCommonNumberCoercion(t *testing.T) {
	got := normalizeItem(t, "", `{
		"score":"8.2","conviction":"0.76","upside_pct":"12.5",
		"fair_value":"245.0","price":"100","target_weight_pct":"12.5"
	}`)
	for key, want := range map[string]float64{
		"score": 8.2, "conviction": 0.76, "upside_pct": 12.5,
		"fair_value": 245.0, "price": 100, "target_weight_pct": 12.5,
	} {
		if got[key] != want {
			t.Errorf("%s = %v (%T), want %v", key, got[key], got[key], want)
		}
	}
}

func TestNormalizeScoreAndConvictionClamp(t *testing.T) {
	for _, tc := range []struct {
		key   string
		input string
		want  float64
	}{
		{"score", "15", 10}, {"score", "-3", 0}, {"score", "8.2", 8.2},
		{"conviction", "1.5", 1}, {"conviction", "-0.2", 0}, {"conviction", "0.76", 0.76},
	} {
		got := normalizeItem(t, "", `{"`+tc.key+`":`+tc.input+`}`)
		if got[tc.key] != tc.want {
			t.Errorf("%s %s => %v, want %v", tc.key, tc.input, got[tc.key], tc.want)
		}
	}
}

// 动量常被写成 -100..100 的百分数。
func TestNormalizeMomentumScore(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  float64
	}{
		{"0.62", 0.62},
		{"62", 0.62},
		{"-62", -0.62},
		{"1", 1},
		{"-1", -1},
		{"100", 1},
		{"-100", -1},
		{"150", 1},   // 超出百分数范围,直接 clamp
		{"-150", -1}, // 同上
	} {
		got := normalizeItem(t, "", `{"momentum_score":`+tc.input+`}`)
		value, _ := valueAsFloat(got["momentum_score"])
		if diff := value - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("momentum_score %s => %v, want %v", tc.input, got["momentum_score"], tc.want)
		}
	}
}

// 置信度同理:72 / 85 换算成 0.72 / 0.85。
func TestNormalizeConfidence(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  float64
	}{
		{"0.71", 0.71}, {"71", 0.71}, {"1", 1}, {"100", 1},
		{"150", 1}, {"-0.5", 0}, {"0", 0},
	} {
		got := normalizeItem(t, "", `{"confidence":`+tc.input+`}`)
		value, _ := valueAsFloat(got["confidence"])
		if diff := value - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("confidence %s => %v, want %v", tc.input, got["confidence"], tc.want)
		}
	}
}

// 下单数量必须是正整数。
func TestNormalizeQuantity(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  uint64
	}{
		{"100", 100}, {"100.4", 100}, {"100.6", 101},
		{"0", 1}, {"-5", 1}, {"0.2", 1}, {`"250"`, 250},
	} {
		got := normalizeItem(t, "", `{"quantity":`+tc.input+`}`)
		quantity, ok := got["quantity"].(uint64)
		if !ok {
			t.Fatalf("quantity %s => %v (%T), want uint64", tc.input, got["quantity"], got["quantity"])
		}
		if quantity != tc.want {
			t.Errorf("quantity %s => %d, want %d", tc.input, quantity, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// normalizeTopLevelFields
// ---------------------------------------------------------------------------

func normalizeTop(t *testing.T, role string, isFinal bool, raw string) map[string]any {
	t.Helper()
	object, ok := asMapValue(decodeJSONValue(t, raw))
	if !ok {
		t.Fatalf("test input must be an object: %s", raw)
	}
	normalizeTopLevelFields(object, role, isFinal)
	return object
}

func TestNormalizeTopLevelStrategist(t *testing.T) {
	got := normalizeTop(t, "strategist", false, `{
		"market_regime":"rotation",
		"key_risks":["  反转  ", ""],
		"views":[
			{"lens":"style_factor","subject":"动量","stance":"overweight","extra":"dropped"},
			{"lens":"weird","subject":"宏观","stance":"weird"},
			{"lens":"MACRO","subject":"   ","stance":"NEUTRAL"},
			{"lens":"MACRO","stance":"NEUTRAL"},
			"not an object"
		]
	}`)
	if got["market_regime"] != "ROTATION" {
		t.Errorf("market_regime = %v, want ROTATION", got["market_regime"])
	}
	if !reflect.DeepEqual(got["key_risks"], []any{"反转"}) {
		t.Errorf("key_risks = %v", got["key_risks"])
	}
	views, ok := got["views"].([]any)
	if !ok {
		t.Fatalf("views = %v", got["views"])
	}
	// 只有前两条有非空 subject。
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2 (blank/missing subject and non-objects dropped)", len(views))
	}
	first, _ := asMapValue(views[0])
	if first["lens"] != "STYLE_FACTOR" || first["stance"] != "OVERWEIGHT" {
		t.Errorf("first view = %v", first)
	}
	if _, present := first["extra"]; present {
		t.Error("views must only keep lens/subject/stance")
	}
	second, _ := asMapValue(views[1])
	if second["lens"] != "OTHERS" || second["stance"] != "NEUTRAL" {
		t.Errorf("unknown enums should fall back: %v", second)
	}
}

func TestNormalizeTopLevelRiskManager(t *testing.T) {
	got := normalizeTop(t, "risk_manager", false, `{
		"hedging_suggestions":["  减仓  ", "", 42],
		"gating_actions":"not an array"
	}`)
	if !reflect.DeepEqual(got["hedging_suggestions"], []any{"减仓"}) {
		t.Errorf("hedging_suggestions = %v", got["hedging_suggestions"])
	}
	if !reflect.DeepEqual(got["gating_actions"], []any{}) {
		t.Errorf("gating_actions = %v, want an empty array", got["gating_actions"])
	}
}

func TestNormalizeTopLevelPortfolioProcess(t *testing.T) {
	got := normalizeTop(t, "portfolio_manager", false, `{
		"decision_type":"rebalance",
		"constraints_applied":["  限制  ", ""],
		"target_allocation":[
			{"symbol":"  aapl.us  ","target_weight_pct":"12.5","current_weight_pct":"9","stance":"overweight","reasoning":"ok","extra":"dropped"},
			{"symbol":"MSFT.US","target_weight_pct":5,"reasoning":"   "},
			{"symbol":"","target_weight_pct":5,"reasoning":"ok"},
			{"symbol":"TSLA.US","reasoning":"ok"},
			"not an object"
		]
	}`)
	if got["decision_type"] != "REBALANCE" {
		t.Errorf("decision_type = %v, want REBALANCE", got["decision_type"])
	}
	if !reflect.DeepEqual(got["constraints_applied"], []any{"限制"}) {
		t.Errorf("constraints_applied = %v", got["constraints_applied"])
	}
	items, ok := got["target_allocation"].([]any)
	if !ok {
		t.Fatalf("target_allocation = %v", got["target_allocation"])
	}
	// 只有第一条三要素齐全。
	if len(items) != 1 {
		t.Fatalf("len(target_allocation) = %d, want 1: %v", len(items), items)
	}
	first, _ := asMapValue(items[0])
	if first["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v, want AAPL.US (trimmed + uppercased)", first["symbol"])
	}
	if first["target_weight_pct"] != 12.5 || first["current_weight_pct"] != 9.0 {
		t.Errorf("weights = %v / %v", first["target_weight_pct"], first["current_weight_pct"])
	}
	if first["stance"] != "OVERWEIGHT" {
		t.Errorf("stance = %v, want OVERWEIGHT", first["stance"])
	}
	if _, present := first["extra"]; present {
		t.Error("target_allocation must only keep the five allowed keys")
	}
}

// stance 缺失时补 NEUTRAL。
func TestNormalizeTopLevelAllocationDefaultStance(t *testing.T) {
	got := normalizeTop(t, "portfolio_manager", false, `{"target_allocation":[
		{"symbol":"AAPL.US","target_weight_pct":10,"reasoning":"ok"}]}`)
	items, _ := got["target_allocation"].([]any)
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	item, _ := asMapValue(items[0])
	if item["stance"] != "NEUTRAL" {
		t.Errorf("stance = %v, want NEUTRAL", item["stance"])
	}
}

// 职能二不做职能一的顶层归一化。
func TestNormalizeTopLevelPortfolioFinalSkipsProcessRules(t *testing.T) {
	got := normalizeTop(t, "portfolio_manager", true, `{"decision_type":"weird"}`)
	if got["decision_type"] != "weird" {
		t.Errorf("final mode must leave decision_type untouched, got %v", got["decision_type"])
	}
}

// 未收录岗位不做顶层归一化。
func TestNormalizeTopLevelUnknownRoleIsNoop(t *testing.T) {
	got := normalizeTop(t, "quant_trading_agent", false, `{"market_regime":"weird"}`)
	if got["market_regime"] != "weird" {
		t.Errorf("unknown role must be a no-op, got %v", got["market_regime"])
	}
}
