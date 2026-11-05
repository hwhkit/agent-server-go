package push

import "fmt"

// RequestError 是 Push 入口的结构化拒绝。字段形态与旧栈 apierror.ApiError
// 对齐（status/title/detail/fields + 可选 urn type），api 层负责渲染为
// problem+json；app 层不依赖任何 HTTP 类型。
type RequestError struct {
	Status int
	// Type 是 RFC7807 problem type（urn:ir:*）；为空时由 api 层按 Status 兜底。
	Type   string
	Title  string
	Detail string
	Fields []FieldError
}

// FieldError 是一条字段级校验错误。
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *RequestError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%d %s: %s", e.Status, e.Title, e.Detail)
	}
	return fmt.Sprintf("%d %s (%d field errors)", e.Status, e.Title, len(e.Fields))
}

func validationError(fields []FieldError) *RequestError {
	return &RequestError{Status: 422, Title: "Validation Failed", Fields: fields}
}

func pushValidation(field, message string) *RequestError {
	return validationError([]FieldError{{Field: field, Message: message}})
}

func unauthorized(detail string) *RequestError {
	return &RequestError{Status: 401, Title: "Unauthorized", Detail: detail}
}

func badRequest(detail string) *RequestError {
	return &RequestError{Status: 400, Title: "Bad Request", Detail: detail}
}

func conflict(detail string) *RequestError {
	return &RequestError{Status: 409, Title: "Conflict", Detail: detail}
}

func internalError(detail string) *RequestError {
	return &RequestError{Status: 500, Title: "Internal Server Error", Detail: detail}
}

func unsupportedMediaType() *RequestError {
	return &RequestError{
		Status: 415, Type: "urn:ir:unsupported-content-type",
		Title: "Unsupported Media Type", Detail: "Content-Type must be application/json",
	}
}

func invalidJSON(detail string) *RequestError {
	return &RequestError{
		Status: 400, Type: "urn:ir:invalid-json",
		Title: "Invalid JSON", Detail: detail,
	}
}

func taskTimeout(budgetSeconds int64) *RequestError {
	return &RequestError{
		Status: 504, Type: "urn:ir:task-timeout", Title: "Task Timeout",
		Detail: fmt.Sprintf("task exceeded the synchronous %d second safety budget", budgetSeconds),
	}
}

// syncFailure 是终态同步失败的回放/构造入口（与旧栈 syncPushFailure 一致）。
func syncFailure(reason string) *RequestError {
	if reason == "usage_unavailable" {
		return &RequestError{
			Status: 502, Type: "urn:ir:usage-unavailable", Title: "Usage Unavailable",
			Detail: "executor did not return final provider token usage",
		}
	}
	return internalError("previous task result could not be serialized")
}
