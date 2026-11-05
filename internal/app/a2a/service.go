// Package a2a 实现 A2A v1.0 协议的用例层：Agent Card 生成（skills 由岗位
// 描述符驱动）、Bearer 派生令牌认证、message → 任务映射、SendMessage /
// GetTask 的 JSON-RPC 方法分派。
//
// HTTP/JSON-RPC 传输拆解（well-known 路由、请求体解析、RPC 信封渲染）属于
// api 层（P6）；协议数据类型在 domain/platform。当前声明的能力边界与旧栈
// 一致：不声明流式，不声明推送通知订阅。
package a2a

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/platform"
)

// TaskStore 是 A2A 任务存储端口（infra/postgres 的 A2ATaskStore 结构性满足；
// 复用 agent_runs 表，payload 语义：执行中存请求信封，终态存 Task 快照）。
type TaskStore interface {
	// Start 登记新任务（初始 running）；created=false 表示 task id 已存在。
	Start(ctx context.Context, agentSlug, taskID string, agentRevision int64,
		executorKind, executionMode string, requestJSON []byte) (created bool, err error)
	// Finish 写入终态与 Task 快照；只允许推进 running 记录。
	Finish(ctx context.Context, agentSlug, taskID, status string, durationMs int64,
		errorCode *string, resultJSON []byte, executorOutput *string) error
	// Get 读取任务记录；未找到返回 found=false。
	Get(ctx context.Context, agentSlug, taskID string) (status string, payload []byte, found bool, err error)
}

// Dispatcher 是任务分发端口（app/dispatch 的 *Service 结构上满足）。
type Dispatcher interface {
	Execute(ctx context.Context, agentCode string, task *platform.TaskContext,
		rawInput any, timeout time.Duration) (*platform.ProcessedOutput, error)
}

// Service 是 A2A 用例服务。并发安全。
type Service struct {
	Tasks      TaskStore
	Dispatcher Dispatcher
}

// StoredRequest 是写进 request_json 的 A2A 侧请求信封。
type StoredRequest struct {
	// Channel 固定 "a2a"，便于在运行记录里区分入口来源。
	Channel   string `json:"channel"`
	ContextID string `json:"contextId"`
	MessageID string `json:"messageId"`
	// Structured 表示调用方是否使用了 DataPart 直通。
	Structured bool `json:"structured"`
	Input      any  `json:"input"`
}

// RestoreTask 把存储记录复原成 A2A Task。
//
// 终态记录里存的是完整 Task 快照；执行中的记录里存的是请求信封，只能取出
// contextId 并合成 WORKING 状态。
func RestoreTask(taskID, status string, payload []byte) *A2ATask {
	state := platform.A2ATaskStateFromRunStatus(status)
	if state.IsTerminal() && len(payload) > 0 {
		var task A2ATask
		if err := json.Unmarshal(payload, &task); err == nil && task.ID != "" {
			// 快照里已经有完整状态与产出物，直接返回。
			return &task
		}
	}
	// 执行中，或快照缺失/损坏（如升级前写入的旧记录）。
	contextID := ""
	if len(payload) > 0 {
		var request StoredRequest
		if err := json.Unmarshal(payload, &request); err == nil {
			contextID = request.ContextID
		}
	}
	effectiveState := state
	if effectiveState == platform.A2ATaskStateUnspecified {
		effectiveState = platform.A2ATaskStateWorking
	}
	return &A2ATask{
		ID:        taskID,
		ContextID: contextID,
		Status:    platform.NewA2ATaskStatus(effectiveState),
	}
}

// CardConfig 是 Agent Card 生成所需的配置切面（装配层从 infra/config 的
// A2AConfig 映射注入；app 层不依赖配置包）。
type CardConfig struct {
	// BasePath 是 JSON-RPC 端点的基础路径（AgentPath = BasePath + "/" + slug）。
	BasePath             string
	RequireBearerAuth    bool
	ProviderOrganization string
	ProviderURL          string
	DocumentationURL     string
	AgentVersion         string
}

// AgentPath 返回 {base_path}/{slug} 的 JSON-RPC 端点路径。
func (c CardConfig) AgentPath(slug string) string { return c.BasePath + "/" + slug }

// randomToken 返回 bytes 字节密码学随机数据的 URL-safe base64(无填充)编码。
// task id / context id 沿用平台既有的随机令牌生成，不额外引入 uuid 依赖。
func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ResolvedAgent 是卡片/认证/映射消费的智能体运行配置快照别名（可读性）。
type ResolvedAgent = agent.ResolvedConfig
