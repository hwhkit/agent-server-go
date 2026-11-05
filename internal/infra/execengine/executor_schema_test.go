package execengine

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func newStringReader(text string) *strings.Reader { return strings.NewReader(text) }

func math_Inf(sign int) float64 { return math.Inf(sign) }
func math_NaN() float64         { return math.NaN() }

// decodeJSONValue 把 JSON 文本解析为 any,便于用真实报文形态写断言。
func decodeJSONValue(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return value
}

// ---------------------------------------------------------------------------
// requiredForRole
// ---------------------------------------------------------------------------

func TestRequiredForRole(t *testing.T) {
	for _, tc := range []struct {
		role    string
		isFinal bool
		want    []string
	}{
		{"risk_manager", false, []string{"gate", "checks"}},
		{"execution_trader", false, []string{"orders"}},
		{"portfolio_manager", true, []string{"timestamp", "orders", "report_markdown"}},
		{"portfolio_manager", false, []string{"decision_type", "rationale"}},
		{"strategist", false, []string{"signals", "rationale"}},
		{"technical_analyst", false, []string{"signals"}},
		{"fundamental_analyst", false, []string{"assessments"}},
		{"", false, []string{"assessments"}},
		{"unknown_role", false, []string{"assessments"}},
		// 首尾空白不影响匹配。
		{"  risk_manager  ", false, []string{"gate", "checks"}},
	} {
		got := requiredForRole(tc.role, tc.isFinal)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("requiredForRole(%q, %t) = %v, want %v", tc.role, tc.isFinal, got, tc.want)
		}
	}
	// isFinal 只影响组合经理。
	for _, role := range []string{"risk_manager", "strategist", "technical_analyst", "execution_trader"} {
		if !reflect.DeepEqual(requiredForRole(role, false), requiredForRole(role, true)) {
			t.Errorf("isFinal must not change requirements for %q", role)
		}
	}
}

// ---------------------------------------------------------------------------
// missingRoleFields:顶层字段
// ---------------------------------------------------------------------------

// 非对象结果 → 全部必填字段都算缺失。
func TestMissingRoleFieldsNonObjectPayload(t *testing.T) {
	for _, candidate := range []any{
		nil, "text", 42.0, []any{1, 2},
		decodeJSONValue(t, `{"payload_result": "not an object"}`),
		decodeJSONValue(t, `{"payload_result": null}`),
	} {
		// 这条早返回路径不排序,直接给出 requiredForRole 的声明顺序(与 rs 一致)。
		got := missingRoleFields(candidate, "risk_manager", false)
		want := []string{"gate", "checks"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("candidate %v => %v, want %v", candidate, got, want)
		}
	}
}

// payload_result 包裹与裸对象等价。
func TestMissingRoleFieldsUnwrapsPayloadResult(t *testing.T) {
	wrapped := decodeJSONValue(t, `{"payload_result":{"assessments":[]}}`)
	bare := decodeJSONValue(t, `{"assessments":[]}`)
	if !reflect.DeepEqual(
		missingRoleFields(wrapped, "fundamental_analyst", false),
		missingRoleFields(bare, "fundamental_analyst", false),
	) {
		t.Error("payload_result wrapping must not change the verdict")
	}
}

// 缺失 / null / 空白字符串都算缺失。
func TestMissingRoleFieldsTopLevelBlankAndNull(t *testing.T) {
	for name, raw := range map[string]string{
		"absent":       `{}`,
		"null":         `{"decision_type":null,"rationale":null}`,
		"blank string": `{"decision_type":"   ","rationale":"\t"}`,
	} {
		got := missingRoleFields(decodeJSONValue(t, raw), "portfolio_manager", false)
		for _, want := range []string{"decision_type", "rationale"} {
			if !slices.Contains(got, want) {
				t.Errorf("%s: missing %q not reported, got %v", name, want, got)
			}
		}
	}
}

// 空主数组只在风控 checks 上算缺失;其它岗位是合法失败结果。
func TestMissingRoleFieldsEmptyPrimaryArray(t *testing.T) {
	risk := missingRoleFields(decodeJSONValue(t, `{"gate":"PASS","checks":[]}`), "risk_manager", false)
	if !slices.Contains(risk, "checks") {
		t.Errorf("an empty risk checks array must be reported, got %v", risk)
	}

	for _, tc := range []struct{ role, raw string }{
		{"fundamental_analyst", `{"assessments":[]}`},
		{"technical_analyst", `{"signals":[]}`},
		{"strategist", `{"signals":[],"rationale":"no signal"}`},
		{"execution_trader", `{"orders":[]}`},
	} {
		if got := missingRoleFields(decodeJSONValue(t, tc.raw), tc.role, false); len(got) != 0 {
			t.Errorf("%s: an empty primary array is a legal failure result, got %v", tc.role, got)
		}
	}
}

