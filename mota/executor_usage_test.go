package mota

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// usageJSON 把 JSON 文本解成 any(数字保留 json.Number),模拟真实执行器回执的
// 解码路径。测试全部走这条路径,避免手搭 map 时丢掉整数/浮点的区分。
func usageJSON(t *testing.T, text string) any {
	t.Helper()
	value, err := decodeJSONNumbers([]byte(text))
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return value
}

// jsonLines 把多段 JSON 拼成 JSONL 文本(每行一个事件,末尾带换行)。
func jsonLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// compactJSONLine 把测试里为了可读性折行写的 JSON 压成单行,便于拼进 JSONL。
func compactJSONLine(text string) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, []byte(text)); err != nil {
		panic("compactJSONLine: " + err.Error())
	}
	return buffer.String()
}

// nestedString 沿着 keys 逐层下钻并把末端读成字符串。
func nestedString(t *testing.T, value any, keys ...string) string {
	t.Helper()
	for _, key := range keys[:len(keys)-1] {
		next, ok := jsonField(value, key)
		if !ok {
			t.Fatalf("missing key %q in %#v", key, value)
		}
		value = next
	}
	text, ok := jsonStringField(value, keys[len(keys)-1])
	if !ok {
		t.Fatalf("key %q is not a string in %#v", keys[len(keys)-1], value)
	}
	return text
}

func wantUint64(t *testing.T, label string, got *uint64, want uint64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", label, want)
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", label, *got, want)
	}
}

func wantNilUint64(t *testing.T, label string, got *uint64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want nil", label, *got)
	}
}

// ---------------------------------------------------------------------------
// decodeJSONNumbers
// ---------------------------------------------------------------------------

func TestDecodeJSONNumbersKeepsIntegerLiterals(t *testing.T) {
	t.Parallel()
	// 9007199254740993 = 2^53+1,float64 无法精确表示;json.Number 必须保留原字面量。
	value := usageJSON(t, `{"n": 9007199254740993}`)
	object, ok := asJSONObject(value)
	if !ok {
		t.Fatal("expected an object")
	}
	number, ok := object["n"].(json.Number)
	if !ok {
		t.Fatalf("n is %T, want json.Number", object["n"])
	}
	if number.String() != "9007199254740993" {
		t.Errorf("n = %s, want 9007199254740993", number)
	}
	got, ok := jsonUint64(object["n"])
	if !ok || got != 9007199254740993 {
		t.Errorf("jsonUint64 = (%d, %v), want (9007199254740993, true)", got, ok)
	}
}

// 对齐 serde_json::from_str 的整串解析语义:首个值之后的残留内容必须报错。
func TestDecodeJSONNumbersRejectsTrailingContent(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`{} junk`, `1 2`, `{"a":1}{"b":2}`, `[] x`} {
		if _, err := decodeJSONNumbers([]byte(text)); err == nil {
			t.Errorf("%q: expected an error", text)
		}
	}
	for _, text := range []string{`{}`, ` {"a":1} `, `1`, `"s"`, `null`, `[1,2]`} {
		if _, err := decodeJSONNumbers([]byte(text)); err != nil {
			t.Errorf("%q: unexpected error %v", text, err)
		}
	}
}

// ---------------------------------------------------------------------------
// usageNumber / jsonUint64 / checkedSum / validateReportedTotal
// ---------------------------------------------------------------------------

func TestJSONUint64MatchesRustAsU64(t *testing.T) {
	t.Parallel()
	accepted := map[string]uint64{
		`0`:                    0,
		`12`:                   12,
		`18446744073709551615`: math.MaxUint64,
	}
	for text, want := range accepted {
		value := usageJSON(t, text)
		got, ok := jsonUint64(value)
		if !ok || got != want {
			t.Errorf("jsonUint64(%s) = (%d, %v), want (%d, true)", text, got, ok, want)
		}
	}
	// 小数、负数、指数写法、超范围、字符串、布尔、null 一律不成立。
	for _, text := range []string{
		`12.0`, `12.5`, `-1`, `1e3`, `18446744073709551616`,
		`"12"`, `true`, `null`, `[]`, `{}`,
	} {
		if _, ok := jsonUint64(usageJSON(t, text)); ok {
			t.Errorf("jsonUint64(%s) unexpectedly succeeded", text)
		}
	}
}

// 上游用默认 json.Unmarshal 解码时数字变成 float64,这条兼容口子只接受
// 有限、非负、整数值。
func TestJSONUint64FloatCompatibility(t *testing.T) {
	t.Parallel()
	if got, ok := jsonUint64(float64(42)); !ok || got != 42 {
		t.Errorf("float64(42) = (%d, %v), want (42, true)", got, ok)
	}
	for _, value := range []float64{
		-1, 12.5, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64,
	} {
		if _, ok := jsonUint64(value); ok {
			t.Errorf("float64(%v) unexpectedly succeeded", value)
		}
	}
	if got, ok := jsonUint64(int(7)); !ok || got != 7 {
		t.Errorf("int(7) = (%d, %v)", got, ok)
	}
	if _, ok := jsonUint64(int(-7)); ok {
		t.Error("int(-7) unexpectedly succeeded")
	}
	if got, ok := jsonUint64(uint64(math.MaxUint64)); !ok || got != math.MaxUint64 {
		t.Errorf("uint64 max = (%d, %v)", got, ok)
	}
}

// usageNumber 的 find_map 语义:某个 key 存在但值不可用时继续试下一个 key。
func TestUsageNumberSkipsUnusableKeys(t *testing.T) {
	t.Parallel()
	usage := usageJSON(t, `{"a": "not a number", "b": null, "c": 12.5, "d": 7}`)
	got, ok := usageNumber(usage, "a", "b", "c", "d")
	if !ok || got != 7 {
		t.Errorf("usageNumber = (%d, %v), want (7, true)", got, ok)
	}
	// 键顺序决定优先级。
	if got, _ := usageNumber(usageJSON(t, `{"x":1,"y":2}`), "y", "x"); got != 2 {
		t.Errorf("usageNumber prefers the first listed key, got %d", got)
	}
	// 非对象输入直接失败。
	for _, text := range []string{`[]`, `"s"`, `1`, `null`} {
		if _, ok := usageNumber(usageJSON(t, text), "a"); ok {
			t.Errorf("usageNumber(%s) unexpectedly succeeded", text)
		}
	}
	if _, ok := usageNumber(usageJSON(t, `{}`), "a", "b"); ok {
		t.Error("usageNumber on an empty object unexpectedly succeeded")
	}
}

func TestCheckedSum(t *testing.T) {
	t.Parallel()
	if got, ok := checkedSum(); !ok || got != 0 {
		t.Errorf("checkedSum() = (%d, %v), want (0, true)", got, ok)
	}
	if got, ok := checkedSum(1, 2, 3); !ok || got != 6 {
		t.Errorf("checkedSum(1,2,3) = (%d, %v), want (6, true)", got, ok)
	}
	if got, ok := checkedSum(math.MaxUint64, 0); !ok || got != math.MaxUint64 {
		t.Errorf("checkedSum(max,0) = (%d, %v)", got, ok)
	}
	if _, ok := checkedSum(math.MaxUint64, 1); ok {
		t.Error("checkedSum(max,1) must report overflow")
	}
	if _, ok := checkedSum(math.MaxUint64/2+1, math.MaxUint64/2+1); ok {
		t.Error("checkedSum must report overflow across the midpoint")
	}
}

