package mota

// 管理台会话鉴权:argon2 口令校验、登录并发闸与失败退避、会话 Cookie。
// 对应 Rust 侧 src/platform/auth.rs。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// SessionCookieName 是管理台会话 Cookie 名。
const SessionCookieName = "mota_admin_session"

// loginSlotCount 是同时进行的口令校验上限。
//
// argon2 是刻意设计成 CPU/内存密集的,不设上限时几十个并发登录就能把服务打满。
// 超过上限直接判限流,而不是排队 —— 排队只会让攻击者拖住更多资源。
const loginSlotCount = 4

// loginFailureGrace 是开始退避前允许的失败次数。
const loginFailureGrace = 3

// loginBackoffMaxExponent 把退避封顶在 2^5 = 32 秒。
const loginBackoffMaxExponent = 5

// ---------------------------------------------------------------------------
// argon2 口令哈希(PHC 字符串格式)
// ---------------------------------------------------------------------------

// argon2Params 是 Rust argon2 crate `Argon2::default()` 的参数,必须逐项一致 ——
// 否则一侧生成的 password_hash 在另一侧验不过。
const (
	argon2DefaultMemory  uint32 = 19456 // KiB
	argon2DefaultTime    uint32 = 2
	argon2DefaultThreads uint8  = 1
	argon2DefaultKeyLen  uint32 = 32
	argon2SaltLen               = 16
	argon2Version               = 19
)

// phcBase64 是 PHC 规定的 base64:标准字母表、**不带填充**。
var phcBase64 = base64.StdEncoding.WithPadding(base64.NoPadding)

// argon2Hash 是解析后的 PHC 口令哈希。
type argon2Hash struct {
	// variant 是 argon2id / argon2i。argon2d 不受支持(x/crypto 没有实现)。
	variant string
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	digest  []byte
}

// HashAdminPassword 用 argon2id 默认参数生成 PHC 口令哈希。
//
// 输出形如 `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`,与 Rust 侧
// `Argon2::default().hash_password(...)` 的输出可互相校验。
func HashAdminPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate argon2 salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt,
		argon2DefaultTime, argon2DefaultMemory, argon2DefaultThreads, argon2DefaultKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, argon2DefaultMemory, argon2DefaultTime, argon2DefaultThreads,
		phcBase64.EncodeToString(salt), phcBase64.EncodeToString(digest)), nil
}

// parseArgon2Hash 解析 PHC 口令哈希。
//
// 参数必须从存储的哈希里读出来(而不是用当前默认值),否则换过参数的老哈希就验不过了。
func parseArgon2Hash(encoded string) (*argon2Hash, error) {
	if len(encoded) > 1024 {
		return nil, errors.New("argon2 hash exceeds the supported length")
	}
	// 形如 ["", "argon2id", "v=19", "m=19456,t=2,p=1", salt, hash]
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return nil, errors.New("argon2 hash must have the $argon2id$v=..$m=..$salt$hash shape")
	}
	variant := parts[1]
	switch variant {
	case "argon2id", "argon2i":
	case "argon2d":
		return nil, errors.New("argon2d is not supported")
	default:
		return nil, fmt.Errorf("unsupported argon2 variant %q", variant)
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, fmt.Errorf("parse argon2 version: %w", err)
	}
	if version != argon2Version {
		return nil, fmt.Errorf("unsupported argon2 version %d", version)
	}
	parsed := &argon2Hash{variant: variant}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&parsed.memory, &parsed.time, &parsed.threads); err != nil {
		return nil, fmt.Errorf("parse argon2 parameters: %w", err)
	}
	if parsed.memory == 0 || parsed.time == 0 || parsed.threads == 0 {
		return nil, errors.New("argon2 parameters must be positive")
	}
	if parsed.memory > 256*1024 || parsed.time > 10 || parsed.threads > 16 {
		return nil, errors.New("argon2 parameters exceed the supported resource limits")
	}
	salt, err := phcBase64.DecodeString(parts[4])
	if err != nil {
		return nil, fmt.Errorf("decode argon2 salt: %w", err)
	}
	digest, err := phcBase64.DecodeString(parts[5])
	if err != nil {
		return nil, fmt.Errorf("decode argon2 hash: %w", err)
	}
	if len(salt) < 8 || len(salt) > 64 || len(digest) < 16 || len(digest) > 64 {
		return nil, errors.New("argon2 salt or hash length is outside the supported range")
	}
	parsed.salt = salt
	parsed.digest = digest
	return parsed, nil
}

