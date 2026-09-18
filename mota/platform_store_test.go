package mota

// PostgreSQL 真库集成测试。运行时必须显式提供隔离数据库 TEST_DATABASE_URL,
// 未提供则整体跳过 —— 等价于 Rust 侧 `postgres-integration` feature 门禁。
//
// 本地起库:
//
//	docker run --rm -d --name mota-agent-pg-test -p 55440:5432 \
//	  -e POSTGRES_USER=mota -e POSTGRES_PASSWORD=mota -e POSTGRES_DB=mota_agent postgres:16-alpine
//	export TEST_DATABASE_URL='postgres://mota:mota@127.0.0.1:55440/mota_agent'

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// requireTestDatabase 在未配置 TEST_DATABASE_URL 时跳过测试,并把 DATABASE_URL
// 指向同一个库(供 NewPlatformState 读取)。
func requireTestDatabase(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	t.Setenv("DATABASE_URL", databaseURL)
	return databaseURL
}

// newTestStore 连接测试库并返回一个 store。
func newTestStore(t *testing.T) *AgentStore {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	cipher, err := NewSecretCipher(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	store, err := ConnectAgentStore(context.Background(), databaseURL, cipher)
	if err != nil {
		t.Fatalf("connect store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// uniqueSeq 保证并发调用 unique 时也不会撞号。
//
// 只靠 time.Now().UnixNano() 是不够的:macOS 上的墙钟分辨率常常粗于 1ns,
// 几十个 goroutine 同时取值会拿到同一个数。对幂等键来说撞号等于「同一个键
// 配了不同的 runId」,服务端会正确地回 409,于是测试莫名其妙地红。
var uniqueSeq atomic.Uint64

// unique 生成带进程号、纳秒与单调序号的唯一标识,让并发/重复运行互不干扰。
func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d-%d", prefix, os.Getpid(), time.Now().UnixNano(), uniqueSeq.Add(1))
}

// uniqueSlug 生成合法的唯一 slug(只含小写字母、数字与单连字符)。
func uniqueSlug(prefix string) string {
	slug := strings.ToLower(fmt.Sprintf("%s-%d-%d-%d", prefix, os.Getpid(), time.Now().UnixNano(), uniqueSeq.Add(1)))
	return strings.ReplaceAll(slug, "--", "-")
}

func stringPtr(value string) *string { return &value }

// ---------------------------------------------------------------------------
// Agent CRUD + nonce + 运行事务
// ---------------------------------------------------------------------------

func TestStoreAgentCRUDNonceAndRunTransaction(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	slug := uniqueSlug("pg-agent")

	secret := "integration-signing-secret"
	input := &AgentInput{
		Slug:          slug,
		Name:          "PostgreSQL integration",
		ExecutorKind:  ExecutorKindCodex,
		ExecutionMode: ExecutionModeLocal,
		SigningSecret: &secret,
	}
	if err := input.Validate(true); err != nil {
		t.Fatalf("validate input: %v", err)
	}

	created, err := store.CreateAgent(ctx, input)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = store.DeleteAgent(ctx, slug) })

	if created.Revision != 1 {
		t.Errorf("revision = %d, want 1", created.Revision)
	}
	if created.SigningSecret != secret {
		t.Errorf("signing_secret roundtrip = %q, want %q", created.SigningSecret, secret)
	}
	if created.ExecutorKind != ExecutorKindCodex {
		t.Errorf("executor_kind = %q, want codex", created.ExecutorKind)
	}
	if created.ExecutionMode != ExecutionModeLocal {
		t.Errorf("execution_mode = %q, want local", created.ExecutionMode)
	}
	if created.CreatedAt == "" || created.UpdatedAt != created.CreatedAt {
		t.Errorf("created_at=%q updated_at=%q, want equal and non-empty", created.CreatedAt, created.UpdatedAt)
	}

	fetched, err := store.GetAgent(ctx, slug)
	if err != nil || fetched == nil {
		t.Fatalf("get agent: %v (nil=%t)", err, fetched == nil)
	}
	if fetched.ID != created.ID {
		t.Errorf("id = %d, want %d", fetched.ID, created.ID)
	}

	// 更新时不带密钥应保留原密钥,并把 revision +1。
	update := &AgentInput{
		Slug:          slug,
		Name:          "PostgreSQL integration v2",
		ExecutorKind:  ExecutorKindPi,
		ExecutionMode: ExecutionModeGateway,
		AllowedTools:  []string{"read", "skill:momentum"},
	}
	if err := update.Validate(false); err != nil {
		t.Fatalf("validate update: %v", err)
	}
	updated, err := store.UpdateAgent(ctx, slug, update)
	if err != nil || updated == nil {
		t.Fatalf("update agent: %v (nil=%t)", err, updated == nil)
	}
	if updated.Revision != 2 {
		t.Errorf("revision = %d, want 2", updated.Revision)
	}
	if updated.SigningSecret != secret {
		t.Errorf("signing_secret should be preserved, got %q", updated.SigningSecret)
	}
	if len(updated.AllowedTools) != 2 || updated.AllowedTools[1] != "skill:momentum" {
		t.Errorf("allowed_tools = %v, want [read skill:momentum]", updated.AllowedTools)
	}

	// 轮换密钥。
	rotated := "rotated-signing-secret-value"
	update.SigningSecret = &rotated
	if err := update.Validate(false); err != nil {
		t.Fatalf("validate rotation: %v", err)
	}
	afterRotation, err := store.UpdateAgent(ctx, slug, update)
	if err != nil || afterRotation == nil {
		t.Fatalf("rotate secret: %v", err)
	}
	if afterRotation.SigningSecret != rotated {
		t.Errorf("signing_secret = %q, want %q", afterRotation.SigningSecret, rotated)
	}
	if afterRotation.Revision != 3 {
		t.Errorf("revision = %d, want 3", afterRotation.Revision)
	}

	// 启用中的 Agent 会出现在心跳密钥列表里。
	secrets, err := store.ListAgentSecrets(ctx)
	if err != nil {
		t.Fatalf("list agent secrets: %v", err)
	}
	found := false
	for _, pair := range secrets {
		if pair[0] == slug {
			found = true
			if pair[1] != rotated {
				t.Errorf("secret for %s = %q, want %q", slug, pair[1], rotated)
			}
		}
	}
	if !found {
		t.Errorf("enabled agent %s missing from ListAgentSecrets", slug)
	}

	// nonce:首次消费成功,重放失败。
	nonce := unique("nonce")
	fresh, err := store.ConsumeRequestNonce(ctx, slug, nonce, time.Now().Unix()+600)
	if err != nil || !fresh {
		t.Fatalf("first nonce consume: fresh=%t err=%v", fresh, err)
	}
	replayed, err := store.ConsumeRequestNonce(ctx, slug, nonce, time.Now().Unix()+600)
	if err != nil {
		t.Fatalf("replay nonce: %v", err)
	}
	if replayed {
		t.Error("replayed nonce must be rejected")
	}

	// 运行记录:StartRun 幂等冲突 + FinalizeRun 只接受 running 态。
	runID := unique("run")
	requestJSON := `{"runId":"x"}`
	started, err := store.StartRun(ctx, slug, runID, afterRotation.Revision,
		string(afterRotation.ExecutorKind), string(afterRotation.ExecutionMode), &requestJSON)
	if err != nil || !started {
		t.Fatalf("start run: started=%t err=%v", started, err)
	}
	again, err := store.StartRun(ctx, slug, runID, afterRotation.Revision, "pi", "gateway", nil)
	if err != nil {
		t.Fatalf("duplicate start run: %v", err)
	}
	if again {
		t.Error("duplicate run id must not insert a second row")
	}

	responseCode := int64(200)
	resultJSON := `{"payload_info":"ok"}`
	if err := store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: slug, RunID: runID, Status: "completed", DurationMs: 42,
		ResponseCode: &responseCode, ResultJSON: &resultJSON,
		ExecutorResultJSON: stringPtr(`{"raw":true}`),
	}); err != nil {
		t.Fatalf("finalize run: %v", err)
	}
	// 第二次终态写入必须失败:该运行已不在 running。
	if err := store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: slug, RunID: runID, Status: "failed", DurationMs: 1,
	}); err == nil {
		t.Error("finalizing a non-running run must fail")
	}

	runs, err := store.ListRuns(ctx, 50, slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != "completed" {
		t.Errorf("status = %q, want completed", run.Status)
	}
	if run.DurationMs == nil || *run.DurationMs != 42 {
		t.Errorf("duration_ms = %v, want 42", run.DurationMs)
	}
	if run.ResponseCode == nil || *run.ResponseCode != 200 {
		t.Errorf("response_code = %v, want 200", run.ResponseCode)
	}
	if run.Input == nil || *run.Input != requestJSON {
		t.Errorf("input = %v, want %q", run.Input, requestJSON)
	}
	if run.Output == nil || *run.Output != resultJSON {
		t.Errorf("output = %v, want %q", run.Output, resultJSON)
	}
	if run.ExecutorOutput == nil || *run.ExecutorOutput != `{"raw":true}` {
		t.Errorf("executor_output = %v", run.ExecutorOutput)
	}
	if run.ExecutorKind == nil || *run.ExecutorKind != "pi" {
		t.Errorf("executor_kind = %v, want pi", run.ExecutorKind)
	}
	if run.CompletedAt == nil {
		t.Error("completed_at must be set")
	}

	// 删除。
	deleted, err := store.DeleteAgent(ctx, slug)
	if err != nil || !deleted {
		t.Fatalf("delete agent: deleted=%t err=%v", deleted, err)
	}
	gone, err := store.GetAgent(ctx, slug)
	if err != nil {
		t.Fatalf("get deleted agent: %v", err)
	}
	if gone != nil {
		t.Error("deleted agent must not be returned")
	}
	deletedAgain, err := store.DeleteAgent(ctx, slug)
	if err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	if deletedAgain {
		t.Error("second delete must report false")
	}
}