// is_none_or:没有自报总量 → 通过;自报了 → 必须严格相等。
func TestValidateReportedTotal(t *testing.T) {
	t.Parallel()
	if !validateReportedTotal(usageJSON(t, `{}`), 150) {
		t.Error("a usage object without a reported total must pass")
	}
	for _, key := range []string{"total_tokens", "totalTokens", "total"} {
		if !validateReportedTotal(usageJSON(t, `{"`+key+`": 150}`), 150) {
			t.Errorf("%s = 150 must pass for expected 150", key)
		}
		if validateReportedTotal(usageJSON(t, `{"`+key+`": 149}`), 150) {
			t.Errorf("%s = 149 must fail for expected 150", key)
		}
	}
	// 自报总量不是整数时读不出来,等同于「没有自报」。
	if !validateReportedTotal(usageJSON(t, `{"total_tokens": "150"}`), 150) {
		t.Error("an unreadable reported total must be treated as absent")
	}
}

// ---------------------------------------------------------------------------
// usageFromGateway
// ---------------------------------------------------------------------------

// 对应 rs openai_gateway_usage_preserves_reported_details。
func TestOpenAIGatewayUsagePreservesReportedDetails(t *testing.T) {
	t.Parallel()
	envelope := usageJSON(t, `{
		"usage": {
			"prompt_tokens": 120,
			"completion_tokens": 30,
			"total_tokens": 150,
			"prompt_tokens_details": {"cached_tokens": 40},
			"completion_tokens_details": {"reasoning_tokens": 10}
		}
	}`)
	usage := usageFromGateway(envelope, GatewayProtocolOpenAI)
	if usage == nil {
		t.Fatal("expected valid OpenAI usage")
	}
	if usage.PromptTokens != 120 {
		t.Errorf("promptTokens = %d, want 120", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 40)
	if usage.CompletionTokens != 30 {
		t.Errorf("completionTokens = %d, want 30", usage.CompletionTokens)
	}
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 10)
	if usage.TotalTokens != 150 {
		t.Errorf("totalTokens = %d, want 150", usage.TotalTokens)
	}
	if usage.InvocationCount != 1 {
		t.Errorf("invocationCount = %d, want 1", usage.InvocationCount)
	}
	if usage.Estimated {
		t.Error("estimated must stay false")
	}
	if usage.Status != "FINAL" {
		t.Errorf("status = %q, want FINAL", usage.Status)
	}
}

// 对应 rs anthropic_gateway_usage_includes_cache_tokens_in_prompt_total。
func TestAnthropicGatewayUsageIncludesCacheTokensInPromptTotal(t *testing.T) {
	t.Parallel()
	envelope := usageJSON(t, `{
		"usage": {
			"input_tokens": 80,
			"cache_read_input_tokens": 25,
			"cache_creation_input_tokens": 5,
			"output_tokens": 20
		}
	}`)
	usage := usageFromGateway(envelope, GatewayProtocolAnthropic)
	if usage == nil {
		t.Fatal("expected valid Anthropic usage")
	}
	if usage.PromptTokens != 110 {
		t.Errorf("promptTokens = %d, want 110 (80+25+5)", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 25)
	if usage.CompletionTokens != 20 {
		t.Errorf("completionTokens = %d, want 20", usage.CompletionTokens)
	}
	if usage.TotalTokens != 130 {
		t.Errorf("totalTokens = %d, want 130", usage.TotalTokens)
	}
}

func TestUsageFromGatewayRejectsIncompleteReports(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"没有 usage 字段":          `{"choices": []}`,
		"usage 为 null":         `{"usage": null}`,
		"缺 prompt":             `{"usage": {"completion_tokens": 30}}`,
		"缺 completion":         `{"usage": {"prompt_tokens": 120}}`,
		"自报 total 与分项不一致":      `{"usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 151}}`,
		"prompt+completion 溢出": `{"usage": {"prompt_tokens": 18446744073709551615, "completion_tokens": 1}}`,
		"缓存超过 prompt 总量":       `{"usage": {"prompt_tokens": 10, "completion_tokens": 1, "cached_input_tokens": 11}}`,
	}
	for name, text := range cases {
		if usage := usageFromGateway(usageJSON(t, text), GatewayProtocolOpenAI); usage != nil {
			t.Errorf("%s: expected nil, got %+v", name, usage)
		}
	}
	// 未知协议不猜键名。
	if usage := usageFromGateway(usageJSON(t, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), GatewayProtocol("grpc")); usage != nil {
		t.Errorf("an unknown protocol must not produce usage, got %+v", usage)
	}
}

// OpenAI 分支的键名别名与平铺回退。
func TestUsageFromGatewayOpenAIKeyAliasesAndFlatFallbacks(t *testing.T) {
	t.Parallel()
	// input_tokens / output_tokens 是 Responses API 的别名。
	usage := usageFromGateway(usageJSON(t, `{"usage":{"input_tokens":10,"output_tokens":4}}`), GatewayProtocolOpenAI)
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 4 {
		t.Fatalf("Responses-style aliases not honoured: %+v", usage)
	}
	wantNilUint64(t, "cachedInputTokens", usage.CachedInputTokens)
	wantNilUint64(t, "reasoningTokens", usage.ReasoningTokens)

	// details 子对象存在但没有目标键时,回退到平铺键。
	usage = usageFromGateway(usageJSON(t, `{"usage":{
		"prompt_tokens": 20, "completion_tokens": 5,
		"prompt_tokens_details": {"audio_tokens": 3},
		"completion_tokens_details": {"audio_tokens": 1},
		"cached_input_tokens": 7, "reasoning_tokens": 2
	}}`), GatewayProtocolOpenAI)
	if usage == nil {
		t.Fatal("expected usage")
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 7)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 2)

	// details 子对象优先于平铺键。
	usage = usageFromGateway(usageJSON(t, `{"usage":{
		"prompt_tokens": 20, "completion_tokens": 5,
		"prompt_tokens_details": {"cached_tokens": 9},
		"completion_tokens_details": {"reasoning_tokens": 8},
		"cached_input_tokens": 7, "reasoning_tokens": 2
	}}`), GatewayProtocolOpenAI)
	if usage == nil {
		t.Fatal("expected usage")
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 9)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 8)
}

// ---------------------------------------------------------------------------
// usageFromAnthropic
// ---------------------------------------------------------------------------

// cached 只在 cache_read > 0 时上报:0 与「无缓存信息」是两种含义。
func TestUsageFromAnthropicReportsCacheOnlyWhenPositive(t *testing.T) {
	t.Parallel()
	usage := usageFromAnthropic(usageJSON(t, `{"input_tokens":10,"cache_read_input_tokens":0,"output_tokens":2}`), 1)
	if usage == nil {
		t.Fatal("expected usage")
	}
	wantNilUint64(t, "cachedInputTokens", usage.CachedInputTokens)
	if usage.PromptTokens != 10 {
		t.Errorf("promptTokens = %d, want 10", usage.PromptTokens)
	}

	usage = usageFromAnthropic(usageJSON(t, `{"input_tokens":10,"cache_read_input_tokens":3,"output_tokens":2}`), 1)
	if usage == nil {
		t.Fatal("expected usage")
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 3)
	if usage.PromptTokens != 13 {
		t.Errorf("promptTokens = %d, want 13", usage.PromptTokens)
	}
}

