package mota

// JSON-RPC 方法分派。对应 rs 侧 src/a2a/handler.rs。
//
// v1.0 的方法名是 PascalCase(SendMessage / GetTask),不是 v0.3 的
// message/send / tasks/get。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// 支持的方法名。未实现的方法要能区分"不存在"和"暂不支持"。
const (
	A2AMethodSendMessage          = "SendMessage"
	A2AMethodGetTask              = "GetTask"
	A2AMethodSendStreamingMessage = "SendStreamingMessage"
	A2AMethodSubscribeToTask      = "SubscribeToTask"
	A2AMethodCancelTask           = "CancelTask"
	A2AMethodListTasks            = "ListTasks"
)

// a2aNotImplemented 是已在规范中定义但本实现尚未提供的方法。返回
// UnsupportedOperation 而不是 MethodNotFound,让调用方知道是能力缺失而非
// 拼错方法名。
var a2aNotImplemented = map[string]bool{
	A2AMethodSendStreamingMessage: true,
	A2AMethodSubscribeToTask:      true,
	A2AMethodCancelTask:           true,
	A2AMethodListTasks:            true,
}

// A2ADispatch 分派一次 JSON-RPC 调用,返回 result 的 JSON 值。
func A2ADispatch(
	ctx context.Context, state *A2aState, runtime *AgentRuntime, request *A2AJSONRPCRequest,
) (json.RawMessage, *A2AJSONRPCError) {
	switch request.Method {
	case A2AMethodSendMessage:
		return a2aSendMessage(ctx, state, runtime, request.Params)
	case A2AMethodGetTask:
		return a2aGetTask(ctx, state, runtime, request.Params)
	default:
		if a2aNotImplemented[request.Method] {
			return nil, A2AErrUnsupportedOperation.WithMetadata(map[string]any{"method": request.Method})
		}
		return nil, A2AErrMethodNotFound.WithMetadata(map[string]any{"method": request.Method})
	}
}

func a2aSendMessage(
	ctx context.Context, state *A2aState, runtime *AgentRuntime, rawParams json.RawMessage,
) (json.RawMessage, *A2AJSONRPCError) {
	var params A2ASendMessageRequest
	if rpcErr := a2aParseParams(rawParams, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if params.Message == nil {
		return nil, a2aFieldViolation("message", "message is required")
	}
	message := params.Message
	if len(message.Parts) == 0 {
		return nil, a2aFieldViolation("message.parts", "at least one part is required")
	}
	for index := range message.Parts {
		if !message.Parts[index].HasSingleContent() {
			return nil, a2aFieldViolation(
				fieldIndexName("message.parts", index),
				"each part must set exactly one of text, raw, url, or data")
		}
	}
	var configuration A2ASendMessageConfiguration
	if params.Configuration != nil {
		configuration = *params.Configuration
	}
	// 推送通知未实现:能力校验必须在开始执行前拒绝(§3.3.4)。
	if configuration.TaskPushNotificationConfig != nil {
		return nil, A2AErrPushNotificationNotSupported.IntoError()
	}
	// 非阻塞模式需要后台执行 + 轮询,属于后续阶段。明确拒绝而不是静默当成
	// 阻塞,否则调用方会以为拿到的是"已创建"的任务。
	if configuration.ReturnImmediately {
		return nil, A2AErrUnsupportedOperation.WithDetail(
			"returnImmediately is not supported yet; omit it to use blocking execution")
	}

	agent := runtime.Config
	// task id / context id 沿用平台既有的随机令牌生成,不额外引入 uuid 依赖。
	taskID := "task-" + randomToken(16)
	// 多轮会话:调用方带了 contextId 就沿用,否则新建。
	contextID := ""
	if message.ContextID != nil && trimSpaceNonEmpty(*message.ContextID) {
		contextID = *message.ContextID
	} else {
		contextID = "ctx-" + randomToken(16)
	}

	mapped := A2AMapRequest(message, agent, taskID)
	storedRequest := &A2AStoredRequest{
		Channel:    "a2a",
		ContextID:  contextID,
		MessageID:  message.MessageID,
		Structured: mapped.Structured,
		Input:      mapped.RawInput,
	}
	outcome, err := state.tasks.Start(ctx, agent, taskID, storedRequest)
	if err != nil {
		return nil, a2aInternalError("register A2A task", err)
	}
	if outcome == A2AStartConflict {
		return nil, a2aInternalError("register A2A task",
			errors.New("generated task id collided with an existing run"))
	}

	started := time.Now()
	timeout := time.Duration(agent.TimeoutSecs) * time.Second
	output, execErr := state.dispatcher.Execute(ctx, agent.Slug, mapped.Context, mapped.RawInput, timeout)
	durationMs := time.Since(started).Milliseconds()

	history := []A2AMessage{*message}
	ApplyA2AHistoryLength(&history, configuration.HistoryLength)

	var task A2ATask
	var errorCode *string
	var executorOutput *string

	if execErr == nil {
		artifactID := "artifact-" + randomToken(12)
		artifact := A2AArtifactFromOutput(artifactID, agent, output)
		task = A2ATask{
			ID: taskID, ContextID: contextID,
			Status:    NewA2ATaskStatus(A2ATaskStateCompleted),
			Artifacts: []A2AArtifact{artifact},
			History:   history,
		}
		executorOutput = output.ExecutorResultJSON()
	} else {
		var dispatchErr *DispatchError
		errors.As(execErr, &dispatchErr)
		task = A2ATask{
			ID: taskID, ContextID: contextID,
			Status:  NewA2ATaskStatusWithMessage(a2aFailureState(dispatchErr), a2aFailureMessage(execErr)),
			History: history,
		}
		if dispatchErr != nil {
			code := dispatchErr.Code
			errorCode = &code
		}
	}

	if err := state.tasks.Finish(ctx, agent.Slug, &task, durationMs, errorCode, executorOutput); err != nil {
		// 执行已经发生,结果不能因为记账失败而丢弃。记录后照常返回。
		a2aLogPersistError(err, agent.Slug, task.ID)
	}

	result, err := json.Marshal(A2ASendMessageResponseWithTask(task))
	if err != nil {
		return nil, a2aInternalError("serialize SendMessage result", err)
	}
	return result, nil
}

func a2aGetTask(
	ctx context.Context, state *A2aState, runtime *AgentRuntime, rawParams json.RawMessage,
) (json.RawMessage, *A2AJSONRPCError) {
	var params A2AGetTaskRequest
	if rpcErr := a2aParseParams(rawParams, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if !trimSpaceNonEmpty(params.ID) {
		return nil, a2aFieldViolation("id", "task id is required")
	}
	// 按 (slug, task id) 查询,天然把可见性限定在当前 Agent 内(§3.1.3 要求
	// 服务端只返回调用方可见的任务)。
	id := trimSpaceValue(params.ID)
	task, err := state.tasks.Get(ctx, runtime.Config.Slug, id)
	if err != nil {
		return nil, a2aInternalError("load A2A task", err)
	}
	if task == nil {
		return nil, A2AErrTaskNotFound.WithMetadata(map[string]any{
			"taskId":    params.ID,
			"timestamp": a2aNowTimestamp(),
		})
	}
	ApplyA2AHistoryLength(&task.History, params.HistoryLength)
	result, err := json.Marshal(task)
	if err != nil {
		return nil, a2aInternalError("serialize GetTask result", err)
	}
	return result, nil
}

// a2aParseParams 反序列化 params;缺省视为空对象,类型不符按
// InvalidParams 返回。
func a2aParseParams(raw json.RawMessage, target any) *A2AJSONRPCError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return A2AErrInvalidParams.WithMetadata(map[string]any{"detail": err.Error()})
	}
	return nil
}

