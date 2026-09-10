package mota

// 本文件对应 rs `src/platform/executor/mod.rs` 里的用量汇总与流式输出解析:
// parse_text_candidate / usage_number / checked_sum / validate_reported_total /
// usage_from_gateway / usage_from_anthropic / usage_from_claude_envelope /
// usage_from_reasonix_metrics / UsageAccumulator / assistant_message_text /
// parse_pi_json_lines / parse_codex_json_lines / extract_json_from_text。
//
// 贯穿全文的一条硬规则:Token 计数只能来自供应商或执行器的原始回执。任何一处
// 拿不到可靠数字、出现溢出、或自报总量与分项不自洽,都必须退化为「无用量」
// (nil)而不是补一个估算值 —— 平台侧会把 estimated 用量当作违约。

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// JSON 取值辅助
// ---------------------------------------------------------------------------

// errTrailingJSON 表示一段文本在首个 JSON 值之后还有残留内容。
// serde_json::from_str 要求整串恰好是一个值,Go 的 json.Decoder 默认允许流式
// 多值,这里显式补齐该检查。
var errTrailingJSON = errors.New("unexpected trailing content after JSON value")

// decodeJSONNumbers 把一段 JSON 文本解码为 any,数字保留为 json.Number。
//
// 用量解析必须走这条路径:token 计数可以大到 float64 无法精确表示,而
// json.Number 保留字面量,配合 usageNumber 的 ParseUint 才能等价于 Rust 的
// Value::as_u64(整数才成立,小数与负数一律不成立)。
func decodeJSONNumbers(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errTrailingJSON
	}
	return value, nil
}

// asJSONObject 把值当作 JSON 对象读取。
func asJSONObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// asJSONArray 把值当作 JSON 数组读取。
func asJSONArray(value any) ([]any, bool) {
	array, ok := value.([]any)
	return array, ok
}

// jsonField 读取对象字段;value 不是对象或字段不存在时 ok 为 false。
// 对应 Rust 的 Value::get(key)。
func jsonField(value any, key string) (any, bool) {
	object, ok := asJSONObject(value)
	if !ok {
		return nil, false
	}
	field, ok := object[key]
	return field, ok
}

// jsonStringField 读取对象里的字符串字段。对应 `get(key).and_then(Value::as_str)`。
func jsonStringField(value any, key string) (string, bool) {
	field, ok := jsonField(value, key)
	if !ok {
		return "", false
	}
	text, ok := field.(string)
	return text, ok
}

// jsonBoolField 读取对象里的布尔字段。对应 `get(key).and_then(Value::as_bool)`。
// 注意 Rust 的 as_bool 不接受 "true" 这类字符串,这里同样只接受真正的布尔。
func jsonBoolField(value any, key string) (bool, bool) {
	field, ok := jsonField(value, key)
	if !ok {
		return false, false
	}
	flag, ok := field.(bool)
	return flag, ok
}

// ---------------------------------------------------------------------------
// 文本候选解析
// ---------------------------------------------------------------------------

// parseTextCandidate 把执行器输出的一段文本转成候选结果值。
//
// 对应 rs parse_text_candidate:
//   - 去空白后为空 → 空字符串(而不是 null,平台侧要求 payload_result 可寻址);
//   - 去空白后能整串解析为 JSON → 解析结果(任意 JSON 值,不限于对象);
//   - 否则 → **未去空白的原文**作为字符串(Rust 用的是 text.to_string())。
func parseTextCandidate(text string) any {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	value, err := decodeJSONNumbers([]byte(strings.TrimSpace(text)))
	if err != nil {
		return text
	}
	return value
}

// ---------------------------------------------------------------------------
// 用量数字读取
// ---------------------------------------------------------------------------

