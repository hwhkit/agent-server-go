package mota

// A2A v1.0 JSON-RPC 端点:SendMessage 与 GetTask 的端到端行为。
// 对应 rs tests/a2a_rpc_test.rs。
//
// 执行器用写到临时目录的假 Pi CLI 顶替,所以整条链路(JSON-RPC -> 映射 ->
// AgentDispatcher -> 本机 CLI -> 岗位归一化 -> Artifact)都是真的,只有
// 最外层的模型调用被替换。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// a2aPiOutput 是假 Pi 输出一份满足基本面岗位契约的结果。
const a2aPiOutput = `{"payload_result":{"assessments":[{"symbol":"AAPL.US","rating":"HOLD","fair_value":200,"reasoning":"A2A end-to-end test."}],"summary":"a2a summary"}}`

type a2aHarness struct {
	server *httptest.Server
}

// newA2AHarness 组装一个走假 Pi CLI 的 A2A 测试服务。cliStdout 为空表示
// 用一个必然失败的 CLI(用于失败路径用例)。
func newA2AHarness(t *testing.T, agents []*AgentConfig, config *A2AConfig, cliStdout string) *a2aHarness {
	t.Helper()
	var script string
	if cliStdout != "" {
		script = "printf '%s' '" + cliStdout + "'\n"
	} else {
		script = "echo 'boom' >&2\nexit 3\n"
	}
	binary := writeFakeCLI(t, script)
	state := a2aTestState(t, agents, config, binary)
	return &a2aHarness{server: httptest.NewServer(A2ARouter(state))}
}

func newA2AFailingHarness(t *testing.T, agents []*AgentConfig, config *A2AConfig) *a2aHarness {
	t.Helper()
	return newA2AHarness(t, agents, config, "")
}

// call 发一次 JSON-RPC 调用,返回 (HTTP 状态, 响应体)。
func (h *a2aHarness) call(t *testing.T, body map[string]any, bearer string) (int, map[string]any) {
	t.Helper()
	return h.callWithHeaders(t, body, bearer, nil)
}

func (h *a2aHarness) callWithHeaders(
	t *testing.T, body map[string]any, bearer string, extra map[string]string,
) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/a2a/desk-1", jsonReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// callRaw 发一次带原始字节 body 的请求(用于构造非法 JSON)。
func (h *a2aHarness) callRaw(t *testing.T, raw []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/a2a/desk-1", jsonReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// sendText 发送一条纯文本消息并返回 JSON-RPC result。
func (h *a2aHarness) sendText(t *testing.T, text string) map[string]any {
	t.Helper()
	status, body := h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "SendMessage",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": "msg-1", "role": "ROLE_USER",
				"parts": []any{map[string]any{"text": text}},
			},
		},
	}, "")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", status, body)
	}
	if _, hasError := body["error"]; hasError {
		t.Fatalf("unexpected error: %v", body)
	}
	return body["result"].(map[string]any)
}

func a2aRPC(method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params}
}

func a2aOneAgent() []*AgentConfig {
	return []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", true)}
}

func jsonReader(raw []byte) *bytes.Reader { return bytes.NewReader(raw) }

// ---------------------------------------------------------------------------
// SendMessage happy path
// ---------------------------------------------------------------------------

