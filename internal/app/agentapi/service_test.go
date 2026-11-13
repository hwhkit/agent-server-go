package agentapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	domain "ir-agent-server/internal/domain/connector"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

type fakeQueue struct {
	claimTask    *domain.Task
	claimOutcome domain.ClaimOutcome
	claimErr     error
	leaseCounts  map[string]int
	renewOutcome domain.RenewOutcome
	completeOut  domain.CompleteOutcome
	completeErr  error
	depth        int
	enqueued     []domain.NewTask
	aborted      []string
	waitState    domain.TaskState
	waitResult   []byte
}

func (f *fakeQueue) Enqueue(_ context.Context, task domain.NewTask) error {
	f.enqueued = append(f.enqueued, task)
	return nil
}
func (f *fakeQueue) Get(context.Context, string) (*domain.Task, error) {
	return nil, domain.ErrTaskNotFound
}
func (f *fakeQueue) WaitTerminal(context.Context, string) (domain.TaskState, []byte, error) {
	return f.waitState, f.waitResult, nil
}
func (f *fakeQueue) Claim(context.Context, domain.ClaimRequest) (*domain.Task, domain.ClaimOutcome, error) {
	return f.claimTask, f.claimOutcome, f.claimErr
}
func (f *fakeQueue) WorkerLeaseCount(_ context.Context, workerID string) (int, error) {
	return f.leaseCounts[workerID], nil
}
func (f *fakeQueue) Renew(context.Context, string, string, time.Duration) (domain.RenewOutcome, error) {
	return f.renewOutcome, nil
}
func (f *fakeQueue) RequestAbort(_ context.Context, runID string) error {
	f.aborted = append(f.aborted, runID)
	return nil
}
func (f *fakeQueue) Complete(context.Context, domain.CompleteRequest) (domain.CompleteOutcome, error) {
	return f.completeOut, f.completeErr
}
func (f *fakeQueue) QueueDepth(context.Context, []string) (int, error) { return f.depth, nil }
func (f *fakeQueue) Sweep(context.Context, domain.SweepPolicy) (domain.SweepStats, error) {
	return domain.SweepStats{}, nil
}
func (f *fakeQueue) RecordDirectRun(context.Context, domain.DirectRun) (string, error) {
	return "succeeded", nil
}

type fakeTokens struct {
	byHash map[string]*domain.Token
	byName map[string]*domain.Token
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{byHash: map[string]*domain.Token{}, byName: map[string]*domain.Token{}}
}

func (f *fakeTokens) Issue(_ context.Context, t *domain.Token) error {
	if existing, ok := f.byName[t.Name]; ok {
		delete(f.byHash, existing.Hash)
	}
	f.byHash[t.Hash] = t
	f.byName[t.Name] = t
	return nil
}
func (f *fakeTokens) GetByHash(_ context.Context, hash string) (*domain.Token, error) {
	if t, ok := f.byHash[hash]; ok {
		return t, nil
	}
	return nil, domain.ErrTokenNotFound
}
func (f *fakeTokens) GetByName(_ context.Context, name string) (*domain.Token, error) {
	if t, ok := f.byName[name]; ok {
		return t, nil
	}
	return nil, domain.ErrTokenNotFound
}
func (f *fakeTokens) List(context.Context) ([]domain.Token, error) {
	out := make([]domain.Token, 0, len(f.byName))
	for _, t := range f.byName {
		out = append(out, *t)
	}
	return out, nil
}
func (f *fakeTokens) RevokeByName(_ context.Context, name string) error {
	t, ok := f.byName[name]
	if !ok {
		return domain.ErrTokenNotFound
	}
	now := time.Now()
	t.RevokedAt = &now
	return nil
}
func (f *fakeTokens) Revoke(_ context.Context, id string, graceUntil *time.Time) error {
	for _, t := range f.byName {
		if t.ID == id {
			now := time.Now()
			t.RevokedAt = &now
			t.GraceUntil = graceUntil
			return nil
		}
	}
	return domain.ErrTokenNotFound
}

type fakeWorkers struct{ beats []domain.Worker }

func (f *fakeWorkers) Beat(_ context.Context, w domain.Worker) error {
	f.beats = append(f.beats, w)
	return nil
}
func (f *fakeWorkers) ListOnline(context.Context) ([]domain.Worker, error) { return f.beats, nil }
func (f *fakeWorkers) MarkOfflineStale(context.Context, time.Time) (int, error) {
	return 0, nil
}

type fakeLister struct{ slugs []string }

func (f fakeLister) RemotePullSlugs(context.Context) ([]string, error) { return f.slugs, nil }