// usageNumber 依次尝试 keys,返回第一个能读成无符号整数的字段。
//
// 对应 rs usage_number。语义要点:某个 key 存在但值不是非负整数时**继续尝试下一个
// key**(Rust 的 find_map 行为),而不是直接失败。
func usageNumber(value any, keys ...string) (uint64, bool) {
	object, ok := asJSONObject(value)
	if !ok {
		return 0, false
	}
	for _, key := range keys {
		field, exists := object[key]
		if !exists {
			continue
		}
		if number, ok := jsonUint64(field); ok {
			return number, true
		}
	}
	return 0, false
}

// jsonUint64 按 Rust Value::as_u64 的语义把一个 JSON 值读成 uint64。
//
// json.Number 走字面量 ParseUint,因此 "12" 成立而 "12.0"、"-1"、"1e3" 都不成立
// —— 与 serde_json 对整数字面量的判定一致,且不损失大整数精度。
// float64/float32 分支是给「上游用默认 json.Unmarshal 解码、已丢失整浮点区分」
// 的调用方留的兼容口子:只接受有限、非负且为整数的值。
func jsonUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := strconv.ParseUint(typed.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return number, true
	case uint64:
		return typed, true
	case uint:
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case float64:
		return floatToUint64(typed)
	case float32:
		return floatToUint64(float64(typed))
	default:
		return 0, false
	}
}

// floatToUint64 只接受有限、非负、整数且在 uint64 范围内的浮点值。
func floatToUint64(value float64) (uint64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value < 0 || value != math.Trunc(value) {
		return 0, false
	}
	// 2^64 无法用 float64 精确表示,用严格小于做上界判断。
	if value >= math.MaxUint64 {
		return 0, false
	}
	return uint64(value), true
}

