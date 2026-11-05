package platform

// JSON-RPC 2.0 信封与 A2A v1.0 错误模型。
// 对应 rs 侧 src/a2a/jsonrpc.rs。
//
// v1.0 的错误 data 是**数组**,每个元素带 @type,使用 ProtoJSON Any 表示
// (§9.5)。A2A 专属错误必须带一个 google.rpc.ErrorInfo,其中 reason 是
// UPPER_SNAKE_CASE 的错误类型名,domain 固定 a2a-protocol.org。这与 v0.3 的
// 自由 data 对象不兼容。

import "encoding/json"

// A2AJSONRPCRequest 是 JSON-RPC 请求信封。
type A2AJSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	// ID 是请求标识。通知(无 id)在 A2A 里没有意义,但仍需能解析出来以便
	// 回报错误。
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// A2AJSONRPCResponse 是 JSON-RPC 响应信封。Result 与 Error 互斥。
type A2AJSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *A2AJSONRPCError `json:"error,omitempty"`
}

// A2AJSONRPCNull 是 JSON null 的原始字节,用于 id 缺省场景。
var A2AJSONRPCNull = json.RawMessage("null")

// NewA2AJSONRPCSuccess 构造成功响应。result 必须能序列化为 JSON。
func NewA2AJSONRPCSuccess(id json.RawMessage, result any) (*A2AJSONRPCResponse, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if id == nil {
		id = A2AJSONRPCNull
	}
	return &A2AJSONRPCResponse{JSONRPC: "2.0", ID: id, Result: encoded}, nil
}

// NewA2AJSONRPCFailure 构造失败响应。
func NewA2AJSONRPCFailure(id json.RawMessage, rpcErr *A2AJSONRPCError) *A2AJSONRPCResponse {
	if id == nil {
		id = A2AJSONRPCNull
	}
	return &A2AJSONRPCResponse{JSONRPC: "2.0", ID: id, Error: rpcErr}
}

// A2AJSONRPCError 是 JSON-RPC 错误对象。
type A2AJSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// A2AErrorKind 是 A2A 错误类型。编码与 docs/specification.md §5.4 的映射表
// 一致。
type A2AErrorKind int

const (
	// 标准 JSON-RPC 错误(§9.5)
	A2AErrJSONParse A2AErrorKind = iota
	A2AErrInvalidRequest
	A2AErrMethodNotFound
	A2AErrInvalidParams
	A2AErrInternal
	// A2A 专属错误(§5.4),范围 -32001 ~ -32099
	A2AErrTaskNotFound
	A2AErrTaskNotCancelable
	A2AErrPushNotificationNotSupported
	A2AErrUnsupportedOperation
	A2AErrContentTypeNotSupported
	A2AErrInvalidAgentResponse
	A2AErrExtendedAgentCardNotConfigured
	A2AErrExtensionSupportRequired
	A2AErrVersionNotSupported
)

// Code 返回该错误类型的 JSON-RPC 错误码。
func (k A2AErrorKind) Code() int {
	switch k {
	case A2AErrJSONParse:
		return -32700
	case A2AErrInvalidRequest:
		return -32600
	case A2AErrMethodNotFound:
		return -32601
	case A2AErrInvalidParams:
		return -32602
	case A2AErrInternal:
		return -32603
	case A2AErrTaskNotFound:
		return -32001
	case A2AErrTaskNotCancelable:
		return -32002
	case A2AErrPushNotificationNotSupported:
		return -32003
	case A2AErrUnsupportedOperation:
		return -32004
	case A2AErrContentTypeNotSupported:
		return -32005
	case A2AErrInvalidAgentResponse:
		return -32006
	case A2AErrExtendedAgentCardNotConfigured:
		return -32007
	case A2AErrExtensionSupportRequired:
		return -32008
	case A2AErrVersionNotSupported:
		return -32009
	default:
		return -32603
	}
}

