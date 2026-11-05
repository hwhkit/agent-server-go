// Package team 定义团队：把一组智能体合并到一起，并以声明式 DAG 实现编排。
//
// 团队是静态编排（v2 范围）：步骤指向前驱、目标为岗位或智能体、失败策略
// 逐步声明。不支持嵌套团队，不支持 LLM 动态决定控制流。
package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// FailureMode 是步骤失败对团队与下游的影响策略（声明在失败步骤自身上）。
type FailureMode string

const (
	// FailFast 任一步骤失败即团队失败，不再派发新步骤。
	FailFast FailureMode = "fail_fast"
	// Continue 失败步骤的下游视为前驱已满足，继续执行。
	Continue FailureMode = "continue"
	// SkipBranch 失败步骤的下游分支整体跳过；其余分支照常，
	// 全部落定后团队终态为 completed（跳过不视为失败）。
	SkipBranch FailureMode = "skip_branch"
)

// DefaultFailureMode 是未声明时的失败策略。
const DefaultFailureMode = FailFast

// ParseFailureMode 严格解析失败策略，空值取默认。
func ParseFailureMode(v string) (FailureMode, error) {
	switch FailureMode(strings.TrimSpace(v)) {
	case "":
		return DefaultFailureMode, nil
	case FailFast, Continue, SkipBranch:
		return FailureMode(v), nil
	}
	return "", fmt.Errorf("on_failure must be fail_fast, continue, or skip_branch")
}

// StepTarget 是步骤目标：岗位 Code 或智能体 Slug，二选一。
// 岗位目标在派发时解析为聘任该岗位的启用智能体（复用既有队列机制）。
type StepTarget struct {
	PositionCode string `json:"position_code,omitempty"`
	AgentSlug    string `json:"agent_slug,omitempty"`
}

// Validate 校验目标恰好声明一种。
func (t StepTarget) Validate() error {
	hasPos := strings.TrimSpace(t.PositionCode) != ""
	hasAgent := strings.TrimSpace(t.AgentSlug) != ""
	if hasPos == hasAgent {
		return errors.New("step target must set exactly one of position_code or agent_slug")
	}
	return nil
}

// StepState 是团队运行中单个步骤的状态。
type StepState string

const (
	StepPending    StepState = "pending"
	StepDispatched StepState = "dispatched"
	StepRunning    StepState = "running"
	StepSucceeded  StepState = "succeeded"
	StepFailed     StepState = "failed"
	StepCanceled   StepState = "canceled"
	StepSkipped    StepState = "skipped"
)

// Settled 报告步骤是否已落定（不会再变化）。
func (s StepState) Settled() bool {
	switch s {
	case StepSucceeded, StepFailed, StepCanceled, StepSkipped:
		return true
	}
	return false
}

// RetryPolicy 是步骤级重试策略。
type RetryPolicy struct {
	// MaxAttempts ≥ 1。注意运行层本身是 at-least-once（租约过期重派），
	// 实际执行次数可能高于 MaxAttempts，副作用幂等是执行方责任。
	MaxAttempts int `json:"max_attempts"`
}

// Step 是 DAG 中的一个步骤。
type Step struct {
	ID     string     `json:"id"` // 团队内唯一，^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$
	Target StepTarget `json:"target"`
	// DependsOn 是前驱步骤 ID；空即入口步骤。
	DependsOn []string `json:"depends_on,omitempty"`
	// InputTemplate 引用上游输出（{{steps.<id>.output...}}），派发时渲染。
	InputTemplate json.RawMessage `json:"input_template,omitempty"`
	Retry         RetryPolicy     `json:"retry"`
	OnFailure     FailureMode     `json:"on_failure"`
	// TimeoutSecs 覆盖目标智能体的墙钟上限；空则用智能体自身配置。
	TimeoutSecs *uint64 `json:"timeout_secs,omitempty"`
}

var stepIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Team 是团队实体。
type Team struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`
	// MaxConcurrentRuns 是同一团队并行 TeamRun 上限（默认 4）。
	MaxConcurrentRuns int       `json:"max_concurrent_runs"`
	Enabled           bool      `json:"enabled"`
	Revision          int64     `json:"revision"`
	CreatedAt         time.Time `json:"-"`
	UpdatedAt         time.Time `json:"-"`
}

// DefaultMaxConcurrentRuns 是团队并发缺省值。
const DefaultMaxConcurrentRuns = 4

// ErrNotFound 是仓储未命中哨兵。
var ErrNotFound = errors.New("team not found")

// ErrRevisionConflict 是 If-Match 乐观并发冲突哨兵。
var ErrRevisionConflict = errors.New("revision conflict")

// Validate 规范化并校验团队定义，包括 DAG 无环。
func (t *Team) Validate() error {
	t.ID = strings.TrimSpace(t.ID)
	t.Name = strings.TrimSpace(t.Name)
	t.Description = strings.TrimSpace(t.Description)
	if t.ID == "" {
		return errors.New("team id is required")
	}
	if n := utf8.RuneCountInString(t.Name); n < 1 || n > 60 {
		return errors.New("team name must be 1-60 characters")
	}
	if t.MaxConcurrentRuns == 0 {
		t.MaxConcurrentRuns = DefaultMaxConcurrentRuns
	}
	if t.MaxConcurrentRuns < 1 || t.MaxConcurrentRuns > 64 {
		return errors.New("max_concurrent_runs must be 1-64")
	}
	if len(t.Steps) == 0 {
		return errors.New("team must have at least one step")
	}
	if len(t.Steps) > 64 {
		return errors.New("team must have at most 64 steps")
	}
	index := make(map[string]*Step, len(t.Steps))
	for i := range t.Steps {
		s := &t.Steps[i]
		s.ID = strings.ToLower(strings.TrimSpace(s.ID))
		if !stepIDPattern.MatchString(s.ID) {
			return fmt.Errorf("step id %q must match %s", s.ID, stepIDPattern)
		}
		if _, dup := index[s.ID]; dup {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		index[s.ID] = s
		if err := s.Target.Validate(); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
		s.Target.PositionCode = strings.ToUpper(strings.TrimSpace(s.Target.PositionCode))
		s.Target.AgentSlug = strings.ToLower(strings.TrimSpace(s.Target.AgentSlug))
		if s.Retry.MaxAttempts == 0 {
			s.Retry.MaxAttempts = 1
		}
		if s.Retry.MaxAttempts < 1 || s.Retry.MaxAttempts > 5 {
			return fmt.Errorf("step %s: retry.max_attempts must be 1-5", s.ID)
		}
		mode, err := ParseFailureMode(string(s.OnFailure))
		if err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
		s.OnFailure = mode
		if s.TimeoutSecs != nil && *s.TimeoutSecs == 0 {
			return fmt.Errorf("step %s: timeout_secs must be > 0 when set", s.ID)
		}
		clean := make([]string, 0, len(s.DependsOn))
		for _, dep := range s.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if dep == s.ID {
				return fmt.Errorf("step %s: self dependency", s.ID)
			}
			clean = append(clean, dep)
		}
		s.DependsOn = clean
	}
	for _, s := range t.Steps {
		for _, dep := range s.DependsOn {
			if _, ok := index[dep]; !ok {
				return fmt.Errorf("step %s: unknown dependency %q", s.ID, dep)
			}
		}
	}
	if _, err := t.TopologicalOrder(); err != nil {
		return err
	}
	return nil
}

// TopologicalOrder 返回确定性拓扑序（同层按 ID 字典序）。
// 存在环时返回错误。
func (t *Team) TopologicalOrder() ([]Step, error) {
	indeg := make(map[string]int, len(t.Steps))
	succ := make(map[string][]string, len(t.Steps))
	byID := make(map[string]Step, len(t.Steps))
	for _, s := range t.Steps {
		byID[s.ID] = s
		indeg[s.ID] = len(s.DependsOn)
		for _, dep := range s.DependsOn {
			succ[dep] = append(succ[dep], s.ID)
		}
	}
	var frontier []string
	for id, d := range indeg {
		if d == 0 {
			frontier = append(frontier, id)
		}
	}
	sort.Strings(frontier)
	out := make([]Step, 0, len(t.Steps))
	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]
		out = append(out, byID[id])
		var next []string
		for _, s := range succ[id] {
			indeg[s]--
			if indeg[s] == 0 {
				next = append(next, s)
			}
		}
		sort.Strings(next)
		frontier = append(frontier, next...)
		sort.Strings(frontier)
	}
	if len(out) != len(t.Steps) {
		return nil, errors.New("team steps contain a dependency cycle")
	}
	return out, nil
}

// Verdict 是团队运行的裁决。
type Verdict string

const (
	// VerdictRunning 表示仍有步骤可推进或在途，团队未落定。
	VerdictRunning Verdict = "running"
	// VerdictCompleted 全部步骤 succeeded（或被 continue/skip_branch 豁免）。
	VerdictCompleted Verdict = "completed"
	// VerdictFailed 任一步骤 failed 且其策略为 fail_fast。
	VerdictFailed Verdict = "failed"
	// VerdictCanceled 由外部取消请求落定。
	VerdictCanceled Verdict = "canceled"
)

// Evaluation 是 Evaluate 的结果。
type Evaluation struct {
	// Ready 是可立即派发的步骤 ID（前驱已满足）。
	Ready []string
	// Skip 是应传播跳过的步骤 ID（上游 skip_branch）。
	Skip []string
	// FailFastStep 非空表示该步骤以 fail_fast 失败，团队应落为 failed。
	FailFastStep string
	// Settled 报告是否全部步骤已落定。
	Settled bool
	// Verdict 在 Settled 时给出团队终态（fail_fast 优先于 Settled 判定）。
	Verdict Verdict
}

// Evaluate 是编排求值的核心纯函数：给定各步骤当前状态，计算可派发集合、
// 应跳过集合与团队裁决。编排引擎（app/teamrun）与单元测试都直接消费它。
//
// 语义：
//   - pending 步骤的前驱全部 succeeded，或前驱 failed 但该前驱策略为
//     continue，视为满足；
//   - 前驱 failed 且策略为 skip_branch，或前驱 canceled/skipped：
//     自身进入 Skip 集合（传递性由引擎迭代求值直至不动点）；
//   - 任一步骤 failed 且策略为 fail_fast：立即裁决 VerdictFailed，
//     不再给出 Ready；
//   - 全部步骤 Settled 且未触发 fail_fast：succeeded/skipped 混合裁决为
//     VerdictCompleted（存在 canceled 步骤时由引擎结合取消请求落
//     VerdictCanceled，Evaluate 不感知取消意图）。
func (t *Team) Evaluate(states map[string]StepState) Evaluation {
	byID := make(map[string]Step, len(t.Steps))
	for _, s := range t.Steps {
		byID[s.ID] = s
	}
	var ev Evaluation
	allSettled := true
	for _, s := range t.Steps {
		st, ok := states[s.ID]
		if !ok {
			st = StepPending
		}
		if st == StepFailed && s.OnFailure == FailFast {
			ev.FailFastStep = s.ID
			ev.Verdict = VerdictFailed
			return ev
		}
		if !st.Settled() {
			allSettled = false
		}
	}
	for _, s := range t.Steps {
		st, ok := states[s.ID]
		if !ok {
			st = StepPending
		}
		if st != StepPending {
			continue
		}
		skip := false
		ready := true
		for _, dep := range s.DependsOn {
			ds, ok := states[dep]
			if !ok {
				ds = StepPending
			}
			switch ds {
			case StepSucceeded:
				// 满足。
			case StepFailed:
				if byID[dep].OnFailure == SkipBranch {
					skip = true
				} else if byID[dep].OnFailure != Continue {
					// fail_fast 已在上面提前裁决，这里防御性视为未满足。
					ready = false
				}
			case StepCanceled, StepSkipped:
				skip = true
			default:
				ready = false
			}
			if skip || !ready {
				break
			}
		}
		switch {
		case skip:
			ev.Skip = append(ev.Skip, s.ID)
		case ready:
			ev.Ready = append(ev.Ready, s.ID)
		}
	}
	sort.Strings(ev.Ready)
	sort.Strings(ev.Skip)
	ev.Settled = allSettled && len(ev.Ready) == 0 && len(ev.Skip) == 0
	if ev.Settled {
		ev.Verdict = VerdictCompleted
	} else {
		ev.Verdict = VerdictRunning
	}
	return ev
}

// Repository 是团队仓储端口。
type Repository interface {
	Get(ctx context.Context, id string) (*Team, error)
	List(ctx context.Context, includeDisabled bool) ([]Team, error)
	Upsert(ctx context.Context, t *Team, ifMatchRevision *int64) (int64, error)
	Delete(ctx context.Context, id string) error
}