func TestA2ARPCSendMessageReturnsACompletedTaskWithArtifacts(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	result := h.sendText(t, "评估 AAPL")

	task := result["task"].(map[string]any)
	if task["id"].(string) == "" {
		t.Error("task id must not be empty")
	}
	if task["contextId"].(string) == "" {
		t.Error("contextId must not be empty")
	}
	status := task["status"].(map[string]any)
	if status["state"] != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %v", status["state"])
	}
	timestamp := status["timestamp"].(string)
	if timestamp[len(timestamp)-1] != 'Z' {
		t.Errorf("timestamp = %q, want Z suffix", timestamp)
	}
	if !containsRune(timestamp, '.') {
		t.Errorf("timestamp = %q, want millisecond precision", timestamp)
	}

	artifacts := task["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(artifacts))
	}
	parts := artifacts[0].(map[string]any)["parts"].([]any)
	part0 := parts[0].(map[string]any)
	if part0["mediaType"] != "application/json" {
		t.Errorf("mediaType = %v", part0["mediaType"])
	}
	data := part0["data"].(map[string]any)
	payloadResult := data["payloadResult"].(map[string]any)
	assessments := payloadResult["assessments"].([]any)
	if assessments[0].(map[string]any)["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v", assessments[0])
	}
	if data["contractVersion"] != "V2" {
		t.Errorf("contractVersion = %v", data["contractVersion"])
	}
	part1 := parts[1].(map[string]any)
	if part1["text"] != "a2a summary" {
		t.Errorf("summary text = %v", part1["text"])
	}
	// v1.0 的 Part 不带 kind 判别字段。
	if _, present := part0["kind"]; present {
		t.Error("part0 must not have a kind field")
	}
	if _, present := part1["kind"]; present {
		t.Error("part1 must not have a kind field")
	}
}

func TestA2ARPCSendMessageEchoesHistoryAndUsesScreamingSnakeRole(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	result := h.sendText(t, "评估 AAPL")
	history := result["task"].(map[string]any)["history"].([]any)
	if len(history) != 1 {
		t.Fatalf("history = %d, want 1", len(history))
	}
	entry := history[0].(map[string]any)
	if entry["role"] != "ROLE_USER" {
		t.Errorf("role = %v", entry["role"])
	}
	if entry["messageId"] != "msg-1" {
		t.Errorf("messageId = %v", entry["messageId"])
	}
}

func TestA2ARPCDataPartEnvelopeDrivesThePositionContract(t *testing.T) {
	// 技术分析岗位的契约主字段是 signals,不是 assessments。
	technical := `{"payload_result":{"signals":[{"symbol":"NVDA.US","bias":"LONG","entry_zone":[100,110],"reasoning":"A2A structured test."}]}}`
	h := newA2AHarness(t, []*AgentConfig{a2aAgentConfig("desk-1", "technical_analyst", true)},
		a2aTestConfig(""), technical)

	status, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "msg-1", "role": "ROLE_USER",
			"parts": []any{map[string]any{
				"data": map[string]any{
					"positionType": "TECHNICAL_ANALYST",
					"templateInputData": []any{
						map[string]any{"name": "securities", "value": []any{"NVDA.US"}},
					},
					"globalParameters": map[string]any{"symbolPool": []any{"NVDA.US"}},
				},
				"mediaType": "application/json",
			}},
		},
	}), "")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, hasError := body["error"]; hasError {
		t.Fatalf("unexpected error: %v", body)
	}
	task := body["result"].(map[string]any)["task"].(map[string]any)
	artifacts := task["artifacts"].([]any)
	parts := artifacts[0].(map[string]any)["parts"].([]any)
	data := parts[0].(map[string]any)["data"].(map[string]any)
	payload := data["payloadResult"].(map[string]any)
	signals := payload["signals"].([]any)
	if signals[0].(map[string]any)["symbol"] != "NVDA.US" {
		t.Errorf("symbol = %v", signals[0])
	}
}

// ---------------------------------------------------------------------------
// GetTask
// ---------------------------------------------------------------------------

func TestA2ARPCGetTaskReturnsThePersistedTask(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	sent := h.sendText(t, "评估 AAPL")
	sentTask := sent["task"].(map[string]any)
	taskID := sentTask["id"].(string)
	contextID := sentTask["contextId"].(string)

	status, body := h.call(t, a2aRPC("GetTask", map[string]any{"id": taskID}), "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, hasError := body["error"]; hasError {
		t.Fatalf("unexpected error: %v", body)
	}
	task := body["result"].(map[string]any)
	if task["id"] != taskID {
		t.Errorf("id = %v, want %v", task["id"], taskID)
	}
	if task["contextId"] != contextID {
		t.Errorf("contextId = %v, want %v", task["contextId"], contextID)
	}
	status2 := task["status"].(map[string]any)
	if status2["state"] != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %v", status2["state"])
	}
	artifacts := task["artifacts"].([]any)
	parts := artifacts[0].(map[string]any)["parts"].([]any)
	data := parts[0].(map[string]any)["data"].(map[string]any)
	payload := data["payloadResult"].(map[string]any)
	assessments := payload["assessments"].([]any)
	if assessments[0].(map[string]any)["symbol"] != "AAPL.US" {
		t.Errorf("symbol = %v", assessments[0])
	}
}

