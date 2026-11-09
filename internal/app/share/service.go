// Package share 是团队分享的公开用例层：/share/{token} 子页面的后端。
//
// 访客凭 token（唯一凭证，不可枚举）完成「看团队 → 触发运行 → 轮询
// 交易信号」。安全口径：
//   - 视图脱敏：只暴露团队名称/描述与岗位标签流程骨架；agent slug、
//     系统提示词、执行配置永不出现；
//   - 限流：每 token 内存令牌桶（默认 6 次/分钟）——公开入口必须防
//     滥用触发真实执行；多副本部署时各副本独立桶，整体上限≈N×rate，
//     更严格的集中限流属于网关职责；
//   - 越权防护：轮询运行时校验 run 属于该 token 指向的团队，token
//     之间不可互看运行记录。
package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ir-agent-server/internal/domain/team"
)

// Runner 是公开触发依赖的编排引擎端口（*teamrun.Engine 结构性满足；
// 接口化以便测试）。
type Runner interface {
	Submit(ctx context.Context, submission Submission) (*team.TeamRun, error)
	Get(ctx context.Context, id string) (*team.TeamRun, error)
}

// Submission 与 teamrun.Submission 同构（避免 app 层反向依赖具体类型）。
type Submission struct {
	TeamID         string
	Input          json.RawMessage
	IdempotencyKey string
	Actor          string
}

// ErrRateLimited 是触发限流（API 层映射 429）。
var ErrRateLimited = errors.New("share trigger rate limit exceeded")

// 主动转发的错误语义（API 层按状态码渲染）。
var (
	ErrNotFound  = team.ErrShareNotFound
	ErrNotShared = errors.New("team is not shared")
)

// ChildRunReader 按运行 ID 读取子运行输出（交易信号载体；*postgres.RunStore
// 结构性满足）。只取输出载荷，不暴露内部行。
type ChildRunReader interface {
	Get(ctx context.Context, runID string) (*ChildRun, error)
}

// ChildRun 是子运行的精简读取面。
type ChildRun struct {
	RunID  string
	Status string
	Output json.RawMessage
}

// Config 是服务配置。
type Config struct {
	// TriggersPerMinute 是每 token 触发限流；<=0 视作 6。
	TriggersPerMinute int
}

// Service 是团队分享公开用例。
type Service struct {
	shares team.ShareRepository
	teams  team.Repository
	engine Runner
	runs   ChildRunReader
	rate   int

	limiterMu sync.Mutex
	buckets   map[string]*tokenBucket
}

// NewService 组装公开分享服务。
func NewService(shares team.ShareRepository, teams team.Repository,
	engine Runner, runs ChildRunReader, cfg Config,
) *Service {
	if cfg.TriggersPerMinute <= 0 {
		cfg.TriggersPerMinute = 6
	}
	return &Service{
		shares: shares, teams: teams, engine: engine, runs: runs,
		rate: cfg.TriggersPerMinute, buckets: map[string]*tokenBucket{},
	}
}

// StepView 是公开流程骨架中的一步（只有岗位标签，无执行细节）。
type StepView struct {
	ID        string   `json:"id"`
	Position  string   `json:"position"` // 岗位代码（FUNDAMENTAL_ANALYST 等）
	DependsOn []string `json:"depends_on,omitempty"`
}