func newTestService(queue *fakeQueue, tokens *fakeTokens, workers *fakeWorkers, remote RemoteAgentLister) *Service {
	return NewService(Deps{
		Config: Config{
			LeaseSeconds: 90, RenewInterval: 30 * time.Second,
			HeartbeatIntervalSeconds: 60, ClaimMaxWait: 50 * time.Millisecond,
			MaxOutstandingLeases: 64, MaxAttempts: 3, QueueTTL: time.Hour,
			WorkerOfflineAfterMiss: 3,
		},
		Queue: queue, Tokens: tokens, Workers: workers, Remote: remote,
	})
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

func TestIssueAuthenticateRevoke(t *testing.T) {
	t.Parallel()
	tokens := newFakeTokens()
	svc := newTestService(&fakeQueue{}, tokens, &fakeWorkers{}, nil)
	ctx := context.Background()

	plaintext, err := svc.IssueToken(ctx, "gpu-box", []string{"Fundamental-Analyst", " "})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(plaintext) < 40 || plaintext[:4] != domain.TokenPrefix {
		t.Fatalf("plaintext = %q", plaintext[:8])
	}

	principal, apiErr := svc.Authenticate(ctx, "Bearer "+plaintext)
	if apiErr != nil {
		t.Fatalf("authenticate: %+v", apiErr)
	}
	if principal.Name != "gpu-box" || len(principal.Scopes) != 1 || principal.Scopes[0] != "fundamental-analyst" {
		t.Errorf("principal = %+v（scope 应小写清洗）", principal)
	}

	// 前缀缺失 / 错误令牌 → 401。
	if _, apiErr := svc.Authenticate(ctx, plaintext[:10]); apiErr == nil || apiErr.Status != 401 {
		t.Errorf("bare token = %+v", apiErr)
	}
	if _, apiErr := svc.Authenticate(ctx, "Bearer act_wrong"); apiErr == nil || apiErr.Status != 401 {
		t.Errorf("wrong token = %+v", apiErr)
	}

	// 同名再签发 = 轮换：旧明文立即失效。
	rotated, err := svc.IssueToken(ctx, "gpu-box", nil)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, apiErr := svc.Authenticate(ctx, "Bearer "+plaintext); apiErr == nil {
		t.Error("old token must die on rotation")
	}
	if principal, apiErr := svc.Authenticate(ctx, "Bearer "+rotated); apiErr != nil || len(principal.Scopes) != 1 || principal.Scopes[0] != "*" {
		t.Errorf("rotated auth = %+v %+v（空 scope 应按通配）", principal, apiErr)
	}

	// 吊销即失效。
	if err := svc.RevokeToken(ctx, "gpu-box"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, apiErr := svc.Authenticate(ctx, "Bearer "+rotated); apiErr == nil || apiErr.Message != "connector token revoked" {
		t.Errorf("revoked auth = %+v", apiErr)
	}
}

// ---------------------------------------------------------------------------
// claim
// ---------------------------------------------------------------------------

func principalWith(scopes ...string) *Principal {
	return &Principal{ID: "1", Name: "p", Scopes: scopes}
}

func TestClaimValidationAndScope(t *testing.T) {
	t.Parallel()
	svc := newTestService(&fakeQueue{}, newFakeTokens(), &fakeWorkers{}, nil)
	ctx := context.Background()

	if _, apiErr := svc.Claim(ctx, principalWith("demo"), ClaimInput{WorkerID: "BAD_ID!"}); apiErr == nil || apiErr.Code != "invalid_worker_id" {
		t.Errorf("bad worker id = %+v", apiErr)
	}
	// scope 不交叉 → 204 no-scope-match。
	_, apiErr := svc.Claim(ctx, principalWith("other-agent"), ClaimInput{WorkerID: "w-1", PreferredSlugs: []string{"demo"}})
	if apiErr == nil || apiErr.Status != 204 || apiErr.EmptyReason != "no-scope-match" {
		t.Errorf("no scope match = %+v", apiErr)
	}
}

func TestClaimDeliveredMapping(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(time.Hour)
	lease := time.Now().Add(90 * time.Second)
	queue := &fakeQueue{
		claimOutcome: domain.ClaimDelivered,
		claimTask: &domain.Task{
			RunID: "run_9f", AgentSlug: "demo", AgentRevision: 7, Attempts: 2,
			Payload: []byte(`{"taskBrief":"…"}`), ExecutionDeadline: deadline, LeaseExpiresAt: lease,
		},
	}
	svc := newTestService(queue, newFakeTokens(), &fakeWorkers{}, nil)
	task, apiErr := svc.Claim(context.Background(), principalWith("demo"), ClaimInput{WorkerID: "gpu-box-03", Capacity: 4})
	if apiErr != nil {
		t.Fatalf("claim: %+v", apiErr)
	}
	if task.RunID != "run_9f" || task.AgentRevision != 7 || task.Attempts != 2 ||
		task.ExecutorKind != "remote-pull" || task.LeaseSeconds != 90 || task.RenewAfterSeconds != 30 {
		t.Errorf("task = %+v", task)
	}
	var input map[string]any
	if err := json.Unmarshal(task.Input, &input); err != nil || input["taskBrief"] != "…" {
		t.Errorf("input = %s", task.Input)
	}
}

func TestClaimRateLimitedAndLeaseConflict(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claimOutcome: domain.ClaimRateLimited}
	svc := newTestService(queue, newFakeTokens(), &fakeWorkers{}, nil)
	_, apiErr := svc.Claim(context.Background(), principalWith("demo"), ClaimInput{WorkerID: "w-1"})
	if apiErr == nil || apiErr.Status != 429 || apiErr.RetryAfter != "2" {
		t.Errorf("rate limited = %+v", apiErr)
	}

	queue = &fakeQueue{leaseCounts: map[string]int{"w-1": 2}, claimOutcome: domain.ClaimEmpty}
	svc = newTestService(queue, newFakeTokens(), &fakeWorkers{}, nil)
	_, apiErr = svc.Claim(context.Background(), principalWith("demo"), ClaimInput{WorkerID: "w-1", InFlight: 1})
	if apiErr == nil || apiErr.Status != 409 || apiErr.Code != "WORKER_LEASE_ACTIVE" {
		t.Errorf("lease conflict = %+v", apiErr)
	}
	// 自报在途与服务端一致时不触发。
	_, apiErr = svc.Claim(context.Background(), principalWith("demo"), ClaimInput{WorkerID: "w-1", InFlight: 2})
	if apiErr != nil && apiErr.Code == "WORKER_LEASE_ACTIVE" {
		t.Errorf("matching in-flight must not conflict: %+v", apiErr)
	}
}

func TestClaimEmptyWindowIs204Empty(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claimOutcome: domain.ClaimEmpty}
	svc := newTestService(queue, newFakeTokens(), &fakeWorkers{}, nil)
	started := time.Now()
	_, apiErr := svc.Claim(context.Background(), principalWith("demo"), ClaimInput{WorkerID: "w-1"})
	if apiErr == nil || apiErr.Status != 204 || apiErr.EmptyReason != "empty" {
		t.Fatalf("empty window = %+v", apiErr)
	}
	if time.Since(started) < 40*time.Millisecond {
		t.Error("claim must hold the long-poll window before 204")
	}
}

