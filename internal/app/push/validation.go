package push

// bound-task v1 信封校验矩阵（自旧栈 push.go 逐条移植）。
// 全部是纯函数：输入是 api 层提取好的原始值（Content-Type 字符串、
// 请求头多值切片、body 字节），输出结构化拒绝；不依赖任何 HTTP 类型。

import (
	"fmt"
	"net/url"
	"strings"

	"ir-agent-server/internal/domain/platform"
)

// positionTypes 是生产协议认可的岗位类型。
var positionTypes = map[string]bool{
	"FUNDAMENTAL_ANALYST": true,
	"TECHNICAL_ANALYST":   true,
	"PORTFOLIO_MANAGER":   true,
	"RISK_MANAGER":        true,
	"EXECUTION_TRADER":    true,
	"STRATEGIST":          true,
	"QUANT_TRADING_AGENT": true,
	"SENTINEL":            true,
}

// globalParameterKeys 是 bound-task v1 允许的 globalParameters 键。
// 平台评测任务会下发风控参数（2026-09-16 实测样本），属于合法契约内容。
var globalParameterKeys = map[string]bool{
	"language": true, "targetMarkets": true, "symbolPool": true, "universeMode": true,
	"riskLevel": true, "maxPositionSizeUsd": true, "maxDrawdownThreshold": true,
	"stopLossThresholdPct": true, "maxInvestmentTargetsUsd": true,
}

// validateContentType 只接受 application/json（允许带参数，如 charset）。
func validateContentType(contentType string) *RequestError {
	base, _, _ := strings.Cut(contentType, ";")
	if strings.EqualFold(strings.TrimSpace(base), "application/json") {
		return nil
	}
	return unsupportedMediaType()
}

// requiredUniqueHeader 要求某个请求头恰好出现一次且为合法 ASCII。
func requiredUniqueHeader(values []string, displayName string) (string, *RequestError) {
	if len(values) != 1 {
		return "", pushValidation(displayName, "must be present exactly once")
	}
	if !asciiHeaderValue(values[0]) {
		return "", pushValidation(displayName, "must be valid ASCII header text")
	}
	return values[0], nil
}

// requiredSignatureHeader 取签名头。缺失是 401（凭证问题），重复是 422
// （请求构造问题）。
func requiredSignatureHeader(values []string) (string, *RequestError) {
	switch len(values) {
	case 0:
		return "", unauthorized("missing " + platform.HeaderSignature + " header")
	case 1:
		if !asciiHeaderValue(values[0]) {
			return "", unauthorized(platform.HeaderSignature + " is invalid")
		}
		return values[0], nil
	default:
		return "", pushValidation(platform.HeaderSignature, "must be present exactly once")
	}
}

// validatePushMetadata 校验 runId / 幂等键：非空、不超长、无首尾空白、无控制字符。
func validatePushMetadata(value, field string, maxLen int) *RequestError {
	if value == "" || len(value) > maxLen || value != strings.TrimSpace(value) ||
		hasASCIIControl(value) {
		return pushValidation(field, fmt.Sprintf(
			"must contain 1-%d characters without outer whitespace or controls", maxLen))
	}
	return nil
}

func hasASCIIControl(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}

