// Package catalog 提供岗位 / 智能体 / 团队的管理用例：校验、revision +
// If-Match 乐观并发、内置资源保护与审计事件生成。
//
// 存储语义（事务内 revision 递增、注册表版本收敛、聘任级联）由
// infra/postgres 仓储保证；本包只做用例编排。审计当前为「变更后追加」，
// 与业务变更的同事务耦合在 P6 admin API 装配时通过 tx 组合器收口
// （见 doc/ARCHITECTURE-V2.md §8）。
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/audit"
	"ir-agent-server/internal/domain/position"
	"ir-agent-server/internal/domain/team"
)

// ErrInvalidInput 表示请求未通过用例级校验（领域校验错误统一包装）。
var ErrInvalidInput = errors.New("invalid input")

// AuditRecorder 记录管理操作审计。
type AuditRecorder interface {
	Append(ctx context.Context, events ...audit.Event) error
}

// Service 是目录管理服务。并发安全（状态全在仓储）。
type Service struct {
	positions position.Repository
	agents    agent.Repository
	teams     team.Repository
	audit     AuditRecorder // 可为 nil（嵌入/测试场景）
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Positions position.Repository
	Agents    agent.Repository
	Teams     team.Repository
	Audit     AuditRecorder
}

// NewService 组装目录服务。
func NewService(deps Deps) *Service {
	return &Service{positions: deps.Positions, agents: deps.Agents,
		teams: deps.Teams, audit: deps.Audit}
}

// record 追加审计事件；失败只记日志路径（返回给调用方决定是否补偿），
// 不阻断已提交的业务变更。
func (s *Service) record(ctx context.Context, events ...audit.Event) error {
	if s.audit == nil || len(events) == 0 {
		return nil
	}
	return s.audit.Append(ctx, events...)
}

func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, position.ErrReserved) || errors.Is(err, position.ErrInUse) ||
		errors.Is(err, position.ErrNotFound) || errors.Is(err, position.ErrRevisionConflict) ||
		errors.Is(err, agent.ErrNotFound) || errors.Is(err, agent.ErrRevisionConflict) ||
		errors.Is(err, team.ErrNotFound) || errors.Is(err, team.ErrRevisionConflict) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
}

// ---------------------------------------------------------------------------
// 岗位
// ---------------------------------------------------------------------------

// ListPositions 返回岗位（内置在前，__CUSTOM__ 垫底——仓储排序保证）。
func (s *Service) ListPositions(ctx context.Context, includeDisabled bool) ([]position.Position, error) {
	return s.positions.List(ctx, includeDisabled)
}

// GetPosition 按 Code 取岗位。
func (s *Service) GetPosition(ctx context.Context, code string) (*position.Position, error) {
	return s.positions.GetByCode(ctx, strings.ToUpper(strings.TrimSpace(code)))
}

// SavePosition 创建或更新岗位（ifMatchRevision 非空时执行 If-Match CAS）。
// 内置岗位的契约引用不可改指（领域校验），__CUSTOM__ 不可经 API 创建。
func (s *Service) SavePosition(ctx context.Context, p *position.Position, ifMatchRevision *int64, actor string) (int64, error) {
	creating := ifMatchRevision == nil
	if err := p.Validate(); err != nil {
		return 0, wrapValidation(err)
	}
	if position.IsBuiltin(p.Code) {
		// 内置岗位允许改名/描述/schema 微调，但必须先存在（不接受"创建内置"）。
		if _, err := s.positions.GetByCode(ctx, p.Code); errors.Is(err, position.ErrNotFound) {
			return 0, fmt.Errorf("%w: builtin position %s is seeded by the system", ErrInvalidInput, p.Code)
		}
	}
	revision, err := s.positions.Upsert(ctx, p, ifMatchRevision)
	if err != nil {
		return 0, wrapValidation(err)
	}
	action := audit.Action("position.updated")
	if creating {
		action = "position.created"
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: action, Target: p.Code})
	return revision, nil
}

// SetPositionEnabled 启停岗位。
func (s *Service) SetPositionEnabled(ctx context.Context, code string, enabled bool, ifMatchRevision *int64, actor string) (int64, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	revision, err := s.positions.SetEnabled(ctx, code, enabled, ifMatchRevision)
	if err != nil {
		return 0, wrapValidation(err)
	}
	action := audit.Action("position.disabled")
	if enabled {
		action = "position.enabled"
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: action, Target: code})
	return revision, nil
}

// DeletePosition 删除岗位。内置岗位与 __CUSTOM__ 拒绝删除；仍被聘任时
// 返回 position.ErrInUse。
func (s *Service) DeletePosition(ctx context.Context, code string, actor string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if position.IsBuiltin(code) {
		return fmt.Errorf("%w: builtin position %s cannot be deleted (disable it instead)",
			position.ErrReserved, code)
	}
	if err := s.positions.Delete(ctx, code); err != nil {
		return wrapValidation(err)
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: "position.deleted", Target: code})
	return nil
}

