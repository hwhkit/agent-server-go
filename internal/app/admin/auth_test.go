package admin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/infra/crypto"
)

// ---------------------------------------------------------------------------
// 假件
// ---------------------------------------------------------------------------

type fakeAccounts struct{ users map[string]*account.Account }

func (f fakeAccounts) Lookup(_ context.Context, username string) (*account.Account, error) {
	if user, ok := f.users[username]; ok {
		out := *user
		return &out, nil
	}
	return nil, nil
}

type fakeSessions struct {
	mu       sync.Mutex
	sessions map[string]*account.Session
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{sessions: map[string]*account.Session{}}
}

func (f *fakeSessions) Put(_ context.Context, session account.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := session
	f.sessions[session.TokenHash] = &stored
	return nil
}

func (f *fakeSessions) Get(_ context.Context, tokenHash string) (*account.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[tokenHash]
	if !ok {
		return nil, account.ErrNotFound
	}
	out := *session
	return &out, nil
}

func (f *fakeSessions) Delete(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	return nil
}

func (f *fakeSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// stubVerify 让测试避开 argon2 成本：hash 形如 "stub:<password>"。
func stubVerify(encoded, password string) bool {
	return encoded == "stub:"+password
}

func newTestAuth(t *testing.T, users map[string]*account.Account) (*Auth, *fakeSessions) {
	t.Helper()
	sessions := newFakeSessions()
	auth, err := NewAuth(Config{
		DummyPasswordHash: "stub:dummy", SessionTTL: time.Hour, Verify: stubVerify,
	}, fakeAccounts{users: users}, sessions)
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	return auth, sessions
}

func adminUser(username, password string) *account.Account {
	return &account.Account{
		Username: username, Role: account.RoleAdmin,
		PasswordHash: "stub:" + password, Enabled: true,
	}
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

func TestLoginSuccessIssuesHashedSession(t *testing.T) {
	t.Parallel()
	auth, sessions := newTestAuth(t, map[string]*account.Account{
		"root": adminUser("root", "s3cret"),
	})
	outcome, err := auth.Login(context.Background(), "root", "s3cret", "10.0.0.1")
	if err != nil || outcome.Kind != LoginAuthenticated {
		t.Fatalf("login = %+v err=%v", outcome, err)
	}
	session := outcome.Session
	if session.Username != "root" || session.Role != account.RoleAdmin ||
		session.CSRFToken == "" || strings.Contains(session.Token, "s3cret") {
		t.Errorf("session = %+v", session)
	}
	// 存储的是令牌哈希，不是明文。
	if _, ok := sessions.sessions[session.Token]; ok {
		t.Error("plaintext token must never be stored")
	}
	if _, ok := sessions.sessions[TokenHash(session.Token)]; !ok {
		t.Error("session must be stored under the token hash")
	}
	// Authenticate 往返。
	user, err := auth.Authenticate(context.Background(), session.Token)
	if err != nil || user == nil || user.Username != "root" || user.Role != account.RoleAdmin {
		t.Fatalf("authenticate = %+v err=%v", user, err)
	}
	// CSRF：正确令牌通过，错误/缺失拒绝。
	if !auth.VerifyCSRF(session.CSRFToken, user) {
		t.Error("csrf must verify")
	}
	if auth.VerifyCSRF("wrong", user) || auth.VerifyCSRF("", user) {
		t.Error("bad csrf must not verify")
	}
	// 登出后会话失效。
	if err := auth.Logout(context.Background(), session.Token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if user, _ := auth.Authenticate(context.Background(), session.Token); user != nil {
		t.Error("session must die after logout")
	}
}

func TestLoginInvalidPaths(t *testing.T) {
	t.Parallel()
	disabled := adminUser("off", "pw")
	disabled.Enabled = false
	auth, _ := newTestAuth(t, map[string]*account.Account{
		"root": adminUser("root", "s3cret"),
		"off":  disabled,
	})
	ctx := context.Background()
	for _, tc := range []struct {
		name           string
		username, pass string
	}{
		{"wrong password", "root", "nope"},
		{"unknown user (dummy hash path)", "ghost", "whatever"},
		{"disabled account", "off", "pw"},
	} {
		outcome, err := auth.Login(ctx, tc.username, tc.pass, "10.0.0."+tc.name)
		if err != nil || outcome.Kind != LoginInvalid {
			t.Errorf("%s = %+v err=%v", tc.name, outcome, err)
		}
	}
}

func TestLoginBackoffAfterGrace(t *testing.T) {
	t.Parallel()
	auth, _ := newTestAuth(t, map[string]*account.Account{
		"root": adminUser("root", "s3cret"),
	})
	ctx := context.Background()
	source := "10.9.9.9"
	// 前 3 次失败是 Invalid（宽限内），第 4 次开始 RateLimited。
	for i := 0; i < loginFailureGrace; i++ {
		outcome, _ := auth.Login(ctx, "root", "bad", source)
		if outcome.Kind != LoginInvalid {
			t.Fatalf("attempt %d = %+v, want Invalid", i+1, outcome)
		}
	}
	outcome, _ := auth.Login(ctx, "root", "s3cret", source)
	if outcome.Kind != LoginRateLimited || outcome.RetryAfterSecs < 1 {
		t.Fatalf("blocked login = %+v, want RateLimited", outcome)
	}
	// 其他来源不受影响（桶按账号+来源组合隔离）。
	outcome, _ = auth.Login(ctx, "root", "s3cret", "10.8.8.8")
	if outcome.Kind != LoginAuthenticated {
		t.Errorf("other source = %+v, want Authenticated", outcome)
	}
	// 成功登录不清别的桶：被封来源仍然被封。
	outcome, _ = auth.Login(ctx, "root", "s3cret", source)
	if outcome.Kind != LoginRateLimited {
		t.Errorf("blocked source after other success = %+v", outcome)
	}
}

func TestLoginConcurrencyGate(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, loginSlotCount)
	sessions := newFakeSessions()
	blockingVerify := func(encoded, password string) bool {
		started <- struct{}{}
		<-release
		return encoded == "stub:"+password
	}
	auth, err := NewAuth(Config{
		DummyPasswordHash: "stub:x", SessionTTL: time.Hour, Verify: blockingVerify,
	}, fakeAccounts{users: map[string]*account.Account{"root": adminUser("root", "pw")}}, sessions)
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	ctx := context.Background()
	// 占满 4 个校验槽，并等待全部真正进入校验（否则轮询者可能抢到空槽）。
	for i := 0; i < loginSlotCount; i++ {
		go func() { _, _ = auth.Login(ctx, "root", "pw", "10.1.1.1") }()
	}
	for i := 0; i < loginSlotCount; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("login slots never filled")
		}
	}
	outcome, err := auth.Login(ctx, "root", "pw", "10.2.2.2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if outcome.Kind != LoginRateLimited || outcome.RetryAfterSecs != 1 {
		t.Fatalf("gate outcome = %+v", outcome)
	}
	close(release)
}

