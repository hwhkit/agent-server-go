package execengine

import (
	"testing"
)

// ---------------------------------------------------------------------------
// normalizeSymbol
// ---------------------------------------------------------------------------

func TestNormalizeSymbolExactAndBaseMatch(t *testing.T) {
	pool := []string{"AAPL.US", "0700.HK", "600519.SH"}
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"AAPL.US", "AAPL.US", true},
		{"aapl.us", "AAPL.US", true},
		{"  AAPL.US  ", "AAPL.US", true},
		// 后缀省略时按基础代码回退匹配。
		{"AAPL", "AAPL.US", true},
		{"aapl", "AAPL.US", true},
		{"0700", "0700.HK", true},
		// 交易所后缀写错也能按基础代码命中。
		{"AAPL.HK", "AAPL.US", true},
		{"MSFT.US", "", false},
		{"", "", false},
	} {
		got, ok := normalizeSymbol(tc.input, pool)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normalizeSymbol(%q) = (%q, %t), want (%q, %t)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeSymbolEmptyPool(t *testing.T) {
	if _, ok := normalizeSymbol("AAPL.US", nil); ok {
		t.Error("an empty pool must not match anything")
	}
}

// ---------------------------------------------------------------------------
// normalizeSymbolItems:项内归一化
// ---------------------------------------------------------------------------

// symbolItems 跑一次 normalizeSymbolItems 并返回结果。
func symbolItems(
	t *testing.T, raw string, pool []string, role string,
	targetCount int, hasTargetCount bool, universeMode string, isFinal, hasTicker bool,
) []any {
	t.Helper()
	items, ok := decodeJSONValue(t, raw).([]any)
	if !ok {
		t.Fatalf("test input must be an array: %s", raw)
	}
	var mode *string
	if universeMode != "" {
		mode = &universeMode
	}
	return normalizeSymbolItems(items, pool, role, targetCount, hasTargetCount, mode, isFinal, hasTicker)
}

func TestNormalizeSymbolItemsRating(t *testing.T) {
	for input, want := range map[string]string{
		"STRONG_BUY": "STRONG_BUY", "STRONG BUY": "STRONG_BUY", "SB": "STRONG_BUY", "sb": "STRONG_BUY",
		"BUY": "BUY", "买入": "BUY", "买": "BUY",
		"HOLD": "HOLD", "NEUTRAL": "HOLD", "持有": "HOLD", "中性": "HOLD",
		"SELL": "SELL", "卖出": "SELL", "卖": "SELL",
		"STRONG_SELL": "STRONG_SELL", "STRONG SELL": "STRONG_SELL", "SS": "STRONG_SELL",
		"garbage": "HOLD", "": "HOLD",
	} {
		got := symbolItems(t, `[{"symbol":"AAPL.US","rating":"`+input+`","reasoning":"r"}]`,
			nil, "fundamental_analyst", 0, false, "", false, false)
		item, _ := asMapValue(got[0])
		if item["rating"] != want {
			t.Errorf("rating %q => %v, want %q", input, item["rating"], want)
		}
	}
}

// fair_value 无法解析时归 0.0(契约要求数字,不能 null)。
func TestNormalizeSymbolItemsFairValue(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  float64
	}{
		{`245.0`, 245}, {`"245.0"`, 245}, {`"245.0 美元"`, 245},
		{`"n/a"`, 0}, {`null`, 0}, {`"$1,250.5"`, 1250.5},
	} {
		got := symbolItems(t, `[{"symbol":"AAPL.US","fair_value":`+tc.input+`,"reasoning":"r"}]`,
			nil, "fundamental_analyst", 0, false, "", false, false)
		item, _ := asMapValue(got[0])
		if item["fair_value"] != tc.want {
			t.Errorf("fair_value %s => %v, want %v", tc.input, item["fair_value"], tc.want)
		}
	}
	// 字段缺失时不创建。
	got := symbolItems(t, `[{"symbol":"AAPL.US","reasoning":"r"}]`,
		nil, "fundamental_analyst", 0, false, "", false, false)
	item, _ := asMapValue(got[0])
	if _, present := item["fair_value"]; present {
		t.Error("absent fair_value must not be created")
	}
}

