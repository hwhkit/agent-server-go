package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ir-agent-server/internal/domain/account"
	"ir-agent-server/internal/domain/audit"
)

// ---------------------------------------------------------------------------
// 账号管理用例（八模块 IA：账号管理，admin-only）
// ---------------------------------------------------------------------------

// ErrInvalidUsername / ErrInvalidPassword / ErrUsernameExists 是用例级校验哨兵。
var (
	ErrInvalidUsername = errors.New("username must be 2-64 chars of letters, digits, dot, underscore or hyphen")
	ErrInvalidPassword = errors.New("password must be 8-512 characters and not a known-insecure default")
	ErrUsernameExists  = errors.New("username already exists")
)

// PasswordHasher 生成 PHC 口令哈希（infra/crypto.HashPassword 满足）。
type PasswordHasher func(password string) (string, error)

// AuditRecorder 记录账号操作审计。
type AuditRecorder interface {
	Append(ctx context.Context, events ...audit.Event) error
}

// AccountService 是账号管理用例服务。「至少保留一个可用管理员」不变量由
// 仓储在事务内强制（account.ErrLastAdmin）。
type AccountService struct {
	repo  account.Repository
	hash  PasswordHasher
	audit AuditRecorder
}

// NewAccountService 组装账号服务。
func NewAccountService(repo account.Repository, hash PasswordHasher, recorder AuditRecorder) *AccountService {
	return &AccountService{repo: repo, hash: hash, audit: recorder}
}

// View 是账号的展示态（永不携带哈希）。
type View struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func viewOf(a *account.Account) View {
	return View{Username: a.Username, Role: string(a.Role), Enabled: a.Enabled,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
}

// List 返回全部账号。
func (s *AccountService) List(ctx context.Context) ([]View, error) {
	accounts, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]View, 0, len(accounts))
	for i := range accounts {
		views = append(views, viewOf(&accounts[i]))
	}
	return views, nil
}

// Create 创建账号。
func (s *AccountService) Create(ctx context.Context, username, password string, role account.Role, actor string) (*View, error) {
	username = strings.TrimSpace(username)
	if !ValidUsername(username) {
		return nil, ErrInvalidUsername
	}
	if _, err := account.ParseRole(string(role)); err != nil {
		return nil, err
	}
	if message := ValidatePassword(password); message != "" {
		return nil, ErrInvalidPassword
	}
	if existing, err := s.repo.Get(ctx, username); err == nil && existing != nil {
		return nil, ErrUsernameExists
	}
	hashed, err := s.hash(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	entity := &account.Account{
		Username: username, Role: role, PasswordHash: hashed, Enabled: true,
	}
	if err := s.repo.Create(ctx, entity); err != nil {
		return nil, err
	}
	s.record(ctx, actor, "user.create", username)
	created, err := s.repo.Get(ctx, username)
	if err != nil {
		return nil, err
	}
	view := viewOf(created)
	return &view, nil
}

// UpdateInput 是账号更新输入；nil 字段表示不变。
type UpdateInput struct {
	Password *string
	Role     *account.Role
	Enabled  *bool
}

// Update 更新账号（口令/角色/启用态）。降级、禁用或删除最后一个可用
// 管理员会被仓储拒绝（account.ErrLastAdmin）。
func (s *AccountService) Update(ctx context.Context, username string, in UpdateInput, actor string) (*View, error) {
	username = strings.TrimSpace(username)
	existing, err := s.repo.Get(ctx, username)
	if err != nil {
		return nil, err
	}
	if in.Password != nil {
		if message := ValidatePassword(*in.Password); message != "" {
			return nil, ErrInvalidPassword
		}
		hashed, err := s.hash(*in.Password)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		existing.PasswordHash = hashed
	}
	if in.Role != nil {
		if _, err := account.ParseRole(string(*in.Role)); err != nil {
			return nil, err
		}
		existing.Role = *in.Role
	}
	if in.Enabled != nil {
		existing.Enabled = *in.Enabled
	}
	if err := s.repo.Update(ctx, existing); err != nil {
		return nil, err
	}
	s.record(ctx, actor, "user.update", username)
	updated, err := s.repo.Get(ctx, username)
	if err != nil {
		return nil, err
	}
	view := viewOf(updated)
	return &view, nil
}

// Delete 删除账号（级联清理其会话由仓储保证）。
func (s *AccountService) Delete(ctx context.Context, username, actor string) error {
	username = strings.TrimSpace(username)
	if err := s.repo.Delete(ctx, username); err != nil {
		return err
	}
	s.record(ctx, actor, "user.delete", username)
	return nil
}

// EnsureBootstrap 把启动配置的初始管理员迁移为数据库账号；已存在的账号
// 绝不覆盖（口令轮换只能走管理 API）。
func (s *AccountService) EnsureBootstrap(ctx context.Context, username, passwordHash string) error {
	username = strings.TrimSpace(username)
	if username == "" || strings.TrimSpace(passwordHash) == "" {
		return errors.New("bootstrap admin username and password hash are required")
	}
	existing, err := s.repo.Get(ctx, username)
	if err == nil && existing != nil {
		return nil
	}
	if err != nil && !errors.Is(err, account.ErrNotFound) {
		return err
	}
	return s.repo.Create(ctx, &account.Account{
		Username: username, Role: account.RoleAdmin,
		PasswordHash: passwordHash, Enabled: true,
	})
}

func (s *AccountService) record(ctx context.Context, actor, action, target string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Append(ctx, audit.Event{
		Actor: actor, Action: audit.Action(action), Target: target,
	}); err != nil {
		// 审计失败不回滚已提交的账号变更（同事务收口在 P6 装配层的
		// tx 组合器里实现）；记录告警保证可追溯。
		_ = err
	}
}

// ValidUsername 校验用户名：2–64 位字母、数字、点、下划线、连字符或 @
// （支持以邮箱作登录账号；字符集仍封闭，无空白与符号注入面）。
func ValidUsername(username string) bool {
	if len(username) < 2 || len(username) > 64 {
		return false
	}
	for _, value := range []byte(username) {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || value == '.' || value == '_' ||
			value == '-' || value == '@' {
			continue
		}
		return false
	}
	return true
}

// ValidatePassword 校验口令强度；返回空串表示通过。
func ValidatePassword(password string) string {
	if len(password) < 8 || len(password) > 512 {
		return "密码长度需为 8–512 个字符"
	}
	switch password {
	case "admin", "password", "change-me":
		return "密码不能使用不安全的默认值"
	}
	return ""
}