// ---------------------------------------------------------------------------
// 智能体
// ---------------------------------------------------------------------------

// ListAgents 返回智能体（含聘任岗位）。
func (s *Service) ListAgents(ctx context.Context, includeDisabled bool) ([]agent.Agent, error) {
	return s.agents.List(ctx, includeDisabled)
}

// GetAgent 按 slug 取智能体。
func (s *Service) GetAgent(ctx context.Context, slug string) (*agent.Agent, error) {
	return s.agents.GetBySlug(ctx, strings.ToLower(strings.TrimSpace(slug)))
}

// SaveAgent 创建或更新智能体。创建必须携带签名密钥；更新时
// a.SigningSecret 为空表示保留既有密文。聘任的岗位必须全部存在。
func (s *Service) SaveAgent(ctx context.Context, a *agent.Agent, ifMatchRevision *int64, actor string) (int64, error) {
	creating := ifMatchRevision == nil
	if err := a.Validate(creating); err != nil {
		return 0, wrapValidation(err)
	}
	for _, code := range a.Positions {
		if _, err := s.positions.GetByCode(ctx, code); err != nil {
			if errors.Is(err, position.ErrNotFound) {
				return 0, fmt.Errorf("%w: position %s does not exist", ErrInvalidInput, code)
			}
			return 0, err
		}
	}
	revision, err := s.agents.Upsert(ctx, a, ifMatchRevision)
	if err != nil {
		return 0, wrapValidation(err)
	}
	action := audit.Action("agent.updated")
	if creating {
		action = "agent.created"
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: action, Target: a.Slug})
	return revision, nil
}

// DeleteAgent 删除智能体（聘任关系级联清理，注册表版本递增由仓储保证）。
func (s *Service) DeleteAgent(ctx context.Context, slug string, actor string) error {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if err := s.agents.Delete(ctx, slug); err != nil {
		return wrapValidation(err)
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: "agent.deleted", Target: slug})
	return nil
}

// AgentsForPosition 返回聘任了指定岗位的启用智能体（编排引擎的岗位目标
// 解析使用；按 slug 排序保证确定性）。
func (s *Service) AgentsForPosition(ctx context.Context, code string) ([]agent.Agent, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	all, err := s.agents.List(ctx, false)
	if err != nil {
		return nil, err
	}
	staffed := make([]agent.Agent, 0, len(all))
	for _, item := range all {
		if item.ServesPosition(code) {
			staffed = append(staffed, item)
		}
	}
	return staffed, nil
}

// ---------------------------------------------------------------------------
// 团队
// ---------------------------------------------------------------------------

// ListTeams 返回团队。
func (s *Service) ListTeams(ctx context.Context, includeDisabled bool) ([]team.Team, error) {
	return s.teams.List(ctx, includeDisabled)
}

// GetTeam 按 ID 取团队。
func (s *Service) GetTeam(ctx context.Context, id string) (*team.Team, error) {
	return s.teams.Get(ctx, strings.TrimSpace(id))
}

// SaveTeam 创建或更新团队。定义先过领域校验（步骤目标形态、依赖存在、
// DAG 无环、重试与失败策略枚举）；步骤目标引用的岗位/智能体必须存在。
func (s *Service) SaveTeam(ctx context.Context, t *team.Team, ifMatchRevision *int64, actor string) (int64, error) {
	creating := ifMatchRevision == nil
	if err := t.Validate(); err != nil {
		return 0, wrapValidation(err)
	}
	for _, step := range t.Steps {
		if step.Target.PositionCode != "" {
			if _, err := s.positions.GetByCode(ctx, step.Target.PositionCode); err != nil {
				if errors.Is(err, position.ErrNotFound) {
					return 0, fmt.Errorf("%w: step %s targets unknown position %s",
						ErrInvalidInput, step.ID, step.Target.PositionCode)
				}
				return 0, err
			}
		}
		if step.Target.AgentSlug != "" {
			if _, err := s.agents.GetBySlug(ctx, step.Target.AgentSlug); err != nil {
				if errors.Is(err, agent.ErrNotFound) {
					return 0, fmt.Errorf("%w: step %s targets unknown agent %s",
						ErrInvalidInput, step.ID, step.Target.AgentSlug)
				}
				return 0, err
			}
		}
	}
	revision, err := s.teams.Upsert(ctx, t, ifMatchRevision)
	if err != nil {
		return 0, wrapValidation(err)
	}
	action := audit.Action("team.updated")
	if creating {
		action = audit.ActionTeamCreated
	} else {
		action = audit.ActionTeamUpdated
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: action, Target: t.ID})
	return revision, nil
}

// DeleteTeam 删除团队定义。在途 TeamRun 持有定义快照，不受删除影响。
func (s *Service) DeleteTeam(ctx context.Context, id string, actor string) error {
	id = strings.TrimSpace(id)
	if err := s.teams.Delete(ctx, id); err != nil {
		return wrapValidation(err)
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: audit.ActionTeamDeleted, Target: id})
	return nil
}
