package mota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// argon2 PHC 编解码
// ---------------------------------------------------------------------------

func TestHashAdminPasswordRoundTrips(t *testing.T) {
	t.Parallel()
	encoded, err := HashAdminPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyAdminPassword(encoded, "correct horse battery staple") {
		t.Error("the original password must verify")
	}
	if VerifyAdminPassword(encoded, "wrong password") {
		t.Error("a different password must not verify")
	}
	if VerifyAdminPassword(encoded, "") {
		t.Error("an empty password must not verify")
	}
}

// 输出必须是 Rust argon2 crate 默认参数的 PHC 串,否则两侧的 password_hash 不互通。
func TestHashAdminPasswordUsesRustDefaultParameters(t *testing.T) {
	t.Parallel()
	encoded, err := HashAdminPassword("pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("encoded = %q, want the Argon2::default() parameter prefix", encoded)
	}
	parsed, err := parseArgon2Hash(encoded)
	if err != nil {
		t.Fatalf("parse own output: %v", err)
	}
	if parsed.variant != "argon2id" {
		t.Errorf("variant = %q", parsed.variant)
	}
	if parsed.memory != 19456 || parsed.time != 2 || parsed.threads != 1 {
		t.Errorf("params = m=%d,t=%d,p=%d, want 19456/2/1", parsed.memory, parsed.time, parsed.threads)
	}
	if len(parsed.salt) != argon2SaltLen {
		t.Errorf("salt length = %d, want %d", len(parsed.salt), argon2SaltLen)
	}
	if len(parsed.digest) != int(argon2DefaultKeyLen) {
		t.Errorf("digest length = %d, want %d", len(parsed.digest), argon2DefaultKeyLen)
	}
	// PHC 的 base64 不带填充。
	if strings.Contains(encoded, "=") && !strings.Contains(encoded, "v=19") {
		t.Error("PHC base64 must not be padded")
	}
}

// 每次哈希用新盐,所以同一口令的输出不同,但都能验过。
func TestHashAdminPasswordUsesFreshSalt(t *testing.T) {
	t.Parallel()
	first, err := HashAdminPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashAdminPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two hashes of the same password must differ (fresh salt)")
	}
	if !VerifyAdminPassword(first, "same") || !VerifyAdminPassword(second, "same") {
		t.Error("both hashes must verify")
	}
}

// 校验时必须用**存储哈希里的参数**,而不是当前默认值 —— 否则换过参数的老哈希验不过。
func TestVerifyAdminPasswordHonoursEmbeddedParameters(t *testing.T) {
	t.Parallel()
	// 一份用非默认参数(m=8192,t=1,p=2)生成的 argon2id 哈希。
	// 注意这里是运行时算出来的,确保与实现一致。
	custom := buildArgon2Hash(t, "argon2id", 8192, 1, 2, "pw")
	if !VerifyAdminPassword(custom, "pw") {
		t.Error("a hash with non-default parameters must still verify")
	}
	if VerifyAdminPassword(custom, "other") {
		t.Error("a wrong password must not verify")
	}
	// argon2i 变体也要支持。
	variant := buildArgon2Hash(t, "argon2i", 8192, 1, 1, "pw")
	if !VerifyAdminPassword(variant, "pw") {
		t.Error("argon2i must be supported")
	}
}

