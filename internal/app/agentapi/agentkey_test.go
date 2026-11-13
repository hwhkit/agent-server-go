package agentapi

// 智能体密钥连接点（方案 B）与机器侧直接提交的用例测试。
// 独立假件（ak* 前缀）避免与 service_test.go 的通用假件耦合。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	domain "ir-agent-server/internal/domain/connector"
)

type akFakeSecrets struct {
	bySlug map[string]struct {
		current, prev []byte
	}
	misses []string
}

func (f *akFakeSecrets) SigningSecrets(_ context.Context, slug string) ([]byte, []byte, error) {
	f.misses = append(f.misses, slug)
	record, ok := f.bySlug[slug]
	if !ok {
		return nil, nil, errors.New("not found")
	}
	return record.current, record.prev, nil
}

type akFakeDenies struct {
	denied map[string]bool
}

func (f *akFakeDenies) Deny(_ context.Context, slug, cluster string) error {
	f.denied[slug+"\x00"+cluster] = true
	return nil
}
func (f *akFakeDenies) Allow(_ context.Context, slug, cluster string) error {
	delete(f.denied, slug+"\x00"+cluster)
	return nil
}
func (f *akFakeDenies) List(context.Context) ([]domain.ClusterDeny, error) {
	return nil, nil
}
func (f *akFakeDenies) Denied(_ context.Context, slug, cluster string) (bool, error) {
	return f.denied[slug+"\x00"+cluster], nil
}

// akFakeQueue 在 fakeQueue 之上补 RecordDirectRun。
type akFakeQueue struct {
	fakeQueue
	directRuns []domain.DirectRun
	directErr  error
}

func (f *akFakeQueue) RecordDirectRun(_ context.Context, run domain.DirectRun) (string, error) {
	if f.directErr != nil {
		return "", f.directErr
	}
	f.directRuns = append(f.directRuns, run)
	return run.Status, nil
}

func akService(t *testing.T, queue *akFakeQueue, secrets *akFakeSecrets, denies *akFakeDenies) *Service {
	t.Helper()
	return NewService(Deps{
		Config: Config{
			LeaseSeconds: 90, RenewInterval: 30 * time.Second,
			HeartbeatIntervalSeconds: 60, ClaimMaxWait: 50 * time.Millisecond,
			MaxOutstandingLeases: 64, MaxAttempts: 3, QueueTTL: time.Hour,
			WorkerOfflineAfterMiss: 3,
		},
		Queue:   queue,
		Tokens:  newFakeTokens(),
		Workers: &fakeWorkers{},
		Remote:  fakeLister{},
		Secrets: secrets,
		Denies:  denies,
	})
}

func akFixtures() (*akFakeSecrets, string, string) {
	secrets := &akFakeSecrets{bySlug: map[string]struct{ current, prev []byte }{
		"analyst": {current: []byte("master-key-current")},
	}}
	derived := domain.DeriveClusterKey([]byte("master-key-current"), "alpha")
	return secrets, "master-key-current", derived
}

func TestAgentKeyMasterAuth(t *testing.T) {
	queue := &akFakeQueue{}
	secrets, master, _ := akFixtures()
	svc := akService(t, queue, secrets, &akFakeDenies{denied: map[string]bool{}})

	principal, failure := svc.Authenticate(context.Background(), "Bearer "+master,
		AgentKeyHint{Slug: "analyst"})
	if failure != nil {
		t.Fatalf("master auth: %+v", failure)
	}
	if principal.ID != "agent:analyst" || principal.Scopes[0] != "analyst" {
		t.Fatalf("principal: %+v", principal)
	}
	// 错误密钥 → 401。
	if _, failure := svc.Authenticate(context.Background(), "Bearer wrong",
		AgentKeyHint{Slug: "analyst"}); failure == nil || failure.Status != 401 {
		t.Fatalf("wrong key: %+v", failure)
	}
	// 缺 slug 头 → 401。
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+master); failure == nil {
		t.Fatalf("missing slug hint: expected failure")
	}
	// act_ 前缀仍走令牌路径（不误入 agent-key 分支）。
	svcTokens := NewService(Deps{Config: Config{}, Tokens: newFakeTokens(), Queue: &akFakeQueue{}, Workers: &fakeWorkers{}})
	plaintext, err := svcTokens.IssueToken(context.Background(), "conn", []string{"analyst"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if principal, failure := svcTokens.Authenticate(context.Background(), "Bearer "+plaintext); failure != nil {
		t.Fatalf("act_ path broken: %+v", failure)
	} else if principal.Name != "conn" {
		t.Fatalf("act_ principal: %+v", principal)
	}
}

func TestAgentKeyDerivedClusterAuth(t *testing.T) {
	queue := &akFakeQueue{}
	secrets, master, derived := akFixtures()
	denies := &akFakeDenies{denied: map[string]bool{}}
	svc := akService(t, queue, secrets, denies)

	principal, failure := svc.Authenticate(context.Background(), "Bearer "+derived,
		AgentKeyHint{Slug: "analyst", Cluster: "alpha"})
	if failure != nil {
		t.Fatalf("derived auth: %+v", failure)
	}
	if principal.Name != "analyst/alpha" {
		t.Fatalf("principal name: %+v", principal)
	}
	// 拿 alpha 子密钥冒充 beta 集群 → 401。
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+derived,
		AgentKeyHint{Slug: "analyst", Cluster: "beta"}); failure == nil || failure.Status != 401 {
		t.Fatalf("cross-cluster key: %+v", failure)
	}
	// 主密钥也能通过集群头（宽限语义：持主密钥者可信）。
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+master,
		AgentKeyHint{Slug: "analyst", Cluster: "alpha"}); failure != nil {
		t.Fatalf("master via cluster header: %+v", failure)
	}
	// 非法 cluster 字符 → 400。
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+derived,
		AgentKeyHint{Slug: "analyst", Cluster: "BAD CLUSTER"}); failure == nil || failure.Status != 400 {
		t.Fatalf("invalid cluster id: %+v", failure)
	}
}