// asciiHeaderValue 要求可见 ASCII（0x20–0x7e），与旧栈一致。
func asciiHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// validateTask 校验整个任务信封。所有字段错误一次性收集后统一返回。
func validateTask(task *platform.BoundTask, headerRunID string) *RequestError {
	var errs []FieldError

	requireExact(&errs, "schemaVersion", task.SchemaVersion, "1.0")
	requireNonEmpty(&errs, "runId", task.RunID)
	requireNonEmpty(&errs, "agentCode", task.AgentCode)
	// templateId / positionType 是平台下发的条件字段：正式任务与连通测试的探测
	// 任务（taskBrief.probe=true）都可能不带，不能当作必填。
	requireNonEmpty(&errs, "callbackUrl", task.CallbackURL)

	if task.RunID != nil && *task.RunID != headerRunID {
		errs = append(errs, fieldError("runId", "must exactly match "+platform.HeaderRunID))
	}
	if task.PositionType != nil && !positionTypes[*task.PositionType] {
		errs = append(errs,
			fieldError("positionType", "is not a supported production position type"))
	}

	if task.TemplateInputData == nil {
		errs = append(errs, fieldError("templateInputData", "is required"))
	} else {
		seen := map[string]bool{}
		for index, entry := range *task.TemplateInputData {
			field := fmt.Sprintf("templateInputData[%d].name", index)
			switch {
			case strings.TrimSpace(entry.Name) == "":
				errs = append(errs, fieldError(field, "must not be empty"))
			case seen[entry.Name]:
				errs = append(errs, fieldError(field, "must be unique within templateInputData"))
			default:
				seen[entry.Name] = true
			}
		}
	}

	if task.TaskBrief != nil {
		if _, ok := platform.AsJSONObject(task.TaskBrief); !ok {
			errs = append(errs, fieldError("taskBrief", "must be an object"))
		}
	}
	validateGlobalParameters(task.GlobalParameters, &errs)
	validateUpstreamPayload(task.UpstreamPayload, &errs)
	validateUpstream(task.Upstream, &errs)
	validateRuntimeContext(task.RuntimeContext, &errs)
	validateCallbackURL(task.CallbackURL, &errs)

	if len(errs) == 0 {
		return nil
	}
	return validationError(errs)
}

// validateGlobalParameters 校验 globalParameters：键白名单 + 各字段类型。
func validateGlobalParameters(value any, errs *[]FieldError) {
	if value == nil {
		return
	}
	parameters, ok := platform.AsJSONObject(value)
	if !ok {
		*errs = append(*errs, fieldError("globalParameters", "must be an object"))
		return
	}
	// 键必须逐个过白名单：多余键说明调用方在用未约定的扩展。
	for key := range parameters {
		if !globalParameterKeys[key] {
			*errs = append(*errs, fieldError("globalParameters."+key,
				"is not allowed by the bound-task v1 contract"))
		}
	}
	if language, present := parameters["language"]; present {
		if _, ok := language.(string); !ok {
			*errs = append(*errs, fieldError("globalParameters.language", "must be a string"))
		}
	}
	if markets, present := parameters["targetMarkets"]; present {
		if !isStringArray(markets, false) {
			*errs = append(*errs,
				fieldError("globalParameters.targetMarkets", "must be an array of strings"))
		}
	}
	if symbols, present := parameters["symbolPool"]; present {
		if !isStringArray(symbols, true) {
			*errs = append(*errs,
				fieldError("globalParameters.symbolPool", "must be an array of non-empty strings"))
		}
	}
	if mode, present := parameters["universeMode"]; present {
		text, ok := mode.(string)
		if !ok || (text != "RESTRICTED" && text != "SEED") {
			*errs = append(*errs,
				fieldError("globalParameters.universeMode", "must be RESTRICTED or SEED"))
		}
	}
}

// isStringArray 判断值是否为字符串数组。requireNonBlank 为 true 时元素还不能是空白。
func isStringArray(value any, requireNonBlank bool) bool {
	items, ok := platform.AsJSONArray(value)
	if !ok {
		return false
	}
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return false
		}
		if requireNonBlank && strings.TrimSpace(text) == "" {
			return false
		}
	}
	return true
}

// validateUpstream 校验上游 DAG 结构：self 是对象，nodes/edges 是数组，
// 每条边都要有非空的 from 与 to。
func validateUpstream(value any, errs *[]FieldError) {
	if value == nil {
		return
	}
	upstream, ok := platform.AsJSONObject(value)
	if !ok {
		*errs = append(*errs, fieldError("upstream", "must be an object"))
		return
	}
	if _, ok := platform.AsJSONObject(upstream["self"]); !ok {
		*errs = append(*errs, fieldError("upstream.self", "must be an object"))
	}
	nodes, nodesOK := platform.AsJSONArray(upstream["nodes"])
	if !nodesOK {
		*errs = append(*errs, fieldError("upstream.nodes", "must be an array"))
	}
	edges, edgesOK := platform.AsJSONArray(upstream["edges"])
	if !edgesOK {
		*errs = append(*errs, fieldError("upstream.edges", "must be an array"))
	}
	for index, node := range nodes {
		if _, ok := platform.AsJSONObject(node); !ok {
			*errs = append(*errs,
				fieldError(fmt.Sprintf("upstream.nodes[%d]", index), "must be an object"))
		}
	}
	for index, edge := range edges {
		valid := false
		if object, ok := platform.AsJSONObject(edge); ok {
			from, fromOK := object["from"].(string)
			to, toOK := object["to"].(string)
			valid = fromOK && toOK &&
				strings.TrimSpace(from) != "" && strings.TrimSpace(to) != ""
		}
		if !valid {
			*errs = append(*errs, fieldError(fmt.Sprintf("upstream.edges[%d]", index),
				"must contain non-empty from and to"))
		}
	}
}

