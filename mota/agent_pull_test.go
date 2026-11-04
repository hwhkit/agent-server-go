package mota

// Agent Pull Connector 的 PostgreSQL 集成测试。
// 与 platform_store_test.go 相同的门禁:未设置 TEST_DATABASE_URL 时整体跳过。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newPullTestEnv 连接隔离测试库,建表并清空 agent_pull_* 与相关 runs。
func newPullTestEnv(t *testing.T) (*AgentPullStore, *AgentAPIConfig) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	pool, err := pgxPoolForTest(t, databaseURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	store, err := NewAgentPullStore(pool)
	if err != nil {
		t.Fatalf("init pull store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, table := range []string{"agent_pull_tasks", "agent_pull_workers", "agent_pull_connectors"} {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM agent_runs WHERE run_id LIKE 'pulltest-%'`); err != nil {
		t.Fatalf("clean agent_runs: %v", err)
	}
	cfg := &AgentAPIConfig{
		LeaseSeconds:           3,
		MaxAttempts:            2,
		ClaimMaxWaitSeconds:    1,
		QueueTTLSeconds:        60,
		SyncHoldMaxSeconds:     2,
		MaxOutstandingLeases:   64,
		WorkerOfflineAfterMiss: 3,
		TokenRotationGraceSecs: 86400,
	}
	return store, cfg
}

func pullTestAgent(slug string, maxConcurrency, queueCapacity int, timeoutSecs uint64) *AgentConfig {
	return &AgentConfig{
		Slug:           slug,
		Name:           slug,
		Enabled:        true,
		ExecutorKind:   ExecutorKindRust,
		ExecutionMode:  ExecutionModeRemotePull,
		TimeoutSecs:    timeoutSecs,
		MaxConcurrency: maxConcurrency,
		QueueCapacity:  queueCapacity,
		Revision:       1,
	}
}

// forceExpireLease 把租约/队列到期时刻改为过去,驱动 reaper 路径。
func forceExpireLease(t *testing.T, store *AgentPullStore, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := store.pool.Exec(ctx,
		`UPDATE agent_pull_tasks SET lease_expires_ns = $1 WHERE run_id = $2`,
		time.Now().Add(-time.Second).UnixNano(), runID); err != nil {
		t.Fatalf("force expire lease: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 租约生命周期
// ---------------------------------------------------------------------------

func TestPullLeaseLifecycle(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	ctx := context.Background()
	agent := pullTestAgent("lifecycle-agent", 2, 5, 600)

	if err := store.EnqueueTask(ctx, "pulltest-1", agent, `{"taskBrief":"hi"}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	task, outcome, err := store.ClaimTask(ctx, "w1", []string{"lifecycle-agent"}, 1, cfg)
	if err != nil || outcome != ClaimDelivered {
		t.Fatalf("claim: outcome=%v err=%v", outcome, err)
	}
	if task.Attempts != 1 || task.State != PullTaskLeased {
		t.Fatalf("claim state: attempts=%d state=%s", task.Attempts, task.State)
	}

	if out, err := store.RenewLease(ctx, task.RunID, "w1", cfg); err != nil || out != RenewOK {
		t.Fatalf("renew: out=%v err=%v", out, err)
	}

	result := `{"status":"succeeded","payload":{"summary":"ok"},"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`
	outcomeC, err := store.CompleteTask(ctx, task.RunID, "w1", "succeeded", result, "")
	if err != nil || !outcomeC.Accepted || outcomeC.RunStatus != "completed" {
		t.Fatalf("complete: %+v err=%v", outcomeC, err)
	}

	// 幂等重放:同结果 → accepted。
	replay, err := store.CompleteTask(ctx, task.RunID, "w1", "succeeded", result, "")
	if err != nil || !replay.Accepted {
		t.Fatalf("complete replay: %+v err=%v", replay, err)
	}
	// 冲突:不同结果 → 409 语义(Conflict)。
	conflict, err := store.CompleteTask(ctx, task.RunID, "w1", "succeeded", `{"status":"succeeded","payload":{"summary":"other"}}`, "")
	if err != nil || conflict.Accepted || !conflict.Conflict {
		t.Fatalf("complete conflict: %+v err=%v", conflict, err)
	}
	// 终态后 renew → Unknown(404)。
	if out, _ := store.RenewLease(ctx, task.RunID, "w1", cfg); out != RenewUnknown {
		t.Fatalf("renew after terminal: out=%v", out)
	}
}

func TestPullQueueCapacity(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	ctx := context.Background()
	agent := pullTestAgent("cap-agent", 1, 0, 600) // 执行1 + 队列0

	if err := store.EnqueueTask(ctx, "pulltest-cap-1", agent, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := store.EnqueueTask(ctx, "pulltest-cap-2", agent, `{}`, cfg.QueueTTL()); err != PullOverloadedError {
		t.Fatalf("second enqueue want PullOverloadedError, got %v", err)
	}
	// 完成后腾出容量。
	task, outcome, _ := store.ClaimTask(ctx, "w1", []string{"cap-agent"}, 1, cfg)
	if outcome != ClaimDelivered {
		t.Fatalf("claim: %v", outcome)
	}
	if _, err := store.CompleteTask(ctx, task.RunID, "w1", "succeeded", `{"status":"succeeded","payload":{}}`, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := store.EnqueueTask(ctx, "pulltest-cap-3", agent, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue after complete: %v", err)
	}
}

func TestPullLeaseExpiryRequeueAndTerminal(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	ctx := context.Background()
	agent := pullTestAgent("expire-agent", 2, 5, 600)

	if err := store.EnqueueTask(ctx, "pulltest-exp", agent, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, outcome, _ := store.ClaimTask(ctx, "w1", []string{"expire-agent"}, 1, cfg)
	if outcome != ClaimDelivered {
		t.Fatalf("claim: %v", outcome)
	}
	// 第一次过期:attempts=1 < 2 → 回队列。
	forceExpireLease(t, store, task.RunID)
	stats, err := store.SweepExpired(ctx, cfg)
	if err != nil || stats.Requeued != 1 {
		t.Fatalf("sweep 1: %+v err=%v", stats, err)
	}
	again, outcome, _ := store.ClaimTask(ctx, "w2", []string{"expire-agent"}, 1, cfg)
	if outcome != ClaimDelivered || again.Attempts != 2 || again.RunID != task.RunID {
		t.Fatalf("re-claim: outcome=%v attempts=%d", outcome, again.Attempts)
	}
	// 第二次过期:attempts=2 >= 2 → 终态 failed(lease_expired)。
	forceExpireLease(t, store, task.RunID)
	stats, err = store.SweepExpired(ctx, cfg)
	if err != nil || stats.TerminalLease != 1 {
		t.Fatalf("sweep 2: %+v err=%v", stats, err)
	}
	final, err := store.GetTask(ctx, task.RunID)
	if err != nil || final.State != PullTaskFailed ||
		!strings.Contains(final.ResultJSON, "lease_expired") {
		t.Fatalf("terminal state: %+v err=%v", final, err)
	}
	// 原 worker 迟到的 complete → superseded(仅归档)。
	late, err := store.CompleteTask(ctx, task.RunID, "w1", "succeeded",
		`{"status":"succeeded","payload":{"summary":"late"}}`, "")
	if err != nil || late.Accepted || late.RunStatus != "superseded" {
		t.Fatalf("late complete: %+v err=%v", late, err)
	}
}

func TestPullDeadlineRejectsRenew(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	ctx := context.Background()
	agent := pullTestAgent("deadline-agent", 1, 5, 1) // 墙钟 1s

	if err := store.EnqueueTask(ctx, "pulltest-dl", agent, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _, _ := store.ClaimTask(ctx, "w1", []string{"deadline-agent"}, 1, cfg)
	// 把墙钟改为过去:renew 必须 TASK_DEADLINE_EXCEEDED。
	if _, err := store.pool.Exec(ctx,
		`UPDATE agent_pull_tasks SET execution_dead_ns = $1 WHERE run_id = $2`,
		time.Now().Add(-time.Second).UnixNano(), task.RunID); err != nil {
		t.Fatalf("force deadline: %v", err)
	}
	if out, _ := store.RenewLease(ctx, task.RunID, "w1", cfg); out != RenewDeadline {
		t.Fatalf("renew want RenewDeadline, got %v", out)
	}
}

func TestPullAbortFlag(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	ctx := context.Background()
	agent := pullTestAgent("abort-agent", 1, 5, 600)

	if err := store.EnqueueTask(ctx, "pulltest-ab", agent, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _, _ := store.ClaimTask(ctx, "w1", []string{"abort-agent"}, 1, cfg)
	if err := store.RequestAbort(ctx, task.RunID); err != nil {
		t.Fatalf("request abort: %v", err)
	}
	if out, _ := store.RenewLease(ctx, task.RunID, "w1", cfg); out != RenewAbort {
		t.Fatalf("renew want RenewAbort, got %v", out)
	}
	// aborted complete → canceled 终态。
	outcome, err := store.CompleteTask(ctx, task.RunID, "w1", "aborted",
		`{"status":"aborted"}`, "")
	if err != nil || !outcome.Accepted || outcome.RunStatus != "canceled" {
		t.Fatalf("abort complete: %+v err=%v", outcome, err)
	}
}

// ---------------------------------------------------------------------------
// HTTP 端点(令牌鉴权 + 四端点串联)
// ---------------------------------------------------------------------------

func TestPullHTTPEndpoints(t *testing.T) {
	store, cfg := newPullTestEnv(t)
	service := NewAgentPullService(cfg, store, nil)
	token, err := service.IssueConnector(context.Background(), "conn-a", []string{"http-agent"})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	agent := pullTestAgent("http-agent", 2, 5, 600)
	if err := store.EnqueueTask(context.Background(), "pulltest-http", agent,
		`{"taskBrief":"do"}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	server := httptest.NewServer(AgentPullRouter(service))
	t.Cleanup(server.Close)
	do := func(method, path, bearer, body string, headers map[string]string) *http.Response {
		t.Helper()
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		req, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 未带令牌 → 401。
	if resp := do(http.MethodPost, "/claim", "", `{}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("claim without token: %d", resp.StatusCode)
	}

	// claim → 200 + runId。
	resp := do(http.MethodPost, "/claim", token,
		`{"workerId":"http-w1","capacity":1}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d", resp.StatusCode)
	}
	var claim map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	runID, _ := claim["runId"].(string)
	if runID == "" || claim["attempts"] != float64(1) {
		t.Fatalf("claim body: %+v", claim)
	}

	// renew → 200 且无 abort。
	headers := map[string]string{"X-Agent-Run-ID": runID, "X-Agent-Worker-ID": "http-w1"}
	resp = do(http.MethodPost, "/renew", token, ``, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: %d", resp.StatusCode)
	}
	var renew map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&renew)
	if renew["abort"] != false {
		t.Fatalf("renew body: %+v", renew)
	}

	// 错误 worker 的 renew → 409(租约不属于它,判定为已过期重派)。
	resp = do(http.MethodPost, "/renew", token, ``,
		map[string]string{"X-Agent-Run-ID": runID, "X-Agent-Worker-ID": "other"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("renew by wrong worker: %d", resp.StatusCode)
	}

	// complete → accepted。
	resp = do(http.MethodPost, "/complete", token,
		`{"status":"succeeded","payload":{"summary":"done"},"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`,
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete: %d", resp.StatusCode)
	}
	var receipt map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&receipt)
	if receipt["accepted"] != true {
		t.Fatalf("complete receipt: %+v", receipt)
	}

	// heartbeat → 200 + queueDepth。
	resp = do(http.MethodPost, "/heartbeat", token,
		`{"workerId":"http-w1","inFlight":0,"capacity":1}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d", resp.StatusCode)
	}

	// scope 之外的 slug → no-scope-match 204。
	other := pullTestAgent("other-agent", 1, 5, 600)
	if err := store.EnqueueTask(context.Background(), "pulltest-other", other, `{}`, cfg.QueueTTL()); err != nil {
		t.Fatalf("enqueue other: %v", err)
	}
	resp = do(http.MethodPost, "/claim", token,
		`{"workerId":"http-w1","capacity":1,"preferredSlugs":["other-agent"]}`, nil)
	if resp.StatusCode != http.StatusNoContent ||
		resp.Header.Get("X-Agent-Api-Reason") != "no-scope-match" {
		t.Fatalf("scoped claim: %d reason=%q", resp.StatusCode, resp.Header.Get("X-Agent-Api-Reason"))
	}
}

// pgxPoolForTest 直连测试库;失败即 Fatal。
func pgxPoolForTest(t *testing.T, databaseURL string) (*pgxpool.Pool, error) {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	poolConfig.MaxConns = 4
	return pgxpool.NewWithConfig(context.Background(), poolConfig)
}
