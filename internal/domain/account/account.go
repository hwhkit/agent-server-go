// Package account 定义账号与会话域。
package account

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Role 是账号角色。
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// ParseRole 严格解析角色。
func ParseRole(v string) (Role, error) {
	switch Role(strings.TrimSpace(v)) {
	case RoleAdmin, RoleUser:
		return Role(v), nil
	}
	return "", errors.New("role must be admin or user")
}

// Account 是管理台账号。密码使用 Argon2id 哈希，明文不落库不回显。
type Account struct {
	Username     string
	Role         Role
	PasswordHash string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session 是一个已认证会话（服务端存储哈希，Cookie 值为不透明令牌）。
type Session struct {
	TokenHash string
	// CSRFToken 是与会话绑定的 CSRF 令牌（写操作要求头部回带）。
	CSRFToken string
	Username  string
	Role      Role
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Errors.
var (
	ErrNotFound        = errors.New("account not found")
	ErrLastAdmin       = errors.New("system must retain at least one enabled admin")
	ErrInvalidPassword = errors.New("invalid credentials")
)

// Repository 是账号仓储端口。Delete/Disable 必须维持「至少一个可用
// 管理员」不变量，违反时返回 ErrLastAdmin。
type Repository interface {
	Get(ctx context.Context, username string) (*Account, error)
	List(ctx context.Context) ([]Account, error)
	Create(ctx context.Context, a *Account) error
	Update(ctx context.Context, a *Account) error
	Delete(ctx context.Context, username string) error
}

// SessionStore 是会话存储端口。
type SessionStore interface {
	Put(ctx context.Context, s Session) error
	Get(ctx context.Context, tokenHash string) (*Session, error)
	Delete(ctx context.Context, tokenHash string) error
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}