func TestUsageFromAnthropicKeyAliases(t *testing.T) {
	t.Parallel()
	// 短键名别名(input/output/cacheRead/cacheWrite/cache_read/cache_write)。
	usage := usageFromAnthropic(usageJSON(t, `{"input":10,"cacheRead":2,"cacheWrite":3,"output":4,"reasoningTokens":6}`), 2)
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 15 {
		t.Errorf("promptTokens = %d, want 15", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 2)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 6)
	if usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", usage.InvocationCount)
	}
	if usage.TotalTokens != 19 {
		t.Errorf("totalTokens = %d, want 19", usage.TotalTokens)
	}

	usage = usageFromAnthropic(usageJSON(t, `{"input":1,"cache_read":2,"cache_write":3,"output":4}`), 1)
	if usage == nil || usage.PromptTokens != 6 {
		t.Fatalf("snake_case cache aliases not honoured: %+v", usage)
	}
}

// 这条路径**不**校验自报 total(Anthropic 不返回 total_tokens),与 rs 一致。
func TestUsageFromAnthropicIgnoresReportedTotal(t *testing.T) {
	t.Parallel()
	usage := usageFromAnthropic(usageJSON(t, `{"input_tokens":10,"output_tokens":2,"total_tokens":999}`), 1)
	if usage == nil {
		t.Fatal("an inconsistent reported total must not be checked on this path")
	}
	if usage.TotalTokens != 12 {
		t.Errorf("totalTokens = %d, want 12 (computed, not reported)", usage.TotalTokens)
	}
}

func TestUsageFromAnthropicRejectsIncompleteReports(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"缺 input":     `{"output_tokens": 2}`,
		"缺 output":    `{"input_tokens": 10}`,
		"prompt 分项溢出": `{"input_tokens": 18446744073709551615, "cache_read_input_tokens": 1, "output_tokens": 1}`,
	}
	for name, text := range cases {
		if usage := usageFromAnthropic(usageJSON(t, text), 1); usage != nil {
			t.Errorf("%s: expected nil, got %+v", name, usage)
		}
	}
	// invocationCount 为 0 被 FinalReportedUsage 拒绝。
	if usage := usageFromAnthropic(usageJSON(t, `{"input_tokens":10,"output_tokens":2}`), 0); usage != nil {
		t.Errorf("invocationCount 0 must be rejected, got %+v", usage)
	}
	// 非对象输入。
	if usage := usageFromAnthropic(usageJSON(t, `"nope"`), 1); usage != nil {
		t.Errorf("a non-object usage must be rejected, got %+v", usage)
	}
}

// ---------------------------------------------------------------------------
// usageFromClaudeEnvelope
// ---------------------------------------------------------------------------

// 对应 rs claude_json_usage_reads_thinking_details。
func TestClaudeJSONUsageReadsThinkingDetails(t *testing.T) {
	t.Parallel()
	envelope := usageJSON(t, `{
		"result": "{\"ok\":true}",
		"is_error": false,
		"num_turns": 1,
		"usage": {
			"input_tokens": 92,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 0,
			"output_tokens": 5,
			"output_tokens_details": {"thinking_tokens": 9}
		}
	}`)
	usage := usageFromClaudeEnvelope(envelope)
	if usage == nil {
		t.Fatal("expected Claude usage")
	}
	if usage.PromptTokens != 92 {
		t.Errorf("promptTokens = %d, want 92", usage.PromptTokens)
	}
	if usage.CompletionTokens != 5 {
		t.Errorf("completionTokens = %d, want 5", usage.CompletionTokens)
	}
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 9)
	if usage.TotalTokens != 97 {
		t.Errorf("totalTokens = %d, want 97", usage.TotalTokens)
	}
	if usage.InvocationCount != 1 {
		t.Errorf("invocationCount = %d, want 1", usage.InvocationCount)
	}
	wantNilUint64(t, "cachedInputTokens", usage.CachedInputTokens)
}

func TestUsageFromClaudeEnvelopeSemantics(t *testing.T) {
	t.Parallel()
	// is_error 明确为 true → 整份丢弃。
	if usage := usageFromClaudeEnvelope(usageJSON(t, `{"is_error":true,"usage":{"input_tokens":1,"output_tokens":1}}`)); usage != nil {
		t.Errorf("is_error true must discard usage, got %+v", usage)
	}
	// is_error 为 false / 缺失 / 非布尔 → 不影响。
	for _, text := range []string{
		`{"is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"is_error":"true","usage":{"input_tokens":1,"output_tokens":1}}`,
	} {
		if usage := usageFromClaudeEnvelope(usageJSON(t, text)); usage == nil {
			t.Errorf("%s: expected usage", text)
		}
	}
	// num_turns / numTurns 作为调用次数,缺失按 1。
	for _, key := range []string{"num_turns", "numTurns"} {
		usage := usageFromClaudeEnvelope(usageJSON(t, `{"`+key+`":4,"usage":{"input_tokens":1,"output_tokens":1}}`))
		if usage == nil || usage.InvocationCount != 4 {
			t.Fatalf("%s: invocationCount not honoured: %+v", key, usage)
		}
	}
	usage := usageFromClaudeEnvelope(usageJSON(t, `{"usage":{"input_tokens":1,"output_tokens":1}}`))
	if usage == nil || usage.InvocationCount != 1 {
		t.Fatalf("missing turn count must default to 1: %+v", usage)
	}
	// 缺 usage → nil。
	if usage := usageFromClaudeEnvelope(usageJSON(t, `{"result":"x"}`)); usage != nil {
		t.Errorf("a missing usage object must yield nil, got %+v", usage)
	}
	// output_tokens_details 里 thinking_tokens 优先于 reasoning_tokens。
	usage = usageFromClaudeEnvelope(usageJSON(t, `{"usage":{"input_tokens":1,"output_tokens":1,
		"output_tokens_details":{"thinking_tokens":9,"reasoning_tokens":3}}}`))
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 9)
	usage = usageFromClaudeEnvelope(usageJSON(t, `{"usage":{"input_tokens":1,"output_tokens":1,
		"output_tokens_details":{"reasoning_tokens":3}}}`))
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 3)
	// details 里都没有时保留 usage 平铺键的结果。
	usage = usageFromClaudeEnvelope(usageJSON(t, `{"usage":{"input_tokens":1,"output_tokens":1,
		"reasoning_tokens":6,"output_tokens_details":{"audio_tokens":1}}}`))
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 6)
}

// ---------------------------------------------------------------------------
// usageAccumulator
// ---------------------------------------------------------------------------

func TestUsageAccumulatorSumsAcrossTurns(t *testing.T) {
	t.Parallel()
	var totals usageAccumulator
	totals.add(100, 20, uint64Ptr(10), nil)
	totals.add(50, 15, uint64Ptr(5), uint64Ptr(30))
	usage := totals.finish()
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 150 || usage.CompletionTokens != 35 {
		t.Errorf("prompt/completion = %d/%d, want 150/35", usage.PromptTokens, usage.CompletionTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 15)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 30)
	if usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", usage.InvocationCount)
	}
	if usage.TotalTokens != 185 {
		t.Errorf("totalTokens = %d, want 185", usage.TotalTokens)
	}
}