// argon2IDKey / argon2IKey 是 x/crypto 的薄封装,集中一处便于测试复用同一实现。
func argon2IDKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	return argon2.IDKey(password, salt, time, memory, threads, keyLen)
}

func argon2IKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	return argon2.Key(password, salt, time, memory, threads, keyLen)
}

// VerifyAdminPassword 用常数时间比较校验口令。哈希无法解析时返回 false。
func VerifyAdminPassword(encoded, password string) bool {
	parsed, err := parseArgon2Hash(encoded)
	if err != nil {
		return false
	}
	keyLen := uint32(len(parsed.digest))
	var candidate []byte
	switch parsed.variant {
	case "argon2id":
		candidate = argon2IDKey([]byte(password), parsed.salt,
			parsed.time, parsed.memory, parsed.threads, keyLen)
	case "argon2i":
		candidate = argon2IKey([]byte(password), parsed.salt,
			parsed.time, parsed.memory, parsed.threads, keyLen)
	default:
		return false
	}
	return subtle.ConstantTimeCompare(candidate, parsed.digest) == 1
}

// ---------------------------------------------------------------------------
// AdminAuth
// ---------------------------------------------------------------------------

// AdminAuth 承载管理台鉴权状态。对应 rs AdminAuth。
type AdminAuth struct {
	dummyPasswordHash string
	bootstrapUsername string
	ttl               time.Duration
	secureCookie      bool
	store             *AgentStore

	// loginSlots 限制并发口令校验数(argon2 是 CPU/内存密集的)。
	loginSlots *semaphore
	// throttleMu 保护「规范化账号+直接来源」失败桶。组合维度避免一个匿名
	// 来源锁死所有账号，也避免其他账号的成功登录清空目标账号的退避。
	throttleMu sync.Mutex
	throttles  map[string]*loginThrottleState
}

type loginThrottleState struct {
	failures     uint32
	blockedUntil time.Time
	lastSeen     time.Time
}

const maxLoginThrottleBuckets = 4096

// LoginOutcomeKind 是登录结果种类。
type LoginOutcomeKind int

const (
	// LoginAuthenticated 校验通过并已签发会话。
	LoginAuthenticated LoginOutcomeKind = iota
	// LoginInvalid 用户名或口令不正确。
	LoginInvalid
	// LoginRateLimited 触发并发闸或失败退避。
	LoginRateLimited
)

// LoginOutcome 是一次登录尝试的结果。
type LoginOutcome struct {
	Kind LoginOutcomeKind
	// Session 仅在 LoginAuthenticated 时非 nil。
	Session *IssuedSession
	// RetryAfterSecs 仅在 LoginRateLimited 时有意义(至少 1)。
	RetryAfterSecs int64
}

// IssuedSession 是新签发的管理会话。
type IssuedSession struct {
	Token     string
	Username  string
	Role      UserRole
	CSRFToken string
	ExpiresAt int64
}

// NewAdminAuth 组装管理台鉴权。
//
// 优先使用配置的 password_hash;只配了明文口令时启动期哈希一次。两者都没有即失败 ——
// 管理台不允许无凭证运行。
func NewAdminAuth(config *PlatformConfig, store *AgentStore) (*AdminAuth, error) {
	var passwordHash string
	switch {
	case config.AdminPasswordHash != "":
		if _, err := parseArgon2Hash(config.AdminPasswordHash); err != nil {
			return nil, fmt.Errorf("parse [admin].password_hash: %w", err)
		}
		passwordHash = config.AdminPasswordHash
	case config.AdminPassword != "":
		hashed, err := HashAdminPassword(config.AdminPassword)
		if err != nil {
			return nil, fmt.Errorf("hash admin password: %w", err)
		}
		passwordHash = hashed
	default:
		return nil, errors.New("admin password is not configured")
	}
	if err := store.EnsureBootstrapAdmin(context.Background(), config.AdminUsername, passwordHash); err != nil {
		return nil, fmt.Errorf("ensure bootstrap admin: %w", err)
	}
	return &AdminAuth{
		dummyPasswordHash: passwordHash,
		bootstrapUsername: config.AdminUsername,
		ttl:               config.AdminSessionTTL,
		secureCookie:      config.SecureCookies,
		store:             store,
		loginSlots:        newSemaphore(loginSlotCount),
		throttles:         make(map[string]*loginThrottleState),
	}, nil
}