// ---------------------------------------------------------------------------
// missingRoleFields:数组项
// ---------------------------------------------------------------------------

func TestMissingRoleFieldsNonObjectItem(t *testing.T) {
	got := missingRoleFields(decodeJSONValue(t, `{"assessments":["oops", 42]}`), "fundamental_analyst", false)
	for _, want := range []string{"assessments[0](object)", "assessments[1](object)"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q, got %v", want, got)
		}
	}
}

func TestMissingRoleFieldsAssessmentItems(t *testing.T) {
	raw := `{"assessments":[
		{"symbol":"AAPL.US","rating":"BUY","fair_value":245.0,"reasoning":"ok"},
		{"symbol":"MSFT.US","rating":"BUY","fair_value":null,"reasoning":"  "}
	]}`
	got := missingRoleFields(decodeJSONValue(t, raw), "fundamental_analyst", false)
	for _, want := range []string{"assessments[1].fair_value", "assessments[1].reasoning"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q, got %v", want, got)
		}
	}
	for _, unwanted := range []string{"assessments[0].symbol", "assessments[0].reasoning"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("valid item must not be flagged: %q in %v", unwanted, got)
		}
	}
}

// 契约允许为 null 的三个限价字段不算缺失。
func TestMissingRoleFieldsNullableLimitPrices(t *testing.T) {
	raw := `{"orders":[{
		"market":"US","symbol":"AAPL.US","direction":"BUY","order_type":"MARKET",
		"limit_price":null,"quantity":100,
		"take_profit_trigger_price":0,"take_profit_order_style":"MARKET","take_profit_limit_price":null,
		"stop_loss_trigger_price":0,"stop_loss_order_style":"MARKET","stop_loss_limit_price":null,
		"reasoning":"ok"
	}]}`
	if got := missingRoleFields(decodeJSONValue(t, raw), "execution_trader", false); len(got) != 0 {
		t.Errorf("nullable limit prices must be accepted, got %v", got)
	}
}

// 非 nullable 字段为 null 仍然算缺失。
func TestMissingRoleFieldsExecutionOrderRequiresAllThirteen(t *testing.T) {
	got := missingRoleFields(decodeJSONValue(t, `{"orders":[{}]}`), "execution_trader", false)
	// 13 个必填字段里有 3 个可为 null,因此空对象应报 10 个。
	want := 10
	count := 0
	for _, field := range got {
		if len(field) > 9 && field[:9] == "orders[0]" {
			count++
		}
	}
	if count != want {
		t.Errorf("empty execution order reported %d item fields, want %d: %v", count, want, got)
	}
	for _, nullable := range []string{
		"orders[0].limit_price", "orders[0].take_profit_limit_price", "orders[0].stop_loss_limit_price",
	} {
		if slices.Contains(got, nullable) {
			t.Errorf("nullable field %q must not be reported: %v", nullable, got)
		}
	}
}

