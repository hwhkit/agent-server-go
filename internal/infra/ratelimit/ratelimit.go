// Package ratelimit 提供基于 PostgreSQL 令牌桶的全局限流。
//
// 双维度限流：IP + 用户（可独立配置速率）。
// 令牌桶状态持久化在 rate_limit_buckets 表中，多副本 Core 实例共享
// 同一份限流计数（不需要 Redis——复用现有 PG，降低部署复杂度）。
//
// 桶的补充是惰性计算的：读取时根据上次时间差补充令牌，
// 不需要后台协程持续刷新。
package ratelimit

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Config 是限流策略配置。
type Config struct {
	// Enabled 为 false 时中间件直通（不影响请求）。
	Enabled bool
	// IPPerMinute 是每 IP 每分钟请求数上限（0 = 不限）。
	IPPerMinute int
	// UserPerMinute 是每用户每分钟请求数上限（0 = 不限）。
	UserPerMinute int
	// Burst 是令牌桶容量（一次性可消耗的令牌数）。
	Burst int
	// KeyPrefix 用于区分不同的限流规则（如 "api" / "admin" / "dynamic"）。
	KeyPrefix string
}

// Defaults 返回生产默认配置。
func Defaults() Config {
	return Config{
		Enabled:       true,
		IPPerMinute:   60,
		UserPerMinute: 30,
		Burst:         10,
	}
}

// Store 是限流桶的持久化存储。
type Store struct {
	db *pgxpool.Pool
}

// NewStore 创建限流存储并确保表结构存在。
func NewStore(ctx context.Context, db *pgxpool.Pool) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS rate_limit_buckets (
			bucket_key TEXT PRIMARY KEY,
			tokens     DOUBLE PRECISION NOT NULL,
			updated_ns BIGINT NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("create rate_limit_buckets: %w", err)
	}
	return s, nil
}

// Take 尝试从令牌桶取一个令牌。返回 true 如果允许。
func (s *Store) Take(ctx context.Context, key string, capacity int, perMinute int) (bool, error) {
	if capacity <= 0 || perMinute <= 0 {
		return true, nil // 不限流
	}
	now := time.Now().UnixNano()
	refillRate := float64(perMinute) / 60.0 // tokens per second

	// Upsert + 惰性补充：单条 SQL 原子完成。
	// capacity 是桶容量；tokens 补充到不超过 capacity。
	// 注意：SQL 必须引用全部绑定参数（曾有未使用的 $2 导致 PG Prepare
	// 阶段 "could not determine data type" 报错）。
	var allowed bool
	err := s.db.QueryRow(ctx, `
		WITH existing AS (
			SELECT tokens, updated_ns FROM rate_limit_buckets WHERE bucket_key = $1
		), updated AS (
			SELECT
				LEAST(
					$2::double precision,
					COALESCE(
						(SELECT tokens FROM existing), $2::double precision
					) + ($4::double precision * (($3::bigint - COALESCE((SELECT updated_ns FROM existing), $3::bigint)) / 1000000000.0))
				) AS new_tokens
		)
		INSERT INTO rate_limit_buckets (bucket_key, tokens, updated_ns)
		VALUES ($1,
			GREATEST(0, (SELECT new_tokens FROM updated) - 1),
			$3
		)
		ON CONFLICT (bucket_key) DO UPDATE SET
			tokens = GREATEST(0, (SELECT new_tokens FROM updated) - 1),
			updated_ns = $3
		RETURNING (SELECT new_tokens FROM updated) >= 1
	`, key, capacity, now, refillRate).Scan(&allowed)
	if err != nil {
		// 存储故障必须真·fail-open：查询失败时放行并上抛，由中间件记日志。
		// （旧实现吞掉错误返回 (false, nil)，实际是 fail-closed 全量 429。）
		return true, err
	}
	return allowed, nil
}

// Middleware 创建限流中间件。
func Middleware(store *Store, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !cfg.Enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			allowed := true

			// IP 维度
			if cfg.IPPerMinute > 0 {
				ip := clientIP(r)
				ipAllowed, err := store.Take(ctx,
					fmt.Sprintf("%s:ip:%s", cfg.KeyPrefix, ip),
					cfg.Burst, cfg.IPPerMinute)
				if err != nil {
					slog.Warn("rate limit: take failed (fail-open)", "err", err)
				} else if !ipAllowed {
					tooManyRequests(w, "ip")
					return
				}
			}

			// 用户维度（从 session 中提取；无 session 则跳过）
			if cfg.UserPerMinute > 0 {
				username := UsernameFromContext(ctx)
				if username != "" {
					userAllowed, err := store.Take(ctx,
						fmt.Sprintf("%s:user:%s", cfg.KeyPrefix, username),
						cfg.Burst, cfg.UserPerMinute)
					if err != nil {
						slog.Warn("rate limit: take failed (fail-open)", "err", err)
					} else if !userAllowed {
						tooManyRequests(w, "user")
						return
					}
				}
			}

			_ = allowed
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP 提取客户端 IP（尊重可信代理头）。
func clientIP(r *http.Request) string {
	// X-Forwarded-For 由反向代理设置；仅信任第一个（最原始的客户端）。
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooManyRequests 写 429 响应。
func tooManyRequests(w http.ResponseWriter, dimension string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(w, `{"error":{"code":"rate_limited","message":"Too many requests (%s dimension). Retry after 60 seconds."}}`, dimension)
}

// UsernameFromContext 从请求上下文提取已认证用户名。
// 依赖 admin 中间件已设置的 session 信息。
type usernameKey struct{}

// WithUsername 将用户名注入请求上下文。
func WithUsername(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, usernameKey{}, username)
}

// UsernameFromContext 提取用户名。
func UsernameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(usernameKey{}).(string)
	return v
}

// CleanupStale 清理超过 1 小时未使用的桶（由 retention 循环调用）。
func (s *Store) CleanupStale(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM rate_limit_buckets WHERE updated_ns < $1`,
		time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

var _ = chi.Router(nil) // 确保引入 chi 依赖