// Username 返回启动配置迁移出的初始管理员用户名；登录认证本身支持数据库中的全部账号。
func (a *AdminAuth) Username() string { return a.bootstrapUsername }

// Login 校验凭证并签发会话。
//
// 两道限流:并发闸(非阻塞,满了直接限流)与失败退避(连续失败 >= 3 次后
// 按 2^(failures-3) 秒封禁,封顶 32 秒)。成功登录会把两者都清零。
func (a *AdminAuth) Login(ctx context.Context, username, password string) (*LoginOutcome, error) {
	return a.LoginFromSource(ctx, username, password, "local")
}

// LoginFromSource applies independent throttles to the normalized account and
// network source. source must come from the direct peer, not an untrusted
// forwarded header.
func (a *AdminAuth) LoginFromSource(
	ctx context.Context, username, password, source string,
) (*LoginOutcome, error) {
	if !a.loginSlots.TryAcquire() {
		return &LoginOutcome{Kind: LoginRateLimited, RetryAfterSecs: 1}, nil
	}
	// 许可要一直持有到 argon2 算完 —— 请求提前断开不能让容量提前释放。
	defer a.loginSlots.Release()

	username = strings.TrimSpace(username)
	source = normalizeLoginSource(source)
	if retryAfter, blocked := a.blockedForKeys(loginThrottleKeys(username, source)...); blocked {
		return &LoginOutcome{Kind: LoginRateLimited, RetryAfterSecs: retryAfter}, nil
	}

	account, err := a.store.GetUserAuth(ctx, username)
	if err != nil {
		return nil, err
	}
	hash := a.dummyPasswordHash
	enabled := false
	if account != nil {
		hash = account.PasswordHash
		enabled = account.Enabled
	}
	// 不存在的用户名也校验 dummy hash，避免账号枚举产生明显时序差异。
	if !VerifyAdminPassword(hash, password) || !enabled {
		a.recordFailureFor(username, source)
		return &LoginOutcome{Kind: LoginInvalid}, nil
	}
	// Clear only this normalized account+source bucket; another account's
	// success can never reset the target account's backoff.
	a.clearFailureBucket(accountSourceThrottleKey(username, source))

	token := randomToken(32)
	csrfToken := randomToken(24)
	expiresAt := time.Now().Unix() + int64(a.ttl.Seconds())
	if account == nil {
		return nil, errors.New("authenticated account disappeared")
	}
	if err := a.store.CreateSession(ctx, tokenHash(token), account.Username, csrfToken, expiresAt); err != nil {
		return nil, err
	}
	return &LoginOutcome{
		Kind:    LoginAuthenticated,
		Session: &IssuedSession{Token: token, Username: account.Username, Role: account.Role, CSRFToken: csrfToken, ExpiresAt: expiresAt},
	}, nil
}

func normalizeLoginSource(source string) string {
	source = strings.TrimSpace(strings.ToLower(source))
	if source == "" {
		return "unknown"
	}
	if len(source) > 128 {
		return source[:128]
	}
	return source
}

func accountSourceThrottleKey(username, source string) string {
	return "account:" + strings.ToLower(strings.TrimSpace(username)) + "\x00" + normalizeLoginSource(source)
}

func loginThrottleKeys(username, source string) []string {
	return []string{accountSourceThrottleKey(username, source)}
}

// blockedFor 报告当前是否处于退避封禁中,以及还要等几秒(至少 1)。
func (a *AdminAuth) blockedFor() (int64, bool) {
	return a.blockedForKeys(accountSourceThrottleKey(a.bootstrapUsername, "local"))
}