func TestClaimWildcardScopeExpands(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claimOutcome: domain.ClaimEmpty}
	svc := newTestService(queue, newFakeTokens(), &fakeWorkers{}, fakeLister{slugs: []string{"a1", "a2"}})
	_, _ = svc.Claim(context.Background(), principalWith("*"), ClaimInput{WorkerID: "w-1", MaxWaitSeconds: 1})
	// 无 remote-pull 智能体时通配展开为空 → no-scope-match。
	svc2 := newTestService(queue, newFakeTokens(), &fakeWorkers{}, fakeLister{})
	_, apiErr := svc2.Claim(context.Background(), principalWith("*"), ClaimInput{WorkerID: "w-1"})
	if apiErr == nil || apiErr.EmptyReason != "no-scope-match" {
		t.Errorf("empty expansion = %+v", apiErr)
	}
}

// ---------------------------------------------------------------------------
// renew / complete / heartbeat
// ---------------------------------------------------------------------------

func TestRenewMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome    domain.RenewOutcome
		wantAbort  bool
		wantStatus int
		wantCode   string
	}{
		{domain.RenewOK, false, 0, ""},
		{domain.RenewAbort, true, 0, ""},
		{domain.RenewLeaseLost, false, 409, "LEASE_LOST"},
		{domain.RenewDeadline, false, 409, "TASK_DEADLINE_EXCEEDED"},
		{domain.RenewUnknown, false, 404, "run_not_found"},
	}
	for _, tc := range cases {
		svc := newTestService(&fakeQueue{renewOutcome: tc.outcome}, newFakeTokens(), &fakeWorkers{}, nil)
		result, apiErr := svc.Renew(context.Background(), "run-1", "w-1")
		if tc.wantStatus == 0 {
			if apiErr != nil || result.Abort != tc.wantAbort {
				t.Errorf("outcome %v = %+v %+v", tc.outcome, result, apiErr)
			}
			continue
		}
		if apiErr == nil || apiErr.Status != tc.wantStatus || apiErr.Code != tc.wantCode {
			t.Errorf("outcome %v = %+v", tc.outcome, apiErr)
		}
	}
	svc := newTestService(&fakeQueue{}, newFakeTokens(), &fakeWorkers{}, nil)
	if _, apiErr := svc.Renew(context.Background(), " ", "w-1"); apiErr == nil || apiErr.Status != 400 {
		t.Errorf("missing run id = %+v", apiErr)
	}
}

func TestCompleteValidationAndOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(&fakeQueue{}, newFakeTokens(), &fakeWorkers{}, nil)

	if _, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{Status: "maybe"}); apiErr == nil || apiErr.Code != "invalid_status" {
		t.Errorf("bad status = %+v", apiErr)
	}
	if _, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{Status: "succeeded"}); apiErr == nil {
		t.Error("succeeded without payload must be rejected")
	}
	if _, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{Status: "failed", Error: &CompleteError{Message: "x"}}); apiErr == nil {
		t.Error("failed without error.code must be rejected")
	}
	if _, apiErr := svc.Complete(ctx, "", "w", CompleteInput{Status: "aborted"}); apiErr == nil || apiErr.Status != 400 {
		t.Errorf("missing run id = %+v", apiErr)
	}

	// 成功回传。
	queue := &fakeQueue{completeOut: domain.CompleteOutcome{Accepted: true, RunStatus: "completed"}}
	svc = newTestService(queue, newFakeTokens(), &fakeWorkers{}, nil)
	result, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{
		Status: "succeeded", Payload: json.RawMessage(`{"summary":"ok"}`),
		Usage: &CompleteUsage{InputTokens: 8210, OutputTokens: 1204, TotalTokens: 9414},
	})
	if apiErr != nil || !result.Accepted || result.RunStatus != "completed" {
		t.Fatalf("complete = %+v %+v", result, apiErr)
	}

	// 冲突与未找到。
	svc = newTestService(&fakeQueue{completeOut: domain.CompleteOutcome{Conflict: true}}, newFakeTokens(), &fakeWorkers{}, nil)
	if _, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{Status: "aborted"}); apiErr == nil || apiErr.Code != "RESULT_CONFLICT" {
		t.Errorf("conflict = %+v", apiErr)
	}
	svc = newTestService(&fakeQueue{completeErr: domain.ErrTaskNotFound}, newFakeTokens(), &fakeWorkers{}, nil)
	if _, apiErr := svc.Complete(ctx, "r", "w", CompleteInput{Status: "aborted"}); apiErr == nil || apiErr.Status != 404 {
		t.Errorf("unknown run = %+v", apiErr)
	}
	// 迟到结果仅归档。
	svc = newTestService(&fakeQueue{completeOut: domain.CompleteOutcome{RunStatus: "superseded"}}, newFakeTokens(), &fakeWorkers{}, nil)
	result, apiErr = svc.Complete(ctx, "r", "w", CompleteInput{Status: "succeeded", Payload: json.RawMessage(`{}`)})
	if apiErr != nil || result.Accepted || result.RunStatus != "superseded" {
		t.Errorf("superseded = %+v %+v", result, apiErr)
	}
}

func TestHeartbeatRegistersAndReports(t *testing.T) {
	t.Parallel()
	workers := &fakeWorkers{}
	queue := &fakeQueue{depth: 3}
	svc := newTestService(queue, newFakeTokens(), workers, nil)

	result, apiErr := svc.Heartbeat(context.Background(), principalWith("demo"), HeartbeatInput{
		WorkerID: "gpu-box-03", InFlight: 1, Capacity: 4,
		Slugs: []string{"demo"}, Version: "1.0.0",
	})
	if apiErr != nil {
		t.Fatalf("heartbeat: %+v", apiErr)
	}
	if result.HeartbeatIntervalSeconds != 60 || result.RenewIntervalSeconds != 30 || result.QueueDepth != 3 {
		t.Errorf("result = %+v", result)
	}
	if len(workers.beats) != 1 || workers.beats[0].Capacity != 4 || workers.beats[0].Version != "1.0.0" {
		t.Errorf("beat = %+v", workers.beats)
	}
	if _, apiErr := svc.Heartbeat(context.Background(), principalWith("demo"), HeartbeatInput{WorkerID: "!"}); apiErr == nil || apiErr.Code != "invalid_worker_id" {
		t.Errorf("bad worker = %+v", apiErr)
	}
}