// checkedSum 求和并检测溢出。空入参返回 (0, true),对应 Rust try_fold 的初值。
func checkedSum(values ...uint64) (uint64, bool) {
	total := uint64(0)
	for _, value := range values {
		if total > math.MaxUint64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// validateReportedTotal 校验上游自报的总量与我们算出的 expected 一致。
//
// 对应 rs validate_reported_total 的 is_none_or:上游**没有**自报总量时视为通过;
// 自报了就必须严格相等 —— 不一致说明分项键名理解有误,宁可丢弃整份用量。
func validateReportedTotal(usage any, expected uint64) bool {
	reported, ok := usageNumber(usage, "total_tokens", "totalTokens", "total")
	if !ok {
		return true
	}
	return reported == expected
}

// ---------------------------------------------------------------------------
// 各执行通道的用量提取
// ---------------------------------------------------------------------------

// usageFromGateway 从 LiteLLM 网关响应里提取用量。对应 rs usage_from_gateway。
//
// OpenAI 协议要求 prompt 与 completion 两项都在,并校验自报 total;
// Anthropic 协议直接委托 usageFromAnthropic(invocationCount 固定 1)。
func usageFromGateway(envelope any, protocol GatewayProtocol) *BoundRunUsage {
	usage, ok := jsonField(envelope, "usage")
	if !ok {
		return nil
	}
	switch protocol {
	case GatewayProtocolOpenAI:
		prompt, ok := usageNumber(usage, "prompt_tokens", "input_tokens")
		if !ok {
			return nil
		}
		completion, ok := usageNumber(usage, "completion_tokens", "output_tokens")
		if !ok {
			return nil
		}
		total, ok := checkedSum(prompt, completion)
		if !ok {
			return nil
		}
		if !validateReportedTotal(usage, total) {
			return nil
		}
		cached := nestedUsageNumber(usage, "prompt_tokens_details", "cached_tokens")
		if cached == nil {
			cached = optionalUsageNumber(usage, "cached_input_tokens")
		}
		reasoning := nestedUsageNumber(usage, "completion_tokens_details", "reasoning_tokens")
		if reasoning == nil {
			reasoning = optionalUsageNumber(usage, "reasoning_tokens")
		}
		return FinalReportedUsage(prompt, cached, completion, reasoning, 1)
	case GatewayProtocolAnthropic:
		return usageFromAnthropic(usage, 1)
	default:
		return nil
	}
}

// optionalUsageNumber 是 usageNumber 的 *uint64 版本,便于直接喂给
// FinalReportedUsage 的可选参数。
func optionalUsageNumber(value any, keys ...string) *uint64 {
	number, ok := usageNumber(value, keys...)
	if !ok {
		return nil
	}
	return &number
}

// nestedUsageNumber 先取子对象再取其中的计数;子对象缺失或子对象里没有这些键时
// 返回 nil,由调用方决定是否回退到平铺键名。
func nestedUsageNumber(value any, container string, keys ...string) *uint64 {
	nested, ok := jsonField(value, container)
	if !ok {
		return nil
	}
	return optionalUsageNumber(nested, keys...)
}

// usageFromAnthropic 从 Anthropic Messages 风格的 usage 对象提取用量。
// 对应 rs usage_from_anthropic。
//
// 语义要点:
//   - prompt = input_tokens + cache_read + cache_write。Anthropic 把缓存命中与
//     缓存写入拆出 input_tokens 单独上报,不加回去会少算 prompt。
//   - cached 只在 cache_read > 0 时上报:0 与「无缓存信息」在契约里是两种含义。
//   - 这条路径**不**校验自报 total(Anthropic 不返回 total_tokens),与 rs 一致。
func usageFromAnthropic(usage any, invocationCount uint64) *BoundRunUsage {
	input, ok := usageNumber(usage, "input_tokens", "input")
	if !ok {
		return nil
	}
	cacheRead, ok := usageNumber(usage, "cache_read_input_tokens", "cacheRead", "cache_read")
	if !ok {
		cacheRead = 0
	}
	cacheWrite, ok := usageNumber(usage, "cache_creation_input_tokens", "cacheWrite", "cache_write")
	if !ok {
		cacheWrite = 0
	}
	prompt, ok := checkedSum(input, cacheRead, cacheWrite)
	if !ok {
		return nil
	}
	completion, ok := usageNumber(usage, "output_tokens", "output")
	if !ok {
		return nil
	}
	var cached *uint64
	if cacheRead > 0 {
		cached = &cacheRead
	}
	return FinalReportedUsage(
		prompt,
		cached,
		completion,
		optionalUsageNumber(usage, "reasoning_tokens", "reasoningTokens"),
		invocationCount,
	)
}

// usageFromClaudeEnvelope 从 Claude Code 的 --output-format json 信封提取用量。
// 对应 rs usage_from_claude_envelope。
//
// 语义要点:
//   - is_error 明确为 true 时整份丢弃(失败轮次的计数不可信);
//   - num_turns / numTurns 是这次执行的模型调用次数,缺失按 1;
//   - Claude 把思考 token 放在 output_tokens_details 里,优先用
//     thinking_tokens,其次 reasoning_tokens,都没有才保留 usage 平铺键的结果。
func usageFromClaudeEnvelope(envelope any) *BoundRunUsage {
	if isError, ok := jsonBoolField(envelope, "is_error"); ok && isError {
		return nil
	}
	usage, ok := jsonField(envelope, "usage")
	if !ok {
		return nil
	}
	invocationCount, ok := usageNumber(envelope, "num_turns", "numTurns")
	if !ok {
		invocationCount = 1
	}
	report := usageFromAnthropic(usage, invocationCount)
	if report == nil {
		return nil
	}
	if thinking := nestedUsageNumber(usage, "output_tokens_details", "thinking_tokens", "reasoning_tokens"); thinking != nil {
		report.ReasoningTokens = thinking
	}
	return report
}

// usageFromReasonixMetrics 从 Reasonix `--metrics` 侧信道文件提取用量。
// 对应 rs usage_from_reasonix_metrics。
//
// Reasonix 的 run 子命令输出纯文本,拿不到用量,只能读它写出的 metrics 文件。
// 该文件有两代格式,判别依据是有没有 usage_by_source:
//
// 新版(1.31.x):顶层 estimated / cost_complete 描述的是**费用报价**是否完整,
// 不能用来判断 Token 是否可信;Token 的事实源是 usage_by_source。因此要求
// complete == true 且 outcome == "success",并且各来源的 prompt/completion 合计
// 必须与顶层严格相等、调用次数合计 > 0,cache_hit + cache_miss 若都在也必须等于
// prompt。任一条不满足就整份丢弃。
//
// 旧版(camelCase):estimated 直接描述 Token 是否为推算值,为 true 即丢弃。
// requestCount 允许为 0(历史兼容,表示一次请求),用 max(1) 归一。
func usageFromReasonixMetrics(metrics any) *BoundRunUsage {
	usage := metrics
	if nested, ok := jsonField(metrics, "usage"); ok {
		usage = nested
	}

	if sourcesValue, ok := jsonField(usage, "usage_by_source"); ok {
		if complete, ok := jsonBoolField(usage, "complete"); !ok || !complete {
			return nil
		}
		if outcome, ok := jsonStringField(usage, "outcome"); !ok || outcome != "success" {
			return nil
		}
		prompt, ok := usageNumber(usage, "prompt_tokens")
		if !ok {
			return nil
		}
		completion, ok := usageNumber(usage, "completion_tokens")
		if !ok {
			return nil
		}
		sources, ok := asJSONObject(sourcesValue)
		if !ok {
			return nil
		}
		var sourcePrompt, sourceCompletion, invocations uint64
		for _, source := range sources {
			sourcePromptTokens, ok := usageNumber(source, "prompt_tokens")
			if !ok {
				return nil
			}
			sourceCompletionTokens, ok := usageNumber(source, "completion_tokens")
			if !ok {
				return nil
			}
			calls, ok := usageNumber(source, "calls")
			if !ok {
				return nil
			}
			if sourcePrompt, ok = checkedSum(sourcePrompt, sourcePromptTokens); !ok {
				return nil
			}
			if sourceCompletion, ok = checkedSum(sourceCompletion, sourceCompletionTokens); !ok {
				return nil
			}
			if invocations, ok = checkedSum(invocations, calls); !ok {
				return nil
			}
		}
		if sourcePrompt != prompt || sourceCompletion != completion || invocations == 0 {
			return nil
		}
		cacheHit := optionalUsageNumber(usage, "cache_hit_tokens")
		cacheMiss := optionalUsageNumber(usage, "cache_miss_tokens")
		if cacheHit != nil && cacheMiss != nil {
			sum, ok := checkedSum(*cacheHit, *cacheMiss)
			if !ok || sum != prompt {
				return nil
			}
		}
		return FinalReportedUsage(
			prompt,
			cacheHit,
			completion,
			optionalUsageNumber(usage, "reasoning_tokens"),
			invocations,
		)
	}

	if estimated, ok := jsonBoolField(usage, "estimated"); ok && estimated {
		return nil
	}
	prompt, ok := usageNumber(usage, "promptTokens", "prompt_tokens")
	if !ok {
		return nil
	}
	completion, ok := usageNumber(usage, "completionTokens", "completion_tokens")
	if !ok {
		return nil
	}
	total, ok := checkedSum(prompt, completion)
	if !ok {
		return nil
	}
	if !validateReportedTotal(usage, total) {
		return nil
	}
	invocations, ok := usageNumber(usage, "requestCount", "invocationCount", "invocation_count")
	if !ok {
		return nil
	}
	if invocations < 1 {
		invocations = 1
	}
	return FinalReportedUsage(
		prompt,
		optionalUsageNumber(usage, "cacheHitTokens", "cachedInputTokens", "cached_input_tokens"),
		completion,
		optionalUsageNumber(usage, "reasoningTokens", "reasoning_tokens"),
		invocations,
	)
}

// ---------------------------------------------------------------------------
// 多轮用量累加
// ---------------------------------------------------------------------------

// usageAccumulator 把流式输出里多轮的用量累加成一份 FINAL 报告。
// 对应 rs UsageAccumulator。
//
// 关键语义:一旦任何一路计数溢出,finish() 直接返回 nil。截断后的数字是错的,
// 上报错数字比不上报危害更大。
// cached / reasoning 只有在至少见过一次时才上报 —— 全程没有缓存信息时上报 0 会
// 被下游误读为「确实没命中缓存」。
type usageAccumulator struct {
	prompt        uint64
	completion    uint64
	cached        uint64
	reasoning     uint64
	cachedSeen    bool
	reasoningSeen bool
	invocations   uint64
	overflowed    bool
}

// accumulateCounter 累加并在溢出时返回 false(此时不修改 slot)。
func accumulateCounter(slot *uint64, value uint64) bool {
	if *slot > math.MaxUint64-value {
		return false
	}
	*slot += value
	return true
}

// add 累加一轮用量,调用次数记 1。
func (a *usageAccumulator) add(prompt, completion uint64, cached, reasoning *uint64) {
	a.addWithCount(prompt, completion, cached, reasoning, 1)
}

// addReport 把一份已有的 FINAL 报告并入累加器。
func (a *usageAccumulator) addReport(usage *BoundRunUsage) {
	if usage == nil {
		return
	}
	a.addWithCount(
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.CachedInputTokens,
		usage.ReasoningTokens,
		usage.InvocationCount,
	)
}

// addWithCount 累加一轮用量并指定调用次数。
func (a *usageAccumulator) addWithCount(
	prompt, completion uint64,
	cached, reasoning *uint64,
	invocationCount uint64,
) {
	if !accumulateCounter(&a.prompt, prompt) {
		a.overflowed = true
	}
	if !accumulateCounter(&a.completion, completion) {
		a.overflowed = true
	}
	if cached != nil {
		a.cachedSeen = true
		if !accumulateCounter(&a.cached, *cached) {
			a.overflowed = true
		}
	}
	if reasoning != nil {
		a.reasoningSeen = true
		if !accumulateCounter(&a.reasoning, *reasoning) {
			a.overflowed = true
		}
	}
	if !accumulateCounter(&a.invocations, invocationCount) {
		a.overflowed = true
	}
}

// finish 收口成一份 FINAL 报告;溢出过或数据不自洽时返回 nil。
func (a *usageAccumulator) finish() *BoundRunUsage {
	if a.overflowed {
		return nil
	}
	var cached, reasoning *uint64
	if a.cachedSeen {
		value := a.cached
		cached = &value
	}
	if a.reasoningSeen {
		value := a.reasoning
		reasoning = &value
	}
	return FinalReportedUsage(a.prompt, cached, a.completion, reasoning, a.invocations)
}

// ---------------------------------------------------------------------------
// 流式输出解析
// ---------------------------------------------------------------------------

// assistantMessageText 从一条 assistant 消息里取出文本。
// 对应 rs assistant_message_text。
//
// 语义要点:
//   - 有 text 字符串字段就直接返回,**即使是空串或纯空白**(Rust 这条路径没有
//     空白检查);
//   - 否则拼接 content[] 中 type == "text" 的 text,用 \n 连接;
//   - 拼接结果去空白后为空则视为「没有文本」,返回 ok=false。
func assistantMessageText(message any) (string, bool) {
	if text, ok := jsonStringField(message, "text"); ok {
		return text, true
	}
	contentValue, ok := jsonField(message, "content")
	if !ok {
		return "", false
	}
	content, ok := asJSONArray(contentValue)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, len(content))
	for _, part := range content {
		if kind, ok := jsonStringField(part, "type"); !ok || kind != "text" {
			continue
		}
		if text, ok := jsonStringField(part, "text"); ok {
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// jsonLineEvents 逐行解析 JSONL,静默跳过解析失败的行。
// 对应 rs `text.lines().filter_map(|line| serde_json::from_str(line).ok())`。
// 执行器的 stdout 里常混入非 JSON 的进度行,不能因此让整次执行失败。
func jsonLineEvents(text string) []any {
	lines := strings.Split(text, "\n")
	events := make([]any, 0, len(lines))
	for _, line := range lines {
		// Rust 的 str::lines 会剥掉行尾的 \r,这里对齐。
		line = strings.TrimSuffix(line, "\r")
		event, err := decodeJSONNumbers([]byte(line))
		if err != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}

// parsePiJSONLines 解析 Pi CLI 的 `--mode json` 事件流。
// 对应 rs parse_pi_json_lines。
//
// 只关心 type == "message_end" 且 message.role == "assistant" 的事件:
//   - 文本:后出现的覆盖先出现的(取最终答案);
//   - stopReason:**每条**匹配事件都会重写(含重写为「没有」),因此最终值取自最后
//     一条 assistant message_end;落在 error / aborted 上即判执行失败;
//   - 用量:prompt = input + cacheRead + cacheWrite,任一步溢出或自报 total 不一致
//     就跳过该轮(continue),不污染累加器;prompt 与 completion 全为 0 的轮次不计
//     (Pi 会为工具回执发空用量事件)。
//
// 返回值:候选结果、用量(没有可信用量时 nil)、执行错误。
func parsePiJSONLines(text string) (any, *BoundRunUsage, *ExecutionError) {
	var finalText *string
	var finalStopReason *string
	var totals usageAccumulator
	sawUsage := false

	for _, event := range jsonLineEvents(text) {
		if kind, ok := jsonStringField(event, "type"); !ok || kind != "message_end" {
			continue
		}
		message, ok := jsonField(event, "message")
		if !ok {
			continue
		}
		if role, ok := jsonStringField(message, "role"); !ok || role != "assistant" {
			continue
		}
		if value, ok := assistantMessageText(message); ok {
			captured := value
			finalText = &captured
		}
		finalStopReason = nil
		if reason, ok := jsonStringField(message, "stopReason"); ok {
			captured := reason
			finalStopReason = &captured
		} else if reason, ok := jsonStringField(message, "stop_reason"); ok {
			captured := reason
			finalStopReason = &captured
		}
		usage, ok := jsonField(message, "usage")
		if !ok {
			continue
		}
		input, ok := usageNumber(usage, "input", "input_tokens")
		if !ok {
			input = 0
		}
		cacheRead, ok := usageNumber(usage, "cacheRead", "cache_read")
		if !ok {
			cacheRead = 0
		}
		cacheWrite, ok := usageNumber(usage, "cacheWrite", "cache_write")
		if !ok {
			cacheWrite = 0
		}
		prompt, ok := checkedSum(input, cacheRead, cacheWrite)
		if !ok {
			continue
		}
		completion, ok := usageNumber(usage, "output", "output_tokens")
		if !ok {
			completion = 0
		}
		total, ok := checkedSum(prompt, completion)
		if !ok {
			continue
		}
		if !validateReportedTotal(usage, total) {
			continue
		}
		if prompt > 0 || completion > 0 {
			var cached *uint64
			if cacheRead > 0 {
				cached = &cacheRead
			}
			totals.add(
				prompt,
				completion,
				cached,
				optionalUsageNumber(usage, "reasoning", "reasoningTokens", "reasoning_tokens"),
			)
			sawUsage = true
		}
	}

	candidate := parseTextCandidate(text)
	if finalText != nil {
		candidate = parseTextCandidate(*finalText)
	}
	if finalStopReason != nil && (*finalStopReason == "error" || *finalStopReason == "aborted") {
		return nil, nil, newExecutionError(
			"executor_failed",
			"Pi final assistant message reported an unsuccessful stop reason",
		).WithExecutorOutput(candidate)
	}
	if !sawUsage {
		return candidate, nil, nil
	}
	return candidate, totals.finish(), nil
}

// parseCodexJSONLines 解析 Codex CLI 的 `exec --json` 事件流。
// 对应 rs parse_codex_json_lines。
//
// 关心三类事件:
//   - item.completed 且 item.type == "agent_message":取文本,后者覆盖前者;
//   - turn.completed:取该轮用量,prompt 与 completion 必须**都**能读到才计入
//     (Codex 不会给出半份用量,读不到说明键名对不上);
//   - turn.failed:判执行失败。
func parseCodexJSONLines(text string) (any, *BoundRunUsage, *ExecutionError) {
	var finalText *string
	turnFailed := false
	var totals usageAccumulator
	sawUsage := false

	for _, event := range jsonLineEvents(text) {
		kind, ok := jsonStringField(event, "type")
		if !ok {
			continue
		}
		switch kind {
		case "item.completed":
			item, ok := jsonField(event, "item")
			if !ok {
				continue
			}
			if itemKind, ok := jsonStringField(item, "type"); !ok || itemKind != "agent_message" {
				continue
			}
			if value, ok := assistantMessageText(item); ok {
				captured := value
				finalText = &captured
			}
		case "turn.completed":
			usage, ok := jsonField(event, "usage")
			if !ok {
				continue
			}
			prompt, promptOK := usageNumber(usage, "input_tokens", "prompt_tokens")
			completion, completionOK := usageNumber(usage, "output_tokens", "completion_tokens")
			if !promptOK || !completionOK {
				continue
			}
			totals.add(
				prompt,
				completion,
				optionalUsageNumber(usage, "cached_input_tokens"),
				optionalUsageNumber(usage, "reasoning_output_tokens", "reasoning_tokens"),
			)
			sawUsage = true
		case "turn.failed":
			turnFailed = true
		}
	}

	candidate := parseTextCandidate(text)
	if finalText != nil {
		candidate = parseTextCandidate(*finalText)
	}
	if turnFailed {
		return nil, nil, newExecutionError(
			"executor_failed",
			"Codex reported a failed turn",
		).WithExecutorOutput(candidate)
	}
	if !sawUsage {
		return candidate, nil, nil
	}
	return candidate, totals.finish(), nil
}

// ---------------------------------------------------------------------------
// 从自由文本里抠 JSON
// ---------------------------------------------------------------------------

// extractJSONFromText 从模型的自然语言输出里提取 JSON。
// 对应 rs extract_json_from_text。
//
// 三级退化:
//  1. 剥掉 markdown ``` 围栏(首行连语言标记一起丢,尾部 ``` 之后的内容丢);
//  2. 整串解析,成功即返回(任意 JSON 值);
//  3. 暴力扫描:从每个 `{` 出发试每个 `}` 收尾,返回第一个能解析成 JSON **对象**
//     的子串 —— 覆盖模型在 JSON 前后夹带解释文字、DSML 片段的情况。
//
// 全部失败时返回**未去空白的原文**字符串(与 rs 的 Value::String(text) 一致)。
func extractJSONFromText(text string) any {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		if index := strings.IndexByte(s, '\n'); index >= 0 {
			s = strings.TrimLeftFunc(s[index+1:], unicode.IsSpace)
		}
		if index := strings.LastIndex(s, "```"); index >= 0 {
			s = strings.TrimRightFunc(s[:index], unicode.IsSpace)
		}
	}
	if value, err := decodeJSONNumbers([]byte(s)); err == nil {
		return value
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		for j := i + 1; j < len(s); j++ {
			if s[j] != '}' {
				continue
			}
			value, err := decodeJSONNumbers([]byte(s[i : j+1]))
			if err != nil {
				continue
			}
			if _, ok := asJSONObject(value); ok {
				return value
			}
		}
	}
	return text
}
