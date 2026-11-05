// Package admin 实现管理台鉴权用例：argon2 口令校验的并发闸与失败退避、
// 会话签发/查验/登出、CSRF 哈希比对。
//
// Cookie/Header 的 HTTP 拆解在 api 层；口令哈希与随机令牌的密码学实现在
// infra/crypto，经端口注入（app 层不依赖 infra）。
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"ir-agent-server/internal/domain/account"
)

// 登录限流参数（与旧栈一致）。
const (
	// loginSlotCount 是同时进行的口令校验上限。argon2 是刻意设计成
	// CPU/内存密集的，不设上限时几十个并发登录就能把服务打满。超过上限
	// 直接判限流而不是排队——排队只会让攻击者拖住更多资源。
	loginSlotCount = 4
	// loginFailureGrace 是开始退避前允许的失败次数。
	loginFailureGrace = 3
	// loginBackoffMaxExponent 把退避封顶在 2^5 = 32 秒。
	loginBackoffMaxExponent = 5
	// maxLoginThrottleBuckets 限制退避桶总量（防内存放大）。
	maxLoginThrottleBuckets = 4096
)

// AccountLookup 是登录用的账号查询端口：账号不存在返回 (nil, nil)。
type AccountLookup interface {
	Lookup(ctx context.Context, username string) (*account.Account, error)
}

// SessionStore 是会话存储端口（infra/postgres 的 SessionStore 适配）。
type SessionStore interface {
	Put(ctx context.Context, session account.Session) error
	// Get 返回有效会话；过期/禁用/不存在统一返回 account.ErrNotFound。
	Get(ctx context.Context, tokenHash string) (*account.Session, error)
	Delete(ctx context.Context, tokenHash string) error
}

// PasswordVerifier 校验 PHC 口令哈希（infra/crypto.VerifyPassword 满足）。
type PasswordVerifier func(encoded, password string) bool

// SessionUser 是一个已认证会话的视图。
type SessionUser struct {
	Username  string
	Role      account.Role
	CSRFToken string
}

// IssuedSession 是新签发的管理会话。Token 明文只在签发响应出现一次。
type IssuedSession struct {
	Token     string
	Username  string
	Role      account.Role
	CSRFToken string
	ExpiresAt int64
}

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
	Kind    LoginOutcomeKind
	Session *IssuedSession
	// RetryAfterSecs 仅在 LoginRateLimited 时有意义（至少 1）。
	RetryAfterSecs int64
}

// Config 是鉴权服务配置。
type Config struct {
	// DummyPasswordHash 是启动配置的管理员口令哈希：不存在的用户名也
	// 校验它，避免账号枚举产生明显时序差异。装配层负责从
	// [admin].password_hash / password 生成并校验格式。
	DummyPasswordHash string
	// SessionTTL 是会话有效期。
	SessionTTL time.Duration
	// Verify 是口令哈希校验函数（注入 infra/crypto.VerifyPassword）。
	Verify PasswordVerifier
}

