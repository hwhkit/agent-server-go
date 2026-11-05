// Package backend 定义执行后端（原「执行职能体」providers）的管理端口与
// 视图类型。后端配置是系统级单例（二进制路径、API key、MCP），智能体只
// 引用 kind/mode；配置保存后对后续任务热更新，运行中的任务继续使用启动
// 时快照。
//
// 具体目录、字段校验、加密存储与热更新实现在 internal/infra/backend。
package backend

import (
	"context"
	"errors"
)

// ErrStaleRevision 是 Provider 乐观并发冲突（If-Match 失败）。
var ErrStaleRevision = errors.New("stale provider revision")

// ErrUnknownProvider 表示未知的 Provider ID。
var ErrUnknownProvider = errors.New("unknown provider")

// ErrStorage 标记后端存储读写失败（API 层映射 500；区别于配置校验错误
// 的 422 与并发冲突的 412）。包装时保留底层错误文本供日志排查。
var ErrStorage = errors.New("provider storage failure")

// Assignment 描述一个后端能承载的「执行器 × 模式」组合。
type Assignment struct {
	Label         string `json:"label"`
	ExecutorKind  string `json:"executor_kind"`
	ExecutionMode string `json:"execution_mode"`
}

// View 是执行后端的管理台展示态（永不携带密钥明文）。
type View struct {
	ID               string         `json:"id"`
	Revision         int64          `json:"revision"`
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Category         string         `json:"category"` // local | remote
	Configured       bool           `json:"configured"`
	Editable         bool           `json:"editable"`
	StatusDetail     string         `json:"status_detail"`
	SecretConfigured bool           `json:"secret_configured"`
	Config           map[string]any `json:"config"`
	Assignments      []Assignment   `json:"assignments"`
}

// Manager 是执行后端管理端口。
type Manager interface {
	// List 返回当前生效配置下的全部后端视图。
	List(ctx context.Context) ([]View, error)
	// Update 校验并保存一个后端的配置（密钥字段空值=保留既有密文），
	// ifMatchRevision 非空时执行 CAS；成功后热更新到执行层。
	Update(ctx context.Context, id string, input map[string]any,
		ifMatchRevision *int64, actor string) (int64, *View, error)
}