// 更新不存在的 Agent 返回 (nil, nil)。
func TestStoreUpdateMissingAgent(t *testing.T) {
	store := newTestStore(t)
	input := &AgentInput{Slug: uniqueSlug("pg-missing"), Name: "missing"}
	if err := input.Validate(false); err != nil {
		t.Fatal(err)
	}
	agent, err := store.UpdateAgent(context.Background(), input.Slug, input)
	if err != nil {
		t.Fatalf("update missing agent: %v", err)
	}
	if agent != nil {
		t.Error("updating a missing agent must return nil")
	}
}

// ---------------------------------------------------------------------------
// 幂等:各分支
// ---------------------------------------------------------------------------

// seedAgent 建一个最小可用 Agent,返回其配置。
func seedAgent(t *testing.T, store *AgentStore, prefix string) *AgentConfig {
	t.Helper()
	ctx := context.Background()
	secret := "seed-signing-secret-value"
	input := &AgentInput{
		Slug:          uniqueSlug(prefix),
		Name:          "seed",
		SigningSecret: &secret,
	}
	if err := input.Validate(true); err != nil {
		t.Fatalf("validate seed agent: %v", err)
	}
	agent, err := store.CreateAgent(ctx, input)
	if err != nil {
		t.Fatalf("create seed agent: %v", err)
	}
	t.Cleanup(func() { _, _ = store.DeleteAgent(ctx, agent.Slug) })
	return agent
}

