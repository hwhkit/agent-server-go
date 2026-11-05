// Package connector 定义远端连接器域：令牌、worker、租约与队列语义。
//
// 对应 /agent-api/v1 协议（claim → renew → complete + heartbeat）。
// 交付语义是 at-least-once：租约过期重派与原执行者迟到的 complete 并存，
// 副作用幂等是连接器责任。
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// TokenPrefix 是管理台签发的连接器令牌前缀。服务端只存哈希。
const TokenPrefix = "act_"

// 时序缺省值，与协议文档对齐；服务端可经 heartbeat 回执下发新间隔。
const (
	DefaultLeaseSeconds      = 90
	DefaultRenewAfterSeconds = 30 // = lease/3
	DefaultHeartbeatSeconds  = 60
	MaxHeartbeatSeconds      = 3600
	DefaultClaimWaitSeconds  = 30
	MinClaimWaitSeconds      = 1
	MaxClaimWaitSeconds      = 35
	OfflineAfterMissedBeats  = 3 // 超过 3 个心跳间隔未上报即离线
)

var workerIDPattern = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)

// ValidateWorkerID 校验连接器自报的稳定标识。
func ValidateWorkerID(id string) error {
	if !workerIDPattern.MatchString(strings.TrimSpace(id)) {
		return errors.New("workerId must be 2-64 chars of [a-z0-9-]")
	}
	return nil
}

// Token 是一个连接器令牌记录（不含明文；明文只在签发响应出现一次）。
type Token struct {
	ID   string
	Name string
	// Scopes 是允许认领的智能体 slug 集合；空表示未限定（不推荐）。
	Scopes    []string
	Hash      string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
	// GraceUntil 是轮换宽限期终点：宽限内新旧令牌并行有效。
	GraceUntil *time.Time
}

// Active 报告令牌在 now 时刻是否有效（未吊销、未过期、或在轮换宽限内）。
func (t Token) Active(now time.Time) bool {
	if t.RevokedAt != nil && now.After(t.GraceEnd()) {
		return false
	}
	if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
		return false
	}
	return true
}

// GraceEnd 返回宽限期终点；未设置时为吊销时刻本身。
func (t Token) GraceEnd() time.Time {
	if t.GraceUntil != nil {
		return *t.GraceUntil
	}
	if t.RevokedAt != nil {
		return *t.RevokedAt
	}
	return time.Time{}
}

// InScope 报告令牌是否允许认领指定 slug。
func (t Token) InScope(slug string) bool {
	if len(t.Scopes) == 0 {
		return true
	}
	for _, s := range t.Scopes {
		if s == slug {
			return true
		}
	}
	return false
}

// Worker 是一次心跳注册的连接器实例。
type Worker struct {
	WorkerID   string
	TokenID    string
	Slugs      []string // capabilities.slugs
	Version    string
	InFlight   int
	Capacity   int
	LastBeatAt time.Time
	// BeatIntervalSeconds 是服务端最近一次下发的上报间隔。
	BeatIntervalSeconds int
}

// Offline 报告 worker 是否已超过离线阈值。
func (w Worker) Offline(now time.Time) bool {
	interval := w.BeatIntervalSeconds
	if interval <= 0 {
		interval = DefaultHeartbeatSeconds
	}
	return now.Sub(w.LastBeatAt) > time.Duration(interval*OfflineAfterMissedBeats)*time.Second
}

// Lease 是一个在途租约。
type Lease struct {
	RunID    string
	WorkerID string
	Attempt  int
	ExpireAt time.Time
	// Deadline 是执行墙钟（首次派发时刻 + 智能体 timeout）；此后续租必拒。
	Deadline time.Time
}

// Errors.
var (
	ErrTokenNotFound = errors.New("connector token not found")
	ErrLeaseLost     = errors.New("lease lost")
	ErrDeadline      = errors.New("task deadline exceeded")
)

// TokenRepository 是令牌仓储端口。Token.Hash 由调用方预先填充
// （crypto.TokenHash(plaintext)）；明文只在签发响应出现一次，不落库。
type TokenRepository interface {
	// Issue 按名字 upsert：同名再签发即令牌轮换（旧哈希立即失效，
	// status 回到 active、宽限清空），与旧栈 IssueConnector 语义一致。
	Issue(ctx context.Context, t *Token) error
	GetByHash(ctx context.Context, hash string) (*Token, error)
	// GetByName 供管理台按名字操作。
	GetByName(ctx context.Context, name string) (*Token, error)
	List(ctx context.Context) ([]Token, error)
	// RevokeByName 立即吊销（无宽限）；名字不存在返回 ErrTokenNotFound。
	RevokeByName(ctx context.Context, name string) error
	// Revoke 按 ID 吊销，可附轮换宽限期终点。
	Revoke(ctx context.Context, id string, graceUntil *time.Time) error
}

// WorkerRegistry 是 worker 心跳注册端口。
type WorkerRegistry interface {
	Beat(ctx context.Context, w Worker) error
	ListOnline(ctx context.Context) ([]Worker, error)
	MarkOfflineStale(ctx context.Context, now time.Time) (int, error)
}

// HashToken 返回连接器令牌的小写十六进制 SHA-256。服务端只存哈希，
// 明文仅在签发响应出现一次。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