func TestParseArgon2HashRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	valid, err := HashAdminPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, "$")

	cases := map[string]string{
		"空串":           "",
		"段数不足":         "$argon2id$v=19$m=19456,t=2,p=1",
		"缺前导 $":        strings.TrimPrefix(valid, "$"),
		"未知变体":         "$argon2xx$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"argon2d 不受支持": "$argon2d$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"版本不对":         "$argon2id$v=16$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"版本段格式错":       "$argon2id$version$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"参数段格式错":       "$argon2id$v=19$memory=1$" + parts[4] + "$" + parts[5],
		"参数为零":         "$argon2id$v=19$m=0,t=2,p=1$" + parts[4] + "$" + parts[5],
		"参数资源过大":       "$argon2id$v=19$m=999999999,t=2,p=1$" + parts[4] + "$" + parts[5],
		"盐不是 base64":   "$argon2id$v=19$m=19456,t=2,p=1$!!!$" + parts[5],
		"哈希不是 base64":  "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$!!!",
		"盐为空":          "$argon2id$v=19$m=19456,t=2,p=1$$" + parts[5],
		"哈希为空":         "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$",
		"bcrypt 格式":    "$2b$12$abcdefghijklmnopqrstuv",
	}
	for name, encoded := range cases {
		if _, err := parseArgon2Hash(encoded); err == nil {
			t.Errorf("%s (%q) must be rejected", name, encoded)
		}
		// 无法解析的哈希在校验时一律返回 false,不能 panic。
		if VerifyAdminPassword(encoded, "pw") {
			t.Errorf("%s must not verify any password", name)
		}
	}
}

// buildArgon2Hash 用指定参数生成 PHC 串(测试用)。
func buildArgon2Hash(t *testing.T, variant string, memory, time uint32, threads uint8, password string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	var digest []byte
	switch variant {
	case "argon2id":
		digest = argon2IDKey([]byte(password), salt, time, memory, threads, argon2DefaultKeyLen)
	case "argon2i":
		digest = argon2IKey([]byte(password), salt, time, memory, threads, argon2DefaultKeyLen)
	default:
		t.Fatalf("unsupported variant %q", variant)
	}
	return "$" + variant + "$v=19$m=" + itoa(int(memory)) + ",t=" + itoa(int(time)) +
		",p=" + itoa(int(threads)) + "$" + phcBase64.EncodeToString(salt) +
		"$" + phcBase64.EncodeToString(digest)
}

func itoa(value int) string { return strconv.Itoa(value) }

// ---------------------------------------------------------------------------
// AdminAuth 构造
// ---------------------------------------------------------------------------

// 优先使用配置的 password_hash;只有明文时才启动期哈希;两者都无则失败。
func TestNewAdminAuthPasswordSources(t *testing.T) {
	store := newTestStore(t)

	// 明文口令 → 启动期哈希。
	cfg := testPlatformConfig(t)
	cfg.AdminPassword = "plain-secret"
	cfg.AdminPasswordHash = ""
	auth, err := NewAdminAuth(cfg, store)
	if err != nil {
		t.Fatalf("plain password: %v", err)
	}
	if !VerifyAdminPassword(auth.dummyPasswordHash, "plain-secret") {
		t.Error("the plain password must be hashed at startup")
	}

	// 预置 hash 优先(即使同时给了明文)。
	preset, err := HashAdminPassword("from-hash")
	if err != nil {
		t.Fatal(err)
	}
	cfg = testPlatformConfig(t)
	cfg.AdminPassword = "ignored-plain"
	cfg.AdminPasswordHash = preset
	auth, err = NewAdminAuth(cfg, store)
	if err != nil {
		t.Fatalf("preset hash: %v", err)
	}
	if auth.dummyPasswordHash != preset {
		t.Error("a configured password_hash must be used verbatim")
	}
	if VerifyAdminPassword(auth.dummyPasswordHash, "ignored-plain") {
		t.Error("the plain password must be ignored when a hash is configured")
	}

	// 无法解析的 hash → 构造失败。
	cfg = testPlatformConfig(t)
	cfg.AdminPasswordHash = "$2b$12$not-argon2"
	cfg.AdminPassword = ""
	if _, err := NewAdminAuth(cfg, store); err == nil ||
		!strings.Contains(err.Error(), "parse [admin].password_hash") {
		t.Errorf("err = %v, want a password_hash parse failure", err)
	}

	// 两者都没有 → 拒绝无凭证运行。
	cfg = testPlatformConfig(t)
	cfg.AdminPassword = ""
	cfg.AdminPasswordHash = ""
	if _, err := NewAdminAuth(cfg, store); err == nil ||
		err.Error() != "admin password is not configured" {
		t.Errorf("err = %v, want a missing-credential failure", err)
	}
}

// ---------------------------------------------------------------------------
// 登录:凭证、并发闸与失败退避
// ---------------------------------------------------------------------------