// 首次申请拿到租约;完成后同键同 hash 回放命中缓存。
func TestStoreIdempotencyNewThenCachedReplay(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-cached")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))
	runID := unique("run")

	first, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if first.Kind != IdempotencyNew {
		t.Fatalf("first kind = %v, want New", first.Kind)
	}
	if first.LeaseID == "" {
		t.Error("first begin must return a lease id")
	}
	if first.RunID != runID {
		t.Errorf("run_id = %q, want %q", first.RunID, runID)
	}

	// 租约仍活着时,同键请求应当报「正在执行」。
	inProgress, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if inProgress.Kind != IdempotencyInProgress {
		t.Fatalf("second kind = %v, want InProgress", inProgress.Kind)
	}

	// 完成运行并缓存响应体。
	requestJSON := `{"a":1}`
	if _, err := store.StartRun(ctx, agent.Slug, runID, agent.Revision, "pi", "gateway", &requestJSON); err != nil {
		t.Fatalf("start run: %v", err)
	}
	responseBody := `{"payload_info":"cached"}`
	if err := store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: agent.Slug, RunID: runID, Status: "completed", DurationMs: 7,
		ResultJSON:  &responseBody,
		Idempotency: CompleteIdempotency(key, first.LeaseID, responseBody),
	}); err != nil {
		t.Fatalf("finalize run: %v", err)
	}

	cached, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("replay begin: %v", err)
	}
	if cached.Kind != IdempotencyCached {
		t.Fatalf("replay kind = %v, want Cached", cached.Kind)
	}
	if cached.ResponseBody != responseBody {
		t.Errorf("cached body = %q, want %q", cached.ResponseBody, responseBody)
	}
	if cached.RunID != runID {
		t.Errorf("cached run_id = %q, want %q", cached.RunID, runID)
	}
}

// 同键但请求体不同 → Conflict。
func TestStoreIdempotencyBodyHashMismatchConflicts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-hash")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))

	if _, err := store.BeginIdempotency(ctx, agent.Slug, key,
		bodyHash([]byte(`{"a":1}`)), unique("run"), true); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	decision, err := store.BeginIdempotency(ctx, agent.Slug, key,
		bodyHash([]byte(`{"a":2}`)), unique("run"), true)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if decision.Kind != IdempotencyConflict {
		t.Fatalf("kind = %v, want Conflict", decision.Kind)
	}
}

// 显式指定 runId 且与已存记录不一致 → Conflict。
func TestStoreIdempotencyExplicitRunIDMismatchConflicts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-runid")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))

	if _, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, "run-original", true); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	decision, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, "run-different", true)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if decision.Kind != IdempotencyConflict {
		t.Fatalf("kind = %v, want Conflict", decision.Kind)
	}

	// enforceRunID=false 时不比对 runId,但租约仍在 → InProgress。
	relaxed, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, "run-different", false)
	if err != nil {
		t.Fatalf("relaxed begin: %v", err)
	}
	if relaxed.Kind != IdempotencyInProgress {
		t.Fatalf("relaxed kind = %v, want InProgress", relaxed.Kind)
	}
}