// 组合经理两个职能用不同的订单字段集。
func TestMissingRoleFieldsPortfolioOrderFieldSets(t *testing.T) {
	// 职能一只要 6 个字段。
	process := `{"decision_type":"REBALANCE","rationale":"r","orders":[
		{"symbol":"AAPL.US","market":"US","direction":"BUY","order_type":"MARKET","quantity":100,"reasoning":"ok"}
	]}`
	if got := missingRoleFields(decodeJSONValue(t, process), "portfolio_manager", false); len(got) != 0 {
		t.Errorf("process-mode order should satisfy the 6-field set, got %v", got)
	}

	// 同一条订单在职能二下会缺 time/ticker/name 等。
	final := `{"timestamp":"2026-01-01T00:00:00Z","report_markdown":"md","orders":[
		{"symbol":"AAPL.US","market":"US","direction":"BUY","order_type":"MARKET","quantity":100,"reasoning":"ok"}
	]}`
	got := missingRoleFields(decodeJSONValue(t, final), "portfolio_manager", true)
	for _, want := range []string{
		"orders[0].time", "orders[0].ticker", "orders[0].name",
		"orders[0].time_in_force", "orders[0].execution_instruction",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("final-mode order missing %q, got %v", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 附加规则 1:technical_analyst 的 entry_zone 形状
// ---------------------------------------------------------------------------

func TestMissingRoleFieldsEntryZoneShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entryZone string
		valid     bool
	}{
		{"empty array", `[]`, true},
		{"two numbers", `[240.0, 242.0]`, true},
		{"two numeric strings", `["240.0", "242.0"]`, true},
		{"one number", `[240.0]`, false},
		{"three numbers", `[1, 2, 3]`, false},
		{"descriptive text", `"240 到 242"`, false},
		{"object", `{"low":240,"high":242}`, false},
		{"non-numeric entries", `["low", "high"]`, false},
		{"null", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"signals":[{"symbol":"AAPL.US","bias":"NEUTRAL","reasoning":"r","entry_zone":` +
				tc.entryZone + `}]}`
			got := missingRoleFields(decodeJSONValue(t, raw), "technical_analyst", false)
			flagged := slices.Contains(got, "signals[0].entry_zone(shape)")
			if tc.valid && flagged {
				t.Errorf("entry_zone %s should be accepted, got %v", tc.entryZone, got)
			}
			if !tc.valid && !flagged {
				t.Errorf("entry_zone %s should be rejected, got %v", tc.entryZone, got)
			}
		})
	}
}

// entry_zone 完全缺失时只由必填字段检查报告,不额外报形状错误。
func TestMissingRoleFieldsAbsentEntryZoneIsNotAShapeError(t *testing.T) {
	raw := `{"signals":[{"symbol":"AAPL.US","bias":"NEUTRAL","reasoning":"r"}]}`
	got := missingRoleFields(decodeJSONValue(t, raw), "technical_analyst", false)
	if slices.Contains(got, "signals[0].entry_zone(shape)") {
		t.Errorf("an absent entry_zone must not be a shape error: %v", got)
	}
	if !slices.Contains(got, "signals[0].entry_zone") {
		t.Errorf("an absent entry_zone must be reported as missing: %v", got)
	}
}

// 形状检查只对技术分析师生效。
func TestMissingRoleFieldsEntryZoneShapeOnlyForTechnical(t *testing.T) {
	raw := `{"signals":[{"symbol":"AAPL.US","direction":"BUY","reasoning":"r","entry_zone":"bad"}],"rationale":"r"}`
	got := missingRoleFields(decodeJSONValue(t, raw), "strategist", false)
	if slices.Contains(got, "signals[0].entry_zone(shape)") {
		t.Errorf("strategist must not enforce entry_zone shape: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 附加规则 2:组合经理职能一的 orders|target_allocation
// ---------------------------------------------------------------------------

func TestMissingRoleFieldsPortfolioRequiresOrdersOrAllocation(t *testing.T) {
	base := `{"decision_type":"NO_ACTION","rationale":"r"`

	// 两者都空 → 报错。
	for name, tail := range map[string]string{
		"both absent": `}`,
		"both empty":  `,"orders":[],"target_allocation":[]}`,
	} {
		got := missingRoleFields(decodeJSONValue(t, base+tail), "portfolio_manager", false)
		if !slices.Contains(got, "orders|target_allocation(non-empty)") {
			t.Errorf("%s: expected the non-empty rule to fire, got %v", name, got)
		}
	}

	// 仅 target_allocation 非空 → 通过(NO_ACTION 用它表达「维持现状」)。
	allocationOnly := base + `,"orders":[],"target_allocation":[
		{"symbol":"AAPL.US","target_weight_pct":12.5,"reasoning":"hold"}]}`
	if got := missingRoleFields(decodeJSONValue(t, allocationOnly), "portfolio_manager", false); len(got) != 0 {
		t.Errorf("a non-empty target_allocation should satisfy the rule, got %v", got)
	}

	// 仅 orders 非空 → 通过。
	ordersOnly := base + `,"orders":[
		{"symbol":"AAPL.US","market":"US","direction":"BUY","order_type":"MARKET","quantity":1,"reasoning":"r"}]}`
	if got := missingRoleFields(decodeJSONValue(t, ordersOnly), "portfolio_manager", false); len(got) != 0 {
		t.Errorf("a non-empty orders array should satisfy the rule, got %v", got)
	}
}

// target_allocation 每项的三要素只看缺失/null(空白字符串允许)。
func TestMissingRoleFieldsAllocationItemFields(t *testing.T) {
	raw := `{"decision_type":"REBALANCE","rationale":"r","target_allocation":[
		{"symbol":"AAPL.US","target_weight_pct":10,"reasoning":"ok"},
		{"symbol":null,"reasoning":"missing weight"},
		{"symbol":"MSFT.US","target_weight_pct":5,"reasoning":"   "}
	]}`
	got := missingRoleFields(decodeJSONValue(t, raw), "portfolio_manager", false)
	for _, want := range []string{
		"target_allocation[1].symbol", "target_allocation[1].target_weight_pct",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q, got %v", want, got)
		}
	}
	// 空白 reasoning 在配置项上是允许的。
	if slices.Contains(got, "target_allocation[2].reasoning") {
		t.Errorf("a blank allocation reasoning must not be flagged: %v", got)
	}
}

// 职能二不做 orders|target_allocation 检查。
func TestMissingRoleFieldsFinalModeSkipsAllocationRule(t *testing.T) {
	raw := `{"timestamp":"t","orders":[],"report_markdown":"md"}`
	got := missingRoleFields(decodeJSONValue(t, raw), "portfolio_manager", true)
	if slices.Contains(got, "orders|target_allocation(non-empty)") {
		t.Errorf("final mode must not enforce the allocation rule: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 附加规则 3:风控 gate 为 WARN/BREACH 时必须有对冲建议
// ---------------------------------------------------------------------------

func TestMissingRoleFieldsRiskHedgingSuggestions(t *testing.T) {
	check := `{"dimension":"VAR","status":"WARN","reasoning":"r"}`
	for _, tc := range []struct {
		name       string
		gate       string
		hedging    string
		wantFlaged bool
	}{
		{"WARN without hedging", "WARN", ``, true},
		{"WARN with empty hedging", "WARN", `,"hedging_suggestions":[]`, true},
		{"WARN with hedging", "WARN", `,"hedging_suggestions":["reduce exposure"]`, false},
		{"BREACH without hedging", "BREACH", ``, true},
		{"BREACH with hedging", "BREACH", `,"hedging_suggestions":["hedge"]`, false},
		{"PASS without hedging", "PASS", ``, false},
		{"non-array hedging on WARN", "WARN", `,"hedging_suggestions":"reduce"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"gate":"` + tc.gate + `","checks":[` + check + `]` + tc.hedging + `}`
			got := missingRoleFields(decodeJSONValue(t, raw), "risk_manager", false)
			flagged := slices.Contains(got, "hedging_suggestions(non-empty)")
			if flagged != tc.wantFlaged {
				t.Errorf("gate=%s hedging=%q => flagged=%t, want %t (%v)",
					tc.gate, tc.hedging, flagged, tc.wantFlaged, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 排序与去重
// ---------------------------------------------------------------------------

func TestMissingRoleFieldsIsSortedAndDeduped(t *testing.T) {
	// 两条订单各缺同名字段,应各自带下标而不合并;整体有序无重复。
	raw := `{"orders":[{},{}]}`
	got := missingRoleFields(decodeJSONValue(t, raw), "execution_trader", false)
	if !slices.IsSorted(got) {
		t.Errorf("result must be sorted: %v", got)
	}
	if len(got) != len(slices.Compact(slices.Clone(got))) {
		t.Errorf("result must be deduped: %v", got)
	}
	if !slices.Contains(got, "orders[0].market") || !slices.Contains(got, "orders[1].market") {
		t.Errorf("per-item fields must keep their index: %v", got)
	}
}

// 完全合法的结果返回空。
func TestMissingRoleFieldsAcceptsValidPayloads(t *testing.T) {
	for role, raw := range map[string]string{
		"fundamental_analyst": `{"assessments":[{"symbol":"AAPL.US","rating":"BUY","fair_value":245,"reasoning":"r"}]}`,
		"technical_analyst":   `{"signals":[{"symbol":"AAPL.US","bias":"LONG_BIAS","entry_zone":[240,242],"reasoning":"r"}]}`,
		"strategist":          `{"signals":[{"symbol":"AAPL.US","direction":"BUY","reasoning":"r"}],"rationale":"r"}`,
		"risk_manager":        `{"gate":"PASS","checks":[{"dimension":"VAR","status":"OK","reasoning":"r"}]}`,
	} {
		if got := missingRoleFields(decodeJSONValue(t, raw), role, false); len(got) != 0 {
			t.Errorf("%s: valid payload flagged %v", role, got)
		}
	}
}

// ---------------------------------------------------------------------------
// roleSchema
// ---------------------------------------------------------------------------

func TestRoleSchemaCoveredRoles(t *testing.T) {
	for _, tc := range []struct {
		role          string
		isFinal       bool
		itemField     string
		wantResultKey string
		itemKeyCount  int
	}{
		{"fundamental_analyst", false, "assessments", "summary", 12},
		{"technical_analyst", false, "signals", "summary", 16},
		{"strategist", false, "signals", "rationale", 7},
		{"risk_manager", false, "checks", "gate", 6},
		{"execution_trader", false, "orders", "execution_summary", 17},
		{"portfolio_manager", true, "orders", "report_markdown", 19},
		{"portfolio_manager", false, "orders", "decision_type", 8},
	} {
		t.Run(tc.role, func(t *testing.T) {
			spec, ok := roleSchema(tc.role, tc.isFinal)
			if !ok {
				t.Fatalf("roleSchema(%q) should be covered", tc.role)
			}
			if spec.itemField != tc.itemField {
				t.Errorf("itemField = %q, want %q", spec.itemField, tc.itemField)
			}
			if !slices.Contains(spec.resultKeys, tc.wantResultKey) {
				t.Errorf("resultKeys %v should contain %q", spec.resultKeys, tc.wantResultKey)
			}
			if len(spec.itemKeys) != tc.itemKeyCount {
				t.Errorf("len(itemKeys) = %d, want %d", len(spec.itemKeys), tc.itemKeyCount)
			}
			// 每个岗位的顶层白名单都必须放行 evidenceRefs 与 extensions。
			for _, key := range []string{"evidenceRefs", "extensions"} {
				if !slices.Contains(spec.resultKeys, key) {
					t.Errorf("resultKeys %v must allow %q", spec.resultKeys, key)
				}
			}
			// 主数组字段本身也必须在顶层白名单里。
			if !slices.Contains(spec.resultKeys, spec.itemField) {
				t.Errorf("resultKeys %v must allow the primary array %q", spec.resultKeys, spec.itemField)
			}
		})
	}
}

// 空岗位默认基本面(与 resolveRoleContract 一致),否则会跳过裁剪与归一化。
func TestRoleSchemaEmptyRoleDefaultsToFundamental(t *testing.T) {
	for _, role := range []string{"", "   ", "\t"} {
		spec, ok := roleSchema(role, false)
		if !ok {
			t.Fatalf("empty role %q must fall back to fundamental_analyst", role)
		}
		if spec.itemField != "assessments" {
			t.Errorf("itemField = %q, want assessments", spec.itemField)
		}
	}
}

// 未收录岗位返回 ok=false:保持原样,只做 evidenceRefs/extensions 规范化。
func TestRoleSchemaUnknownRole(t *testing.T) {
	if _, ok := roleSchema("quant_trading_agent", false); ok {
		t.Error("an unknown role must not be covered")
	}
}

// 组合经理两个职能的白名单必须不同。
func TestRoleSchemaPortfolioBranchesDiffer(t *testing.T) {
	process, _ := roleSchema("portfolio_manager", false)
	final, _ := roleSchema("portfolio_manager", true)
	if slices.Equal(process.resultKeys, final.resultKeys) {
		t.Error("process and final result keys must differ")
	}
	if slices.Equal(process.itemKeys, final.itemKeys) {
		t.Error("process and final item keys must differ")
	}
	// 职能二用 ticker,职能一用 symbol。
	if !slices.Contains(final.itemKeys, "ticker") {
		t.Error("final orders must allow ticker")
	}
	if slices.Contains(process.itemKeys, "ticker") {
		t.Error("process orders must not allow ticker")
	}
	if !slices.Contains(process.itemKeys, "weight_delta_pct") {
		t.Error("process orders must allow weight_delta_pct")
	}
}

// 白名单必须覆盖对应的必填字段,否则裁剪会把必填字段删掉。
func TestRoleSchemaItemKeysCoverRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		role     string
		isFinal  bool
		required []string
	}{
		{"fundamental_analyst", false, assessmentRequired},
		{"technical_analyst", false, signalRequired},
		{"strategist", false, strategySignalRequired},
		{"risk_manager", false, checkRequired},
		{"execution_trader", false, executionOrderRequired},
		{"portfolio_manager", false, pmOrderRequired},
		{"portfolio_manager", true, pmFinalOrderRequired},
	} {
		spec, ok := roleSchema(tc.role, tc.isFinal)
		if !ok {
			t.Fatalf("%s should be covered", tc.role)
		}
		for _, field := range tc.required {
			if !slices.Contains(spec.itemKeys, field) {
				t.Errorf("%s (isFinal=%t): itemKeys must allow required field %q",
					tc.role, tc.isFinal, field)
			}
		}
	}
}

// 顶层白名单必须覆盖 requiredForRole,否则裁剪会删掉必填顶层字段。
func TestRoleSchemaResultKeysCoverRequiredTopLevel(t *testing.T) {
	for _, tc := range []struct {
		role    string
		isFinal bool
	}{
		{"fundamental_analyst", false}, {"technical_analyst", false}, {"strategist", false},
		{"risk_manager", false}, {"execution_trader", false},
		{"portfolio_manager", false}, {"portfolio_manager", true},
	} {
		spec, ok := roleSchema(tc.role, tc.isFinal)
		if !ok {
			t.Fatalf("%s should be covered", tc.role)
		}
		for _, field := range requiredForRole(tc.role, tc.isFinal) {
			if !slices.Contains(spec.resultKeys, field) {
				t.Errorf("%s (isFinal=%t): resultKeys must allow required field %q",
					tc.role, tc.isFinal, field)
			}
		}
	}
}

func TestRetainKeys(t *testing.T) {
	object := map[string]any{"keep": 1, "also": 2, "drop": 3, "gone": 4}
	retainKeys(object, []string{"keep", "also", "absent"})
	if len(object) != 2 {
		t.Fatalf("object = %v, want only the allowed keys", object)
	}
	for _, key := range []string{"keep", "also"} {
		if _, present := object[key]; !present {
			t.Errorf("allowed key %q was dropped", key)
		}
	}
	for _, key := range []string{"drop", "gone"} {
		if _, present := object[key]; present {
			t.Errorf("disallowed key %q survived", key)
		}
	}
}

// ---------------------------------------------------------------------------
// valueAsFloat
// ---------------------------------------------------------------------------

func TestValueAsFloat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  float64
		ok    bool
	}{
		{"float", 245.5, 245.5, true},
		{"negative float", -12.25, -12.25, true},
		{"zero", 0.0, 0, true},
		{"int", 100, 100, true},
		{"numeric string", "245.0", 245, true},
		{"string with units", "245.0 美元", 245, true},
		{"string with currency prefix", "$245.5", 245.5, true},
		{"negative string", "-3.5", -3.5, true},
		{"percent string", "12.5%", 12.5, true},
		{"blank string", "   ", 0, false},
		{"non-numeric string", "high", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
		{"array", []any{1}, 0, false},
		{"object", map[string]any{"a": 1}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := valueAsFloat(tc.value)
			if ok != tc.ok {
				t.Fatalf("valueAsFloat(%v) ok = %t, want %t", tc.value, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("valueAsFloat(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// JSON 解析出的数字(float64)与 json.Number 都要能处理。
func TestValueAsFloatFromJSON(t *testing.T) {
	value := decodeJSONValue(t, `{"n": 245.5}`).(map[string]any)["n"]
	got, ok := valueAsFloat(value)
	if !ok || got != 245.5 {
		t.Errorf("JSON number => (%v, %t), want (245.5, true)", got, ok)
	}

	decoder := json.NewDecoder(newStringReader(`{"n": 42}`))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	got, ok = valueAsFloat(decoded["n"])
	if !ok || got != 42 {
		t.Errorf("json.Number => (%v, %t), want (42, true)", got, ok)
	}
}

// 非有限数一律拒绝(避免把 Inf/NaN 写进契约字段)。
func TestValueAsFloatRejectsNonFinite(t *testing.T) {
	for _, value := range []any{
		math_Inf(1), math_Inf(-1), math_NaN(),
	} {
		if _, ok := valueAsFloat(value); ok {
			t.Errorf("valueAsFloat(%v) must be rejected", value)
		}
	}
}