func TestA2ARPCGetTaskHonoursHistoryLengthZero(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	sent := h.sendText(t, "评估 AAPL")
	taskID := sent["task"].(map[string]any)["id"].(string)

	_, body := h.call(t, a2aRPC("GetTask", map[string]any{"id": taskID, "historyLength": 0}), "")
	result := body["result"].(map[string]any)
	// historyLength=0 表示不返回历史,字段应被省略。
	if _, present := result["history"]; present {
		t.Errorf("history should be omitted, got %v", body)
	}
}

func TestA2ARPCGetTaskForUnknownIDIsTaskNotFound(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	status, body := h.call(t, a2aRPC("GetTask", map[string]any{"id": "task-does-not-exist"}), "")

	// 协议层错误仍然是 HTTP 200。
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32001 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	info := data[0].(map[string]any)
	if info["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
		t.Errorf("@type = %v", info["@type"])
	}
	if info["reason"] != "TASK_NOT_FOUND" {
		t.Errorf("reason = %v", info["reason"])
	}
	if info["domain"] != "a2a-protocol.org" {
		t.Errorf("domain = %v", info["domain"])
	}
	metadata := info["metadata"].(map[string]any)
	if metadata["taskId"] != "task-does-not-exist" {
		t.Errorf("metadata.taskId = %v", metadata["taskId"])
	}
}

// ---------------------------------------------------------------------------
// 执行失败
// ---------------------------------------------------------------------------

func TestA2ARPCExecutorFailureBecomesAFailedTaskNotAnRPCError(t *testing.T) {
	h := newA2AFailingHarness(t, a2aOneAgent(), a2aTestConfig(""))
	result := h.sendText(t, "评估 AAPL")
	task := result["task"].(map[string]any)
	status := task["status"].(map[string]any)
	if status["state"] != "TASK_STATE_FAILED" {
		t.Errorf("state = %v", status["state"])
	}
	// 失败通过稳定公开文案带回，不能泄露子进程状态、路径或 Provider 回执。
	message := status["message"].(map[string]any)
	if message["role"] != "ROLE_AGENT" {
		t.Errorf("role = %v", message["role"])
	}
	parts := message["parts"].([]any)
	if text := parts[0].(map[string]any)["text"].(string); text != "Agent execution failed" {
		t.Errorf("failure message = %q, want sanitized public text", text)
	}
	if artifacts, ok := task["artifacts"].([]any); ok && len(artifacts) != 0 {
		t.Errorf("artifacts = %v, want empty", artifacts)
	}
}

func TestA2ARPCFailedTaskIsStillRetrievableViaGetTask(t *testing.T) {
	h := newA2AFailingHarness(t, a2aOneAgent(), a2aTestConfig(""))
	sent := h.sendText(t, "评估 AAPL")
	taskID := sent["task"].(map[string]any)["id"].(string)

	_, body := h.call(t, a2aRPC("GetTask", map[string]any{"id": taskID}), "")
	result := body["result"].(map[string]any)
	status := result["status"].(map[string]any)
	if status["state"] != "TASK_STATE_FAILED" {
		t.Errorf("state = %v", status["state"])
	}
}

func TestA2ARPCDisabledAgentReturnsARejectedTaskState(t *testing.T) {
	// 停用的 Agent 在路由层就 404,不会进入协议处理。
	h := newA2AHarness(t, []*AgentConfig{a2aAgentConfig("desk-1", "fundamental_analyst", false)},
		a2aTestConfig(""), a2aPiOutput)
	status, _ := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
	}), "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// ---------------------------------------------------------------------------