// newTestAuth 组装一个用固定口令的 AdminAuth。
func newTestAuth(t *testing.T, password string) *AdminAuth {
	t.Helper()
	store := newTestStore(t)
	cfg := testPlatformConfig(t)
	cfg.AdminUsername = uniqueSlug("auth-admin")
	cfg.AdminPassword = password
	cfg.AdminPasswordHash = ""
	auth, err := NewAdminAuth(cfg, store)
	if err != nil {
		t.Fatalf("build auth: %v", err)
	}
	return auth
}

func TestAdminAuthLoginIssuesSession(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	outcome, err := auth.Login(context.Background(), auth.Username(), "pw-correct")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if outcome.Kind != LoginAuthenticated {
		t.Fatalf("kind = %v, want authenticated", outcome.Kind)
	}
	if len(outcome.Session.Token) == 0 || len(outcome.Session.CSRFToken) == 0 {
		t.Error("both tokens must be issued")
	}
	if outcome.Session.Token == outcome.Session.CSRFToken {
		t.Error("the session token and CSRF token must differ")
	}
	if outcome.Session.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expiresAt = %d, want a future timestamp", outcome.Session.ExpiresAt)
	}
	// 会话必须能按 token 哈希查回来。
	record, err := auth.store.GetSession(context.Background(), tokenHash(outcome.Session.Token))
	if err != nil || record == nil {
		t.Fatalf("session lookup = (%v, %v)", record, err)
	}
	if record.Username != auth.Username() || record.CSRFToken != outcome.Session.CSRFToken {
		t.Errorf("session record = %+v", record)
	}
}

// 用户名与口令都要对;任一不对都只暴露同一条结果。
func TestAdminAuthLoginRejectsWrongCredentials(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	cases := []struct{ user, password string }{
		{auth.Username(), "pw-wrong"},
		{"root", "pw-correct"},
		{"", "pw-correct"},
		{auth.Username(), ""},
	}
	for _, item := range cases {
		// 每次都重置退避,避免第 3 次以后变成限流。
		auth.clearFailures()
		outcome, err := auth.Login(context.Background(), item.user, item.password)
		if err != nil {
			t.Fatalf("%+v: %v", item, err)
		}
		if outcome.Kind != LoginInvalid {
			t.Errorf("%+v: kind = %v, want invalid", item, outcome.Kind)
		}
		if outcome.Session != nil {
			t.Errorf("%+v: a failed login must not issue a session", item)
		}
	}
}