func (a *AdminAuth) blockedForKeys(keys ...string) (int64, bool) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	var retryAfter int64
	for _, key := range keys {
		state := a.throttles[key]
		if state == nil {
			continue
		}
		remaining := time.Until(state.blockedUntil)
		if remaining <= 0 {
			continue
		}
		seconds := int64(remaining.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		if seconds > retryAfter {
			retryAfter = seconds
		}
	}
	return retryAfter, retryAfter > 0
}

// recordFailure 累加失败次数并在超过宽限后设置封禁。
func (a *AdminAuth) recordFailure() {
	a.recordFailureFor(a.bootstrapUsername, "local")
}

func (a *AdminAuth) recordFailureFor(username, source string) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	now := time.Now()
	a.pruneThrottleBucketsLocked(now)
	for _, key := range loginThrottleKeys(username, source) {
		state := a.throttles[key]
		if state == nil {
			state = &loginThrottleState{}
			a.throttles[key] = state
		}
		state.lastSeen = now
		if state.failures < ^uint32(0) {
			state.failures++
		}
		if state.failures < loginFailureGrace {
			continue
		}
		exponent := state.failures - loginFailureGrace
		if exponent > loginBackoffMaxExponent {
			exponent = loginBackoffMaxExponent
		}
		state.blockedUntil = now.Add(time.Duration(uint64(1)<<exponent) * time.Second)
	}
}

// clearFailures 清空退避状态。
func (a *AdminAuth) clearFailures() {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	a.throttles = make(map[string]*loginThrottleState)
}

func (a *AdminAuth) clearFailureBucket(key string) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	delete(a.throttles, key)
}

func (a *AdminAuth) pruneThrottleBucketsLocked(now time.Time) {
	if a.throttles == nil {
		a.throttles = make(map[string]*loginThrottleState)
	}
	for key, state := range a.throttles {
		if now.Sub(state.lastSeen) > 10*time.Minute && now.After(state.blockedUntil) {
			delete(a.throttles, key)
		}
	}
	for len(a.throttles) >= maxLoginThrottleBuckets {
		var oldestKey string
		var oldest time.Time
		for key, state := range a.throttles {
			if oldestKey == "" || state.lastSeen.Before(oldest) {
				oldestKey, oldest = key, state.lastSeen
			}
		}
		if oldestKey == "" {
			break
		}
		delete(a.throttles, oldestKey)
	}
}

// Authenticate 从请求 Cookie 里取出会话;没有 Cookie 时返回 (nil, nil)。
func (a *AdminAuth) Authenticate(ctx context.Context, r *http.Request) (*SessionRecord, error) {
	token, ok := sessionCookieValue(r)
	if !ok {
		return nil, nil
	}
	return a.store.GetSession(ctx, tokenHash(token))
}

// Logout 删除当前会话记录。没有 Cookie 时是 no-op。
func (a *AdminAuth) Logout(ctx context.Context, r *http.Request) error {
	token, ok := sessionCookieValue(r)
	if !ok {
		return nil
	}
	return a.store.DeleteSession(ctx, tokenHash(token))
}

// VerifyCSRF 比对 X-CSRF-Token 与会话记录里的令牌。
//
// 比较的是两侧的哈希而不是原文 —— 长度不同的输入也能走等长常数时间比较。
func (a *AdminAuth) VerifyCSRF(r *http.Request, session *SessionRecord) bool {
	provided := r.Header.Get("X-CSRF-Token")
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(tokenHash(provided)), []byte(tokenHash(session.CSRFToken))) == 1
}

// SessionCookie 生成签发会话用的 Set-Cookie 值。
func (a *AdminAuth) SessionCookie(issued *IssuedSession) string {
	cookie := fmt.Sprintf("%s=%s; Path=/; HttpOnly; SameSite=Strict; Max-Age=%d",
		SessionCookieName, issued.Token, int64(a.ttl.Seconds()))
	if a.secureCookie {
		cookie += "; Secure"
	}
	return cookie
}

// ClearCookie 生成清除会话用的 Set-Cookie 值。
func (a *AdminAuth) ClearCookie() string {
	cookie := fmt.Sprintf("%s=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0", SessionCookieName)
	if a.secureCookie {
		cookie += "; Secure"
	}
	return cookie
}

// sessionCookieValue 从请求里取会话 Cookie。
func sessionCookieValue(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}
