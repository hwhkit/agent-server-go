// Package catalog 提供岗位 / 智能体 / 团队的管理用例：校验、revision +
// If-Match 乐观并发、内置资源保护与审计事件生成。
//
// 存储语义（事务内 revision 递增、注册表版本收敛、聘任级联）由
// infra/postgres 仓储保证；本包只做用例编排。
//
// 审计口径（C2 评估收口）：「变更后追加 + 失败留日志」而非同事务耦合——
// 审计是旁路观测面，同事务化需要 tx 端口贯穿全部仓储（抽象成本高），
// 且「业务成功但审计失败」的正确处置是可观测（slog.Error）而非回滚业务。
// 该决策随 v0.2 R8 记录于 doc/v0.2-ITERATIONS.md。
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	shares    team.ShareRepository // 可为 nil（分享未启用）
	audit     AuditRecorder        // 可为 nil（嵌入/测试场景）
}

// Deps 是 Service 的依赖注入。
type Deps struct {
	Positions position.Repository
	Agents    agent.Repository
	Teams     team.Repository
	Shares    team.ShareRepository // 可 nil（未启用分享）
	Audit     AuditRecorder
}

// NewService 组装目录服务。
func NewService(deps Deps) *Service {
	return &Service{positions: deps.Positions, agents: deps.Agents,
		teams: deps.Teams, shares: deps.Shares, audit: deps.Audit}
}

// record 追加审计事件（C2 收口：全服务唯一的审计出口）。审计失败绝不
// 回滚已提交的业务变更（审计是旁路观测），但必须留日志——静默丢失审计
// 行等于合规盲区。
func (s *Service) record(ctx context.Context, events ...audit.Event) error {
	if s.audit == nil || len(events) == 0 {
		return nil
	}
	if err := s.audit.Append(ctx, events...); err != nil {
		slog.Error("catalog audit append failed (business change already committed)",
			"err", err, "events", len(events))
		return err
	}
	return nil
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

// SearchAgents 是千级智能体规模的分页/搜索列表（管理台用）；结果不解密
// 签名密钥，只带 SecretConfigured 标记。
func (s *Service) SearchAgents(ctx context.Context, q agent.ListQuery) ([]agent.Agent, int64, error) {
	return s.agents.Search(ctx, q)
}

// ExportAgents 导出全部智能体（迁移格式：含签名密钥密文，明文永不过手）。
func (s *Service) ExportAgents(ctx context.Context) ([]agent.Export, error) {
	return s.agents.ExportAll(ctx)
}

// ImportAgentEntry 是批量导入的单条输入。
type ImportAgentEntry struct {
	// 内嵌 agent.Agent：迁移格式的顶层键（slug/name/binding/…）直接映射
	// 字段（agent.SigningSecret 带 json:"-"——明文密钥走本条目专属字段，
	// 保证任何 Agent 序列化都永不携带明文）。
	agent.Agent
	SigningSecret string `json:"signing_secret,omitempty"` // 明文录入（新环境）
	SecretEnc     string `json:"secret_enc,omitempty"`     // 密文迁移（同主密钥）
}

// ImportAgentResult 是批量导入的单条结果。
type ImportAgentResult struct {
	Slug     string `json:"slug"`
	Status   string `json:"status"` // created | updated | failed
	Revision int64  `json:"revision,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ImportAgents 批量导入智能体（上限 500/批，与列表上限对齐）。逐条独立
// 落库：单条失败不阻断整批，失败明细随结果返回。整批追加一条
// agent.import 审计事件（批量操作有自己的审计行，含成败汇总）。
func (s *Service) ImportAgents(ctx context.Context, entries []ImportAgentEntry,
	actor string,
) ([]ImportAgentResult, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("import payload must contain at least one agent")
	}
	if len(entries) > 500 {
		return nil, fmt.Errorf("import batch size %d exceeds the 500 limit", len(entries))
	}
	results := make([]ImportAgentResult, 0, len(entries))
	created, updated, failed := 0, 0, 0
	for _, entry := range entries {
		result := ImportAgentResult{Slug: entry.Agent.Slug}
		status, err := s.importOne(ctx, entry)
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			failed++
		} else {
			result.Status = status
			if status == "created" {
				created++
			} else {
				updated++
			}
		}
		results = append(results, result)
	}
	_ = s.record(ctx, audit.Event{
		Actor: actor, Action: "agent.import",
		Target: fmt.Sprintf("%d agents", len(entries)),
		Detail: json.RawMessage(fmt.Sprintf(
			`{"total":%d,"created":%d,"updated":%d,"failed":%d}`,
			len(entries), created, updated, failed)),
	})
	return results, nil
}

// importOne 落库单条导入：密文路径走 ImportUpsert，明文路径走 Upsert。
// created/updated 以导入前的存在性判定（Upsert 的 revision 值域不可靠）。
func (s *Service) importOne(ctx context.Context, entry ImportAgentEntry) (string, error) {
	entity := entry.Agent
	if entry.SigningSecret != "" {
		entity.SigningSecret = []byte(entry.SigningSecret)
	}
	existed := true
	if _, err := s.agents.GetBySlug(ctx, entity.Slug); errors.Is(err, agent.ErrNotFound) {
		existed = false
	} else if err != nil {
		return "", err
	}
	var revision int64
	var err error
	if entry.SecretEnc != "" {
		revision, err = s.agents.ImportUpsert(ctx, &entity, entry.SecretEnc, nil)
	} else {
		revision, err = s.agents.Upsert(ctx, &entity, nil)
	}
	if err != nil {
		return "", err
	}
	_ = revision
	if existed {
		return "updated", nil
	}
	return "created", nil
}

// GetAgent 按 slug 取智能体。
func (s *Service) GetAgent(ctx context.Context, slug string) (*agent.Agent, error) {
	return s.agents.GetBySlug(ctx, strings.ToLower(strings.TrimSpace(slug)))
}

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
	// 删除团队时连带撤销分享（公开链接不得悬空指向已删团队）。
	if s.shares != nil {
		_ = s.shares.Revoke(ctx, id)
	}
	if err := s.teams.Delete(ctx, id); err != nil {
		return wrapValidation(err)
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: audit.ActionTeamDeleted, Target: id})
	return nil
}

// ShareTeam 为团队签发（或轮换）公开分享链接。仅启用团队可分享——
// 分享页是公开入口，停用团队不得继续被触发。
func (s *Service) ShareTeam(ctx context.Context, id, actor string) (*team.TeamShare, error) {
	if s.shares == nil {
		return nil, errors.New("team sharing is not configured")
	}
	id = strings.TrimSpace(id)
	definition, err := s.teams.Get(ctx, id)
	if err != nil {
		return nil, wrapValidation(err)
	}
	if !definition.Enabled {
		return nil, fmt.Errorf("%w: an enabled team is required for sharing", ErrInvalidInput)
	}
	share, err := s.shares.Create(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: "team.share", Target: id})
	return share, nil
}

// RevokeTeamShare 撤销团队的公开分享链接（即时失效）。
func (s *Service) RevokeTeamShare(ctx context.Context, id, actor string) error {
	if s.shares == nil {
		return errors.New("team sharing is not configured")
	}
	if err := s.shares.Revoke(ctx, strings.TrimSpace(id)); err != nil {
		return err
	}
	_ = s.record(ctx, audit.Event{Actor: actor, Action: "team.share.revoked", Target: id})
	return nil
}