// 方法与协议层校验
// ---------------------------------------------------------------------------

func TestA2ARPCUnknownMethodIsMethodNotFound(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("message/send", map[string]any{}), "")
	// v0.3 的 slash 方法名在 v1.0 下不存在。
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32601 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	if data[0].(map[string]any)["reason"] != "METHOD_NOT_FOUND" {
		t.Errorf("reason = %v", data[0])
	}
}

func TestA2ARPCDeclaredButUnimplementedMethodsReportUnsupportedOperation(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	for _, method := range []string{
		"SendStreamingMessage", "SubscribeToTask", "CancelTask", "ListTasks",
	} {
		_, body := h.call(t, a2aRPC(method, map[string]any{"id": "t"}), "")
		rpcErr := body["error"].(map[string]any)
		if int(rpcErr["code"].(float64)) != -32004 {
			t.Errorf("%s: code = %v", method, rpcErr["code"])
		}
		data := rpcErr["data"].([]any)
		if data[0].(map[string]any)["reason"] != "UNSUPPORTED_OPERATION" {
			t.Errorf("%s: reason = %v", method, data[0])
		}
	}
}

func TestA2ARPCMalformedJSONIsParseError(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	resp := h.callRaw(t, []byte("{not json"))
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32700 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	if body["id"] != nil {
		t.Errorf("id = %v, want null", body["id"])
	}
}

func TestA2ARPCWrongJSONRPCVersionIsInvalidRequest(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, map[string]any{
		"jsonrpc": "1.0", "id": 3, "method": "GetTask", "params": map[string]any{"id": "t"},
	}, "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32600 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	// 错误响应必须回填请求 id。
	if int(body["id"].(float64)) != 3 {
		t.Errorf("id = %v, want 3", body["id"])
	}
}

func TestA2ARPCMissingMessageIsAFieldViolation(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{}), "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32602 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	violations := data[1].(map[string]any)
	if violations["@type"] != "type.googleapis.com/google.rpc.BadRequest" {
		t.Errorf("@type = %v", violations["@type"])
	}
	fieldViolations := violations["fieldViolations"].([]any)
	if fieldViolations[0].(map[string]any)["field"] != "message" {
		t.Errorf("field = %v", fieldViolations[0])
	}
}

func TestA2ARPCEmptyPartsIsAFieldViolation(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER", "parts": []any{}},
	}), "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32602 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	violations := data[1].(map[string]any)["fieldViolations"].([]any)
	if violations[0].(map[string]any)["field"] != "message.parts" {
		t.Errorf("field = %v", violations[0])
	}
}

func TestA2ARPCPartWithTwoContentMembersIsRejected(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "hi", "data": map[string]any{"a": 1}}},
		},
	}), "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32602 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	violations := data[1].(map[string]any)["fieldViolations"].([]any)
	if violations[0].(map[string]any)["field"] != "message.parts[0]" {
		t.Errorf("field = %v", violations[0])
	}
}

func TestA2ARPCRejectsInvalidMessageIdentityRoleAndUnsupportedParts(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	for _, message := range []map[string]any{
		{"messageId": "", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "x"}}},
		{"messageId": "m", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "x"}}},
		{"messageId": "m", "role": "ROLE_USER", "parts": []any{map[string]any{"url": "https://example.test/file"}}},
	} {
		_, body := h.call(t, a2aRPC("SendMessage", map[string]any{"message": message}), "")
		rpcErr := body["error"].(map[string]any)
		if int(rpcErr["code"].(float64)) != -32602 {
			t.Errorf("message=%v code=%v, want invalid params", message, rpcErr["code"])
		}
	}
}

func TestA2ARPCPushNotificationConfigIsRefusedBeforeExecution(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
		"configuration": map[string]any{
			"taskPushNotificationConfig": map[string]any{"url": "https://example.com"},
		},
	}), "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32003 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	if data[0].(map[string]any)["reason"] != "PUSH_NOTIFICATION_NOT_SUPPORTED" {
		t.Errorf("reason = %v", data[0])
	}
}