// cached / reasoning 只在见过一次时才上报:全程没有缓存信息时上报 0 会被下游
// 误读为「确实没命中缓存」。
func TestUsageAccumulatorOmitsUnseenOptionals(t *testing.T) {
	t.Parallel()
	var totals usageAccumulator
	totals.add(10, 2, nil, nil)
	usage := totals.finish()
	if usage == nil {
		t.Fatal("expected usage")
	}
	wantNilUint64(t, "cachedInputTokens", usage.CachedInputTokens)
	wantNilUint64(t, "reasoningTokens", usage.ReasoningTokens)

	// 见过一次 0 就要上报 0(明确的「未命中」信号)。
	var withZero usageAccumulator
	withZero.add(10, 2, uint64Ptr(0), uint64Ptr(0))
	usage = withZero.finish()
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 0)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 0)
}

// 溢出即整份作废:截断后的数字比不上报危害更大。
func TestUsageAccumulatorDiscardsEverythingOnOverflow(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*usageAccumulator){
		"prompt 溢出": func(a *usageAccumulator) {
			a.add(math.MaxUint64, 0, nil, nil)
			a.add(1, 0, nil, nil)
		},
		"completion 溢出": func(a *usageAccumulator) {
			a.add(0, math.MaxUint64, nil, nil)
			a.add(0, 1, nil, nil)
		},
		"cached 溢出": func(a *usageAccumulator) {
			a.add(0, 0, uint64Ptr(math.MaxUint64), nil)
			a.add(0, 0, uint64Ptr(1), nil)
		},
		"reasoning 溢出": func(a *usageAccumulator) {
			a.add(0, 0, nil, uint64Ptr(math.MaxUint64))
			a.add(0, 0, nil, uint64Ptr(1))
		},
		"invocations 溢出": func(a *usageAccumulator) {
			a.addWithCount(1, 1, nil, nil, math.MaxUint64)
			a.addWithCount(1, 1, nil, nil, 1)
		},
	} {
		var totals usageAccumulator
		mutate(&totals)
		if usage := totals.finish(); usage != nil {
			t.Errorf("%s: expected nil, got %+v", name, usage)
		}
	}
}

// 空累加器的 invocations 为 0,FinalReportedUsage 会拒绝。
func TestUsageAccumulatorEmptyIsRejected(t *testing.T) {
	t.Parallel()
	var totals usageAccumulator
	if usage := totals.finish(); usage != nil {
		t.Errorf("an empty accumulator must not produce usage, got %+v", usage)
	}
}

func TestUsageAccumulatorAddReport(t *testing.T) {
	t.Parallel()
	var totals usageAccumulator
	totals.addReport(FinalReportedUsage(100, uint64Ptr(20), 10, uint64Ptr(4), 3))
	totals.addReport(FinalReportedUsage(50, nil, 5, nil, 2))
	totals.addReport(nil) // nil 报告直接忽略,不影响计数
	usage := totals.finish()
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 150 || usage.CompletionTokens != 15 {
		t.Errorf("prompt/completion = %d/%d, want 150/15", usage.PromptTokens, usage.CompletionTokens)
	}
	if usage.InvocationCount != 5 {
		t.Errorf("invocationCount = %d, want 5 (3+2)", usage.InvocationCount)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 20)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 4)
}

// 累加结果自身也要过 FinalReportedUsage 的自洽校验。
func TestUsageAccumulatorRejectsCachedAbovePrompt(t *testing.T) {
	t.Parallel()
	var totals usageAccumulator
	totals.add(5, 1, uint64Ptr(9), nil)
	if usage := totals.finish(); usage != nil {
		t.Errorf("cached above prompt must be rejected, got %+v", usage)
	}
}

// ---------------------------------------------------------------------------
// assistantMessageText
// ---------------------------------------------------------------------------

func TestAssistantMessageText(t *testing.T) {
	t.Parallel()
	// text 字段优先,并且**即使是空白也照样返回**(rs 这条路径没有空白检查)。
	for _, text := range []string{"hello", "", "   ", "\n"} {
		got, ok := assistantMessageText(usageJSON(t, `{"text":`+jsonString(text)+`}`))
		if !ok || got != text {
			t.Errorf("text %q => (%q, %v), want (%q, true)", text, got, ok, text)
		}
	}
	// text 优先于 content。
	got, ok := assistantMessageText(usageJSON(t, `{"text":"direct","content":[{"type":"text","text":"nested"}]}`))
	if !ok || got != "direct" {
		t.Errorf("got (%q, %v), want (\"direct\", true)", got, ok)
	}
	// content[] 只拼 type == "text" 的片段,用 \n 连接。
	got, ok = assistantMessageText(usageJSON(t, `{"content":[
		{"type":"thinking","text":"skip me"},
		{"type":"text","text":"line one"},
		{"type":"tool_use","name":"read"},
		{"type":"text","text":"line two"},
		{"type":"text"}
	]}`))
	if !ok || got != "line one\nline two" {
		t.Errorf("got (%q, %v), want (\"line one\\nline two\", true)", got, ok)
	}
	// 拼接结果全空白 → 视为没有文本。
	for _, text := range []string{
		`{"content":[]}`,
		`{"content":[{"type":"text","text":"  "}]}`,
		`{"content":[{"type":"text","text":""},{"type":"text","text":"\n"}]}`,
		`{"content":[{"type":"tool_use","name":"read"}]}`,
	} {
		if got, ok := assistantMessageText(usageJSON(t, text)); ok {
			t.Errorf("%s => (%q, true), want ok=false", text, got)
		}
	}
	// 没有 text 也没有 content,或 content 不是数组,或 text 不是字符串。
	for _, text := range []string{`{}`, `{"content":"nope"}`, `{"content":null}`, `{"text":123}`, `[]`, `"s"`} {
		if _, ok := assistantMessageText(usageJSON(t, text)); ok {
			t.Errorf("%s must yield ok=false", text)
		}
	}
}

// ---------------------------------------------------------------------------
// parseTextCandidate
// ---------------------------------------------------------------------------

func TestParseTextCandidate(t *testing.T) {
	t.Parallel()
	// 空白 → 空字符串(而不是 null)。
	for _, text := range []string{"", "   ", "\n\t "} {
		if got := parseTextCandidate(text); got != "" {
			t.Errorf("%q => %#v, want empty string", text, got)
		}
	}
	// 能整串解析的 JSON → 解析结果(任意 JSON 值)。
	object, ok := asJSONObject(parseTextCandidate(`  {"a":1}  `))
	if !ok {
		t.Fatal("expected an object")
	}
	if number, _ := jsonUint64(object["a"]); number != 1 {
		t.Errorf("a = %#v, want 1", object["a"])
	}
	if got := parseTextCandidate(`[1,2]`); len(got.([]any)) != 2 {
		t.Errorf("array candidate = %#v", got)
	}
	if got := parseTextCandidate(`"quoted"`); got != "quoted" {
		t.Errorf("string candidate = %#v, want \"quoted\"", got)
	}
	if got := parseTextCandidate(`true`); got != true {
		t.Errorf("bool candidate = %#v, want true", got)
	}
	if got := parseTextCandidate(`null`); got != nil {
		t.Errorf("null candidate = %#v, want nil", got)
	}
	// 解析不了 → **未去空白的原文**。
	if got := parseTextCandidate("  plain text  "); got != "  plain text  " {
		t.Errorf("unparsable candidate = %#v, want the untrimmed original", got)
	}
	// 首个 JSON 值之后有残留 → 整串不算 JSON。
	if got := parseTextCandidate(`{"a":1} trailing`); got != `{"a":1} trailing` {
		t.Errorf("trailing content candidate = %#v", got)
	}
}