func TestNewAuthValidation(t *testing.T) {
	t.Parallel()
	sessions := newFakeSessions()
	accounts := fakeAccounts{users: map[string]*account.Account{}}
	if _, err := NewAuth(Config{SessionTTL: time.Hour, Verify: stubVerify}, accounts, sessions); err == nil {
		t.Error("missing dummy hash must fail")
	}
	if _, err := NewAuth(Config{DummyPasswordHash: "x", Verify: stubVerify}, accounts, sessions); err == nil {
		t.Error("missing ttl must fail")
	}
	if _, err := NewAuth(Config{DummyPasswordHash: "x", SessionTTL: time.Hour}, accounts, sessions); err == nil {
		t.Error("missing verifier must fail")
	}
}

// TestVerifyAgainstRealArgon2 用 infra/crypto 的真实 PHC 实现走一遍
// 签发→校验闭环（测试文件允许跨层引用；生产依赖仍走注入）。
func TestVerifyAgainstRealArgon2(t *testing.T) {
	t.Parallel()
	hash, err := crypto.HashPassword("real-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	auth, _ := newTestAuth(t, map[string]*account.Account{
		"root": {Username: "root", Role: account.RoleAdmin, PasswordHash: hash, Enabled: true},
	})
	auth.verify = crypto.VerifyPassword
	outcome, err := auth.Login(context.Background(), "root", "real-password", "local")
	if err != nil || outcome.Kind != LoginAuthenticated {
		t.Fatalf("real argon2 login = %+v err=%v", outcome, err)
	}
	outcome, _ = auth.Login(context.Background(), "root", "wrong", "local2")
	if outcome.Kind != LoginInvalid {
		t.Errorf("wrong password = %+v", outcome)
	}
	// 哨兵错误透传。
	lookupAuth, _ := newTestAuth(t, nil)
	lookupAuth.accounts = failingAccounts{}
	if _, err := lookupAuth.Login(context.Background(), "x", "y", "z"); !errors.Is(err, errLookup) {
		t.Errorf("lookup error = %v", err)
	}
}

var errLookup = errors.New("db down")

type failingAccounts struct{}

func (failingAccounts) Lookup(context.Context, string) (*account.Account, error) {
	return nil, errLookup
}