// 连续失败 >= 3 次后进入指数退避;成功登录会清零。
func TestAdminAuthLoginBackoffAfterRepeatedFailures(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	// 前两次只是 invalid,不封禁。
	for i := 0; i < loginFailureGrace-1; i++ {
		outcome, err := auth.Login(context.Background(), auth.Username(), "nope")
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Kind != LoginInvalid {
			t.Fatalf("attempt %d: kind = %v, want invalid", i+1, outcome.Kind)
		}
	}
	if _, blocked := auth.blockedFor(); blocked {
		t.Fatalf("must not be blocked before %d failures", loginFailureGrace)
	}

	// 第 3 次失败触发封禁。
	outcome, err := auth.Login(context.Background(), auth.Username(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != LoginInvalid {
		t.Fatalf("kind = %v, want invalid on the failure that arms the backoff", outcome.Kind)
	}
	retryAfter, blocked := auth.blockedFor()
	if !blocked {
		t.Fatal("the third failure must arm the backoff")
	}
	if retryAfter < 1 {
		t.Errorf("retryAfter = %d, want at least 1", retryAfter)
	}

	// 封禁期内即使凭证正确也走限流。
	outcome, err = auth.Login(context.Background(), auth.Username(), "pw-correct")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != LoginRateLimited {
		t.Fatalf("kind = %v, want rate limited while blocked", outcome.Kind)
	}
	if outcome.RetryAfterSecs < 1 {
		t.Errorf("retryAfter = %d, want at least 1", outcome.RetryAfterSecs)
	}

	// 手动解除封禁后正确凭证通过,并把计数清零。
	auth.clearFailures()
	outcome, err = auth.Login(context.Background(), auth.Username(), "pw-correct")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != LoginAuthenticated {
		t.Fatalf("kind = %v, want authenticated", outcome.Kind)
	}
	if _, blocked := auth.blockedFor(); blocked {
		t.Error("a successful login must clear the backoff")
	}
	auth.throttleMu.Lock()
	state := auth.throttles[accountSourceThrottleKey(auth.Username(), "local")]
	var failures uint32
	if state != nil {
		failures = state.failures
	}
	auth.throttleMu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after a successful login", failures)
	}
}

// 退避时长按 2^(failures-3) 递增,封顶 32 秒。
func TestAdminAuthBackoffGrowsAndCaps(t *testing.T) {
	t.Parallel()
	auth := &AdminAuth{loginSlots: newSemaphore(loginSlotCount)}
	expected := map[uint32]int64{3: 1, 4: 2, 5: 4, 6: 8, 7: 16, 8: 32, 9: 32, 20: 32}
	for failures := uint32(1); failures <= 20; failures++ {
		auth.recordFailure()
		want, checked := expected[failures]
		if !checked {
			continue
		}
		remaining, blocked := auth.blockedFor()
		if !blocked {
			t.Fatalf("failures=%d: expected a block", failures)
		}
		// blockedFor 向下取整,所以允许比目标少 1 秒。
		if remaining != want && remaining != want-1 {
			t.Errorf("failures=%d: retryAfter = %d, want about %d", failures, remaining, want)
		}
	}
}

func TestAdminAuthThrottleIsScopedAndCannotBeClearedByAnotherAccount(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	const sourceA = "198.51.100.10"
	const sourceB = "198.51.100.11"
	for i := 0; i < loginFailureGrace-1; i++ {
		auth.recordFailureFor(auth.Username(), sourceA)
	}

	password := "ordinary-user-password"
	hash, err := HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	username := unique("throttle-user")
	created, err := auth.store.CreateUser(context.Background(), username, hash, UserRoleUser)
	if err != nil || !created {
		t.Fatalf("create user = %t, %v", created, err)
	}
	t.Cleanup(func() { _, _ = auth.store.DeleteUser(context.Background(), username) })
	outcome, err := auth.LoginFromSource(context.Background(), username, password, sourceA)
	if err != nil || outcome.Kind != LoginAuthenticated {
		t.Fatalf("ordinary user login = %+v, %v", outcome, err)
	}

	// The successful user login must not reset sourceA's failures against the
	// admin. The next failure arms that account+source bucket.
	auth.recordFailureFor(auth.Username(), sourceA)
	if _, blocked := auth.blockedForKeys(
		accountSourceThrottleKey(auth.Username(), sourceA)); !blocked {
		t.Fatal("a different account login cleared the target-account throttle")
	}
	if _, blocked := auth.blockedForKeys(loginThrottleKeys(auth.Username(), sourceB)...); blocked {
		t.Fatal("failures from sourceA must not lock out sourceB")
	}
}

// 并发闸满时直接限流,不排队。
func TestAdminAuthLoginRateLimitsWhenSlotsExhausted(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	// 占满所有登录许可。
	held := 0
	for auth.loginSlots.TryAcquire() {
		held++
	}
	defer func() {
		for i := 0; i < held; i++ {
			auth.loginSlots.Release()
		}
	}()
	if held != loginSlotCount {
		t.Fatalf("acquired %d slots, want %d", held, loginSlotCount)
	}

	started := time.Now()
	outcome, err := auth.Login(context.Background(), auth.Username(), "pw-correct")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != LoginRateLimited {
		t.Fatalf("kind = %v, want rate limited", outcome.Kind)
	}
	if outcome.RetryAfterSecs != 1 {
		t.Errorf("retryAfter = %d, want 1", outcome.RetryAfterSecs)
	}
	// 必须立刻返回,不能等许可。
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("rate limiting took %v, want it immediate", elapsed)
	}
}

