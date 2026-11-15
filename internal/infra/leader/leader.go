// Package leader implements PostgreSQL advisory-lock based leader election.
//
// Multi-replica Core instances all serve API traffic, but background loops
// (reaper, retention, team engine) should only run on ONE instance to avoid
// duplicate work. This is the lightest-weight leader election — no external
// dependency (no Redis, no etcd), just PG advisory locks that auto-release
// on connection loss.
//
// The lock is NOT blocking: TryAcquire returns immediately. The caller
// runs a renewal loop to keep the lock alive.
package leader

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"time"
)

// ElectionKey is the advisory lock key for the leader election.
const ElectionKey = 778899 // arbitrary unique int64

// Elector manages leader election for this instance.
type Elector struct {
	db *pgxpool.Pool
	// isLeader is the current leadership status.
	isLeader bool
	// renewInterval is how often to re-acquire the advisory lock.
	renewInterval time.Duration
}

// New creates an elector. Call StartRenewal to begin maintaining leadership.
func New(db *pgxpool.Pool) *Elector {
	return &Elector{
		db:            db,
		renewInterval: 10 * time.Second,
	}
}

// IsLeader reports whether this instance currently holds the lock.
func (e *Elector) IsLeader() bool {
	return e.isLeader
}

// TryAcquire attempts to get the advisory lock (non-blocking).
// Returns true if this instance is now the leader.
func (e *Elector) TryAcquire(ctx context.Context) bool {
	var ok bool
	err := e.db.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, ElectionKey).Scan(&ok)
	if err != nil {
		slog.Warn("leader election: try acquire failed", "err", err)
		return false
	}
	e.isLeader = ok
	return ok
}

// StartRenewal runs a background loop maintaining leadership.
// If the lock is lost (PG restart, network partition), isLeader goes false
// and we retry. Context cancel stops the loop.
func (e *Elector) StartRenewal(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(e.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				e.release()
				return
			case <-ticker.C:
				e.TryAcquire(ctx)
			}
		}
	}()
}

// release drops the advisory lock.
func (e *Elector) release() {
	if e.isLeader {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, ElectionKey)
		e.isLeader = false
	}
}

// RunIfLeader runs fn only if this instance is the leader.
// For critical loops that must not run on followers.
func (e *Elector) RunIfLeader(fn func()) {
	if e.isLeader {
		fn()
	}
}