// a2aFieldViolation 是字段级校验错误。带 google.rpc.BadRequest 明细,规范
// 推荐这种做法(§9.5)。
func a2aFieldViolation(field, description string) *A2AJSONRPCError {
	errorInfo := map[string]any{
		"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
		"reason": A2AErrInvalidParams.Reason(),
		"domain": A2AErrorDomain,
	}
	badRequest := map[string]any{
		"@type": "type.googleapis.com/google.rpc.BadRequest",
		"fieldViolations": []map[string]any{
			{"field": field, "description": description},
		},
	}
	data, _ := json.Marshal([]any{errorInfo, badRequest})
	return &A2AJSONRPCError{
		Code: A2AErrInvalidParams.Code(), Message: A2AErrInvalidParams.Message(), Data: data,
	}
}

// a2aFailureState 把分发错误映射为 A2A 任务状态。容量类错误是"没做",
// 不是"失败"。
func a2aFailureState(dispatchErr *DispatchError) A2ATaskState {
	if dispatchErr == nil {
		return A2ATaskStateFailed
	}
	switch dispatchErr.Code {
	case "agent_disabled", "agent_not_found", "agent_overloaded":
		return A2ATaskStateRejected
	default:
		return A2ATaskStateFailed
	}
}

// a2aFailureMessage 把失败原因作为状态消息带回调用方。
func a2aFailureMessage(err error) A2AMessage {
	return A2AMessage{
		MessageID: "msg-" + randomToken(12),
		Role:      A2ARoleAgent,
		Parts:     []A2APart{A2APartText(err.Error())},
	}
}

// a2aInternalError 内部错误统一收敛:日志里留细节,回给调用方的只有错误码。
func a2aInternalError(context string, err error) *A2AJSONRPCError {
	a2aLogError(context, err)
	return A2AErrInternal.IntoError()
}

func trimSpaceNonEmpty(value string) bool { return trimSpaceValue(value) != "" }

func trimSpaceValue(value string) string { return strings.TrimSpace(value) }

// fieldIndexName 构造 "prefix[index]" 形式的字段名,用于 field-violation 消息。
func fieldIndexName(prefix string, index int) string {
	return fmt.Sprintf("%s[%d]", prefix, index)
}

// a2aLogError 统一记录 A2A 内部错误。
func a2aLogError(context string, err error) {
	slog.Error("A2A internal error", "context", context, "error", err)
}

// a2aLogPersistError 记录一次任务结果落库失败(执行已经发生,只记日志,
// 不影响已经返回给调用方的结果)。
func a2aLogPersistError(err error, slug, taskID string) {
	slog.Error("persist A2A task result", "error", err, "slug", slug, "task_id", taskID)
}