// validateUpstreamPayload 校验上游产出数组：每项都要有非空 agentInstanceId 与
// **存在**的 payloadResult（值本身不限类型，允许 null）。
func validateUpstreamPayload(value any, errs *[]FieldError) {
	if value == nil {
		return
	}
	items, ok := platform.AsJSONArray(value)
	if !ok {
		*errs = append(*errs, fieldError("upstreamPayload", "must be an array"))
		return
	}
	for index, raw := range items {
		item, ok := platform.AsJSONObject(raw)
		if !ok {
			*errs = append(*errs,
				fieldError(fmt.Sprintf("upstreamPayload[%d]", index), "must be an object"))
			continue
		}
		agentInstanceID, isString := item["agentInstanceId"].(string)
		if !isString || strings.TrimSpace(agentInstanceID) == "" {
			*errs = append(*errs,
				fieldError(fmt.Sprintf("upstreamPayload[%d].agentInstanceId", index),
					"is required and must be a non-empty string"))
		}
		if _, present := item["payloadResult"]; !present {
			*errs = append(*errs,
				fieldError(fmt.Sprintf("upstreamPayload[%d].payloadResult", index), "is required"))
		}
	}
}

// validateRuntimeContext 校验运行期上下文：只允许 isFinal，且必须是布尔。
func validateRuntimeContext(value any, errs *[]FieldError) {
	if value == nil {
		return
	}
	object, ok := platform.AsJSONObject(value)
	if !ok {
		*errs = append(*errs, fieldError("runtimeContext", "must be an object"))
		return
	}
	for key := range object {
		if key != "isFinal" {
			*errs = append(*errs, fieldError("runtimeContext."+key,
				"is not allowed by the bound-task v1 contract"))
		}
	}
	if _, ok := object["isFinal"].(bool); !ok {
		*errs = append(*errs, fieldError("runtimeContext.isFinal",
			"is required and must be a boolean when runtimeContext is present"))
	}
}

// validateCallbackURL 要求绝对 HTTPS 地址；HTTP 只对回环放行（本地集成测试用）。
func validateCallbackURL(value *string, errs *[]FieldError) {
	if value == nil {
		return
	}
	const message = "must be an absolute HTTPS URL (HTTP is allowed only for loopback tests)"
	parsed, err := url.Parse(*value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		*errs = append(*errs, fieldError("callbackUrl", message))
		return
	}
	if parsed.User != nil {
		*errs = append(*errs, fieldError("callbackUrl", "must not contain credentials"))
		return
	}
	if parsed.Scheme == "https" {
		return
	}
	if parsed.Scheme == "http" && platform.IsLoopbackHost(parsed.Hostname()) {
		return
	}
	*errs = append(*errs, fieldError("callbackUrl", message))
}

func fieldError(field, message string) FieldError {
	return FieldError{Field: field, Message: message}
}

// requireNonEmpty 要求字段存在且去空白后非空。
func requireNonEmpty(errs *[]FieldError, field string, value *string) {
	if value == nil || strings.TrimSpace(*value) == "" {
		*errs = append(*errs, fieldError(field, "is required and must not be empty"))
	}
}

// requireExact 要求字段严格等于期望值（缺失也算不等）。
func requireExact(errs *[]FieldError, field string, value *string, expected string) {
	if value == nil || *value != expected {
		*errs = append(*errs, fieldError(field, fmt.Sprintf("must equal %q", expected)))
	}
}