// 陈旧 processing 记录(updated_at 超过 7200s)可被接管。
func TestStoreIdempotencyStaleLeaseTakeover(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-stale")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))
	originalRun := unique("run-original")

	first, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, originalRun, false)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	// 把 updated_at 推到 3 小时前,模拟卡死的请求。
	staleAt := time.Now().UTC().Add(-3 * time.Hour).
		Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := store.pool.Exec(ctx,
		"UPDATE idempotency_records SET updated_at = $3 WHERE agent_slug = $1 AND key = $2",
		agent.Slug, key, staleAt); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	takeoverRun := unique("run-takeover")
	second, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, takeoverRun, false)
	if err != nil {
		t.Fatalf("takeover begin: %v", err)
	}
	if second.Kind != IdempotencyNew {
		t.Fatalf("takeover kind = %v, want New", second.Kind)
	}
	if second.LeaseID == first.LeaseID {
		t.Error("takeover must issue a fresh lease id")
	}
	// 原记录没有对应的 agent_runs 行,因此沿用原 run_id。
	if second.RunID != originalRun {
		t.Errorf("takeover run_id = %q, want %q (reuse when no run row exists)", second.RunID, originalRun)
	}

	// 旧租约已失效:用它提交终态必须失败。
	if _, err := store.StartRun(ctx, agent.Slug, second.RunID, agent.Revision, "pi", "gateway", nil); err != nil {
		t.Fatalf("start run: %v", err)
	}
	err = store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: agent.Slug, RunID: second.RunID, Status: "completed", DurationMs: 1,
		Idempotency: CompleteIdempotency(key, first.LeaseID, "{}"),
	})
	if err == nil {
		t.Error("stale lease must not be able to finalize")
	}
}

// 陈旧租约 + 已存在 running 运行(非 enforce):旧运行被标 stale_idempotency_lease,
// 接管方换用自己的 run_id。
func TestStoreIdempotencyStaleTakeoverMarksRunningRunFailed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-stalerun")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))
	originalRun := unique("run-original")

	if _, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, originalRun, false); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := store.StartRun(ctx, agent.Slug, originalRun, agent.Revision, "pi", "gateway", nil); err != nil {
		t.Fatalf("start original run: %v", err)
	}
	staleAt := time.Now().UTC().Add(-3 * time.Hour).
		Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := store.pool.Exec(ctx,
		"UPDATE idempotency_records SET updated_at = $3 WHERE agent_slug = $1 AND key = $2",
		agent.Slug, key, staleAt); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	takeoverRun := unique("run-takeover")
	decision, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, takeoverRun, false)
	if err != nil {
		t.Fatalf("takeover begin: %v", err)
	}
	if decision.Kind != IdempotencyNew {
		t.Fatalf("kind = %v, want New", decision.Kind)
	}
	if decision.RunID != takeoverRun {
		t.Errorf("run_id = %q, want the takeover run %q", decision.RunID, takeoverRun)
	}

	runs, err := store.ListRuns(ctx, 50, agent.Slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var original *RunView
	for i := range runs {
		if runs[i].RunID == originalRun {
			original = &runs[i]
		}
	}
	if original == nil {
		t.Fatalf("original run %s missing", originalRun)
	}
	if original.Status != "failed" {
		t.Errorf("original run status = %q, want failed", original.Status)
	}
	if original.ErrorCode == nil || *original.ErrorCode != "stale_idempotency_lease" {
		t.Errorf("original run error_code = %v, want stale_idempotency_lease", original.ErrorCode)
	}
}

// 陈旧租约 + 已完成运行 + 显式 runId → Conflict(已完成的运行无法安全重放)。
func TestStoreIdempotencyStaleTakeoverRefusesCompletedExplicitRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-donerun")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))
	runID := unique("run")

	first, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := store.StartRun(ctx, agent.Slug, runID, agent.Revision, "pi", "gateway", nil); err != nil {
		t.Fatalf("start run: %v", err)
	}
	// 完成运行,但把幂等记录留在 processing 且改成陈旧。
	if err := store.FinalizeRun(ctx, RunFinalization{
		AgentSlug: agent.Slug, RunID: runID, Status: "completed", DurationMs: 1,
	}); err != nil {
		t.Fatalf("finalize run: %v", err)
	}
	_ = first
	staleAt := time.Now().UTC().Add(-3 * time.Hour).
		Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := store.pool.Exec(ctx,
		"UPDATE idempotency_records SET updated_at = $3 WHERE agent_slug = $1 AND key = $2",
		agent.Slug, key, staleAt); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	decision, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if decision.Kind != IdempotencyConflict {
		t.Fatalf("kind = %v, want Conflict", decision.Kind)
	}
}