// Auth 是管理台鉴权服务。并发安全。
type Auth struct {
	dummyPasswordHash string
	ttl               time.Duration
	verify            PasswordVerifier
	accounts          AccountLookup
	sessions          SessionStore

	// loginSlots 限制并发口令校验数。
	loginSlots chan struct{}
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

// NewAuth 组装鉴权服务。
func NewAuth(config Config, accounts AccountLookup, sessions SessionStore) (*Auth, error) {
	if strings.TrimSpace(config.DummyPasswordHash) == "" {
		return nil, errors.New("admin password hash is required")
	}
	if config.Verify == nil {
		return nil, errors.New("password verifier is required")
	}
	if config.SessionTTL <= 0 {
		return nil, errors.New("session ttl must be positive")
	}
	return &Auth{
		dummyPasswordHash: config.DummyPasswordHash,
		ttl:               config.SessionTTL,
		verify:            config.Verify,
		accounts:          accounts,
		sessions:          sessions,
		loginSlots:        make(chan struct{}, loginSlotCount),
		throttles:         map[string]*loginThrottleState{},
	}, nil
}

// Login 校验凭证并签发会话。
//
// 两道限流：并发闸（非阻塞，满了直接限流）与失败退避（连续失败 ≥ 3 次后
// 按 2^(failures-3) 秒封禁，封顶 32 秒）。成功登录只清空本账号+来源的桶。
// source 必须来自直接对端（或显式受信代理链），不是不可信的转发头。
func (a *Auth) Login(ctx context.Context, username, password, source string) (*LoginOutcome, error) {
	select {
	case a.loginSlots <- struct{}{}:
		defer func() { <-a.loginSlots }()
	default:
		return &LoginOutcome{Kind: LoginRateLimited, RetryAfterSecs: 1}, nil
	}
	// 许可要一直持有到 argon2 算完——请求提前断开不能让容量提前释放。

	username = strings.TrimSpace(username)
	source = normalizeLoginSource(source)
	if retryAfter, blocked := a.blockedFor(accountSourceThrottleKey(username, source)); blocked {
		return &LoginOutcome{Kind: LoginRateLimited, RetryAfterSecs: retryAfter}, nil
	}

	stored, err := a.accounts.Lookup(ctx, username)
	if err != nil {
		return nil, err
	}
	hash := a.dummyPasswordHash
	enabled := false
	if stored != nil {
		hash = stored.PasswordHash
		enabled = stored.Enabled
	}
	// 不存在的用户名也校验 dummy 哈希，避免账号枚举时序侧信道。
	if !a.verify(hash, password) || !enabled {
		a.recordFailure(username, source)
		return &LoginOutcome{Kind: LoginInvalid}, nil
	}
	a.clearFailureBucket(accountSourceThrottleKey(username, source))
	if stored == nil {
		return nil, errors.New("authenticated account disappeared")
	}

	token := randomToken(32)
	csrfToken := randomToken(24)
	expiresAt := time.Now().Unix() + int64(a.ttl.Seconds())
	if err := a.sessions.Put(ctx, account.Session{
		TokenHash: TokenHash(token), Username: stored.Username,
		Role: stored.Role, CSRFToken: csrfToken,
		ExpiresAt: time.Unix(expiresAt, 0).UTC(),
	}); err != nil {
		return nil, err
	}
	return &LoginOutcome{
		Kind: LoginAuthenticated,
		Session: &IssuedSession{
			Token: token, Username: stored.Username, Role: stored.Role,
			CSRFToken: csrfToken, ExpiresAt: expiresAt,
		},
	}, nil
}

// Authenticate 按 Cookie 令牌查验会话；无效/过期/不存在统一返回 (nil, nil)。
func (a *Auth) Authenticate(ctx context.Context, cookieToken string) (*SessionUser, error) {
	if strings.TrimSpace(cookieToken) == "" {
		return nil, nil
	}
	session, err := a.sessions.Get(ctx, TokenHash(cookieToken))
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &SessionUser{Username: session.Username, Role: session.Role,
		CSRFToken: session.CSRFToken}, nil
}

// Logout 删除会话记录。空令牌是 no-op。
func (a *Auth) Logout(ctx context.Context, cookieToken string) error {
	if strings.TrimSpace(cookieToken) == "" {
		return nil
	}
	return a.sessions.Delete(ctx, TokenHash(cookieToken))
}

// VerifyCSRF 比对提供的令牌与会话记录。比较的是两侧哈希而不是原文——
// 长度不同的输入也能走等长常数时间比较。
func (a *Auth) VerifyCSRF(provided string, session *SessionUser) bool {
	if provided == "" || session == nil {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(TokenHash(provided)), []byte(TokenHash(session.CSRFToken))) == 1
}

// ---------------------------------------------------------------------------
// 退避桶
// ---------------------------------------------------------------------------

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

func (a *Auth) blockedFor(key string) (int64, bool) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	state := a.throttles[key]
	if state == nil {
		return 0, false
	}
	remaining := time.Until(state.blockedUntil)
	if remaining <= 0 {
		return 0, false
	}
	seconds := int64(remaining.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return seconds, true
}

func (a *Auth) recordFailure(username, source string) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	now := time.Now()
	a.pruneThrottleBucketsLocked(now)
	key := accountSourceThrottleKey(username, source)
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
		return
	}
	exponent := state.failures - loginFailureGrace
	if exponent > loginBackoffMaxExponent {
		exponent = loginBackoffMaxExponent
	}
	state.blockedUntil = now.Add(time.Duration(uint64(1)<<exponent) * time.Second)
}

func (a *Auth) clearFailureBucket(key string) {
	a.throttleMu.Lock()
	defer a.throttleMu.Unlock()
	delete(a.throttles, key)
}

func (a *Auth) pruneThrottleBucketsLocked(now time.Time) {
	if a.throttles == nil {
		a.throttles = map[string]*loginThrottleState{}
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

// ---------------------------------------------------------------------------
// 令牌工具（与 infra/crypto 同算法；app 层不依赖 infra，故本地实现）
// ---------------------------------------------------------------------------

// TokenHash 返回不透明令牌的小写十六进制 SHA-256。
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(raw)
}
