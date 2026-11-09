package team

// 团队分享（公开子页面）：把组建好的团队以不可枚举链接分享出去，
// 访客在 /share/{token} 页面触发运行并查看交易信号输出。
//
// 安全口径：token 是唯一凭证（32 字节密码学随机，URL-safe），不枚举、
// 不携带任何身份信息；分享视图只暴露岗位标签与流程骨架，永不暴露
// agent slug、系统提示词或执行配置；撤销即时生效（活跃分享唯一索引）。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"
)

// ShareTokenPrefix 是分享令牌的固定前缀（便于日志 grep 与防误当 slug）。
const ShareTokenPrefix = "sh_"

// ErrShareNotFound 是分享未命中/已撤销哨兵（对外语义相同：链接无效）。
var ErrShareNotFound = errors.New("share not found")

// TeamShare 是一个团队分享链接。一个团队同一时刻至多一个活跃分享
// （存储层 partial unique 保证）；「重新生成」= 撤销旧令牌 + 签发新令牌。
type TeamShare struct {
	Token     string
	TeamID    string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Active 报告分享是否仍有效。
func (s *TeamShare) Active() bool { return s != nil && s.RevokedAt == nil }

// NewShareToken 生成 `sh_` + 32 字节密码学随机的 URL-safe 令牌。
func NewShareToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return ShareTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// ShareRepository 是分享存储端口。Create 即轮换（事务内撤销该团队既有
// 活跃分享后插入新令牌）——调用方不需要两步。
type ShareRepository interface {
	// Create 为 teamID 签发新分享（撤销旧活跃分享），返回带定稿时间戳的分享。
	Create(ctx context.Context, teamID string) (*TeamShare, error)
	// Resolve 按 token 取活跃分享；未命中或已撤销返回 ErrShareNotFound。
	Resolve(ctx context.Context, token string) (*TeamShare, error)
	// Revoke 撤销 teamID 的活跃分享（无活跃分享是 no-op）。
	Revoke(ctx context.Context, teamID string) error
}