// reasoning 为空时补占位文案。
func TestNormalizeSymbolItemsReasoningFallback(t *testing.T) {
	for _, raw := range []string{
		`[{"symbol":"AAPL.US"}]`,
		`[{"symbol":"AAPL.US","reasoning":""}]`,
		`[{"symbol":"AAPL.US","reasoning":"   "}]`,
		`[{"symbol":"AAPL.US","reasoning":null}]`,
	} {
		got := symbolItems(t, raw, nil, "fundamental_analyst", 0, false, "", false, false)
		item, _ := asMapValue(got[0])
		if item["reasoning"] != placeholderNoReasoning {
			t.Errorf("input %s => reasoning %v, want the placeholder", raw, item["reasoning"])
		}
	}
	// 非空 reasoning 保持原样。
	got := symbolItems(t, `[{"symbol":"AAPL.US","reasoning":"真实理由"}]`,
		nil, "fundamental_analyst", 0, false, "", false, false)
	item, _ := asMapValue(got[0])
	if item["reasoning"] != "真实理由" {
		t.Errorf("reasoning = %v, want it preserved", item["reasoning"])
	}
}

// 风控 checks 无 symbol 语义:必须原样返回,不能被符号池过滤掉。
func TestNormalizeSymbolItemsSkipsRiskManager(t *testing.T) {
	raw := `[{"dimension":"var","status":"warn","reasoning":"r"},
	          {"dimension":"weird","status":"fail","reasoning":"r2"}]`
	got := symbolItems(t, raw, []string{"AAPL.US"}, "risk_manager", 0, false, "", false, false)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (checks must survive symbol filtering)", len(got))
	}
	// 项内归一化仍然生效。
	first, _ := asMapValue(got[0])
	if first["dimension"] != "VAR" || first["status"] != "WARN" {
		t.Errorf("first check = %v", first)
	}
	second, _ := asMapValue(got[1])
	if second["dimension"] != "OTHERS" || second["status"] != "CRIT" {
		t.Errorf("second check = %v", second)
	}
}

// 没有符号池时不做过滤/补全。
func TestNormalizeSymbolItemsWithoutPool(t *testing.T) {
	raw := `[{"symbol":"UNKNOWN.US","reasoning":"r"},{"symbol":"OTHER.US","reasoning":"r"}]`
	got := symbolItems(t, raw, nil, "fundamental_analyst", 0, false, "", false, false)
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (no pool means no filtering)", len(got))
	}
}

// ---------------------------------------------------------------------------
// RESTRICTED:池外过滤 + 写法标准化 + 去重
// ---------------------------------------------------------------------------

// symbolsOf 抽出每项的标的代码,便于比较顺序。
func symbolsOf(items []any, key string) []string {
	var symbols []string
	for _, item := range items {
		object, ok := asMapValue(item)
		if !ok {
			continue
		}
		text, _ := object[key].(string)
		symbols = append(symbols, text)
	}
	return symbols
}