func TestAgentKeyClusterDeny(t *testing.T) {
	queue := &akFakeQueue{}
	secrets, _, derived := akFixtures()
	denies := &akFakeDenies{denied: map[string]bool{}}
	svc := akService(t, queue, secrets, denies)

	if err := svc.DenyCluster(context.Background(), "analyst", "alpha"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+derived,
		AgentKeyHint{Slug: "analyst", Cluster: "alpha"}); failure == nil || failure.Status != 403 {
		t.Fatalf("denied cluster: %+v", failure)
	}
	// 主密钥不受集群名单影响。
	secrets2, master, _ := akFixtures()
	svc2 := akService(t, queue, secrets2, denies)
	if _, failure := svc2.Authenticate(context.Background(), "Bearer "+master,
		AgentKeyHint{Slug: "analyst", Cluster: "alpha"}); failure != nil {
		t.Fatalf("master after deny: %+v", failure)
	}
}

func TestAgentKeyRotationGrace(t *testing.T) {
	queue := &akFakeQueue{}
	secrets := &akFakeSecrets{bySlug: map[string]struct{ current, prev []byte }{
		"analyst": {current: []byte("new-key"), prev: []byte("old-key")},
	}}
	svc := akService(t, queue, secrets, &akFakeDenies{denied: map[string]bool{}})
	if _, failure := svc.Authenticate(context.Background(), "Bearer old-key",
		AgentKeyHint{Slug: "analyst"}); failure != nil {
		t.Fatalf("prev master during grace: %+v", failure)
	}
	derivedOld := domain.DeriveClusterKey([]byte("old-key"), "alpha")
	if _, failure := svc.Authenticate(context.Background(), "Bearer "+derivedOld,
		AgentKeyHint{Slug: "analyst", Cluster: "alpha"}); failure != nil {
		t.Fatalf("prev derived during grace: %+v", failure)
	}
}

func TestSubmitRunValidationAndScope(t *testing.T) {
	queue := &akFakeQueue{}
	secrets, _, _ := akFixtures()
	svc := akService(t, queue, secrets, &akFakeDenies{denied: map[string]bool{}})
	principal := &Principal{ID: "agent:analyst", Name: "analyst", Scopes: []string{"analyst"}}

	// 成功提交。
	result, failure := svc.SubmitRun(context.Background(), principal, SubmitRunInput{
		Slug: "analyst", Status: "succeeded",
		Payload: json.RawMessage(`{"summary":"cron ok"}`),
		Input:   json.RawMessage(`{"day":"monday"}`),
	})
	if failure != nil || !result.Accepted || result.RunStatus != "succeeded" {
		t.Fatalf("submit: %+v failure=%+v", result, failure)
	}
	if !strings.HasPrefix(result.RunID, "run_") {
		t.Fatalf("generated runId: %q", result.RunID)
	}
	if len(queue.directRuns) != 1 || queue.directRuns[0].Slug != "analyst" {
		t.Fatalf("queue record: %+v", queue.directRuns)
	}

	// scope 外 slug（agent-key 主体被锁死在自己的 slug）→ 403。
	if _, failure := svc.SubmitRun(context.Background(), principal, SubmitRunInput{
		Slug: "other", Status: "succeeded", Payload: json.RawMessage(`{}`)}); failure == nil || failure.Status != 403 {
		t.Fatalf("scope escape: %+v", failure)
	}
	// succeeded 缺 payload → 400。
	if _, failure := svc.SubmitRun(context.Background(), principal, SubmitRunInput{
		Slug: "analyst", Status: "succeeded"}); failure == nil || failure.Status != 400 {
		t.Fatalf("missing payload: %+v", failure)
	}
	// aborted 不是合法直接提交状态 → 400。
	if _, failure := svc.SubmitRun(context.Background(), principal, SubmitRunInput{
		Slug: "analyst", Status: "aborted", Payload: json.RawMessage(`{}`)}); failure == nil {
		t.Fatalf("aborted accepted: %+v", failure)
	}
	// 未注册 slug（scope 覆盖时才走得到队列层；scope 外先 403 防枚举）→ 404。
	brokenQueue := &akFakeQueue{directErr: domain.ErrUnknownAgent}
	brokenSvc := NewService(Deps{Config: Config{}, Queue: brokenQueue, Tokens: newFakeTokens(),
		Workers: &fakeWorkers{}, Secrets: secrets})
	wildcard := &Principal{ID: "tok:1", Name: "conn", Scopes: []string{"*"}}
	if _, failure := brokenSvc.SubmitRun(context.Background(), wildcard, SubmitRunInput{
		Slug: "ghost", Status: "succeeded", Payload: json.RawMessage(`{}`)}); failure == nil || failure.Status != 404 {
		t.Fatalf("unknown agent: %+v", failure)
	}
}