// ---------------------------------------------------------------------------
// parsePiJSONLines
// ---------------------------------------------------------------------------

// 对应 rs pi_json_lines_extract_final_text_and_aggregate_authoritative_usage。
func TestPiJSONLinesExtractFinalTextAndAggregateAuthoritativeUsage(t *testing.T) {
	t.Parallel()
	output := `{"payload_result":{"assessments":[],"summary":"done"}}`
	first := `{"type":"message_end","message":{"role":"assistant",
		"content":[{"type":"text","text":"intermediate"}],
		"stopReason":"toolUse",
		"usage":{"input":100,"output":20,"cacheRead":10,"cacheWrite":5,"totalTokens":135}}}`
	second := `{"type":"message_end","message":{"role":"assistant",
		"content":[{"type":"text","text":` + jsonString(output) + `}],
		"stopReason":"stop",
		"usage":{"input":50,"output":15,"cacheRead":5,"cacheWrite":0,"reasoning":30,"totalTokens":70}}}`

	candidate, usage, execErr := parsePiJSONLines(jsonLines(compactJSONLine(first), compactJSONLine(second)))
	if execErr != nil {
		t.Fatalf("unexpected execution error: %v", execErr)
	}
	summary := nestedString(t, candidate, "payload_result", "summary")
	if summary != "done" {
		t.Errorf("payload_result.summary = %q, want \"done\"", summary)
	}
	if usage == nil {
		t.Fatal("expected Pi message_end usage")
	}
	if usage.PromptTokens != 170 {
		t.Errorf("promptTokens = %d, want 170 (115+55)", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 15)
	if usage.CompletionTokens != 35 {
		t.Errorf("completionTokens = %d, want 35", usage.CompletionTokens)
	}
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 30)
	if usage.TotalTokens != 205 {
		t.Errorf("totalTokens = %d, want 205", usage.TotalTokens)
	}
	if usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", usage.InvocationCount)
	}
}

// 对应 rs pi_json_lines_reject_unsuccessful_final_message。
func TestPiJSONLinesRejectUnsuccessfulFinalMessage(t *testing.T) {
	t.Parallel()
	event := `{"type":"message_end","message":{"role":"assistant","stopReason":"error",
		"content":[{"type":"text","text":"provider failed"}],
		"usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12}}}`
	candidate, usage, execErr := parsePiJSONLines(jsonLines(compactJSONLine(event)))
	if execErr == nil {
		t.Fatal("expected an execution error")
	}
	if execErr.Code != "executor_failed" {
		t.Errorf("code = %q, want executor_failed", execErr.Code)
	}
	if candidate != nil || usage != nil {
		t.Errorf("the error path must not return a candidate or usage: %#v / %+v", candidate, usage)
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "provider failed") {
		t.Errorf("executor output = %v, want it to carry the executor text", raw)
	}
}

// aborted 与 error 同等对待;其他 stopReason 放行。
func TestPiJSONLinesStopReasonHandling(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"error", "aborted"} {
		event := `{"type":"message_end","message":{"role":"assistant","stopReason":"` + reason + `","text":"x"}}`
		if _, _, execErr := parsePiJSONLines(jsonLines(event)); execErr == nil {
			t.Errorf("stopReason %q must fail the execution", reason)
		}
	}
	for _, reason := range []string{"stop", "toolUse", "maxTokens", "ERROR", ""} {
		event := `{"type":"message_end","message":{"role":"assistant","stopReason":"` + reason + `","text":"x"}}`
		if _, _, execErr := parsePiJSONLines(jsonLines(event)); execErr != nil {
			t.Errorf("stopReason %q must be allowed, got %v", reason, execErr)
		}
	}
	// stop_reason 是 stopReason 的别名。
	event := `{"type":"message_end","message":{"role":"assistant","stop_reason":"error","text":"x"}}`
	if _, _, execErr := parsePiJSONLines(jsonLines(event)); execErr == nil {
		t.Error("stop_reason must be honoured as an alias")
	}
	// stopReason 优先于 stop_reason。
	event = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","stop_reason":"error","text":"x"}}`
	if _, _, execErr := parsePiJSONLines(jsonLines(event)); execErr != nil {
		t.Errorf("stopReason must win over stop_reason, got %v", execErr)
	}
	// 只看**最后一条** assistant message_end 的 stopReason:先失败后成功 → 放行。
	if _, _, execErr := parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","stopReason":"error","text":"a"}}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","text":"b"}}`,
	)); execErr != nil {
		t.Errorf("only the last stop reason counts, got %v", execErr)
	}
	// 最后一条没有 stopReason 时,之前的失败状态会被清掉。
	if _, _, execErr := parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","stopReason":"error","text":"a"}}`,
		`{"type":"message_end","message":{"role":"assistant","text":"b"}}`,
	)); execErr != nil {
		t.Errorf("a later message without a stop reason must clear the failure, got %v", execErr)
	}
}

// 只有 type == message_end 且 message.role == assistant 的事件被采纳。
func TestPiJSONLinesIgnoresIrrelevantEvents(t *testing.T) {
	t.Parallel()
	candidate, usage, execErr := parsePiJSONLines(jsonLines(
		`not json at all`,
		`{"type":"message_start","message":{"role":"assistant","text":"ignored","usage":{"input":9,"output":9}}}`,
		`{"type":"message_end","message":{"role":"user","text":"ignored","usage":{"input":9,"output":9}}}`,
		`{"type":"message_end","text":"no message field"}`,
		`{"type":"message_end","message":{"role":"assistant","text":"{\"ok\":true}","usage":{"input":10,"output":2}}}`,
		`{"no type field":1}`,
	))
	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	object, ok := asJSONObject(candidate)
	if !ok || object["ok"] != true {
		t.Errorf("candidate = %#v, want the assistant message_end payload", candidate)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.InvocationCount != 1 {
		t.Errorf("usage = %+v, want only the assistant message_end counted", usage)
	}
}

// 没有任何 message_end 时,候选退化为整段原文。
func TestPiJSONLinesFallsBackToWholeOutput(t *testing.T) {
	t.Parallel()
	// 整段就是一个 JSON 对象(非 JSONL)时也能用。
	candidate, usage, execErr := parsePiJSONLines(`{"payload_result":{"summary":"raw"}}`)
	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil without any usage event", usage)
	}
	if got := nestedString(t, candidate, "payload_result", "summary"); got != "raw" {
		t.Errorf("summary = %q, want \"raw\"", got)
	}
	// 整段不是 JSON 时保留原文。
	candidate, _, _ = parsePiJSONLines("plain executor chatter\n")
	if candidate != "plain executor chatter\n" {
		t.Errorf("candidate = %#v, want the untouched raw output", candidate)
	}
	// 空输出 → 空字符串。
	candidate, _, _ = parsePiJSONLines("")
	if candidate != "" {
		t.Errorf("candidate = %#v, want an empty string", candidate)
	}
}