// View 是分享页的团队公开视图（脱敏）。
type View struct {
	TeamID      string     `json:"team_id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Steps       []StepView `json:"steps"`
}

// View 返回分享页公开视图：token 校验 + 团队仍启用（停用团队分享即时
// 失效，与撤销同语义）。
func (s *Service) View(ctx context.Context, token string) (*View, error) {
	share, definition, err := s.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	_ = share
	steps := make([]StepView, 0, len(definition.Steps))
	for _, step := range definition.Steps {
		position := step.Target.PositionCode
		if position == "" {
			position = "AGENT"
		}
		steps = append(steps, StepView{ID: step.ID, Position: position, DependsOn: step.DependsOn})
	}
	return &View{
		TeamID: definition.ID, Name: definition.Name,
		Description: definition.Description, Steps: steps,
	}, nil
}

// Trigger 以访客输入触发团队运行。幂等键由服务端派生（share 溯源 +
// 客户端可选去重），重复点击不重复执行。
func (s *Service) Trigger(ctx context.Context, token, clientKey, actor string,
	input json.RawMessage,
) (*team.TeamRun, error) {
	if err := s.allow(token); err != nil {
		return nil, err
	}
	share, definition, err := s.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	idem := fmt.Sprintf("share:%s:%s:%s", share.Token, definition.ID,
		strings.TrimSpace(clientKey))
	if idem == fmt.Sprintf("share:%s:%s:", share.Token, definition.ID) {
		// 无客户端键：随机一次性键（每次点击独立触发，由限流兜底）。
		idem += time.Now().UTC().Format("20060102T150405.000000000")
	}
	return s.engine.Submit(ctx, Submission{
		TeamID: definition.ID, Input: input, IdempotencyKey: idem, Actor: actor,
	})
}

// StepOutput 是公开轮询里一步的结果（交易信号片段）。
type StepOutput struct {
	StepID string          `json:"step_id"`
	State  string          `json:"state"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// RunView 是公开轮询响应：状态 + 各步输出（成功步骤带 Output）。
type RunView struct {
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	Steps     []StepOutput `json:"steps"`
	CreatedAt time.Time    `json:"created_at"`
}

// Run 轮询一次分享触发的历史运行：运行必须属于该 token 的团队。
func (s *Service) Run(ctx context.Context, token, runID string) (*RunView, error) {
	share, _, err := s.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	run, err := s.engine.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.TeamID != share.TeamID {
		// 越权（拿别团队的 runID 来查）：对外语义与未命中等同。
		return nil, ErrNotShared
	}
	view := &RunView{ID: run.ID, Status: string(run.Status), CreatedAt: run.CreatedAt}
	for stepID, stepRun := range run.Steps {
		out := StepOutput{StepID: stepID, State: string(stepRun.State)}
		if stepRun.Error != nil {
			out.Error = stepRun.Error.Code
		}
		if stepRun.State == team.StepSucceeded && strings.HasPrefix(stepRun.OutputRef, "run:") {
			childID := strings.TrimPrefix(stepRun.OutputRef, "run:")
			if child, childErr := s.runs.Get(ctx, childID); childErr == nil && len(child.Output) > 0 {
				out.Output = child.Output
			}
		}
		view.Steps = append(view.Steps, out)
	}
	return view, nil
}

// resolve 校验 token 并取「仍启用」的团队定义。
func (s *Service) resolve(ctx context.Context, token string) (*team.TeamShare, *team.Team, error) {
	share, err := s.shares.Resolve(ctx, strings.TrimSpace(token))
	if err != nil {
		return nil, nil, err
	}
	definition, err := s.teams.Get(ctx, share.TeamID)
	if err != nil {
		return nil, nil, ErrNotShared
	}
	if !definition.Enabled {
		return nil, nil, ErrNotShared
	}
	return share, definition, nil
}

// allow 是触发限流（每 token 令牌桶）。
func (s *Service) allow(token string) error {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	bucket, ok := s.buckets[token]
	if !ok {
		bucket = &tokenBucket{capacity: float64(s.rate), tokens: float64(s.rate)}
		s.buckets[token] = bucket
	}
	if !bucket.take(time.Now()) {
		return ErrRateLimited
	}
	return nil
}

// tokenBucket 是最小令牌桶：capacity 份额度、每秒补 capacity/60。
type tokenBucket struct {
	capacity float64
	tokens   float64
	last     time.Time
}

func (b *tokenBucket) take(now time.Time) bool {
	if b.last.IsZero() {
		b.last = now
	}
	b.tokens += now.Sub(b.last).Seconds() * b.capacity / 60
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
