package mota

import (
	"context"
	"time"
)

// stateFinalizationTimeout bounds the small database transaction that records
// a terminal result or releases an idempotency lease. It must outlive request
// cancellation, but must not keep a handler or background worker stuck forever.
const stateFinalizationTimeout = 5 * time.Second

// stateFinalizationContext preserves request-scoped values while detaching
// cancellation and applying a fresh, explicit persistence deadline.
func stateFinalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), stateFinalizationTimeout)
}