// prompt 与 completion 全为 0 的轮次不计入(Pi 会为工具回执发空用量事件)。
func TestPiJSONLinesSkipsZeroAndUntrustworthyUsage(t *testing.T) {
	t.Parallel()
	// 全零用量 → 没有任何用量。
	_, usage, _ := parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","text":"x","usage":{"input":0,"output":0}}}`,
	))
	if usage != nil {
		t.Errorf("usage = %+v, want nil for an all-zero usage event", usage)
	}
	// 自报 total 与分项不一致的那一轮被跳过,其余轮次照常累加。
	_, usage, _ = parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","text":"x","usage":{"input":100,"output":20,"totalTokens":999}}}`,
		`{"type":"message_end","message":{"role":"assistant","text":"y","usage":{"input":10,"output":2,"totalTokens":12}}}`,
	))
	if usage == nil {
		t.Fatal("expected the consistent turn to still be counted")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.InvocationCount != 1 {
		t.Errorf("usage = %+v, want only the consistent turn", usage)
	}
	// prompt 分项溢出的那一轮被跳过。
	_, usage, _ = parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","text":"x","usage":{"input":18446744073709551615,"cacheRead":1,"output":1}}}`,
	))
	if usage != nil {
		t.Errorf("usage = %+v, want nil when the prompt subtotal overflows", usage)
	}
	// message 里没有 usage → 不计,但文本仍被采纳。
	candidate, usage, _ := parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","text":"{\"ok\":1}"}}`,
	))
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
	if _, ok := asJSONObject(candidate); !ok {
		t.Errorf("candidate = %#v, want the parsed object", candidate)
	}
}

// input/output 的下划线别名与 reasoning 的三种键名。
func TestPiJSONLinesUsageKeyAliases(t *testing.T) {
	t.Parallel()
	_, usage, _ := parsePiJSONLines(jsonLines(
		`{"type":"message_end","message":{"role":"assistant","text":"x","usage":{"input_tokens":10,"output_tokens":2,"cache_read":3,"cache_write":1}}}`,
	))
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 14 {
		t.Errorf("promptTokens = %d, want 14 (10+3+1)", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 3)
	for _, key := range []string{"reasoning", "reasoningTokens", "reasoning_tokens"} {
		_, usage, _ := parsePiJSONLines(jsonLines(
			`{"type":"message_end","message":{"role":"assistant","text":"x","usage":{"input":10,"output":2,"` + key + `":7}}}`,
		))
		if usage == nil {
			t.Fatalf("%s: expected usage", key)
		}
		wantUint64(t, key, usage.ReasoningTokens, 7)
	}
}

// ---------------------------------------------------------------------------
// parseCodexJSONLines
// ---------------------------------------------------------------------------

// 对应 rs codex_json_lines_extract_result_and_usage。
func TestCodexJSONLinesExtractResultAndUsage(t *testing.T) {
	t.Parallel()
	result := `{"payload_result":{"assessments":[],"summary":"codex"}}`
	events := jsonLines(
		`{"type":"item.completed","item":{"type":"agent_message","text":`+jsonString(result)+`}}`,
		`{"type":"turn.completed","usage":{"input_tokens":90,"cached_input_tokens":30,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":4}}`,
	)
	candidate, usage, execErr := parseCodexJSONLines(events)
	if execErr != nil {
		t.Fatalf("unexpected execution error: %v", execErr)
	}
	if got := nestedString(t, candidate, "payload_result", "summary"); got != "codex" {
		t.Errorf("payload_result.summary = %q, want \"codex\"", got)
	}
	if usage == nil {
		t.Fatal("expected Codex turn usage")
	}
	if usage.PromptTokens != 90 {
		t.Errorf("promptTokens = %d, want 90", usage.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 30)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 4)
	if usage.TotalTokens != 100 {
		t.Errorf("totalTokens = %d, want 100", usage.TotalTokens)
	}
	if usage.InvocationCount != 1 {
		t.Errorf("invocationCount = %d, want 1", usage.InvocationCount)
	}
}

// 对应 rs codex_json_lines_reject_failed_turn。
func TestCodexJSONLinesRejectFailedTurn(t *testing.T) {
	t.Parallel()
	events := jsonLines(
		`{"type":"error","message":"provider failed"}`,
		`{"type":"turn.failed","error":{"message":"provider failed"}}`,
	)
	candidate, usage, execErr := parseCodexJSONLines(events)
	if execErr == nil {
		t.Fatal("expected an execution error")
	}
	if execErr.Code != "executor_failed" {
		t.Errorf("code = %q, want executor_failed", execErr.Code)
	}
	if candidate != nil || usage != nil {
		t.Errorf("the error path must not return a candidate or usage: %#v / %+v", candidate, usage)
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "provider failed") {
		t.Errorf("executor output = %v, want it to carry the raw stream", raw)
	}
}

// item.completed 只在 item.type == agent_message 时取文本;多条时后者覆盖前者。
func TestCodexJSONLinesTextSelection(t *testing.T) {
	t.Parallel()
	candidate, _, execErr := parseCodexJSONLines(jsonLines(
		`{"type":"item.completed","item":{"type":"reasoning","text":"ignored"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","text":"ignored"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"item.completed","no item":1}`,
		`{"type":"item.started","item":{"type":"agent_message","text":"ignored"}}`,
	))
	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if candidate != "second" {
		t.Errorf("candidate = %#v, want \"second\" (the last agent_message)", candidate)
	}
}