// completed 但 run_id 为 NULL 的遗留缓存行会被作废并重新执行一次。
func TestStoreIdempotencyLegacyCompletedRowIsReexecuted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-legacy")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))

	// 直接写一条迁移前形态的记录:completed、有响应体、run_id 为 NULL。
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO idempotency_records (agent_slug, key, request_hash, status, `+
			`response_body, lease_id, run_id, updated_at) `+
			`VALUES ($1, $2, $3, 'completed', $4, NULL, NULL, $5)`,
		agent.Slug, key, hash, `{"legacy":true}`, nowTimestamp()); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	runID := unique("run")
	decision, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, runID, true)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if decision.Kind != IdempotencyNew {
		t.Fatalf("kind = %v, want New (legacy cache must be invalidated)", decision.Kind)
	}
	if decision.RunID != runID {
		t.Errorf("run_id = %q, want %q", decision.RunID, runID)
	}
}

// ReleaseIdempotency 删除本请求的租约,后续同键请求可重新取得租约。
func TestStoreReleaseIdempotencyAllowsRetry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-release")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))

	first, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, unique("run"), false)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if err := store.ReleaseIdempotency(ctx, agent.Slug, key, first.LeaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, unique("run"), false)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if second.Kind != IdempotencyNew {
		t.Fatalf("kind = %v, want New after release", second.Kind)
	}
}

// 释放时 lease 不匹配则不删除任何行(不报错,但记录仍在)。
func TestStoreReleaseIdempotencyIgnoresForeignLease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-foreign")
	key := IdempotencyRecordKey(agent.ID, agent.Revision, unique("idem"))
	hash := bodyHash([]byte(`{"a":1}`))

	if _, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, unique("run"), false); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.ReleaseIdempotency(ctx, agent.Slug, key, "not-the-real-lease"); err != nil {
		t.Fatalf("release with foreign lease: %v", err)
	}
	decision, err := store.BeginIdempotency(ctx, agent.Slug, key, hash, unique("run"), false)
	if err != nil {
		t.Fatalf("begin again: %v", err)
	}
	if decision.Kind != IdempotencyInProgress {
		t.Fatalf("kind = %v, want InProgress (record must survive)", decision.Kind)
	}
}

// 幂等键含 revision:Agent 更新后旧缓存不会被命中。
func TestStoreIdempotencyKeyIsScopedToRevision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-idem-rev")
	rawKey := unique("idem")
	hash := bodyHash([]byte(`{"a":1}`))

	keyV1 := IdempotencyRecordKey(agent.ID, agent.Revision, rawKey)
	keyV2 := IdempotencyRecordKey(agent.ID, agent.Revision+1, rawKey)
	if keyV1 == keyV2 {
		t.Fatal("record keys for different revisions must differ")
	}
	if _, err := store.BeginIdempotency(ctx, agent.Slug, keyV1, hash, unique("run"), false); err != nil {
		t.Fatalf("begin v1: %v", err)
	}
	decision, err := store.BeginIdempotency(ctx, agent.Slug, keyV2, hash, unique("run"), false)
	if err != nil {
		t.Fatalf("begin v2: %v", err)
	}
	if decision.Kind != IdempotencyNew {
		t.Fatalf("kind = %v, want New for a new revision", decision.Kind)
	}
}

// ---------------------------------------------------------------------------
// 通道事件
// ---------------------------------------------------------------------------

func TestStoreChannelEventsRoundtripAndFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-chan")
	otherAgent := seedAgent(t, store, "pg-chan-other")
	runID := unique("run")

	statusCode := int64(200)
	accepted := true
	latency := int64(123)
	if err := store.RecordChannelEvent(ctx, ChannelEventRecord{
		EventType:  "callback",
		AgentSlug:  &agent.Slug,
		RunID:      &runID,
		URL:        "https://platform.example.com/callback",
		StatusCode: &statusCode,
		Accepted:   &accepted,
		RunStatus:  stringPtr("SUCCEEDED"),
		LatencyMs:  &latency,
		Input:      stringPtr(`{"payload_info":"x"}`),
		Output:     stringPtr(`{"code":"OK"}`),
	}); err != nil {
		t.Fatalf("record callback event: %v", err)
	}

	pass := true
	if err := store.RecordChannelEvent(ctx, ChannelEventRecord{
		EventType: "heartbeat",
		AgentSlug: &agent.Slug,
		URL:       "https://platform.example.com/heartbeat",
		Pass:      &pass,
		Detail:    stringPtr("self-check ok"),
	}); err != nil {
		t.Fatalf("record heartbeat event: %v", err)
	}

	if err := store.RecordChannelEvent(ctx, ChannelEventRecord{
		EventType: "callback",
		AgentSlug: &otherAgent.Slug,
		URL:       "https://platform.example.com/callback",
	}); err != nil {
		t.Fatalf("record other-agent event: %v", err)
	}

	// 按 slug 过滤。
	bySlug, err := store.ListChannelEvents(ctx, "", agent.Slug, 50)
	if err != nil {
		t.Fatalf("list by slug: %v", err)
	}
	if len(bySlug) != 2 {
		t.Fatalf("len(bySlug) = %d, want 2", len(bySlug))
	}
	// 时间倒序:heartbeat 后写,应排在前。
	if bySlug[0].EventType != "heartbeat" {
		t.Errorf("bySlug[0].eventType = %q, want heartbeat (newest first)", bySlug[0].EventType)
	}

	// 按 eventType + slug 过滤。
	byBoth, err := store.ListChannelEvents(ctx, "callback", agent.Slug, 50)
	if err != nil {
		t.Fatalf("list by type+slug: %v", err)
	}
	if len(byBoth) != 1 {
		t.Fatalf("len(byBoth) = %d, want 1", len(byBoth))
	}
	event := byBoth[0]
	if event.RunID == nil || *event.RunID != runID {
		t.Errorf("runId = %v, want %q", event.RunID, runID)
	}
	if event.StatusCode == nil || *event.StatusCode != 200 {
		t.Errorf("statusCode = %v, want 200", event.StatusCode)
	}
	if event.Accepted == nil || !*event.Accepted {
		t.Errorf("accepted = %v, want true", event.Accepted)
	}
	if event.RunStatus == nil || *event.RunStatus != "SUCCEEDED" {
		t.Errorf("runStatus = %v, want SUCCEEDED", event.RunStatus)
	}
	if event.LatencyMs == nil || *event.LatencyMs != 123 {
		t.Errorf("latencyMs = %v, want 123", event.LatencyMs)
	}
	if event.Input == nil || *event.Input != `{"payload_info":"x"}` {
		t.Errorf("input = %v", event.Input)
	}
	if event.Output == nil || *event.Output != `{"code":"OK"}` {
		t.Errorf("output = %v", event.Output)
	}
	if event.Pass != nil {
		t.Errorf("pass = %v, want nil for a callback event", event.Pass)
	}
	if event.CreatedAt == "" {
		t.Error("createdAt must be set")
	}

	// 按 eventType 过滤(跨 Agent)。
	byType, err := store.ListChannelEvents(ctx, "callback", "", 200)
	if err != nil {
		t.Fatalf("list by type: %v", err)
	}
	if len(byType) < 2 {
		t.Errorf("len(byType) = %d, want >= 2", len(byType))
	}
	for _, item := range byType {
		if item.EventType != "callback" {
			t.Errorf("unexpected eventType %q in callback filter", item.EventType)
		}
	}

	// limit 收敛到 1..200。
	limited, err := store.ListChannelEvents(ctx, "", agent.Slug, 0)
	if err != nil {
		t.Fatalf("list with limit 0: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit 0 should clamp to 1, got %d rows", len(limited))
	}
}

// ChannelEventView 的 JSON 键必须是 camelCase(管理台前端依赖)。
func TestChannelEventViewJSONKeys(t *testing.T) {
	encoded, err := json.Marshal(ChannelEventView{EventType: "callback", URL: "https://x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"eventType"`, `"agentSlug"`, `"runId"`, `"url"`, `"statusCode"`, `"accepted"`,
		`"runStatus"`, `"pass"`, `"latencyMs"`, `"detail"`, `"input"`, `"output"`, `"createdAt"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("ChannelEventView JSON is missing %s: %s", key, encoded)
		}
	}
}

// RunView / AgentView 的 JSON 键必须是 snake_case。
func TestRunAndAgentViewJSONKeys(t *testing.T) {
	runEncoded, err := json.Marshal(RunView{RunID: "r", AgentSlug: "a", Status: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"run_id"`, `"agent_slug"`, `"agent_revision"`, `"executor_kind"`, `"execution_mode"`,
		`"status"`, `"duration_ms"`, `"error_code"`, `"response_code"`, `"input"`, `"output"`,
		`"executor_output"`, `"created_at"`, `"completed_at"`,
	} {
		if !strings.Contains(string(runEncoded), key) {
			t.Errorf("RunView JSON is missing %s: %s", key, runEncoded)
		}
	}

	agent := &AgentConfig{
		Slug: "research-one", ExecutorKind: ExecutorKindPi, ExecutionMode: ExecutionModeGateway,
		SignatureEncoding: "hex", SigningSecret: "super-secret-value",
	}
	agentEncoded, err := json.Marshal(agent.ToView("http://127.0.0.1:8080/"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"executor_kind"`, `"execution_mode"`, `"signing_secret_configured"`,
		`"signing_secret_hint"`, `"endpoint"`, `"workspace_path"`, `"timeout_secs"`,
		`"max_concurrency"`, `"queue_capacity"`, `"revision"`,
	} {
		if !strings.Contains(string(agentEncoded), key) {
			t.Errorf("AgentView JSON is missing %s: %s", key, agentEncoded)
		}
	}
	// 明文签名密钥绝不能出现在展示态里。
	if strings.Contains(string(agentEncoded), "super-secret-value") {
		t.Fatalf("AgentView must never expose the signing secret: %s", agentEncoded)
	}
	if !strings.Contains(string(agentEncoded), `"endpoint":"http://127.0.0.1:8080/agent/research-one"`) {
		t.Errorf("endpoint not derived correctly: %s", agentEncoded)
	}
}

// ---------------------------------------------------------------------------
// 会话 / 审计 / 过期清理
// ---------------------------------------------------------------------------

func TestStoreSessionLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	username := unique("session-admin")
	if err := store.EnsureBootstrapAdmin(ctx, username, "test-hash"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.DeleteUser(ctx, username) })
	token := randomToken(32)
	hash := tokenHash(token)
	expiresAt := time.Now().Unix() + 3600

	if err := store.CreateSession(ctx, hash, username, "csrf-token-value", expiresAt); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteSession(ctx, hash) })

	record, err := store.GetSession(ctx, hash)
	if err != nil || record == nil {
		t.Fatalf("get session: %v (nil=%t)", err, record == nil)
	}
	if record.Username != username || record.Role != UserRoleAdmin || record.CSRFToken != "csrf-token-value" {
		t.Errorf("session = %+v", record)
	}
	if record.ExpiresAt != expiresAt {
		t.Errorf("expires_at = %d, want %d", record.ExpiresAt, expiresAt)
	}

	// 同 token 再次创建即覆盖(ON CONFLICT DO UPDATE)。
	if err := store.CreateSession(ctx, hash, username, "rotated-csrf", expiresAt+1); err != nil {
		t.Fatalf("recreate session: %v", err)
	}
	record, err = store.GetSession(ctx, hash)
	if err != nil || record == nil {
		t.Fatalf("get session after upsert: %v", err)
	}
	if record.CSRFToken != "rotated-csrf" {
		t.Errorf("csrf_token = %q, want rotated-csrf", record.CSRFToken)
	}

	if err := store.DeleteSession(ctx, hash); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	gone, err := store.GetSession(ctx, hash)
	if err != nil {
		t.Fatalf("get deleted session: %v", err)
	}
	if gone != nil {
		t.Error("deleted session must not be returned")
	}
}

// 已过期的会话不会被返回,即使行还在。
func TestStoreExpiredSessionIsNotReturned(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	username := unique("expired-admin")
	if err := store.EnsureBootstrapAdmin(ctx, username, "test-hash"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.DeleteUser(ctx, username) })
	hash := tokenHash(randomToken(32))
	if err := store.CreateSession(ctx, hash, username, "csrf", time.Now().Unix()-1); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteSession(ctx, hash) })

	record, err := store.GetSession(ctx, hash)
	if err != nil {
		t.Fatalf("get expired session: %v", err)
	}
	if record != nil {
		t.Error("expired session must not be returned")
	}
}

func TestStoreUserRolesSessionsAndActorScopedAudits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	admin := unique("account-admin")
	user := unique("account-user")
	if err := store.EnsureBootstrapAdmin(ctx, admin, "test-admin-hash"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.DeleteUser(ctx, admin) })
	if created, err := store.CreateUser(ctx, user, "test-user-hash", UserRoleUser); err != nil || !created {
		t.Fatalf("create user = %v, %v", created, err)
	}
	t.Cleanup(func() { _, _ = store.DeleteUser(ctx, user) })
	if got, err := store.GetUserAuth(ctx, admin); err != nil || got == nil || got.Role != UserRoleAdmin {
		t.Fatalf("admin auth = %#v, %v", got, err)
	}
	if got, err := store.GetUser(ctx, user); err != nil || got == nil || got.Role != UserRoleUser {
		t.Fatalf("user view = %#v, %v", got, err)
	}

	tokenHashValue := tokenHash(randomToken(32))
	if err := store.CreateSession(ctx, tokenHashValue, user, "csrf", time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	if session, err := store.GetSession(ctx, tokenHashValue); err != nil || session == nil || session.Role != UserRoleUser {
		t.Fatalf("user session = %#v, %v", session, err)
	}

	if err := store.RecordAudit(ctx, admin, "user.create", user, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAudit(ctx, user, "agent.update", "role-agent", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	userAudits, err := store.ListAuditLogs(ctx, 100, user)
	if err != nil || len(userAudits) == 0 {
		t.Fatalf("user audits = %#v, %v", userAudits, err)
	}
	for _, entry := range userAudits {
		if entry.Actor != user {
			t.Fatalf("leaked another actor: %#v", entry)
		}
	}
	allAudits, err := store.ListAuditLogs(ctx, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var foundAdmin, foundUser bool
	for _, entry := range allAudits {
		foundAdmin = foundAdmin || entry.Actor == admin
		foundUser = foundUser || entry.Actor == user
	}
	if !foundAdmin || !foundUser {
		t.Fatalf("all audits missing actors: admin=%t user=%t", foundAdmin, foundUser)
	}

	disabled := false
	updated, err := store.UpdateUser(ctx, user, nil, UserRoleUser, disabled)
	if err != nil || updated == nil || updated.Enabled {
		t.Fatalf("disable user = %#v, %v", updated, err)
	}
	if session, err := store.GetSession(ctx, tokenHashValue); err != nil || session != nil {
		t.Fatalf("disabled user session survived: %#v, %v", session, err)
	}
}

func TestStoreRecordAudit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	target := unique("audit-target")
	if err := store.RecordAudit(ctx, "admin", "agent.create", target,
		map[string]any{"revision": 1, "enabled": true}); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	var actor, action, details string
	err := store.pool.QueryRow(ctx,
		"SELECT actor, action, details FROM audit_log WHERE target = $1", target).
		Scan(&actor, &action, &details)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor != "admin" || action != "agent.create" {
		t.Errorf("actor=%q action=%q", actor, action)
	}
	if !strings.Contains(details, `"revision":1`) || !strings.Contains(details, `"enabled":true`) {
		t.Errorf("details = %s", details)
	}
}

func TestStoreExecutorProviderIsEncryptedAtRest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	providerID := unique("provider")
	key := "executor_provider:" + providerID
	t.Cleanup(func() { _, _ = store.pool.Exec(ctx, "DELETE FROM platform_settings WHERE key = $1", key) })

	want := map[string]any{"enabled": true, "api_key": "provider-secret-0123456789", "model": "gpt-test"}
	if err := store.PutExecutorProvider(ctx, providerID, want); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.pool.QueryRow(ctx, "SELECT value FROM platform_settings WHERE key = $1", key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "provider-secret") || strings.Contains(raw, "gpt-test") {
		t.Fatalf("provider configuration was stored in plaintext: %q", raw)
	}
	got, err := store.GetExecutorProvider(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != want["api_key"] || got["model"] != want["model"] {
		t.Fatalf("provider roundtrip = %#v", got)
	}
}

// claimPeriodicGC 在窗口内只放行一次。
func TestClaimPeriodicGCThrottles(t *testing.T) {
	var last atomic.Int64
	now := time.Now().Unix()

	if !claimPeriodicGC(&last, now) {
		t.Fatal("first claim should be allowed")
	}
	if claimPeriodicGC(&last, now) {
		t.Error("second claim inside the window must be denied")
	}
	if claimPeriodicGC(&last, now+periodicGCIntervalSecs-1) {
		t.Error("claim just before the window elapses must be denied")
	}
	if !claimPeriodicGC(&last, now+periodicGCIntervalSecs) {
		t.Error("claim after the window elapses should be allowed")
	}
}

// 过期 nonce 会在周期清理时被删除,使同一个 nonce 可以再次使用。
func TestStoreExpiredNoncesAreCollected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-nonce-gc")
	nonce := unique("nonce")

	// 写一个已过期的 nonce。
	fresh, err := store.ConsumeRequestNonce(ctx, agent.Slug, nonce, time.Now().Unix()-10)
	if err != nil || !fresh {
		t.Fatalf("seed expired nonce: fresh=%t err=%v", fresh, err)
	}
	// 强制下一次调用执行 GC。
	store.lastNonceGC.Store(0)
	reused, err := store.ConsumeRequestNonce(ctx, agent.Slug, nonce, time.Now().Unix()+600)
	if err != nil {
		t.Fatalf("reuse after gc: %v", err)
	}
	if !reused {
		t.Error("an expired nonce must be collected and become reusable")
	}
}

// ListRuns 不带 slug 时跨 Agent,并把 limit 收敛到 1..200。
func TestStoreListRunsLimitAndScope(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	agent := seedAgent(t, store, "pg-runs-limit")

	for i := 0; i < 3; i++ {
		runID := unique(fmt.Sprintf("run-%d", i))
		if _, err := store.StartRun(ctx, agent.Slug, runID, agent.Revision, "pi", "gateway", nil); err != nil {
			t.Fatalf("start run %d: %v", i, err)
		}
	}

	all, err := store.ListRuns(ctx, 200, agent.Slug)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// 时间倒序。
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt < all[i].CreatedAt {
			t.Errorf("runs are not ordered newest-first: %q before %q", all[i-1].CreatedAt, all[i].CreatedAt)
		}
	}

	clamped, err := store.ListRuns(ctx, 0, agent.Slug)
	if err != nil {
		t.Fatalf("list runs with limit 0: %v", err)
	}
	if len(clamped) != 1 {
		t.Errorf("limit 0 should clamp to 1, got %d", len(clamped))
	}

	crossAgent, err := store.ListRuns(ctx, 200, "")
	if err != nil {
		t.Fatalf("list runs across agents: %v", err)
	}
	if len(crossAgent) < 3 {
		t.Errorf("cross-agent listing returned %d rows, want >= 3", len(crossAgent))
	}
}

// 定宽时间戳保证 TEXT 列上的字典序等于时间序。
func TestNowTimestampIsFixedWidthAndSortable(t *testing.T) {
	first := nowTimestamp()
	if len(first) != len("2006-01-02T15:04:05.000000000Z") {
		t.Fatalf("timestamp %q has unexpected width %d", first, len(first))
	}
	if !strings.HasSuffix(first, "Z") {
		t.Errorf("timestamp %q must end with Z", first)
	}
	if _, err := parseTimestamp(first); err != nil {
		t.Fatalf("timestamp %q must parse as RFC3339: %v", first, err)
	}
	time.Sleep(2 * time.Millisecond)
	second := nowTimestamp()
	if !(first < second) {
		t.Errorf("lexicographic order must match chronological order: %q !< %q", first, second)
	}
}