func TestNormalizeSymbolItemsRestrictedFiltersOutsidePool(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US"}
	raw := `[{"symbol":"aapl","reasoning":"r"},
	          {"symbol":"TSLA.US","reasoning":"outside the pool"},
	          {"symbol":"msft.us","reasoning":"r"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)

	want := []string{"AAPL.US", "MSFT.US"}
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, want) {
		t.Errorf("symbols = %v, want %v (TSLA filtered, writing standardized)", diff, want)
	}
}

// 重复标的只保留第一条。
func TestNormalizeSymbolItemsDedupes(t *testing.T) {
	pool := []string{"AAPL.US"}
	raw := `[{"symbol":"AAPL.US","reasoning":"first"},
	          {"symbol":"aapl","reasoning":"duplicate"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	item, _ := asMapValue(got[0])
	if item["reasoning"] != "first" {
		t.Errorf("reasoning = %v, want the first occurrence kept", item["reasoning"])
	}
}

// universeMode 缺省按 RESTRICTED 处理。
func TestNormalizeSymbolItemsDefaultsToRestricted(t *testing.T) {
	pool := []string{"AAPL.US"}
	raw := `[{"symbol":"TSLA.US","reasoning":"outside"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 0, false, "", false, false)
	// TSLA 被过滤,AAPL 被补位。
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, []string{"AAPL.US"}) {
		t.Errorf("symbols = %v, want [AAPL.US]", diff)
	}
}

// ---------------------------------------------------------------------------
// SEED:允许池外机会,不过滤、不补全、不截断
// ---------------------------------------------------------------------------

func TestNormalizeSymbolItemsSeedKeepsOutsidePool(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US"}
	raw := `[{"symbol":"aapl.us","reasoning":"r"},
	          {"symbol":"tsla.us","reasoning":"discovered outside the pool"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 0, false, "SEED", false, false)

	want := []string{"AAPL.US", "TSLA.US"}
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, want) {
		t.Errorf("symbols = %v, want %v (SEED keeps discoveries, uppercased)", diff, want)
	}
	// SEED 不补全池内缺失的 MSFT。
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (SEED must not backfill)", len(got))
	}
}

// SEED 大小写不敏感。
func TestNormalizeSymbolItemsSeedIsCaseInsensitive(t *testing.T) {
	got := symbolItems(t, `[{"symbol":"aapl.us","reasoning":"r"}]`,
		[]string{"AAPL.US"}, "fundamental_analyst", 0, false, "seed", false, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

// SEED 也去重(按大写形式)。
func TestNormalizeSymbolItemsSeedDedupes(t *testing.T) {
	raw := `[{"symbol":"tsla.us","reasoning":"first"},{"symbol":"TSLA.US","reasoning":"dup"}]`
	got := symbolItems(t, raw, []string{"AAPL.US"}, "fundamental_analyst", 0, false, "SEED", false, false)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
}

// SEED 下空标的会被丢弃(trimUpper 后为空串,仍会占一个去重位)。
func TestNormalizeSymbolItemsSeedEmptySymbol(t *testing.T) {
	raw := `[{"symbol":"","reasoning":"r"},{"symbol":"TSLA.US","reasoning":"r"}]`
	got := symbolItems(t, raw, []string{"AAPL.US"}, "fundamental_analyst", 0, false, "SEED", false, false)
	// 空串与 TSLA 各占一条(rs 同此:SEED 只做 uppercase,不校验非空)。
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

// ---------------------------------------------------------------------------
// RESTRICTED:占位补全与条数对齐
// ---------------------------------------------------------------------------

// 模型输出 0 条时也要按池补全(否则 EXACT_ITEMS 断言会挂)。
func TestNormalizeSymbolItemsBackfillsFromEmpty(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US", "TSLA.US"}
	got := symbolItems(t, `[]`, pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, pool) {
		t.Errorf("symbols = %v, want the full pool %v", diff, pool)
	}
	// 补位项必须满足契约必填字段。
	for _, item := range got {
		object, _ := asMapValue(item)
		if object["rating"] != "HOLD" || object["fair_value"] != 0.0 {
			t.Errorf("placeholder = %v", object)
		}
		if object["reasoning"] != placeholderNoAssessment {
			t.Errorf("placeholder reasoning = %v", object["reasoning"])
		}
	}
}

// 部分缺失时按池顺序补齐。
func TestNormalizeSymbolItemsBackfillsMissingInPoolOrder(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US", "TSLA.US"}
	raw := `[{"symbol":"TSLA.US","reasoning":"model output"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)
	// 已有的排在前,补位的按池顺序追加。
	want := []string{"TSLA.US", "AAPL.US", "MSFT.US"}
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, want) {
		t.Errorf("symbols = %v, want %v", diff, want)
	}
}

// targetCount 在 1..len(pool) 内时按它对齐(精选场景)。
func TestNormalizeSymbolItemsHonoursTargetCount(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US", "TSLA.US", "NVDA.US"}
	got := symbolItems(t, `[]`, pool, "fundamental_analyst", 2, true, "RESTRICTED", false, false)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, []string{"AAPL.US", "MSFT.US"}) {
		t.Errorf("symbols = %v, want the first two pool entries", diff)
	}
}

// 超出模型输出时截断到 targetCount。
func TestNormalizeSymbolItemsTruncatesToTargetCount(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US", "TSLA.US"}
	raw := `[{"symbol":"AAPL.US","reasoning":"r"},
	          {"symbol":"MSFT.US","reasoning":"r"},
	          {"symbol":"TSLA.US","reasoning":"r"}]`
	got := symbolItems(t, raw, pool, "fundamental_analyst", 2, true, "RESTRICTED", false, false)
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (truncated)", len(got))
	}
}

// 越界的 targetCount 退回全覆盖。
func TestNormalizeSymbolItemsIgnoresOutOfRangeTargetCount(t *testing.T) {
	pool := []string{"AAPL.US", "MSFT.US"}
	for _, target := range []int{0, -1, 3, 99} {
		got := symbolItems(t, `[]`, pool, "fundamental_analyst", target, true, "RESTRICTED", false, false)
		if len(got) != len(pool) {
			t.Errorf("targetCount %d => len %d, want the full pool (%d)", target, len(got), len(pool))
		}
	}
}

// ---------------------------------------------------------------------------
// 各岗位的占位项形状
// ---------------------------------------------------------------------------

func TestPlaceholderItemShapes(t *testing.T) {
	for _, tc := range []struct {
		role      string
		isFinal   bool
		symbolKey string
		required  []string
	}{
		{"fundamental_analyst", false, "symbol", assessmentRequired},
		{"technical_analyst", false, "symbol", signalRequired},
		{"strategist", false, "symbol", strategySignalRequired},
		{"execution_trader", false, "symbol", executionOrderRequired},
		{"portfolio_manager", false, "symbol", pmOrderRequired},
		{"portfolio_manager", true, "ticker", pmFinalOrderRequired},
	} {
		name := tc.role
		if tc.isFinal {
			name += "_final"
		}
		t.Run(name, func(t *testing.T) {
			placeholder, ok := placeholderItem(tc.role, "0700.HK", tc.isFinal)
			if !ok {
				t.Fatalf("%s should produce a placeholder", tc.role)
			}
			// 占位项必须自带岗位契约的全部必填字段。
			for _, field := range tc.required {
				if _, present := placeholder[field]; !present {
					t.Errorf("placeholder missing required field %q: %v", field, placeholder)
				}
			}
			if placeholder[tc.symbolKey] != "0700.HK" {
				t.Errorf("%s = %v, want 0700.HK", tc.symbolKey, placeholder[tc.symbolKey])
			}
			// 市场按后缀推断。
			if market, present := placeholder["market"]; present && market != "HK" {
				t.Errorf("market = %v, want HK", market)
			}
			// 占位项必须通过 schema 校验。
			payload := map[string]any{}
			arrayKey, _, _ := primaryArrayForRole(tc.role, tc.isFinal)
			payload[arrayKey] = []any{placeholder}
			for _, field := range requiredForRole(tc.role, tc.isFinal) {
				if _, present := payload[field]; !present {
					payload[field] = "filled by the caller"
				}
			}
			if missing := missingRoleFields(payload, tc.role, tc.isFinal); len(missing) != 0 {
				t.Errorf("placeholder fails schema validation: %v", missing)
			}
		})
	}
}

// 未收录岗位不产出占位项。
func TestPlaceholderItemUnknownRole(t *testing.T) {
	if _, ok := placeholderItem("quant_trading_agent", "AAPL.US", false); ok {
		t.Error("an unknown role must not produce a placeholder")
	}
}

// 补位项也走 order 归一化,保证 MARKET 单没有限价。
func TestNormalizeSymbolItemsExecutionPlaceholderIsContractValid(t *testing.T) {
	pool := []string{"AAPL.US"}
	got := symbolItems(t, `[]`, pool, "execution_trader", 0, false, "RESTRICTED", false, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	order, _ := asMapValue(got[0])
	if order["order_type"] != "MARKET" {
		t.Errorf("order_type = %v, want MARKET", order["order_type"])
	}
	if order["limit_price"] != nil {
		t.Errorf("limit_price = %v, want nil for a MARKET order", order["limit_price"])
	}
	if order["quantity"] != 1 {
		t.Errorf("quantity = %v, want 1", order["quantity"])
	}
}

// ---------------------------------------------------------------------------
// ticker vs symbol
// ---------------------------------------------------------------------------

// isFinal 时标的写进 ticker;非 final 时移除 ticker 改用 symbol。
func TestNormalizeSymbolItemsTickerHandling(t *testing.T) {
	pool := []string{"AAPL.US"}

	// hasTicker=true 且 isFinal=true:从 ticker 读,写回 ticker。
	got := symbolItems(t, `[{"ticker":"aapl","reasoning":"r"}]`,
		pool, "portfolio_manager", 0, false, "RESTRICTED", true, true)
	item, _ := asMapValue(got[0])
	if item["ticker"] != "AAPL.US" {
		t.Errorf("ticker = %v, want AAPL.US", item["ticker"])
	}

	// isFinal=false:即使模型给了 ticker,也要移除并改用 symbol。
	got = symbolItems(t, `[{"ticker":"aapl","reasoning":"r"}]`,
		pool, "portfolio_manager", 0, false, "RESTRICTED", false, true)
	item, _ = asMapValue(got[0])
	if _, present := item["ticker"]; present {
		t.Errorf("process mode must drop ticker: %v", item)
	}
	if item["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v, want AAPL.US", item["symbol"])
	}
}

// hasTicker=false 时从 symbol 读,即使对象里也有 ticker。
func TestNormalizeSymbolItemsReadsSymbolWhenHasTickerIsFalse(t *testing.T) {
	pool := []string{"AAPL.US"}
	got := symbolItems(t, `[{"symbol":"aapl","ticker":"WRONG.US","reasoning":"r"}]`,
		pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	item, _ := asMapValue(got[0])
	if item["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v, want AAPL.US", item["symbol"])
	}
}

// 非对象项在有池时会被丢弃(拿不到标的)。
func TestNormalizeSymbolItemsDropsNonObjectsWhenPooled(t *testing.T) {
	pool := []string{"AAPL.US"}
	got := symbolItems(t, `["oops", 42, {"symbol":"AAPL.US","reasoning":"r"}]`,
		pool, "fundamental_analyst", 0, false, "RESTRICTED", false, false)
	if diff := symbolsOf(got, "symbol"); !slicesEqual(diff, []string{"AAPL.US"}) {
		t.Errorf("symbols = %v, want [AAPL.US]", diff)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