// turn.completed 需要 prompt 与 completion **都**能读到才计入;多轮累加。
func TestCodexJSONLinesUsageAggregation(t *testing.T) {
	t.Parallel()
	_, usage, _ := parseCodexJSONLines(jsonLines(
		`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":20}}`,
		`{"type":"turn.completed","usage":{"prompt_tokens":50,"completion_tokens":5,"reasoning_tokens":3}}`,
	))
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 150 || usage.CompletionTokens != 15 {
		t.Errorf("prompt/completion = %d/%d, want 150/15", usage.PromptTokens, usage.CompletionTokens)
	}
	if usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", usage.InvocationCount)
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 20)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 3)

	// 半份用量整轮跳过。
	for _, text := range []string{
		`{"type":"turn.completed","usage":{"input_tokens":100}}`,
		`{"type":"turn.completed","usage":{"output_tokens":10}}`,
		`{"type":"turn.completed","usage":{}}`,
		`{"type":"turn.completed"}`,
		`{"type":"turn.completed","usage":null}`,
	} {
		if _, usage, _ := parseCodexJSONLines(jsonLines(text)); usage != nil {
			t.Errorf("%s: usage = %+v, want nil", text, usage)
		}
	}
	// reasoning_output_tokens 优先于 reasoning_tokens。
	_, usage, _ = parseCodexJSONLines(jsonLines(
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":9,"reasoning_tokens":3}}`,
	))
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 9)
	// Codex 的全零用量仍会被计入(与 Pi 不同,这里没有 >0 的门槛)。
	_, usage, _ = parseCodexJSONLines(jsonLines(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`))
	if usage == nil || usage.TotalTokens != 0 || usage.InvocationCount != 1 {
		t.Errorf("usage = %+v, want a zero-token single invocation", usage)
	}
}

// turn.failed 在流中任意位置都生效,并且带上已解析出的文本。
func TestCodexJSONLinesFailedTurnCarriesParsedText(t *testing.T) {
	t.Parallel()
	_, _, execErr := parseCodexJSONLines(jsonLines(
		`{"type":"turn.failed","error":{"message":"boom"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"partial\":true}"}}`,
	))
	if execErr == nil {
		t.Fatal("expected an execution error")
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil || !strings.Contains(*raw, "partial") {
		t.Errorf("executor output = %v, want the parsed agent_message", raw)
	}
}

// 没有任何 agent_message 时候选退化为整段原文。
func TestCodexJSONLinesFallsBackToWholeOutput(t *testing.T) {
	t.Parallel()
	candidate, usage, execErr := parseCodexJSONLines("codex chatter without json\n")
	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
	if candidate != "codex chatter without json\n" {
		t.Errorf("candidate = %#v, want the untouched raw output", candidate)
	}
}

// ---------------------------------------------------------------------------
// usageFromReasonixMetrics
// ---------------------------------------------------------------------------

// 对应 rs reasonix_metrics_reject_estimates_and_accept_final_counts。
func TestReasonixMetricsRejectEstimatesAndAcceptFinalCounts(t *testing.T) {
	t.Parallel()
	// 旧版 camelCase:estimated == true → 丢弃。
	if usage := usageFromReasonixMetrics(usageJSON(t, `{
		"promptTokens": 100, "completionTokens": 20, "requestCount": 1, "estimated": true
	}`)); usage != nil {
		t.Errorf("estimated metrics must be rejected, got %+v", usage)
	}

	usage := usageFromReasonixMetrics(usageJSON(t, `{
		"promptTokens": 100, "completionTokens": 20, "totalTokens": 120,
		"cacheHitTokens": 25, "reasoningTokens": 5, "requestCount": 2, "estimated": false
	}`))
	if usage == nil {
		t.Fatal("expected final Reasonix metrics")
	}
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 25)
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 5)
	if usage.InvocationCount != 2 {
		t.Errorf("invocationCount = %d, want 2", usage.InvocationCount)
	}

	// requestCount == 0 是历史兼容写法,表示一次请求。
	legacy := usageFromReasonixMetrics(usageJSON(t, `{"promptTokens": 10, "completionTokens": 2, "requestCount": 0}`))
	if legacy == nil {
		t.Fatal("a zero requestCount must mean one legacy request")
	}
	if legacy.InvocationCount != 1 {
		t.Errorf("invocationCount = %d, want 1", legacy.InvocationCount)
	}

	// 新版 1.31.x:顶层 estimated / cost_complete 只描述费用报价,Token 仍然精确。
	current := usageFromReasonixMetrics(usageJSON(t, `{
		"prompt_tokens": 980, "completion_tokens": 5,
		"cache_hit_tokens": 0, "cache_miss_tokens": 980,
		"estimated": true, "cost_complete": false,
		"complete": true, "outcome": "success",
		"usage_by_source": {"executor": {"calls": 1, "prompt_tokens": 980, "completion_tokens": 5}}
	}`))
	if current == nil {
		t.Fatal("Reasonix 1.31 token metrics are exact despite an unavailable cost quote")
	}
	if current.PromptTokens != 980 {
		t.Errorf("promptTokens = %d, want 980", current.PromptTokens)
	}
	wantUint64(t, "cachedInputTokens", current.CachedInputTokens, 0)
	if current.CompletionTokens != 5 {
		t.Errorf("completionTokens = %d, want 5", current.CompletionTokens)
	}
	if current.TotalTokens != 985 {
		t.Errorf("totalTokens = %d, want 985", current.TotalTokens)
	}
	if current.InvocationCount != 1 {
		t.Errorf("invocationCount = %d, want 1", current.InvocationCount)
	}
	if current.Estimated {
		t.Error("estimated must stay false")
	}
}

// metrics 可以把用量包在 usage 子对象里,也可以直接平铺在顶层。
func TestReasonixMetricsAcceptsNestedAndFlatShapes(t *testing.T) {
	t.Parallel()
	nested := usageFromReasonixMetrics(usageJSON(t, `{"usage":{"promptTokens":10,"completionTokens":2,"requestCount":1}}`))
	flat := usageFromReasonixMetrics(usageJSON(t, `{"promptTokens":10,"completionTokens":2,"requestCount":1}`))
	if nested == nil || flat == nil {
		t.Fatalf("both shapes must work: nested=%+v flat=%+v", nested, flat)
	}
	if nested.TotalTokens != flat.TotalTokens {
		t.Errorf("nested %d != flat %d", nested.TotalTokens, flat.TotalTokens)
	}
	// usage 为 null 时子对象里什么都读不到。
	if usage := usageFromReasonixMetrics(usageJSON(t, `{"usage":null,"promptTokens":10,"completionTokens":2,"requestCount":1}`)); usage != nil {
		t.Errorf("a null usage object must not fall back to the top level, got %+v", usage)
	}
}

// 新版 shape 的四道门禁。
func TestReasonixMetricsNewShapeGates(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"complete 不为 true": `{"prompt_tokens":10,"completion_tokens":2,"complete":false,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"complete 缺失": `{"prompt_tokens":10,"completion_tokens":2,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"outcome 不是 success": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"partial",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"outcome 缺失": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"顶层缺 prompt_tokens": `{"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"顶层缺 completion_tokens": `{"prompt_tokens":10,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"来源 prompt 合计不等于顶层": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":9,"completion_tokens":2}}}`,
		"来源 completion 合计不等于顶层": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":1}}}`,
		"来源 calls 合计为 0": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":0,"prompt_tokens":10,"completion_tokens":2}}}`,
		"某个来源缺 calls": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"prompt_tokens":10,"completion_tokens":2}}}`,
		"某个来源缺 prompt_tokens": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"completion_tokens":2}}}`,
		"usage_by_source 不是对象": `{"prompt_tokens":10,"completion_tokens":2,"complete":true,"outcome":"success",
			"usage_by_source":[]}`,
		"cache_hit + cache_miss 不等于 prompt": `{"prompt_tokens":10,"completion_tokens":2,
			"cache_hit_tokens":1,"cache_miss_tokens":1,"complete":true,"outcome":"success",
			"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`,
		"来源为空对象时 calls 合计为 0": `{"prompt_tokens":0,"completion_tokens":0,"complete":true,"outcome":"success",
			"usage_by_source":{}}`,
	}
	for name, text := range cases {
		if usage := usageFromReasonixMetrics(usageJSON(t, text)); usage != nil {
			t.Errorf("%s: expected nil, got %+v", name, usage)
		}
	}

	// 多来源合计自洽时采信,calls 相加。
	usage := usageFromReasonixMetrics(usageJSON(t, `{
		"prompt_tokens":30,"completion_tokens":6,"reasoning_tokens":4,
		"complete":true,"outcome":"success",
		"usage_by_source":{
			"executor":{"calls":2,"prompt_tokens":20,"completion_tokens":4},
			"planner":{"calls":1,"prompt_tokens":10,"completion_tokens":2}
		}}`))
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.InvocationCount != 3 {
		t.Errorf("invocationCount = %d, want 3", usage.InvocationCount)
	}
	wantUint64(t, "reasoningTokens", usage.ReasoningTokens, 4)
	wantNilUint64(t, "cachedInputTokens", usage.CachedInputTokens)
	// 只有 cache_hit 没有 cache_miss 时不做配平校验,直接上报。
	usage = usageFromReasonixMetrics(usageJSON(t, `{
		"prompt_tokens":10,"completion_tokens":2,"cache_hit_tokens":4,
		"complete":true,"outcome":"success",
		"usage_by_source":{"a":{"calls":1,"prompt_tokens":10,"completion_tokens":2}}}`))
	wantUint64(t, "cachedInputTokens", usage.CachedInputTokens, 4)
}

