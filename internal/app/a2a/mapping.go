package a2a

// A2A Message <-> 内部 TaskContext / ProcessedOutput 的映射。
// 对应 rs 侧 src/a2a/mapping.rs。
//
// A2A 调用方发来的是 message.parts,而执行器要的是结构化的
// templateInputData + positionType + 岗位契约。两者不自动对齐,因此提供
// 两条通道:
//
//  1. DataPart 直通(首选):parts 里出现 {"data": {...}} 时,把 data 当作
//     既有的 bound-task 信封解析,语义与 /agent/{slug} 完全一致。
//  2. TextPart 降级:只有文本时,文本进 taskBrief.userCustomInstructions,
//     positionType 由 Agent 的岗位推导。产出仍走岗位契约校验与纠正重试。

import (
	"encoding/json"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/position"
	"strings"
)

// a2aPositionTypeForRole 把岗位标识映射为协议 positionType。内部用小写
// 下划线,协议用大写下划线。
func a2aPositionTypeForRole(roleID string) string {
	switch roleID {
	case "technical_analyst":
		return "TECHNICAL_ANALYST"
	case "strategist":
		return "STRATEGIST"
	case "risk_manager":
		return "RISK_MANAGER"
	case "execution_trader":
		return "EXECUTION_TRADER"
	case "portfolio_manager":
		return "PORTFOLIO_MANAGER"
	default:
		return "FUNDAMENTAL_ANALYST"
	}
}

// A2AMappedRequest 是一次 A2A 请求解析出的执行输入。
type A2AMappedRequest struct {
	Context *TaskContext
	// RawInput 是传给执行器的原始输入(审计与运行记录用)。
	RawInput any
	// Structured 表示调用方是否使用了 DataPart 直通。
	Structured bool
}

// A2AMapRequest 把 A2A 消息映射为执行上下文。
//
// agent 用于在调用方未显式给出 positionType 时推导岗位;taskID 作为内部
// run_id,保证运行记录与 A2A task 一一对应。
func A2AMapRequest(message *A2AMessage, agent *agent.ResolvedConfig, taskID string) *A2AMappedRequest {
	if data, ok := message.FirstData(); ok {
		if envelope, isObject := data.(map[string]any); isObject {
			return a2aMapStructured(envelope, message, agent, taskID)
		}
		// data 存在但不是对象(数组/标量)时无法当信封解析,退回文本通道并
		// 把它作为附加输入保留。
		return a2aMapText(message, agent, taskID, data)
	}
	return a2aMapText(message, agent, taskID, nil)
}

// a2aMapStructured 是 DataPart 直通:data 即 bound-task 信封。
func a2aMapStructured(
	envelope map[string]any, message *A2AMessage, agent *agent.ResolvedConfig, taskID string,
) *A2AMappedRequest {
	// 解析失败不算致命:退回把整个 data 当 params 用。
	var task BoundTask
	if encoded, err := json.Marshal(envelope); err == nil {
		_ = json.Unmarshal(encoded, &task)
	}

	var params any
	if task.TemplateInputData != nil || envelope["params"] != nil {
		params = task.ParamsView(envelope)
	} else {
		params = envelope
	}

	// 调用方没给 positionType 时,用 Agent 自身岗位补齐,否则岗位契约校验
	// 会退化到默认的基本面结构。
	positionType := task.PositionType
	if positionType == nil || strings.TrimSpace(*positionType) == "" {
		positionType = a2aDefaultPositionType(agent)
	}

	// 文本内容作为补充指令并入 taskBrief,不丢失调用方的自然语言要求。
	taskBrief := a2aMergeTextIntoBrief(task.TaskBrief, message.TextContent())
	if taskBrief == nil {
		taskBrief = envelope
	}

	upstream := task.UpstreamPayload
	if upstream == nil {
		upstream = task.Upstream
	}

	context := &TaskContext{
		RunID:               &taskID,
		TaskBrief:           taskBrief,
		Params:              params,
		PositionType:        positionType,
		Language:            task.Language(),
		SymbolPool:          ExtractSymbolPool(task.GlobalParameters),
		UniverseMode:        ExtractUniverseMode(task.GlobalParameters),
		UpstreamSymbols:     ExtractUpstreamSymbols(upstream, task.PositionType),
		ForceFailureDefault: ExtractFailurePath(taskBrief),
		RuntimeContext:      task.RuntimeContext,
	}
	return &A2AMappedRequest{Context: context, RawInput: envelope, Structured: true}
}

