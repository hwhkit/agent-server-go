package agentapi

// 机器侧直接提交（POST /agent-api/v1/runs）：定时任务/本地触发的运行
// 不经租约，连接器（或 cron 调 ir-connector submit）直接回传结果。
// 幂等键为 (slug, runId)；重放返回首次状态。

import (
	"context"
	"encoding/json"
	"strings"

	domain "ir-agent-server/internal/domain/connector"
)

// SubmitRunInput 是一次机器侧直接提交。
type SubmitRunInput struct {
	Slug    string
	Status  string // succeeded | failed
	RunID   string // 空 = 服务端生成
	Payload json.RawMessage
	Input   json.RawMessage
	Error   *CompleteError
	Usage   *CompleteUsage
	Metrics *CompleteMetrics
}

// SubmitRunResult 是提交回执；Accepted 恒为 true（含幂等重放），
// RunStatus 是首次落库的终态。
type SubmitRunResult struct {
	RunID     string
	Accepted  bool
	RunStatus string
}

// scopeAllowsPrincipal 报告主体是否被授权操作 slug。
func scopeAllowsPrincipal(principal *Principal, slug string) bool {
	for _, scope := range principal.Scopes {
		if scope == "*" || scope == slug {
			return true
		}
	}
	return false
}

// SubmitRun 幂等登记机器侧直接运行。
func (s *Service) SubmitRun(ctx context.Context, principal *Principal, in SubmitRunInput) (*SubmitRunResult, *Failure) {
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		return nil, failure(400, "invalid_request", "slug is required")
	}
	if !scopeAllowsPrincipal(principal, slug) {
		return nil, failure(403, "forbidden",
			"slug "+slug+" is outside this principal's scope")
	}
	switch in.Status {
	case "succeeded", "failed":
	default:
		return nil, failure(400, "invalid_status", "status must be succeeded or failed")
	}
	if in.Status == "succeeded" && len(in.Payload) == 0 {
		return nil, failure(400, "invalid_request", "payload is required for succeeded")
	}
	if in.Status == "failed" && (in.Error == nil || in.Error.Code == "") {
		return nil, failure(400, "invalid_request", "error.code is required for failed")
	}

	// 规范化结果文档：与 complete 同构，另带 input 供审计还原触发参数。
	resultDoc := map[string]any{
		"status":  in.Status,
		"payload": json.RawMessage(in.Payload),
		"input":   json.RawMessage(in.Input),
		"usage":   in.Usage,
		"metrics": in.Metrics,
	}
	if in.Error != nil {
		resultDoc["error"] = in.Error
	}
	result, err := json.Marshal(resultDoc)
	if err != nil {
		return nil, failure(400, "invalid_request", "result is not serializable")
	}
	runID := strings.TrimSpace(in.RunID)
	if runID == "" {
		runID = "run_" + randomToken(12)
	}
	errorCode := ""
	if in.Error != nil {
		errorCode = in.Error.Code
	}
	runStatus, rerr := s.queue.RecordDirectRun(ctx, domain.DirectRun{
		Slug: slug, RunID: runID, Status: in.Status,
		Input: in.Input, Result: result, ErrorCode: errorCode,
	})
	if rerr != nil {
		if rerr == domain.ErrUnknownAgent {
			return nil, failure(404, "unknown_agent", "agent "+slug+" is not registered")
		}
		return nil, failure(500, "storage_error", "record run failed")
	}
	return &SubmitRunResult{RunID: runID, Accepted: true, RunStatus: runStatus}, nil
}