// 旧版 shape 的门禁与键名别名。
func TestReasonixMetricsLegacyShapeGates(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"缺 promptTokens":       `{"completionTokens":2,"requestCount":1}`,
		"缺 completionTokens":   `{"promptTokens":10,"requestCount":1}`,
		"缺调用次数":                `{"promptTokens":10,"completionTokens":2}`,
		"自报 totalTokens 不一致":   `{"promptTokens":10,"completionTokens":2,"totalTokens":99,"requestCount":1}`,
		"prompt+completion 溢出": `{"promptTokens":18446744073709551615,"completionTokens":1,"requestCount":1}`,
	}
	for name, text := range cases {
		if usage := usageFromReasonixMetrics(usageJSON(t, text)); usage != nil {
			t.Errorf("%s: expected nil, got %+v", name, usage)
		}
	}
	// 调用次数的三种键名。
	for _, key := range []string{"requestCount", "invocationCount", "invocation_count"} {
		usage := usageFromReasonixMetrics(usageJSON(t, `{"promptTokens":10,"completionTokens":2,"`+key+`":3}`))
		if usage == nil || usage.InvocationCount != 3 {
			t.Fatalf("%s: invocationCount not honoured: %+v", key, usage)
		}
	}
	// 缓存与 reasoning 的键名别名。
	for _, key := range []string{"cacheHitTokens", "cachedInputTokens", "cached_input_tokens"} {
		usage := usageFromReasonixMetrics(usageJSON(t, `{"promptTokens":10,"completionTokens":2,"requestCount":1,"`+key+`":4}`))
		wantUint64(t, key, usage.CachedInputTokens, 4)
	}
	for _, key := range []string{"reasoningTokens", "reasoning_tokens"} {
		usage := usageFromReasonixMetrics(usageJSON(t, `{"promptTokens":10,"completionTokens":2,"requestCount":1,"`+key+`":4}`))
		wantUint64(t, key, usage.ReasoningTokens, 4)
	}
	// 旧版也接受 snake_case 的 token 键名。
	if usage := usageFromReasonixMetrics(usageJSON(t, `{"prompt_tokens":10,"completion_tokens":2,"requestCount":1}`)); usage == nil {
		t.Error("legacy shape must accept snake_case token keys")
	}
	// 非对象 metrics。
	for _, text := range []string{`[]`, `"s"`, `1`, `null`} {
		if usage := usageFromReasonixMetrics(usageJSON(t, text)); usage != nil {
			t.Errorf("%s: expected nil, got %+v", text, usage)
		}
	}
}

// ---------------------------------------------------------------------------
// extractJSONFromText
// ---------------------------------------------------------------------------

func TestExtractJSONFromTextStripsMarkdownFences(t *testing.T) {
	t.Parallel()
	cases := []string{
		"```json\n{\"a\":1}\n```",
		"```\n{\"a\":1}\n```",
		"```JSON\n  {\"a\":1}  \n```\n\n",
		"  ```json\n{\"a\":1}\n```  ",
	}
	for _, text := range cases {
		object, ok := asJSONObject(extractJSONFromText(text))
		if !ok {
			t.Errorf("%q: expected an object, got %#v", text, extractJSONFromText(text))
			continue
		}
		if number, _ := jsonUint64(object["a"]); number != 1 {
			t.Errorf("%q: a = %#v, want 1", text, object["a"])
		}
	}
}

func TestExtractJSONFromTextParsesWholeString(t *testing.T) {
	t.Parallel()
	// 无围栏、直接就是 JSON。
	if _, ok := asJSONObject(extractJSONFromText(`  {"a":1}  `)); !ok {
		t.Error("a bare JSON object must parse")
	}
	// 整串解析支持非对象值(与暴力扫描阶段不同)。
	if got := extractJSONFromText(`[1,2]`); len(got.([]any)) != 2 {
		t.Errorf("array = %#v, want a 2-element array", got)
	}
	if got := extractJSONFromText(`42`); got == nil {
		t.Error("a bare number must parse")
	}
	if got := extractJSONFromText(`"quoted"`); got != "quoted" {
		t.Errorf("string = %#v, want \"quoted\"", got)
	}
}

// 暴力扫描:模型在 JSON 前后夹带解释文字时仍能抠出第一个合法对象。
func TestExtractJSONFromTextScansForFirstObject(t *testing.T) {
	t.Parallel()
	got := extractJSONFromText("这是我的分析结果:\n{\"payload_result\":{\"summary\":\"ok\"}}\n希望有帮助!")
	if summary := nestedString(t, got, "payload_result", "summary"); summary != "ok" {
		t.Errorf("summary = %q, want \"ok\"", summary)
	}
	// 嵌套对象:从第一个 { 出发,内层 } 收不成合法对象,继续扫到外层 } 才成立。
	got = extractJSONFromText(`prefix {"outer":{"inner":1}} suffix`)
	object, ok := asJSONObject(got)
	if !ok {
		t.Fatalf("got %#v, want an object", got)
	}
	if _, ok := jsonField(object, "outer"); !ok {
		t.Errorf("got %#v, want the full outer object", object)
	}
	// 只返回**对象**,数组子串不算。
	got = extractJSONFromText(`noise [1,2] noise {"a":1} noise`)
	if _, ok := asJSONObject(got); !ok {
		t.Errorf("got %#v, want the object rather than the array", got)
	}
	// 第一个能解析成对象的子串胜出。
	got = extractJSONFromText(`x {"first":1} y {"second":2} z`)
	if _, ok := jsonField(got, "first"); !ok {
		t.Errorf("got %#v, want the first parsable object", got)
	}
	// 围栏内夹带解释文字时,剥围栏后仍走暴力扫描。
	got = extractJSONFromText("```json\n说明:\n{\"a\":1}\n```")
	if _, ok := asJSONObject(got); !ok {
		t.Errorf("got %#v, want an object", got)
	}
}

// 全部失败时返回**未去空白的原文**。
func TestExtractJSONFromTextFallsBackToRawText(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"  no json here  ",
		"",
		"{ unbalanced",
		"{not: valid}",
		"}{",
	} {
		if got := extractJSONFromText(text); got != text {
			t.Errorf("%q => %#v, want the untrimmed original", text, got)
		}
	}
}

// 中文等多字节内容不会因为按字节扫描而切坏。
func TestExtractJSONFromTextHandlesMultibyteContent(t *testing.T) {
	t.Parallel()
	got := extractJSONFromText("结论如下。{\"评级\":\"买入\",\"理由\":\"估值偏低\"} 以上。")
	object, ok := asJSONObject(got)
	if !ok {
		t.Fatalf("got %#v, want an object", got)
	}
	if object["评级"] != "买入" {
		t.Errorf("评级 = %#v, want 买入", object["评级"])
	}
	if object["理由"] != "估值偏低" {
		t.Errorf("理由 = %#v, want 估值偏低", object["理由"])
	}
}

// 大整数在提取路径上也不掉精度。
func TestExtractJSONFromTextKeepsLargeIntegers(t *testing.T) {
	t.Parallel()
	object, ok := asJSONObject(extractJSONFromText(`prefix {"id": 9007199254740993} suffix`))
	if !ok {
		t.Fatal("expected an object")
	}
	number, ok := object["id"].(json.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Errorf("id = %#v, want json.Number 9007199254740993", object["id"])
	}
}