// a2aMapText 是纯文本降级通道。
func a2aMapText(message *A2AMessage, agent *agent.ResolvedConfig, taskID string, extraData any) *A2AMappedRequest {
	instructions := message.TextContent()
	var taskBrief map[string]any
	if strings.TrimSpace(instructions) != "" {
		taskBrief = map[string]any{"userCustomInstructions": instructions}
	}
	var taskBriefValue any
	if taskBrief != nil {
		taskBriefValue = taskBrief
	}

	params := map[string]any{}
	if extraData != nil {
		params["input"] = extraData
	}

	rawInput := map[string]any{
		"source":    "a2a",
		"messageId": message.MessageID,
		"taskBrief": taskBriefValue,
	}

	context := &TaskContext{
		RunID:               &taskID,
		TaskBrief:           taskBriefValue,
		Params:              params,
		PositionType:        a2aDefaultPositionType(agent),
		ForceFailureDefault: ExtractFailurePath(taskBriefValue),
	}
	return &A2AMappedRequest{Context: context, RawInput: rawInput, Structured: false}
}

// a2aDefaultPositionType 由 Agent 的 SystemPrompt 岗位推导协议
// positionType;自定义提示词(非预置岗位)返回 nil,交由执行层按自定义处理。
func a2aDefaultPositionType(agent *agent.ResolvedConfig) *string {
	descriptor, ok := position.ResolveDescriptor(agent.SystemPrompt)
	if !ok {
		return nil
	}
	value := a2aPositionTypeForRole(descriptor.ID)
	return &value
}

// a2aMergeTextIntoBrief 把 A2A 的自然语言内容并入
// taskBrief.userCustomInstructions。已有内容时追加而不是覆盖。
func a2aMergeTextIntoBrief(brief any, text string) any {
	text = strings.TrimSpace(text)
	if text == "" {
		return brief
	}
	var object map[string]any
	switch typed := brief.(type) {
	case map[string]any:
		object = make(map[string]any, len(typed)+1)
		for k, v := range typed {
			object[k] = v
		}
	case string:
		object = map[string]any{"userCustomInstructions": typed}
	case nil:
		object = map[string]any{}
	default:
		object = map[string]any{"upstreamRequirements": typed}
	}
	merged := text
	if existing, ok := object["userCustomInstructions"].(string); ok && strings.TrimSpace(existing) != "" {
		merged = existing + "\n" + text
	}
	object["userCustomInstructions"] = merged
	return object
}

// A2AArtifactFromOutput 把执行结果映射为 A2A 产出物。
//
// 一个 artifact 两个 part:结构化 payload_result(DataPart)在前,人读摘要
// payload_info(TextPart)在后。这样只认文本的客户端也能用。
func A2AArtifactFromOutput(artifactID string, agent *agent.ResolvedConfig, output *ProcessedOutput) A2AArtifact {
	payload := output.ToTransportPayload(nil)
	parts := []A2APart{A2APartData(map[string]any{
		"payloadResult":   payload.PayloadResult,
		"contractVersion": payload.ContractVersion,
	})}
	if info, ok := a2aUsableSummaryText(payload.PayloadInfo); ok {
		parts = append(parts, A2APartText(info))
	}
	name := agent.Name + " result"
	var description *string
	if descriptor, ok := position.ResolveDescriptor(agent.SystemPrompt); ok {
		text := descriptor.Label + " 岗位契约输出"
		description = &text
	}
	return A2AArtifact{
		ArtifactID:  artifactID,
		Name:        &name,
		Description: description,
		Parts:       parts,
	}
}

// a2aUsableSummaryText 判断 payload_info 是否有真正可读的内容。
//
// ProcessedOutput.SummaryText() 在 summary 无法抽出文本时会回退到
// jsonString(summary),对空白字符串就得到带引号的 "\"   \""。直接判空会把
// 这种退化值当成正文塞进 TextPart,所以要先剥掉外层引号再判断。
func a2aUsableSummaryText(info string) (string, bool) {
	trimmed := strings.TrimSpace(info)
	unquoted := trimmed
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		unquoted = trimmed[1 : len(trimmed)-1]
	}
	if strings.TrimSpace(unquoted) == "" {
		return "", false
	}
	return trimmed, true
}

// A2APayloadFromArtifact 从产出物里取回结构化结果,供 GetTask 复原。
func A2APayloadFromArtifact(artifact *A2AArtifact) (any, bool) {
	for i := range artifact.Parts {
		if kind, value := artifact.Parts[i].Content(); kind == A2APartIsData {
			return value, true
		}
	}
	return nil, false
}