func TestA2ARPCReturnImmediatelyIsExplicitlyUnsupported(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
		"configuration": map[string]any{"returnImmediately": true},
	}), "")
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32004 {
		t.Errorf("code = %v", rpcErr["code"])
	}
}

func TestA2ARPCMultiTurnReusesTheSuppliedContextID(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	first := h.sendText(t, "第一轮")
	firstTask := first["task"].(map[string]any)
	contextID := firstTask["contextId"].(string)

	_, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{
			"messageId": "msg-2", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "第二轮"}}, "contextId": contextID,
		},
	}), "")
	task := body["result"].(map[string]any)["task"].(map[string]any)
	if task["contextId"] != contextID {
		t.Errorf("contextId = %v, want %v", task["contextId"], contextID)
	}
	// 同一 context 下是两个不同的 task。
	if task["id"] == firstTask["id"] {
		t.Error("expected a different task id for the second turn")
	}
}

func TestA2ARPCVersionHeaderNegotiation(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aTestConfig(""), a2aPiOutput)
	send := a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
	})

	// 同主版本放行。
	for _, version := range []string{"1.0", "1"} {
		_, body := h.callWithHeaders(t, send, "", map[string]string{"A2A-Version": version})
		if _, hasError := body["error"]; hasError {
			t.Errorf("version %s: unexpected error %v", version, body)
		}
	}

	// v0.3 客户端必须被明确拒绝,而不是被当成 v1.0 处理。
	_, body := h.callWithHeaders(t, send, "", map[string]string{"A2A-Version": "0.3"})
	rpcErr := body["error"].(map[string]any)
	if int(rpcErr["code"].(float64)) != -32009 {
		t.Errorf("code = %v", rpcErr["code"])
	}
	data := rpcErr["data"].([]any)
	if data[0].(map[string]any)["reason"] != "VERSION_NOT_SUPPORTED" {
		t.Errorf("reason = %v", data[0])
	}
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

func a2aAuthTestConfig() *A2AConfig {
	config := a2aTestConfig("")
	config.RequireBearerAuth = true
	return config
}

func TestA2ARPCRequestWithoutBearerTokenIsUnauthorized(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aAuthTestConfig(), a2aPiOutput)
	status, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
	}), "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	errObj := body["error"].(map[string]any)
	details := errObj["details"].([]any)
	if details[0].(map[string]any)["reason"] != "UNAUTHENTICATED" {
		t.Errorf("reason = %v", details[0])
	}
}

func TestA2ARPCUnauthorizedResponseAdvertisesTheBearerScheme(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aAuthTestConfig(), a2aPiOutput)
	resp := h.callRaw(t, []byte("{}"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="a2a"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestA2ARPCDerivedBearerTokenIsAccepted(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aAuthTestConfig(), a2aPiOutput)
	token := A2ADeriveToken(a2aTestSecret, "desk-1")
	status, body := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
	}), token)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, hasError := body["error"]; hasError {
		t.Fatalf("unexpected error: %v", body)
	}
	task := body["result"].(map[string]any)["task"].(map[string]any)
	status2 := task["status"].(map[string]any)
	if status2["state"] != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %v", status2["state"])
	}
}

func TestA2ARPCSigningSecretItselfIsNotAValidBearerToken(t *testing.T) {
	// token 是派生值;直接用 signing secret 不应通过,否则等于把 HMAC 密钥
	// 暴露在每个请求头里。
	h := newA2AHarness(t, a2aOneAgent(), a2aAuthTestConfig(), a2aPiOutput)
	status, _ := h.call(t, a2aRPC("SendMessage", map[string]any{
		"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
			"parts": []any{map[string]any{"text": "x"}}},
	}), a2aTestSecret)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestA2ARPCTokenFromAnotherAgentIsRejected(t *testing.T) {
	h := newA2AHarness(t, a2aOneAgent(), a2aAuthTestConfig(), a2aPiOutput)
	status, _ := h.call(t, a2aRPC("GetTask", map[string]any{"id": "t"}),
		A2ADeriveToken(a2aTestSecret, "other-desk"))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
