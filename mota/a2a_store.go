package mota

// A2A 任务持久化。对应 rs 侧 src/a2a/store.rs。
//
// 复用既有 agent_runs 表,不新建任务表:
//
//   - Task.ID = agent_runs.run_id,主键 (agent_slug, run_id) 天然保证同一
//     Agent 下 task id 唯一。
//   - 完成态的 A2A Task 序列化后写进 result_json,GetTask 直接复原。
//   - contextId 在任务还在执行时也要能读到,因此随请求信封写进 request_json。
//   - 好处:A2A 运行自动出现在管理台运行记录里,并沿用 agent_revision 快照。
//
// 提供内存后端(与 PushIdempotencyStore 的内存后端同样的用意),让协议层
// 测试不依赖 PostgreSQL。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// A2AStoredRequest 是写进 request_json 的 A2A 侧请求信封。
type A2AStoredRequest struct {
	// Channel 固定 "a2a",便于在运行记录里区分入口来源。
	Channel   string `json:"channel"`
	ContextID string `json:"contextId"`
	MessageID string `json:"messageId"`
	// Structured 表示调用方是否使用了 DataPart 直通。
	Structured bool `json:"structured"`
	Input      any  `json:"input"`
}

// a2aMemoryRecord 是单条内存任务记录。payload 语义与 PostgreSQL 侧一致:
// 执行中存请求信封,终态存 Task 快照。
type a2aMemoryRecord struct {
	status  string
	payload *string
}

// A2AStartOutcome 是任务创建结果。
type A2AStartOutcome int

const (
	A2AStartCreated A2AStartOutcome = iota
	// A2AStartConflict 表示 task id 已存在。A2A 的 task id 由服务端生成,
	// 出现冲突说明重放或碰撞。
	A2AStartConflict
)

// A2ATaskStore 是 A2A 任务存储。恰好一个后端字段非 nil(与
// TaskExecutionBackend 的双态设计一致)。
type A2ATaskStore struct {
	postgres *AgentStore

	memoryMu      sync.Mutex
	memoryRecords map[[2]string]*a2aMemoryRecord
}

// NewA2APostgresTaskStore 构造 PostgreSQL 后端。
func NewA2APostgresTaskStore(store *AgentStore) *A2ATaskStore {
	return &A2ATaskStore{postgres: store}
}

// NewA2AInMemoryTaskStore 构造内存后端(测试/嵌入用)。
func NewA2AInMemoryTaskStore() *A2ATaskStore {
	return &A2ATaskStore{memoryRecords: make(map[[2]string]*a2aMemoryRecord)}
}

// IsInMemory 报告当前是否为内存后端。
func (s *A2ATaskStore) IsInMemory() bool { return s.postgres == nil }

// Start 登记一个新任务,初始状态为 running(对外映射成 TASK_STATE_WORKING)。
func (s *A2ATaskStore) Start(
	ctx context.Context, agent *AgentConfig, taskID string, request *A2AStoredRequest,
) (A2AStartOutcome, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}
	requestText := string(requestJSON)

	if s.IsInMemory() {
		s.memoryMu.Lock()
		defer s.memoryMu.Unlock()
		key := [2]string{agent.Slug, taskID}
		if _, exists := s.memoryRecords[key]; exists {
			return A2AStartConflict, nil
		}
		s.memoryRecords[key] = &a2aMemoryRecord{status: "running", payload: &requestText}
		return A2AStartCreated, nil
	}

	created, err := s.postgres.StartRun(ctx, agent.Slug, taskID, agent.Revision,
		agent.ExecutorKind.String(), agent.ExecutionMode.String(), &requestText)
	if err != nil {
		return 0, err
	}
	if created {
		return A2AStartCreated, nil
	}
	return A2AStartConflict, nil
}

// Finish 写入终态与完整 Task 快照。
func (s *A2ATaskStore) Finish(
	ctx context.Context, agentSlug string, task *A2ATask, durationMs int64,
	errorCode *string, executorOutput *string,
) error {
	status := task.Status.State.ToRunStatus()
	resultJSON, err := json.Marshal(task)
	if err != nil {
		return err
	}
	resultText := string(resultJSON)

	if s.IsInMemory() {
		s.memoryMu.Lock()
		defer s.memoryMu.Unlock()
		key := [2]string{agentSlug, task.ID}
		record, exists := s.memoryRecords[key]
		if !exists || record.status != "running" {
			return errors.New("run is no longer in the running state")
		}
		record.status = status
		record.payload = &resultText
		return nil
	}

	var responseCode int64 = 200
	return s.postgres.FinalizeRun(ctx, RunFinalization{
		AgentSlug:  agentSlug,
		RunID:      task.ID,
		Status:     status,
		DurationMs: durationMs,
		ErrorCode:  errorCode,
		// A2A 的错误在 JSON-RPC 信封里,HTTP 层始终 200。
		ResponseCode:       &responseCode,
		ResultJSON:         &resultText,
		ExecutorResultJSON: executorOutput,
		Idempotency:        nil,
	})
}

// Get 读取任务。执行中的任务没有快照可复原,合成一个 WORKING 状态返回。
// 不存在时返回 (nil, nil)。
func (s *A2ATaskStore) Get(ctx context.Context, agentSlug, taskID string) (*A2ATask, error) {
	var status string
	var payload *string

	if s.IsInMemory() {
		s.memoryMu.Lock()
		record, exists := s.memoryRecords[[2]string{agentSlug, taskID}]
		s.memoryMu.Unlock()
		if !exists {
			return nil, nil
		}
		status, payload = record.status, record.payload
	} else {
		run, err := s.postgres.GetRun(ctx, agentSlug, taskID)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, nil
		}
		status = run.Status
		payload = run.Output
		if payload == nil {
			payload = run.Input
		}
	}

	return a2aRestoreTask(taskID, status, payload), nil
}

// a2aRestoreTask 把存储记录复原成 A2A Task。
//
// 终态记录里存的是完整 Task 快照;执行中的记录里存的是请求信封,只能取出
// contextId 并合成 WORKING 状态。
func a2aRestoreTask(taskID, status string, payload *string) *A2ATask {
	state := A2ATaskStateFromRunStatus(status)
	if state.IsTerminal() && payload != nil {
		var task A2ATask
		if err := json.Unmarshal([]byte(*payload), &task); err == nil && task.ID != "" {
			// 快照里已经有完整状态与产出物,直接返回。
			return &task
		}
	}
	// 执行中,或快照缺失/损坏(如升级前写入的旧记录)。
	contextID := ""
	if payload != nil {
		var request A2AStoredRequest
		if err := json.Unmarshal([]byte(*payload), &request); err == nil {
			contextID = request.ContextID
		}
	}
	effectiveState := state
	if effectiveState == A2ATaskStateUnspecified {
		effectiveState = A2ATaskStateWorking
	}
	return &A2ATask{
		ID:        taskID,
		ContextID: contextID,
		Status:    NewA2ATaskStatus(effectiveState),
	}
}