// 登录许可用完就还:连续多次登录都能通过。
func TestAdminAuthLoginReleasesSlots(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	for i := 0; i < loginSlotCount+2; i++ {
		outcome, err := auth.Login(context.Background(), auth.Username(), "pw-correct")
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if outcome.Kind != LoginAuthenticated {
			t.Fatalf("iteration %d: kind = %v", i, outcome.Kind)
		}
	}
	for i := 0; i < loginSlotCount; i++ {
		if !auth.loginSlots.TryAcquire() {
			t.Fatalf("login slot %d leaked", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 会话读取、登出与 CSRF
// ---------------------------------------------------------------------------

func TestAdminAuthAuthenticateAndLogout(t *testing.T) {
	auth := newTestAuth(t, "pw-correct")
	outcome, err := auth.Login(context.Background(), auth.Username(), "pw-correct")
	if err != nil {
		t.Fatal(err)
	}

	// 没有 Cookie → (nil, nil),不是错误。
	bare := httptest.NewRequest(http.MethodGet, "/api/admin/session", nil)
	session, err := auth.Authenticate(context.Background(), bare)
	if err != nil || session != nil {
		t.Fatalf("bare request => (%v, %v), want (nil, nil)", session, err)
	}
	// 空值 Cookie 同样视为没有。
	bare.AddCookie(&http.Cookie{Name: SessionCookieName, Value: ""})
	if session, err := auth.Authenticate(context.Background(), bare); err != nil || session != nil {
		t.Fatalf("empty cookie => (%v, %v), want (nil, nil)", session, err)
	}

	// 带 Cookie → 命中会话。
	request := httptest.NewRequest(http.MethodGet, "/api/admin/session", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: outcome.Session.Token})
	session, err = auth.Authenticate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.Username != auth.Username() {
		t.Fatalf("session = %+v", session)
	}

	// CSRF:令牌一致才通过。
	if auth.VerifyCSRF(request, session) {
		t.Error("a request without X-CSRF-Token must fail CSRF")
	}
	request.Header.Set("X-CSRF-Token", "wrong")
	if auth.VerifyCSRF(request, session) {
		t.Error("a wrong CSRF token must fail")
	}
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	if !auth.VerifyCSRF(request, session) {
		t.Error("the matching CSRF token must pass")
	}

	// 登出后会话消失。
	if err := auth.Logout(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	session, err = auth.Authenticate(context.Background(), request)
	if err != nil || session != nil {
		t.Fatalf("after logout => (%v, %v), want (nil, nil)", session, err)
	}
	// 没有 Cookie 时登出是 no-op。
	if err := auth.Logout(context.Background(), bare); err != nil {
		t.Errorf("logout without a cookie must be a no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cookie 生成
// ---------------------------------------------------------------------------

func TestAdminAuthCookieAttributes(t *testing.T) {
	t.Parallel()
	issued := &IssuedSession{Token: "tok", CSRFToken: "csrf", ExpiresAt: 0}

	insecure := &AdminAuth{ttl: 3600 * time.Second, secureCookie: false}
	cookie := insecure.SessionCookie(issued)
	for _, marker := range []string{
		SessionCookieName + "=tok", "Path=/", "HttpOnly", "SameSite=Strict", "Max-Age=3600",
	} {
		if !strings.Contains(cookie, marker) {
			t.Errorf("session cookie %q is missing %q", cookie, marker)
		}
	}
	if strings.Contains(cookie, "Secure") {
		t.Error("Secure must be omitted when secure_cookies is false")
	}
	clear := insecure.ClearCookie()
	if !strings.Contains(clear, SessionCookieName+"=;") || !strings.Contains(clear, "Max-Age=0") {
		t.Errorf("clear cookie = %q", clear)
	}

	secure := &AdminAuth{ttl: 60 * time.Second, secureCookie: true}
	if !strings.HasSuffix(secure.SessionCookie(issued), "; Secure") {
		t.Errorf("session cookie = %q, want a Secure suffix", secure.SessionCookie(issued))
	}
	if !strings.HasSuffix(secure.ClearCookie(), "; Secure") {
		t.Errorf("clear cookie = %q, want a Secure suffix", secure.ClearCookie())
	}
}