// Message 返回规范给出的标准 message 文案。
func (k A2AErrorKind) Message() string {
	switch k {
	case A2AErrJSONParse:
		return "Invalid JSON payload"
	case A2AErrInvalidRequest:
		return "Request payload validation error"
	case A2AErrMethodNotFound:
		return "Method not found"
	case A2AErrInvalidParams:
		return "Invalid parameters"
	case A2AErrInternal:
		return "Internal error"
	case A2AErrTaskNotFound:
		return "Task not found"
	case A2AErrTaskNotCancelable:
		return "Task cannot be canceled"
	case A2AErrPushNotificationNotSupported:
		return "Push Notification is not supported"
	case A2AErrUnsupportedOperation:
		return "This operation is not supported"
	case A2AErrContentTypeNotSupported:
		return "Incompatible content types"
	case A2AErrInvalidAgentResponse:
		return "Invalid agent response"
	case A2AErrExtendedAgentCardNotConfigured:
		return "Extended agent card not configured"
	case A2AErrExtensionSupportRequired:
		return "Extension support required"
	case A2AErrVersionNotSupported:
		return "Version not supported"
	default:
		return "Internal error"
	}
}

// Reason 返回 google.rpc.ErrorInfo.reason,UPPER_SNAKE_CASE。
func (k A2AErrorKind) Reason() string {
	switch k {
	case A2AErrJSONParse:
		return "JSON_PARSE_ERROR"
	case A2AErrInvalidRequest:
		return "INVALID_REQUEST"
	case A2AErrMethodNotFound:
		return "METHOD_NOT_FOUND"
	case A2AErrInvalidParams:
		return "INVALID_PARAMS"
	case A2AErrInternal:
		return "INTERNAL_ERROR"
	case A2AErrTaskNotFound:
		return "TASK_NOT_FOUND"
	case A2AErrTaskNotCancelable:
		return "TASK_NOT_CANCELABLE"
	case A2AErrPushNotificationNotSupported:
		return "PUSH_NOTIFICATION_NOT_SUPPORTED"
	case A2AErrUnsupportedOperation:
		return "UNSUPPORTED_OPERATION"
	case A2AErrContentTypeNotSupported:
		return "CONTENT_TYPE_NOT_SUPPORTED"
	case A2AErrInvalidAgentResponse:
		return "INVALID_AGENT_RESPONSE"
	case A2AErrExtendedAgentCardNotConfigured:
		return "EXTENDED_AGENT_CARD_NOT_CONFIGURED"
	case A2AErrExtensionSupportRequired:
		return "EXTENSION_SUPPORT_REQUIRED"
	case A2AErrVersionNotSupported:
		return "VERSION_NOT_SUPPORTED"
	default:
		return "INTERNAL_ERROR"
	}
}

// IntoError 构造不带 metadata 的错误对象。
func (k A2AErrorKind) IntoError() *A2AJSONRPCError {
	return k.WithMetadata(nil)
}

// WithMetadata 构造带 ErrorInfo.metadata 的错误。metadata 只允许字符串值,
// 非字符串会被转成字符串表示,避免泄漏内部结构。
func (k A2AErrorKind) WithMetadata(metadata map[string]any) *A2AJSONRPCError {
	errorInfo := map[string]any{
		"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
		"reason": k.Reason(),
		"domain": A2AErrorDomain,
	}
	if len(metadata) > 0 {
		normalized := make(map[string]string, len(metadata))
		for key, value := range metadata {
			if text, ok := value.(string); ok {
				normalized[key] = text
			} else {
				normalized[key] = JSONString(value)
			}
		}
		errorInfo["metadata"] = normalized
	}
	data, _ := json.Marshal([]any{errorInfo})
	return &A2AJSONRPCError{Code: k.Code(), Message: k.Message(), Data: data}
}

// WithDetail 是覆盖 message 的变体,用于把执行层的具体原因带给调用方。
func (k A2AErrorKind) WithDetail(detail string) *A2AJSONRPCError {
	err := k.IntoError()
	err.Message = detail
	return err
}
