// Package audit 定义审计域：系统操作日志。
//
// 权限不变量在领域层收口：非管理员只能查询本人事件。API 层隐藏入口
// 只是第二道保险，不是权限边界。
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"ir-agent-server/internal/domain/account"
)

// Action 是审计动作标识（点分小写）。
type Action string

// 既有动作常量在 infra/api 迁移时按现网 audit_log 取值补齐；先固定新增域。
const (
	ActionTeamCreated   Action = "team.created"
	ActionTeamUpdated   Action = "team.updated"
	ActionTeamDeleted   Action = "team.deleted"
	ActionTeamRunSubmit Action = "team_run.submitted"
	ActionTeamRunCancel Action = "team_run.canceled"
)

// Event 是一条审计事件。
type Event struct {
	ID        int64
	Actor     string // 账号名；系统动作为 "system"
	Action    Action
	Target    string
	Detail    json.RawMessage
	CreatedAt time.Time
}

// Query 是审计查询条件。
type Query struct {
	Actor  string
	Action Action
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// ErrPermissionDenied 是领域权限哨兵；API 层映射 403。
var ErrPermissionDenied = errors.New("audit log requires admin role")

// ErrNotFound 是仓储未命中哨兵。
var ErrNotFound = errors.New("audit event not found")

// Authorize 按调用者角色收紧查询：非 admin 强制只查本人事件。
// 这是审计可见性的唯一裁决点。
func (q *Query) Authorize(role account.Role, actor string) error {
	if role == account.RoleAdmin {
		return nil
	}
	if role != account.RoleUser {
		return ErrPermissionDenied
	}
	q.Actor = actor
	return nil
}

// Repository 是审计仓储端口。业务写操作与对应审计事件必须在同一事务
// （由 app 层的事务编排保证），审计失败回滚业务变更。
type Repository interface {
	Append(ctx context.Context, events ...Event) error
	List(ctx context.Context, q Query) (items []Event, total int, err error)
	Get(ctx context.Context, id int64) (*Event, error)
}
